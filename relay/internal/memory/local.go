package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// Compile-time proof that the store still satisfies the boundary.
var _ Connector = (*Local)(nil)

// Local is the store that ships inside the daemon. It exists because the
// alternative to a memory service being installed was no memory at all: a
// daemon with nowhere to write observes a session, answers nothing, and forgets
// it, which is indistinguishable from not being installed. Nothing here has to
// be started, fetched or configured.
//
// Everything is held in memory and the whole set is rewritten to one JSON file
// after each change. That is the right shape for what this actually holds — a
// few hundred facts a person could read in an afternoon, and one working note
// per open session — and it makes every read a comparison over a slice rather
// than a query anybody has to keep a service alive to answer. A store large
// enough for that to hurt is a store that wants mcp-memory-service, which is
// what SHOULDER_MEMORY_URL selects.
type Local struct {
	path string
	emb  Embedder

	// log is optional and reports only what happens off the request path,
	// which is the re-embedding pass: nothing above the store can see it. It
	// is set through a method rather than assigned to a field because that
	// pass starts with the store: a caller assigning to it would be writing
	// what the pass is already reading.
	logMu sync.RWMutex
	log   *slog.Logger

	mu   sync.RWMutex
	recs []Record
	vecs map[string]vector // by record id

	// reembedMu makes the pass one at a time; the store lock is never held
	// across inference.
	reembedMu sync.Mutex

	// stop and done belong to the pass the store starts on opening, which
	// writes into the store's directory on its own schedule: whoever owns the
	// directory has to be able to end it and know it has ended.
	stop context.CancelFunc
	done chan struct{}
}

// Embedder turns text into a dense vector, so the store can rank by meaning
// rather than by words in common. It is an interface and it is allowed to be
// nil: a daemon that has been given no embedding model still has to recall
// things, and lexical scoring needs nothing installed and no network.
//
// ID names the model. Vectors are stored beside the records that produced them
// and a record whose vector came from a different ID is scored lexically until
// it is written again, because comparing two models' vectors produces a number
// that looks like a similarity and means nothing.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	ID() string
}

// Vocabulary is the part of an embedder that can say which words it could not
// see. An embedder with a fixed word list produces the same vector for two
// sentences that differ only in words outside it, and the cosine between them
// is not a measurement: the model was handed the same input twice. A store
// that reads that cosine as a similarity refuses the second sentence as a
// restatement of the first, and the caller's answer to a refusal is to
// supersede what it collided with, so a fact is deleted.
//
// It is optional, and an embedder that does not implement it is taken to have
// seen every word. That is the honest default rather than the convenient one:
// a model that tokenises into word pieces has no word outside its vocabulary
// in this sense — an unfamiliar one is spelled out of pieces it does know and
// the vector moves — so there is nothing for it to report.
//
// The word asked about is one of the store's own tokens: lower case, no
// punctuation, at least two characters. An implementation may lower case
// again, and must not stem: the question is which words the model looked up
// and failed to find, and it looked up exactly these.
type Vocabulary interface {
	Knows(word string) bool
}

// vector is one record's embedding and the model that produced it.
type vector struct {
	Model  string    `json:"model"`
	Values []float32 `json:"values"`
}

// file is the on-disk shape. The version exists so a later format can be read
// rather than mistaken for a corrupt one.
type file struct {
	Version int               `json:"version"`
	Records []Record          `json:"records"`
	Vectors map[string]vector `json:"vectors,omitempty"`
}

const fileVersion = 1

// Where a write is refused as saying what the store already says, once by each
// measure. All of them are high because the two mistakes do not cost the same:
// a refusal hands the caller the record it collided with and the caller
// replaces that record, so a false collision overwrites a fact with an
// unrelated one, while a missed collision costs a redundant record that
// consolidation merges later.
//
// sparseRestatement is words in common and is the same for every model, because
// no model produced it.
const sparseRestatement = 0.9

