package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	mu   sync.RWMutex
	recs []Record
	vecs map[string]vector // by record id
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
// measure. Both are high because the two mistakes do not cost the same: a
// refusal hands the caller the record it collided with and the caller replaces
// that record, so a false collision overwrites a fact with an unrelated one,
// while a missed collision costs a redundant record that consolidation merges
// later.
//
// The embedding threshold sits above every paraphrase measured against this
// table (0.70 to 0.82) and below every restatement of one sentence in another's
// words (0.95 and up), which includes a sentence and its own negation — that is
// a contradiction of the stored fact, and colliding is exactly right for it.
const (
	denseRestatement = 0.94
	// denseRestatementLexicalFloor is the second opinion the embedding needs
	// before a write is refused. Two sentences that mean the same thing this
	// strongly are also built of much the same words; requiring both is what
	// keeps a measure that cannot see identifiers from deciding on its own
	// that two facts are one.
	denseRestatementLexicalFloor = 0.5
	sparseRestatement            = 0.9
)

// defaultMinScore is the floor a search applies when the caller names none.
// Without one every search answers with the whole scope in ranked order, and
// the advisor is handed the least irrelevant fact in the store as though it
// were relevant. Calibrated against the shipping table: a fact that answers the
// query scores 0.68 and up, and unrelated ones sit below 0.45.
const defaultMinScore = 0.35

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
		return l, nil
	case err != nil:
		return nil, fmt.Errorf("local store %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return l, nil
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("local store %s is not readable: %w", path, err)
	}
	l.recs = f.Records
	if f.Vectors != nil {
		l.vecs = f.Vectors
	}
	return l, nil
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
	if collided := l.similarLocked(r, vec); collided != "" {
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
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	floor := q.MinScore
	if floor <= 0 {
		floor = defaultMinScore
	}

	qvec := l.embed(ctx, q.Text)

	l.mu.RLock()
	defer l.mu.RUnlock()

	candidates := make([]Record, 0, len(l.recs))
	for _, r := range l.recs {
		if matches(r, q) {
			candidates = append(candidates, r)
		}
	}

	idf := l.idfLocked()
	qtokens := weigh(tokenise(q.Text), idf)

	scored := make([]Record, 0, len(candidates))
	for _, r := range candidates {
		score := l.scoreLocked(r, qvec, qtokens, idf)
		if score <= 0 {
			continue
		}
		if score < floor {
			continue
		}
		out := readable(r, q)
		out.Score = score
		scored = append(scored, out)
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	return scored, nil
}

// scoreLocked prefers the embedding when both sides have one from the same
// model, and falls back to words in common otherwise. The fallback is not a
// degraded mode to apologise for: it is the only scoring a daemon with no
// embedding model has, and it is what makes the store work out of the box.
func (l *Local) scoreLocked(r Record, qvec *vector, qtokens map[string]float64, idf map[string]float64) float64 {
	lexical := sparse(qtokens, weigh(tokenise(r.Content), idf))
	if qvec != nil {
		if v, ok := l.vecs[r.ID]; ok && v.Model == qvec.Model {
			// Both, weighted towards meaning. The embedding answers the
			// question the words cannot — a fact worded differently — and the
			// words answer the one the embedding cannot: which of eight
			// sentences about services and ports is the one about this
			// service. Measured over a corpus with both kinds in it, the pair
			// beats either alone.
			return denseWeight*dense(qvec.Values, v.Values) + (1-denseWeight)*lexical
		}
	}
	return lexical
}

// similarLocked reports the record a write would be a restatement of, if there
// is one. Only the scope and kind the write is landing in are considered, and
// both measures get a say: the embedding catches the same claim in different
// words, and words in common catch a sentence about identifiers and paths that
// no embedding table has ever seen.
func (l *Local) similarLocked(r Record, vec *vector) string {
	idf := l.idfLocked()
	tokens := weigh(tokenise(r.Content), idf)
	for _, existing := range l.recs {
		if !samePlace(existing, r) || existing.Kind != r.Kind {
			continue
		}
		// A sentence that differs only in a number is not a restatement of it.
		// Ports, versions, namespaces, table numbers: the model cannot see any
		// of them — a numeric token is not in its vocabulary and contributes
		// nothing to the vector — so a family of facts about eight services
		// reads as one fact repeated, and the second write is refused as a
		// restatement of the first. The caller's answer to a refusal is to
		// supersede what it collided with, so the family collapses to whichever
		// member was written last and every question about the others is
		// answered confidently and wrongly. Measured, not imagined: eight facts
		// in, five kept.
		if !sameNumbers(r.Content, existing.Content) {
			continue
		}
		lexical := sparse(tokens, weigh(tokenise(existing.Content), idf))
		if vec != nil {
			if v, ok := l.vecs[existing.ID]; ok && v.Model == vec.Model {
				// Both measures, because at this similarity a restatement is
				// close in words as well: an embedding alone at 0.94 is
				// sometimes two different facts wearing the same sentence.
				if dense(vec.Values, v.Values) >= denseRestatement && lexical >= denseRestatementLexicalFloor {
					return existing.ID
				}
			}
		}
		if lexical >= sparseRestatement {
			return existing.ID
		}
	}
	return ""
}

// sameNumbers reports whether two texts carry the same numbers. It is the one
// literal comparison in a similarity measure, and it is here because numbers
// are precisely what the similarity cannot read: to an embedding built from
// English, 8081 and 8082 are the same absence of a word.
func sameNumbers(a, b string) bool {
	na, nb := numbers(a), numbers(b)
	if len(na) != len(nb) {
		return false
	}
	for token := range na {
		if _, ok := nb[token]; !ok {
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

// idfLocked weighs a word by how rare it is in this store, so that a term every
// record shares — the name of the project, the vocabulary of the work — stops
// counting as evidence of anything.
func (l *Local) idfLocked() map[string]float64 {
	df := map[string]int{}
	for _, r := range l.recs {
		for token := range set(tokenise(r.Content)) {
			df[token]++
		}
	}
	n := float64(len(l.recs))
	idf := make(map[string]float64, len(df))
	for token, count := range df {
		// 1 + n/df rather than n/df: with the latter a term present in every
		// record weighs nothing, and in a store holding one record that is
		// every term, so the only fact there is could never be recalled.
		idf[token] = math.Log(1 + n/float64(count))
	}
	return idf
}

// embed is a no-op without an embedding model, which is the shipping default.
func (l *Local) embed(ctx context.Context, text string) *vector {
	if l.emb == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	values, err := l.emb.Embed(ctx, text)
	if err != nil || len(values) == 0 {
		// A failed embedding is not a failed write. The record is still stored
		// and still found lexically, which is what the store does anyway when
		// no model is configured at all.
		return nil
	}
	return &vector{Model: l.emb.ID(), Values: values}
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