// The embedding threshold sits above every paraphrase measured against the
// compiled-in table (0.70 to 0.82) and below every restatement of one sentence
// in another's words (0.95 and up), which includes a sentence and its own
// negation — that is a contradiction of the stored fact, and colliding is
// exactly right for it.
const (
	denseRestatement = 0.94
	// denseRestatementOpposite is where the same line falls when the two texts
	// disagree on polarity, and against this table it is not a lower number.
	//
	// It would be, if the table could read a negator. It cannot: "not" and
	// "never" are ordinary words averaged in with the rest, so a claim and its
	// negation come out almost parallel, and so does a pair of distinct facts
	// one content word apart when that word is one the table has never seen.
	// Measured over the 40 opposite-polarity pairs swept for the MiniLM
	// constant below, the strongest pair of distinct facts — "the frontend is
	// not deployed to Cloudflare" against "the backend is deployed to
	// Cloudflare" — scores 0.9899, above fifteen of the eighteen claim/negation
	// pairs, because neither "frontend" nor "backend" is in the vocabulary and
	// the two sentences reduce to the same vector. Every value below 0.94 buys
	// a contradiction at the price of a false collision: 0.92 catches nothing
	// more at all, and 0.88 catches two or three more and takes one more
	// distinct pair with it in at least one rarity regime. Lowering it here
	// would be a constant with no measurement under it, so it is not lowered.
	denseRestatementOpposite = denseRestatement
	// denseRestatementLexicalFloor is the second opinion the embedding needs
	// before a write is refused. Two sentences that mean the same thing this
	// strongly are also built of much the same words; requiring both is what
	// keeps a measure that cannot see identifiers from deciding on its own
	// that two facts are one.
	denseRestatementLexicalFloor = 0.5
)

// Where the same line falls for all-MiniLM-L6-v2, and it is not where the
// compiled-in table put it, for the same reason the search floor is not: a
// threshold is a property of the model, and the constant measured against one
// space is a different question asked of another. It moves up rather than
// down, which is the opposite of what the search floor did.
//
// Measured over 50 pairs embedded by the model itself — 20 that must collide
// (restatements and negations of one claim) and 30 that must not (distinct
// facts, most of them one content word apart inside one frame). Misses and
// false collisions are counted apart because they do not cost the same, and
// the sweep was run twice: once with the rarity table of a store holding one
// record, which is what a write is actually compared against, and once with
// one built from all fifty, because the lexical cosine moves with it and a
// constant that only holds in one of them is not a constant. Both agree here.
//
//	dense  misses/false  margin
//	0.94    12/0         0.0016
//	0.945   12/0         0.0012
//	0.95    13/0         0.0113
//	0.955   13/0         0.0063
//	0.96    13/0         0.0013
//
// The margin is the distance from the threshold to the nearest pair of either
// kind, and 0.94 has almost none: the strongest pair of genuinely different
// facts in the set, "the frontend is deployed to Cloudflare" against "the
// backend is deployed to Cloudflare", scores 0.9384. The constant clears it by
// a six-thousandth, and a refusal is a supersede, so the store is that far from
// deleting one of the two. At 0.95 it clears it by 0.0116 and sits 0.0113 under
// the weakest restatement it still catches.
//
// What it costs is one pair of the twenty: "the test suite needs network
// access" against "the test suite does not need network access", at 0.9462.
// That is the cheap mistake — a second record that Consolidate merges — and
// the one avoided is not. It is worth saying plainly that this does not make
// the model good at the question: it scores those two Cloudflare sentences
// above eleven of the twenty restatements, including a sentence and its own
// negation at 0.9176, so under this model the dense measure catches the
// restatements that happen to be worded alike and nothing else. A number is
// not the fix for that.
const (
	minilmDenseRestatement = 0.95
	// The lexical floor is not moved, because it was measured not to matter
	// here: no pair of distinct facts in the set reaches 0.95 by the embedding
	// at all, so the floor never saves one, and every restatement that does
	// reach it has 0.63 of its words in common or more, so the floor never
	// costs one either. Sweeping it from 0.35 to 0.65 changed neither count.
	minilmDenseRestatementLexicalFloor = denseRestatementLexicalFloor
)

// Where the line falls when the two texts disagree on polarity, which is a
// different question and gets a different number.
//
// The two mistakes swap places here. A contradiction that is not refused is
// not a redundant record for consolidation to merge later: the refusal is the
// only way the caller is told which record to supersede, so a missed collision
// leaves the store holding a fact and its denial at once, and every search for
// the topic answers with whichever of the two ranks higher. Under 0.95 that is
// most of them — of eighteen claim/negation pairs measured, it catches seven.
//
// Measured over 57 pairs embedded by the model itself: 18 opposite-polarity
// pairs that must collide, 22 opposite-polarity pairs that must not (distinct
// facts carrying a negator on one side, fourteen of them one content word
// apart inside one frame), and 17 same-polarity pairs carried along so the
// sweep can show that nothing on the same-polarity path moves. Swept in both
// rarity regimes — the store holding one record, which is what a write is
// actually compared against, and one built from all 57 — because the lexical
// cosine moves with the table and a constant that holds in only one of them is
// not a constant. Misses are counted over the 18, false collisions over the
// whole 57, and the two regimes are separated by a slash where they differ.
//
//	oppDense  misses  false  margin
//	0.95      11 / 9    0    0.0008
//	0.90       2 / 2    0    0.0037 / 0.0164
//	0.88       1 / 1    0    0.0021
//	0.87       1 / 1    0    0.0110
//	0.85       0 / 0    1    —
//
// 0.87 because it is the middle of the only gap there is. The strongest pair
// of distinct facts, "the frontend is not deployed to Cloudflare" against "the
// backend is deployed to Cloudflare", scores 0.8590; the weakest claim and
// negation the constant still catches, "the store deduplicates on write"
// against "the store does not deduplicate on write", scores 0.8821; 0.87 sits
// 0.0110 above the first and 0.0121 below the second.
//
// What it costs is one pair of the eighteen, "the pipeline runs on merge"
// against "the pipeline does not run on merge" at 0.8528 — which is *below*
// the strongest pair of distinct facts, so no threshold catches it without
// also deleting a fact. That is the honest shape of this measure: the two
// classes overlap by 0.0062, and the constant is a cut through an overlap
// placed so that the whole of the overlap falls on the safe side. It is not a
// separation, and no number here would be.
//
// The lexical floor is not given a second value, and not because it does the
// same work on both paths: on this one it does none. The guess it was worth
// measuring — a negation shares nearly all its words with the claim it
// denies, so the floor should sort the contradictions from the distinct facts
// — is wrong in both directions. A negator is a word the claim does not have,
// and on a short fact that is most of the difference: "the daemon needs a
// token" against "the daemon does not need a token" shares 0.5572 of its
// words. A distinct pair one content word apart shares nearly all of them:
// "the reader lock is not held during search" against "the writer lock is held
// during search" reaches 0.7455. The classes overlap by 0.19, so no floor
// separates them, and every floor high enough to exclude the second throws
// away most of the first.
//
// 0.50 stands because it is free there and is still the second opinion that
// stops an embedding refusing a write on its own: at 0.87 every verdict in the
// set is unchanged for any floor from 0 to 0.5572, so it has 0.0572 of
// headroom in the tighter of the two rarity regimes.
const minilmDenseRestatementOpposite = 0.87

// restatement is the pair of thresholds a refusal is judged by, so that a model
// entry cannot supply one of them and silently inherit the other's calibration.
type restatement struct {
	dense float64
	// opposite is the dense threshold for a write whose polarity disagrees
	// with the record it is being compared against. It is a field of its own
	// rather than a factor applied to dense because the two are separate
	// measurements of separate questions, and a model that moves one has said
	// nothing about the other.
	opposite     float64
	lexicalFloor float64
}

// denseFor is the threshold this pair judges a write by, given whether the two
// texts agree on polarity.
func (r restatement) denseFor(agrees bool) float64 {
	if agrees {
		return r.dense
	}
	return r.opposite
}

// restatements is the pair per model, keyed on the id the vectors carry —
// which is the model that actually embedded them, not the one the store was
// configured with, because the composite embedder answers with the fallback
// until the model has landed. A model that is not in the table gets the
// compiled-in table's numbers.
//
// The MiniLM id is spelled out rather than imported for the same reason
// minScores spells it out: the package that produces it imports this one.
var restatements = map[string]restatement{
	"all-minilm-l6-v2-f32-v1": {
		dense:        minilmDenseRestatement,
		opposite:     minilmDenseRestatementOpposite,
		lexicalFloor: minilmDenseRestatementLexicalFloor,
	},
}

// restatementFor is the pair of thresholds for a write whose vector came from
// model. The empty model is a write with no embedding at all, which never
// reaches the dense measure.
func restatementFor(model string) restatement {
	if r, ok := restatements[model]; ok {
		return r
	}
	return restatement{
		dense:        denseRestatement,
		opposite:     denseRestatementOpposite,
		lexicalFloor: denseRestatementLexicalFloor,
	}
}

// defaultMinScore is the floor a search applies when the caller names none and
// nothing is known about what scored it. Without a floor every search answers
// with the whole scope in ranked order, and the advisor is handed the least
// irrelevant fact in the store as though it were relevant. Calibrated against
// the shipping table: a fact that answers the query scores 0.68 and up, and
// unrelated ones sit below 0.45. It is also the floor for a search with no
// embedding at all, where the whole of the score is words in common.
const defaultMinScore = 0.35

// minilmMinScore is the same floor for all-MiniLM-L6-v2, and it is a different
// number because a floor is a property of the model rather than of the store:
// the two spaces put their cosines in different places, so the constant
// measured against one of them is a different question asked of the other. At
// 0.35 this model answers ten of the comparison corpus's fifteen questions and
// ranks all fifteen correctly — five right answers thrown away by a constant,
// not by the ranking.
//
// Swept over that corpus with the model ready before the first write, recall@1
// is 14/15 at both 0.10 and 0.15, 13/15 at 0.20 and 0.25, 12/15 at 0.30 and
// 10/15 at 0.35. The two that tie on recall do not tie on what else they
// return: 0.10 hands back 5.9 records a query out of a store of twenty, and
// 0.15 hands back 3.1. 0.15 is therefore the highest floor that costs no
// answer.
//
// It costs none by very little, and that is worth knowing before anybody
// raises it: the weakest correct first place is bracketed between 0.1505 and
// 0.152, so it clears this floor by about a hundredth of itself. Nothing here
// separates that question from the fifteenth one either, which no stored fact
// answers and whose best wrong answer scores 0.36 — above every floor swept.
// A floor is a blunt instrument on this corpus and the honest reading of the
// sweep is that anything from 0.10 to 0.15 ranks identically and differs only
// in how much of the store it hands over.
const minilmMinScore = 0.15

// minScores is the floor per model, keyed on the id the query's vector carries
// — which is the model that actually scored it, not the one the store was
// configured with, because the composite embedder answers with the fallback
// until the model has landed. A model that is not in the table gets the
// default.
//
// The MiniLM id is spelled out rather than imported because the package that
// produces it imports this one, and inverting that would drag the model
// runtime into every build of the store.
var minScores = map[string]float64{
	"all-minilm-l6-v2-f32-v1": minilmMinScore,
}

// minScoreFor is the floor for a search whose query vector came from model.
// The empty model is a search with no embedding at all.
func minScoreFor(model string) float64 {
	if floor, ok := minScores[model]; ok {
		return floor
	}
	return defaultMinScore
}

// denseWeight is how much of a search score is meaning rather than words in
// common. It is not a half because the embedding is the better measure on the
// question this store exists to answer — recalling a fact somebody worded
// differently — and the words are there to break the ties it cannot see.
const denseWeight = 0.65

// DefaultLocalPath is where the facts live when nobody says otherwise. It
// follows the XDG data directory rather than the config one because this is
// data the user did not write by hand and cannot usefully edit.
func DefaultLocalPath() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "shoulder-daemon", "facts.json")
}

// DefaultModelDir is where downloaded embedding models live when nobody says
// otherwise. It is under the XDG cache directory rather than the data one
// because a model is a copy of something published: deleting it costs a
// download, never a fact.
func DefaultModelDir() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "shoulder-daemon", "models")
}

// NewLocal opens the store at path, creating nothing until something is
// written. A file that cannot be parsed is an error rather than an empty store:
// starting fresh on top of somebody's facts would silently discard them.
func NewLocal(path string, emb Embedder) (*Local, error) {
	if path == "" {
		return nil, errors.New("local store has no path")
	}
	l := &Local{path: path, emb: emb, vecs: map[string]vector{}}

	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own setting
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil
	case err != nil:
		return nil, fmt.Errorf("local store %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		var f file
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("local store %s is not readable: %w", path, err)
		}
		l.recs = f.Records
		if f.Vectors != nil {
			l.vecs = f.Vectors
		}
	}
	// The vectors on disk are whatever model wrote them. Bringing them up to
	// the current one is done behind the store, not before it opens: a store
	// is useful on words alone the moment it is read, and a model that takes
	// minutes to arrive must not hold a session's recall for that long. An
	// empty store starts the watcher too: everything written before the model
	// arrives carries the fallback's vector and is scored on words against
	// the model's queries until something brings it up to date.
	ctx, stop := context.WithCancel(context.Background())
	l.stop, l.done = stop, make(chan struct{})
	go l.watch(ctx)
	return l, nil
}

// Close ends the re-embedding pass and returns once it has, so nothing writes
// to the store's directory afterwards. Every vector the pass already made is
// saved before it stops, and so is one the embedder still hands back for the
// record in progress; an embedder that gives that record up on the
// cancellation leaves it, with the records the pass had not reached, for the
// next start. The store still answers calls made after it; only the work
// nobody asked for is stopped. It is safe to call more than once.
func (l *Local) Close() error {
	l.stop()
	<-l.done
	return nil
}

// SetLog gives the store somewhere to report its own background work. Nil is
// legal and means it reports nothing.
func (l *Local) SetLog(log *slog.Logger) {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	l.log = log
}

func (l *Local) logger() *slog.Logger {
	l.logMu.RLock()
	defer l.logMu.RUnlock()
	return l.log
}

func (l *Local) Name() string { return "local" }

// Path is where this store writes, for a daemon that wants to say so once at
// startup. Somebody looking for their facts should not have to guess.
func (l *Local) Path() string { return l.path }

// Len is the number of current records. It exists for the startup line and for
// tests; nothing in the hot path asks.
func (l *Local) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.recs)
}

func (l *Local) Store(ctx context.Context, r Record) (string, error) {
	r.Project = storedProject(r)
	r.Dir = ""
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	r.ID = contentID(r.Content)

	vec := l.embed(ctx, r.Content)

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, existing := range l.recs {
		if existing.ID == r.ID {
			return "", ErrDuplicateExact
		}
	}
	// Deduplication is confined to the place the record would land. Two
	// projects saying the same sentence about themselves are two facts, and a
	// store that refused the second one would answer the wrong project's
	// question with the first project's record.
	if collided := newRanker(l.recs, l.vecs, l.emb).similar(r, vec); collided != "" {
		return "", &ErrDuplicateSemantic{Collided: collided}
	}

	l.recs = append(l.recs, r)
	if vec != nil {
		l.vecs[r.ID] = *vec
	}
	if err := l.saveLocked(); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Supersede replaces one record with another in the place the original was.
// The target is looked up rather than trusted: a caller naming a record that is
// gone, or one belonging to another project, is corrected rather than obeyed,
// because writing anyway would move knowledge instead of fixing it.
func (l *Local) Supersede(ctx context.Context, oldID string, r Record) (string, error) {
	r.Project = storedProject(r)
	r.Dir = ""
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	r.ID = contentID(r.Content)

	vec := l.embed(ctx, r.Content)

	l.mu.Lock()
	defer l.mu.Unlock()

	at := -1
	for i := range l.recs {
		if l.recs[i].ID == oldID {
			at = i
			break
		}
	}
	if at < 0 {
		return "", &ErrCrossScopeSupersede{OldID: oldID, Scope: r.Scope, Project: r.Project}
	}
	if !samePlace(l.recs[at], r) {
		return "", &ErrCrossScopeSupersede{
			OldID: oldID, Scope: r.Scope, Project: r.Project, Elsewhere: true,
		}
	}
	// A replacement whose content is already stored elsewhere would leave two
	// records with one id, and the id is how everything above this package
	// talks about a record.
	if r.ID != oldID {
		for i := range l.recs {
			if i != at && l.recs[i].ID == r.ID {
				return "", ErrDuplicateExact
			}
		}
	}

	delete(l.vecs, oldID)
	l.recs[at] = r
	if vec != nil {
		l.vecs[r.ID] = *vec
	}
	if err := l.saveLocked(); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Forget deletes one record. Naming a record that is in another scope is a
// no-op rather than a deletion, because deletion is the one verb with nothing
// to fall back on and a caller that has the wrong place has the wrong record.
func (l *Local) Forget(_ context.Context, id string, where Query) error {
	if id == "" {
		return ErrForgetUnidentified
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for i := range l.recs {
		if l.recs[i].ID != id {
			continue
		}
		if where.Scope != scope.Any && !inScope(l.recs[i], where) {
			return nil
		}
		l.recs = append(l.recs[:i], l.recs[i+1:]...)
		delete(l.vecs, id)
		return l.saveLocked()
	}
	// Already gone is what the caller wanted, and a janitor tidying up after a
	// session that crashed asks for records it may have removed already.
	return nil
}

// List is exhaustive within one scope, newest first. It is what a digest reads,
// which is why the boundary refuses to call it without a scope.
func (l *Local) List(_ context.Context, q Query) ([]Record, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]Record, 0, len(l.recs))
	for _, r := range l.recs {
		if !matches(r, q) {
			continue
		}
		out = append(out, readable(r, q))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Search ranks by similarity and returns only what it actually matched. A
// record it could not score at all is not a weak answer to the question, it is
// not an answer, so it is left out rather than returned with a zero that a
// caller's floor is then forbidden to drop.
func (l *Local) Search(ctx context.Context, q Query) ([]Record, error) {
	qvec := l.embed(ctx, q.Text)

	l.mu.RLock()
	defer l.mu.RUnlock()

	return newRanker(l.recs, l.vecs, l.emb).search(q, qvec), nil
}

// sameNumbers reports whether two texts carry the same numbers. It is the one
// literal comparison in a similarity measure, and it is here because numbers
// are precisely what the similarity cannot read: to an embedding built from
// English, 8081 and 8082 are the same absence of a word.
func sameNumbers(a, b string) bool {
	return sameWords(numbers(a), numbers(b))
}

// unknownWords is the tokens of s that the embedder could not look up. Two
// texts whose sets differ are two texts the embedding did not read the
// difference between, and comparing those sets is the same literal comparison
// the numbers get, for the same reason: those words are precisely what the
// similarity cannot see. The table of 40,000 English words that ships has
// never seen "frontend", "backend" or "cloudflare", so "the frontend is
// deployed to Cloudflare" and "the backend is deployed to Cloudflare" reduce
// to the three words it does hold and embed to the identical vector — a cosine
// of exactly 1.0000, measured.
//
// The tokeniser is the store's own, which is the one the compiled-in table
// tokenises with too, so these are the words it actually failed to find rather
// than a second guess at them. A nil vocabulary is an embedder that was not
// asked or did not answer: every word counts as seen and the guard is inert.
func unknownWords(v Vocabulary, s string) map[string]struct{} {
	if v == nil {
		return nil
	}
	out := map[string]struct{}{}
	for _, token := range tokenise(s) {
		if !v.Knows(token) {
			out[oneForm(token)] = struct{}{}
		}
	}
	return out
}

// oneForm folds a trailing s, so that a word and its plural — or a verb and
// its third person — are one unknown rather than two.
//
// The store's own tokeniser refuses to stem, and says why: a stemmer is a
// fixed opinion about a vocabulary nobody here has seen. This is the one place
// that reasoning inverts. What reaches here is by construction the words the
// table does not hold, and the table holds ordinary English in every form
// these facts use — deploy, deploys, deployed, deployment; service, services;
// branch, branches; run, runs, ran, running — so what is left is jargon,
// identifiers and product names, and for those two forms are one word.
//
// It is not a nicety. English negates with do-support, which moves the verb:
// "the store deduplicates on write" is denied by "the store does not
// deduplicate on write", and neither form of that verb is in the table. Read
// literally, the two texts differ in a word the model could not see, the guard
// refuses to let the embedding speak, and the contradiction is never refused
// — which on the opposite-polarity path is the expensive mistake, because a
// contradiction that is not refused leaves the store holding a fact and its
// denial at once and answering with whichever ranks higher.
//
// Measured over the 57-pair polarity set and the 51-pair set under the
// compiled-in table, folding is the strictly better of the two decisions: it
// costs no false collision on either set and recovers that pair, which is the
// only pair in 108 that the choice moves at all.
//
// What it risks is two distinct out-of-vocabulary words one trailing s apart,
// which fold together and let the embedding decide after all. Neither set
// holds such a pair, and pluralising a noun in an otherwise identical sentence
// states the same fact rather than a different one, which is why.
func oneForm(token string) string {
	// Two characters or fewer are left alone. The tokeniser has already
	// dropped everything shorter than two, so folding "js" to "j" would
	// produce a token that cannot occur on the other side.
	if len(token) > 2 && strings.HasSuffix(token, "s") {
		return token[:len(token)-1]
	}
	return token
}

// sameWords reports whether two sets of tokens hold the same tokens.
func sameWords(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for token := range a {
		if _, ok := b[token]; !ok {
			return false
		}
	}
	return true
}

func numbers(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, token := range tokenise(s) {
		if strings.ContainsFunc(token, unicode.IsDigit) {
			out[token] = struct{}{}
		}
	}
	return out
}

func (l *Local) embed(ctx context.Context, text string) *vector {
	return embedWith(ctx, l.emb, text)
}

// embedWith is a no-op without an embedding model, which is the shipping
// default. A failed embedding is not a failed write: the record is still
// stored and still found lexically, which is what the store does anyway when
// no model is configured at all.
func embedWith(ctx context.Context, emb Embedder, text string) *vector {
	v, err := embedTagged(ctx, emb, text)
	if err != nil {
		return nil
	}
	return v
}

// embedTagged embeds text and names the model that did it. The name is read
// before and after, because an embedder that is loading a better model can
// switch between the two calls, and a vector from one space under the other's
// name would be compared as though it belonged there. One retry is enough: a
// model arrives once.
func embedTagged(ctx context.Context, emb Embedder, text string) (*vector, error) {
	if emb == nil || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	for range 2 {
		model := emb.ID()
		values, err := emb.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		if emb.ID() != model {
			continue
		}
		if len(values) == 0 {
			return nil, nil
		}
		return &vector{Model: model, Values: values}, nil
	}
	return nil, errors.New("the embedding model changed during inference")
}

// saveLocked writes the whole store through a temporary file in the same
// directory, so a crash mid-write leaves the previous facts rather than half of
// the new ones.
func (l *Local) saveLocked() error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("local store %s: %w", dir, err)
	}
	body, err := json.MarshalIndent(file{Version: fileVersion, Records: l.recs, Vectors: l.vecs}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".facts-*.json")
	if err != nil {
		return fmt.Errorf("local store %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name) // a no-op once the rename below has succeeded
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, l.path)
}

// matches is the scope, project and kind filter both reads share. Kind is
// compared exactly, which makes a query that never mentioned it a query for
// facts.
func matches(r Record, q Query) bool {
	if r.Kind != q.Kind {
		return false
	}
	if q.Scope == scope.Any {
		return true
	}
	return inScope(r, q)
}

func inScope(r Record, q Query) bool {
	if r.Scope != q.Scope {
		return false
	}
	if q.Scope != scope.Local {
		return true
	}
	return r.ProjectKey() == scope.Key(q.Project)
}

// samePlace reports whether two records belong in the same scope and project.
func samePlace(a, b Record) bool {
	if a.Scope != b.Scope {
		return false
	}
	if a.Scope != scope.Local {
		return true
	}
	return a.ProjectKey() == b.ProjectKey()
}

// storedProject is what goes on disk: the key, never the path. The daemon is
// the only reader of this file today, but a project path is the one piece of
// local layout in a record, and there is no reason for it to be written down to
// be read back by something that already knows it.
func storedProject(r Record) string {
	if r.Scope != scope.Local || r.Project == "" {
		return ""
	}
	return r.ProjectKey()
}

// readable is a record as a caller gets it: the project it asked about, rather
// than the key that is stored, when it named one.
func readable(r Record, q Query) Record {
	if q.Project != "" && r.Scope == scope.Local {
		r.Project = q.Project
	}
	return r
}

// contentID is the record's handle. It is the hash of the content because a
// supersede has to produce a different id from the record it replaces, and
// because two writes of the same sentence are the same fact.
func contentID(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// tokenise is deliberately plain: lower case, split on anything that is not a
// letter or digit, drop single characters. There is no stemming and no stop
// list, because the inverse document frequency above already discounts the
// words this store sees everywhere, and a stop list is a fixed opinion about a
// vocabulary nobody here has seen.
func tokenise(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if len(f) > 1 {
			out = append(out, f)
		}
	}
	return out
}

// terms is what a search compares words on: the tokens, and every adjacent
// pair of them. A bag of words cannot tell "staging deploys" from "deploys
// staging", and most of what this store holds is short sentences that share
// their vocabulary and differ in how it is arranged. A pair is a term like any
// other, so its rarity is measured the same way — and a phrase the store has
// seen once is rare, which is what makes matching it count for something.
func terms(s string) []string {
	tokens := tokenise(s)
	out := make([]string, 0, 2*len(tokens))
	out = append(out, tokens...)
	return append(out, bigrams(tokens)...)
}

func bigrams(tokens []string) []string {
	if len(tokens) < 2 {
		return nil
	}
	out := make([]string, 0, len(tokens)-1)
	for i := 1; i < len(tokens); i++ {
		out = append(out, tokens[i-1]+" "+tokens[i])
	}
	return out
}

// samePolarity reports whether two texts agree on whether they are negated.
// It is a coarser test than counting: "do not need it any more" and "don't
// need it" carry two negators and one, and are the same claim.
func samePolarity(a, b string) bool {
	return (negators(a) > 0) == (negators(b) > 0)
}

// negators counts the words that flip a sentence. It reads the text itself
// rather than the tokens, because the tokeniser strips "can't" to "can" and
// "won't" to "won", and both of those are ordinary words.
func negators(s string) int { return len(negatorWords(s)) }

func negatorWords(s string) []string {
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\'' && r != '\u2019'
	})
	var found []string
	for i, w := range words {
		w = strings.ReplaceAll(w, "\u2019", "'")
		switch {
		case strings.HasSuffix(w, "n't"), w == "cannot":
			found = append(found, w)
		case w == "not", w == "never", w == "no", w == "without", w == "anymore":
			found = append(found, w)
		case w == "any" && i+1 < len(words) && words[i+1] == "more":
			found = append(found, "any more")
		}
	}
	return found
}

// Negator names the first word in s this store's polarity gate reads as a
// negation, if there is one. It is exported for the measurements that ask
// whether a model followed the affirmative-form rule the write prompts state:
// the alternative was a second copy of this list living in a test file, which
// would answer for a polarity gate that is not the one the store runs.
func Negator(s string) (string, bool) {
	if found := negatorWords(s); len(found) > 0 {
		return found[0], true
	}
	return "", false
}

func set(tokens []string) map[string]struct{} {
	out := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		out[t] = struct{}{}
	}
	return out
}

// weigh turns tokens into a term-frequency vector weighted by rarity.
func weigh(tokens []string, idf map[string]float64) map[string]float64 {
	if len(tokens) == 0 {
		return nil
	}
	out := make(map[string]float64, len(tokens))
	for _, t := range tokens {
		w, ok := idf[t]
		if !ok {
			// A word this store has never held is as rare as a word it holds
			// once, not infinitely rare: the query is not evidence about the
			// store's vocabulary.
			w = math.Log(2)
		}
		out[t] += w
	}
	return out
}

// sparse is the cosine of two weighted bags of words, in [0,1].
func sparse(a, b map[string]float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, na, nb float64
	for t, w := range a {
		na += w * w
		if v, ok := b[t]; ok {
			dot += w * v
		}
	}
	for _, w := range b {
		nb += w * w
	}
	if dot == 0 || na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// phraseBonus is how much a match on word order can add to a search score.
// Measured over the comparison corpus, pairs inside the cosine cost two
// questions at full weight and gained none at any weight, because a question
// and the fact that answers it are seldom phrased alike: every pair that fails
// to match swells the norm and drags the answer towards the floor without
// moving the wrong answers relative to it. As a bonus the pairs can only lift a
// record, and the margin between the answer and the runner-up widened on every
// question where they matched.
const phraseBonus = 0.2

// phrased scores a question against a record: the cosine over the words,
// raised by the cosine over the pairs of adjacent words. Deduplication does not
// use it, and compares the words alone: in a store of corrections the same
// words in another order are a contradiction of the same fact, and colliding is
// what lets the write supersede it.
func phrased(a, b map[string]float64) float64 {
	score := sparse(single(a), single(b)) + phraseBonus*sparse(paired(a), paired(b))
	if score > 1 {
		return 1
	}
	return score
}

func single(m map[string]float64) map[string]float64 { return part(m, false) }
func paired(m map[string]float64) map[string]float64 { return part(m, true) }

func part(m map[string]float64, pairs bool) map[string]float64 {
	out := make(map[string]float64, len(m))
	for t, w := range m {
		if strings.ContainsRune(t, ' ') == pairs {
			out[t] = w
		}
	}
	return out
}

// dense is the cosine of two embeddings, clamped into [0,1] so that a score
// from a model and a score from words in common are the same kind of number to
// a caller comparing them against one floor.
func dense(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	cos := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if cos < 0 {
		return 0
	}
	if cos > 1 {
		return 1
	}
	return cos
}
