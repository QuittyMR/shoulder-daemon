package memory

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// openLocal opens the store at path and closes it when the test ends. Cleanups
// run last-registered first, so the close lands before the removal of a
// temporary directory the path was taken from, which the re-embedding pass
// would otherwise still be writing into.
func openLocal(t *testing.T, path string, emb Embedder) *Local {
	t.Helper()
	l, err := NewLocal(path, emb)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// newLocal builds the store as the daemon ships it: with the embedding table
// compiled into the binary. A test against a store scoring some other way is a
// test of something nobody runs.
func newLocal(t *testing.T) *Local {
	t.Helper()
	return openLocal(t, filepath.Join(t.TempDir(), "facts.json"), vectors.Embedder{})
}

// newLexicalLocal is the store with no embedding model, which is what a build
// whose table failed to load falls back to.
func newLexicalLocal(t *testing.T) *Local {
	t.Helper()
	return openLocal(t, filepath.Join(t.TempDir(), "facts.json"), nil)
}

// The store that ships is held to the same contract as the one that talks to a
// service, because it is the one almost everybody will actually run.
func TestLocalConformance(t *testing.T) {
	dir := t.TempDir()
	n := 0
	TestConnector(t, func() Connector {
		n++
		l := openLocal(t, filepath.Join(dir, "facts-"+strings.Repeat("x", n)+".json"), vectors.Embedder{})
		return l
	})
}

func TestLocalKeepsFactsAcrossARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	first := openLocal(t, path, vectors.Embedder{})
	const fact = "the integration tests need a live Postgres"
	id, err := first.Store(ctx, Record{Content: fact, Category: "structure", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// The daemon exits when the last session ends and is started again by the
	// editor, so this is the ordinary case, not a disaster case.
	second := openLocal(t, path, vectors.Embedder{})
	got, err := second.List(ctx, Query{Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Content != fact {
		t.Fatalf("the facts did not survive the restart: %+v", got)
	}
	if got[0].ID != id || got[0].Category != "structure" {
		t.Errorf("the record came back different: %+v", got[0])
	}
}

func TestLocalRefusesToOpenAFileItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "facts.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Starting empty here would leave the daemon writing a fresh store over
	// somebody's facts and reporting itself healthy while it did it.
	if _, err := NewLocal(path, nil); err == nil {
		t.Fatal("an unreadable store must be an error, not an empty one")
	}
}

func TestLocalStoresNothingUntilSomethingIsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "facts.json")
	openLocal(t, path, nil)
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening the store must not create anything on disk")
	}
}

func TestLocalWritesTheFileOnlyToItsOwner(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	if _, err := l.Store(ctx, Record{Content: "prefers terse answers", Scope: scope.Global}); err != nil {
		t.Fatalf("store: %v", err)
	}
	info, err := os.Stat(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	// Everything a person has ever said in front of an agent is in this file.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %o, want 600", perm)
	}
}

func TestLocalRefusesTheSameFactTwice(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	rec := Record{Content: "the main branch is master", Scope: scope.Global}
	if _, err := l.Store(ctx, rec); err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := l.Store(ctx, rec); !errors.Is(err, ErrDuplicateExact) {
		t.Fatalf("got %v, want ErrDuplicateExact", err)
	}
}

// Reconciliation depends on this: the refusal is what tells the pipeline to
// supersede the record it collided with rather than write a second wording.
func TestLocalRefusesARestatementAndNamesWhatItCollidedWith(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	id, err := l.Store(ctx, Record{
		Content: "the deployment script lives in bin/ship and is run by hand",
		Scope:   scope.Global,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_, err = l.Store(ctx, Record{
		Content: "the deployment script lives in bin/ship and is run by hand.",
		Scope:   scope.Global,
	})
	var dup *ErrDuplicateSemantic
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want ErrDuplicateSemantic", err)
	}
	if dup.Collided != id {
		t.Errorf("collided with %q, want %q; a caller cannot supersede what it is not told about", dup.Collided, id)
	}
}

// Two projects are allowed to say the same thing about themselves.
func TestLocalDeduplicatesOnlyWithinOnePlace(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	const same = "the main branch is called master"
	if _, err := l.Store(ctx, Record{Content: same, Scope: scope.Local, Project: "/a"}); err != nil {
		t.Fatalf("store in /a: %v", err)
	}
	// The content hash is the id, so the same sentence in two projects is one
	// record either way; what must not happen is the second write being
	// refused as a restatement of the first project's knowledge.
	_, err := l.Store(ctx, Record{Content: same + " here", Scope: scope.Local, Project: "/b"})
	if err != nil {
		t.Fatalf("store in /b: %v", err)
	}
	got, err := l.List(ctx, Query{Scope: scope.Local, Project: "/b"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the second project holds %d records, want 1: %+v", len(got), got)
	}
}

func TestLocalRanksTheRelevantFactFirst(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	for _, content := range []string{
		"the integration tests need a live Postgres on port 5544",
		"lunch is at one",
		"releases are cut on the last Thursday of the month",
	} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("store %q: %v", content, err)
		}
	}
	got, err := l.Search(ctx, Query{Text: "which port does Postgres run on for the tests", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("nothing was recalled")
	}
	if !strings.Contains(got[0].Content, "Postgres") {
		t.Fatalf("the wrong record ranked first: %+v", got)
	}
	if got[0].Score <= 0 {
		t.Errorf("a ranked answer must carry a score, got %v", got[0].Score)
	}
}

// A search is not a listing. Returning the records it could not match at all,
// scored zero, would hand the advisor the whole store as though it were an
// answer.
func TestLocalSearchLeavesOutWhatItCouldNotMatch(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	for _, content := range []string{"the CI runner is self-hosted", "the office cat is called Biscuit"} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("store: %v", err)
		}
	}
	got, err := l.Search(ctx, Query{Text: "runner self-hosted CI", Limit: 10, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range got {
		if strings.Contains(r.Content, "Biscuit") {
			t.Fatalf("an unrelated record was returned: %+v", got)
		}
	}
}

// One fact is the state every new install is in, and the rarity weighting has
// to survive it: with one record every word in the store is in every record.
func TestLocalRecallsTheOnlyFactItHolds(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	if _, err := l.Store(ctx, Record{Content: "the main branch is called master", Scope: scope.Global}); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, err := l.Search(ctx, Query{Text: "what is the main branch called", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a store holding one fact recalled %d: %+v", len(got), got)
	}
}

// The project path is local layout. Nothing needs it on disk to answer a
// question asked from inside that project.
func TestLocalWritesNoProjectPathToDisk(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	const project = "/home/somebody/Software/secret-client-work"
	if _, err := l.Store(ctx, Record{Content: "the API is versioned in the path", Scope: scope.Local, Project: project}); err != nil {
		t.Fatalf("store: %v", err)
	}
	raw, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), project) {
		t.Error("the project path was written to the store")
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("the store is not readable JSON: %v", err)
	}
	if len(f.Records) != 1 || f.Records[0].Project != scope.Key(project) {
		t.Fatalf("the project was stored as %+v", f.Records)
	}
}

// stubEmbedder scores by hand: two texts are close when they share their first
// word, which no lexical measure here would agree with, so a test can tell
// which of the two rankings actually ran.
type stubEmbedder struct{ id string }

func (s stubEmbedder) ID() string { return s.id }

func (s stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, 26)
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 {
		return v, nil
	}
	if c := fields[0][0]; c >= 'a' && c <= 'z' {
		v[c-'a'] = 1
	}
	return v, nil
}

func TestLocalRanksByTheEmbeddingWhenItHasOne(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	l := openLocal(t, path, stubEmbedder{id: "stub-v1"})
	for _, content := range []string{"zebra crossing the road", "postgres runs on 5544"} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("store: %v", err)
		}
	}
	got, err := l.Search(ctx, Query{Text: "zebra unrelated words entirely", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || !strings.HasPrefix(got[0].Content, "zebra") {
		t.Fatalf("the embedding was not used to rank: %+v", got)
	}
}

// Two models' vectors are not comparable, and a number that looks like a
// similarity but is not is worse than no number.
func TestLocalIgnoresVectorsFromAnotherModel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	first := openLocal(t, path, stubEmbedder{id: "stub-v1"})
	if _, err := first.Store(ctx, Record{Content: "the deploy target is staging", Scope: scope.Global}); err != nil {
		t.Fatalf("store: %v", err)
	}

	second := openLocal(t, path, stubEmbedder{id: "stub-v2"})
	got, err := second.Search(ctx, Query{Text: "what is the deploy target", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Scored lexically, which is the point: the old vector was not used, and
	// the record was not lost either.
	if len(got) != 1 {
		t.Fatalf("the record was lost when the model changed: %+v", got)
	}
	// The pass behind the store brings the vector up to the new model.
	waitForModel(t, second, got[0].ID, "stub-v2")
}

// An embedder that is down must not take the daemon's memory down with it.
type brokenEmbedder struct{}

func (brokenEmbedder) ID() string { return "broken" }
func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("no route to host")
}

func TestLocalStoresAndRecallsWhenTheEmbedderFails(t *testing.T) {
	ctx := context.Background()
	l := openLocal(t, filepath.Join(t.TempDir(), "facts.json"), brokenEmbedder{})
	if _, err := l.Store(ctx, Record{Content: "the staging cluster is rebuilt nightly", Scope: scope.Global}); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, err := l.Search(ctx, Query{Text: "when is staging rebuilt", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("a broken embedder cost the store its recall: %+v", got)
	}
}

// The store is worth having only if it recalls a fact somebody worded
// differently. Nothing in this query shares a word with the record it must
// find, and two of the three records it must not.
func TestLocalRecallsAFactWordedDifferently(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	for _, content := range []string{
		"we ship to the staging cluster",
		"the office cat is called Biscuit",
		"releases are cut on the last Thursday of the month",
	} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("store %q: %v", content, err)
		}
	}
	got, err := l.Search(ctx, Query{Text: "where does this get deployed", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("a fact in different words was not recalled at all")
	}
	if !strings.Contains(got[0].Content, "staging") {
		t.Fatalf("the wrong record ranked first: %+v", got)
	}
}

// A store that answers every question with its least irrelevant fact is worse
// than one that answers nothing: the advisor injects it, and the session is
// told something untrue about itself.
func TestLocalSaysNothingWhenItKnowsNothingRelevant(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	for _, content := range []string{
		"the office cat is called Biscuit",
		"lunch is at one",
	} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("store: %v", err)
		}
	}
	got, err := l.Search(ctx, Query{Text: "which migration tool does this project use", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a store with nothing relevant answered anyway: %+v", got)
	}
}

// A restatement in other words is what the reconciliation loop is for: it is
// refused, the caller is told which record it collided with, and supersedes
// that one instead of writing a second wording of the same thing.
func TestLocalRefusesTheSameClaimInOtherWords(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	id, err := l.Store(ctx, Record{Content: "releases ship on Fridays", Scope: scope.Global})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_, err = l.Store(ctx, Record{Content: "releases never ship on Fridays", Scope: scope.Global})
	var dup *ErrDuplicateSemantic
	if !errors.As(err, &dup) {
		t.Fatalf("got %v, want the contradiction to collide with the fact it contradicts", err)
	}
	if dup.Collided != id {
		t.Errorf("collided with %q, want %q", dup.Collided, id)
	}
}

// Two facts about different things must both fit in one project, however
// similar the shape of the sentences.
func TestLocalKeepsTwoDifferentFactsThatReadAlike(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	for _, content := range []string{
		"the deploy script lives in bin/ship",
		"the release notes live in docs/changelog.md",
		"the integration tests need a live Postgres",
	} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Local, Project: "/p"}); err != nil {
			t.Fatalf("store %q: %v", content, err)
		}
	}
	got, err := l.List(ctx, Query{Scope: scope.Local, Project: "/p"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the store holds %d of the three facts: %+v", len(got), got)
	}
}

// The table is compiled in, so it is only absent from a build that went wrong.
// Recall still has to work when it is.
func TestLocalStillRecallsWithNoEmbeddingModel(t *testing.T) {
	ctx := context.Background()
	l := newLexicalLocal(t)
	for _, content := range []string{
		"the integration tests need a live Postgres on port 5544",
		"the office cat is called Biscuit",
	} {
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("store: %v", err)
		}
	}
	got, err := l.Search(ctx, Query{Text: "which port does Postgres listen on for the tests", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || !strings.Contains(got[0].Content, "Postgres") {
		t.Fatalf("lexical recall did not find the record: %+v", got)
	}
}

// Private is a placement hint for stores that file records where others can
// read them. This one has a single file, so it keeps the flag and does nothing
// with it: a private record is listed and found like any other.
func TestLocalKeepsThePrivateFlagAndFiltersOnNothing(t *testing.T) {
	ctx := context.Background()
	l := newLocal(t)
	if _, err := l.Store(ctx, Record{Content: "prefers rebasing over merging", Private: true, Scope: scope.Global}); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, err := l.List(ctx, Query{Scope: scope.Global})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || !got[0].Private {
		t.Fatalf("the flag did not survive the round trip: %+v", got)
	}
	found, err := l.Search(ctx, Query{Text: "rebase or merge", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("a private record was filtered out of a search: %+v", found)
	}
}

func TestTermsAreTheWordsAndTheirAdjacentPairs(t *testing.T) {
	got := strings.Join(terms("deploys to staging, then production"), "|")
	const want = "deploys|to|staging|then|production|deploys to|to staging|staging then|then production"
	if got != want {
		t.Fatalf("terms: got %q, want %q", got, want)
	}
	if got := bigrams([]string{"alone"}); got != nil {
		t.Errorf("one word has no pairs, got %v", got)
	}
	// Everything else in the package still reads plain tokens; pairs must never
	// leak out of tokenise.
	for _, tok := range tokenise("deploys to staging") {
		if strings.ContainsRune(tok, ' ') {
			t.Fatalf("tokenise produced a pair: %q", tok)
		}
	}
}

// A question is never phrased like the fact that answers it, so a pair that
// fails to match is not evidence against a record. Word order can only lift a
// search score, never lower it below what the words alone earn.
func TestPhrasedNeverScoresBelowTheWordsAlone(t *testing.T) {
	idf := map[string]float64{}
	for _, pair := range [][2]string{
		{"when is staging rebuilt", "the staging cluster is rebuilt nightly"},
		{"what port does the catalogue service listen on", "the catalogue service listens on port 8082"},
		{"lunch", "the office cat is called Biscuit"},
	} {
		q, r := weigh(terms(pair[0]), idf), weigh(terms(pair[1]), idf)
		words := sparse(weigh(tokenise(pair[0]), idf), weigh(tokenise(pair[1]), idf))
		if got := phrased(q, r); got < words || got > 1 {
			t.Errorf("%q vs %q: phrased %v, words alone %v", pair[0], pair[1], got, words)
		}
	}
}

// The same words in another order answer a different question, and the record
// arranged like the question is the one that answers it. The store has no
// embedding here because the shipping table cannot see order at all: it scores
// both sentences identical, and the words are the only channel left to tell
// them apart.
//
// The two are seeded rather than stored, because the collision rule reads them
// as one fact on purpose: in a store of corrections the same words the other
// way round are a contradiction of the same claim, and a write that reorders
// a fact is meant to supersede it. Holding both is what a hand-edited file
// produces, and it is the state in which ranking has to choose.
func TestLocalTellsWordOrderApart(t *testing.T) {
	ctx := context.Background()
	l := newLexicalLocal(t)
	const (
		first  = "staging deploys before production"
		second = "production deploys before staging"
	)
	ids := seed(t, l, first)
	_, err := l.Store(ctx, Record{Content: second, Scope: scope.Global})
	var dup *ErrDuplicateSemantic
	if !errors.As(err, &dup) || dup.Collided != ids[0] {
		t.Fatalf("storing the reordering got %v, want a collision with %q so the write supersedes it", err, ids[0])
	}
	seed(t, l, second)
	for _, ask := range []struct{ query, want string }{
		{"staging deploys", first},
		{"production deploys", second},
	} {
		got, err := l.Search(ctx, Query{Text: ask.query, Limit: 5, Scope: scope.Global})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("%q: recalled %d records, want both: %+v", ask.query, len(got), got)
		}
		if got[0].Content != ask.want || got[0].Score <= got[1].Score {
			t.Errorf("%q: ranked %q (%v) over %q (%v); a bag of words cannot tell these apart",
				ask.query, got[0].Content, got[0].Score, got[1].Content, got[1].Score)
		}
	}
}

func TestNegatorsAreCountedFromTheTextNotTheTokens(t *testing.T) {
	for _, c := range []struct {
		text string
		want int
	}{
		{"the integration tests do not need a live Postgres any more", 2},
		{"we can't deploy on Fridays", 1},
		{"it isn’t run in CI", 1},
		{"there is no longer a staging cluster", 1},
		{"nobody deploys without telling on-call", 1},
		{"the linter cannot be skipped anymore", 2},
		// The tokeniser strips these to "won" and "can", which are words.
		{"we won the bid and you can deploy", 0},
		{"the office cat is called Biscuit", 0},
	} {
		if got := negators(c.text); got != c.want {
			t.Errorf("negators(%q) = %d, want %d", c.text, got, c.want)
		}
	}
	if samePolarity("releases ship on Fridays", "releases never ship on Fridays") {
		t.Error("a sentence and its negation read as the same polarity")
	}
	if !samePolarity("do not need it any more", "don't need it") {
		t.Error("two negated sentences read as different polarities; the count is not the polarity")
	}
}

// A fact that says something is no longer the case is the answer to the
// question of whether it is. Polarity is a rule for which record a write
// collides with; it is never a filter on what a search returns.
func TestLocalRecallsANegatedFactForItsTopic(t *testing.T) {
	ctx := context.Background()
	for name, l := range map[string]*Local{"embedded": newLocal(t), "lexical": newLexicalLocal(t)} {
		for _, content := range []string{
			"the office cat is called Biscuit",
			"the integration tests do not need a live Postgres any more; they use the in-memory fake",
			"lunch is at one",
		} {
			if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
				t.Fatalf("%s: store %q: %v", name, content, err)
			}
		}
		got, err := l.Search(ctx, Query{Text: "do the tests need a database running", Limit: 5, Scope: scope.Global})
		if err != nil {
			t.Fatalf("%s: search: %v", name, err)
		}
		if len(got) == 0 || !strings.Contains(got[0].Content, "Postgres") {
			t.Errorf("%s: the negated fact was not recalled first: %+v", name, got)
		}
	}
}

// seed places records in the store without the collision rule seeing them. A
// store holding a claim and its contradiction is reachable — the docs files
// are edited by hand, and so is facts.json — and it is the one state in which
// the rule has a choice to make.
func seed(t *testing.T, l *Local, contents ...string) []string {
	t.Helper()
	ids := make([]string, 0, len(contents))
	for _, content := range contents {
		r := Record{ID: contentID(content), Content: content, Scope: scope.Global}
		vec := l.embed(context.Background(), content)

		// Under the lock, like every other write: the pass the store starts
		// for itself is reading these two while this test fills them.
		l.mu.Lock()
		l.recs = append(l.recs, r)
		if vec != nil {
			l.vecs[r.ID] = *vec
		}
		l.mu.Unlock()
		ids = append(ids, r.ID)
	}
	return ids
}

// What polarity decides, and what it deliberately does not.
//
// A negated restatement still collides with the fact it contradicts: that
// collision is how the pipeline supersedes a fact somebody has corrected, and
// a guard that refused it would leave both claims stored and the old one
// recalled. Polarity is a preference among collisions, not a veto: when the
// store holds the claim in the write's own polarity as well, that record is the
// one being restated and the one the caller is told about, whichever was
// stored first. Naming the contradiction instead would have the caller replace
// the fact that disagrees with the write and keep two records that agree.
func TestLocalCollidesWithTheRecordOfItsOwnPolarityFirst(t *testing.T) {
	ctx := context.Background()
	const (
		claim      = "releases ship on Fridays"
		contrary   = "releases do not ship on Fridays"
		correction = "releases never ship on Fridays"
	)
	collidedWith := func(t *testing.T, l *Local) string {
		t.Helper()
		_, err := l.Store(ctx, Record{Content: correction, Scope: scope.Global})
		var dup *ErrDuplicateSemantic
		if !errors.As(err, &dup) {
			t.Fatalf("got %v, want ErrDuplicateSemantic", err)
		}
		return dup.Collided
	}

	t.Run("a contradiction collides with the fact it contradicts", func(t *testing.T) {
		l := newLocal(t)
		ids := seed(t, l, claim)
		if got := collidedWith(t, l); got != ids[0] {
			t.Errorf("collided with %q, want the contradicted fact %q", got, ids[0])
		}
	})
	t.Run("the same claim in the same polarity wins over the contradiction", func(t *testing.T) {
		l := newLocal(t)
		ids := seed(t, l, claim, contrary)
		if got := collidedWith(t, l); got != ids[1] {
			t.Errorf("collided with %q, want the record of the same polarity %q; the caller would overwrite the fact that disagrees and keep two that agree", got, ids[1])
		}
	})
}

// angledEmbedder puts every text it knows on the unit circle at the angle it
// was given, so a test can name the cosine between a query and a record
// exactly and say which side of a floor the score falls on. A text it does not
// know sits at a right angle to everything, which scores zero.
type angledEmbedder struct {
	id     string
	angles map[string]float64
}

func (a angledEmbedder) ID() string { return a.id }

func (a angledEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	theta, ok := a.angles[text]
	if !ok {
		return []float32{0, 0, 1}, nil
	}
	return []float32{float32(math.Cos(theta)), float32(math.Sin(theta)), 0}, nil
}

// unsettled is an embedder that never reports settling, which holds the pass
// the store starts on opening until the store is closed, and so holds every
// record at the model that wrote it for as long as a test needs.
type unsettled struct{ Embedder }

func (unsettled) Settled() <-chan struct{} { return nil }

// TestSearchTakesItsFloorFromTheModelThatScoredTheQuery is the whole point of
// the floor being a table: the same ranking, the same score, and two answers,
// because the constant that was measured against one model is not a
// measurement of another.
func TestSearchTakesItsFloorFromTheModelThatScoredTheQuery(t *testing.T) {
	ctx := context.Background()
	const (
		fact  = "biscuit"
		query = "lunchtime"
	)
	// No word in common, so the whole score is the embedding: 0.65 * 0.4,
	// which is 0.26 — under the default floor and over MiniLM's.
	angles := map[string]float64{fact: 0, query: math.Acos(0.4)}

	search := func(t *testing.T, model string, floor float64) []Record {
		t.Helper()
		l := openLocal(t, filepath.Join(t.TempDir(), "facts.json"), angledEmbedder{id: model, angles: angles})
		if _, storeErr := l.Store(ctx, Record{Content: fact, Scope: scope.Global}); storeErr != nil {
			t.Fatalf("store: %v", storeErr)
		}
		got, err := l.Search(ctx, Query{Text: query, Scope: scope.Global, MinScore: floor})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		return got
	}

	t.Run("a model with no floor of its own gets the default", func(t *testing.T) {
		if got := search(t, "some-other-table-v1", 0); len(got) != 0 {
			t.Fatalf("got %d records at %.2f, want none: the default floor is %.2f", len(got), got[0].Score, defaultMinScore)
		}
	})
	t.Run("MiniLM gets the floor measured against MiniLM", func(t *testing.T) {
		got := search(t, "all-minilm-l6-v2-f32-v1", 0)
		if len(got) != 1 {
			t.Fatalf("got %d records, want the one fact: a score of 0.26 is over MiniLM's floor of %.2f", len(got), minilmMinScore)
		}
	})
	t.Run("a floor the caller named wins over the model's", func(t *testing.T) {
		if got := search(t, "all-minilm-l6-v2-f32-v1", 0.5); len(got) != 0 {
			t.Fatalf("got %d records, want none: the caller asked for 0.50", len(got))
		}
		if got := search(t, "some-other-table-v1", 0.2); len(got) != 1 {
			t.Fatalf("got %d records, want the one fact: the caller asked for 0.20", len(got))
		}
	})
	t.Run("a search with no embedding at all gets the default", func(t *testing.T) {
		if got := minScoreFor(""); got != defaultMinScore {
			t.Fatalf("minScoreFor(\"\") = %v, want %v", got, defaultMinScore)
		}
	})
}

// While a model is landing the store holds vectors from two of them, and a
// record still on the old one is scored on words in common however good the
// query's own vector is. It has to be judged as what it is: the new model's
// floor is a measurement of the new model, and a record that model never
// touched has no business borrowing it.
func TestSearchJudgesARecordTheModelHasNotReachedOnTheDefaultFloor(t *testing.T) {
	ctx := context.Background()
	const (
		reached = "the office cat is called Biscuit"
		behind  = "the office dog is called Rusty"
		query   = "which animal lives here"
	)
	path := filepath.Join(t.TempDir(), "facts.json")
	angles := map[string]float64{reached: 0, behind: 0, query: math.Acos(0.4)}

	// The second record is written while the old model is still answering, so
	// its vector is tagged with that model and never compared again.
	old := openLocal(t, path, angledEmbedder{id: "old-table-v1", angles: angles})
	if _, storeErr := old.Store(ctx, Record{Content: behind, Scope: scope.Global}); storeErr != nil {
		t.Fatalf("store: %v", storeErr)
	}
	// The new model has not settled, so the pass that would bring the first
	// record up to it is still waiting when the search runs.
	l := openLocal(t, path, unsettled{angledEmbedder{id: "all-minilm-l6-v2-f32-v1", angles: angles}})
	if _, storeErr := l.Store(ctx, Record{Content: reached, Scope: scope.Global}); storeErr != nil {
		t.Fatalf("store: %v", storeErr)
	}

	// Both score 0.26 by the model and neither shares a word with the query,
	// so the only thing that can tell them apart is which measure produced the
	// number.
	got, err := l.Search(ctx, Query{Text: query, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want only the one the model reached: %+v", len(got), got)
	}
	if got[0].Content != reached {
		t.Fatalf("got %q, want %q", got[0].Content, reached)
	}
}

// TestARestatementIsJudgedByTheThresholdsOfTheModelThatEmbeddedIt is the point
// of the restatement thresholds being a table, and it says nothing about any
// real model: one pair of writes, one cosine, and two opposite verdicts,
// because the numbers measured against one embedding space are not a
// measurement of another.
func TestARestatementIsJudgedByTheThresholdsOfTheModelThatEmbeddedIt(t *testing.T) {
	ctx := context.Background()
	const (
		stored = "the daemon keeps its log in one file"
		// Seven tokens of eight in common with stored, which is a lexical
		// cosine of 0.875: over both lexical floors, so neither of them
		// decides this, and under sparseRestatement, so words alone do not
		// decide it either. What is left is the embedding threshold.
		second = "the daemon keeps its log in one place"
	)
	// Over the threshold measured against the compiled-in table and under the
	// one measured against MiniLM.
	angles := map[string]float64{stored: 0, second: math.Acos(0.945)}

	write := func(t *testing.T, model string) error {
		t.Helper()
		l := openLocal(t, filepath.Join(t.TempDir(), "facts.json"), angledEmbedder{id: model, angles: angles})
		if _, storeErr := l.Store(ctx, Record{Content: stored, Scope: scope.Global}); storeErr != nil {
			t.Fatalf("store: %v", storeErr)
		}
		_, err := l.Store(ctx, Record{Content: second, Scope: scope.Global})
		return err
	}
	refused := func(t *testing.T, err error) bool {
		t.Helper()
		var dup *ErrDuplicateSemantic
		if err != nil && !errors.As(err, &dup) {
			t.Fatalf("store: %v", err)
		}
		return err != nil
	}

	t.Run("a model with no thresholds of its own gets the compiled-in table's", func(t *testing.T) {
		if !refused(t, write(t, "some-other-table-v1")) {
			t.Fatalf("kept at 0.945, want it refused: the threshold measured against that table is %v", denseRestatement)
		}
	})
	t.Run("MiniLM gets the thresholds measured against MiniLM", func(t *testing.T) {
		if refused(t, write(t, "all-minilm-l6-v2-f32-v1")) {
			t.Fatalf("refused at 0.945, want it kept: MiniLM's threshold is %v", minilmDenseRestatement)
		}
	})
	t.Run("a write with no embedding at all gets the compiled-in table's", func(t *testing.T) {
		if got := restatementFor(""); got.dense != denseRestatement || got.lexicalFloor != denseRestatementLexicalFloor {
			t.Fatalf("restatementFor(\"\") = %+v, want %v and %v", got, denseRestatement, denseRestatementLexicalFloor)
		}
	})
}

// TestMiniLMKeepsTheRestatementThresholdsItWasMeasuredAt pins the literals so
// that changing them is a decision somebody made rather than a constant
// somebody rounded. They were measured over fifty pairs embedded by the model
// itself and the margin either way is about 0.011, which is a hundredth: there
// is no room here for a number chosen because it matches the one above it.
func TestMiniLMKeepsTheRestatementThresholdsItWasMeasuredAt(t *testing.T) {
	got := restatementFor("all-minilm-l6-v2-f32-v1")
	if got.dense != 0.95 || got.lexicalFloor != 0.5 {
		t.Fatalf("MiniLM restatement = %+v, want dense 0.95 and lexical floor 0.5", got)
	}
	if got.dense <= denseRestatement {
		t.Fatalf("MiniLM dense %v is not above the compiled-in table's %v; this model puts two different facts at 0.9384 and the table exists to stay clear of them",
			got.dense, denseRestatement)
	}
	if got := restatementFor("all-minilm-l6-v2-f32-v1-typo"); got.dense != denseRestatement {
		t.Fatalf("an id not in the table got %+v, want the compiled-in table's %v", got, denseRestatement)
	}
}

// Which of the two dense thresholds a refusal is judged by is decided by the
// polarity of the two texts, not only by the model. The pair of writes here
// scores exactly the same cosine against the same stored claim and gets
// opposite verdicts, which is the whole of the rule: the number that says two
// sentences are one claim restated is not the number that says one denies the
// other, because a negator is a word the claim does not have and the model
// puts the pair further apart than it puts the same claim twice.
func TestRestatementThresholdIsTheOneForThePolarity(t *testing.T) {
	ctx := context.Background()
	const (
		claim      = "the pipeline runs on merge"
		negation   = "the pipeline does not run on merge"
		paraphrase = "the pipeline runs on release"
	)

	// The premise of the case, asserted rather than assumed: both writes have
	// to reach the dense path at all, which means enough words in common to
	// clear the floor and not so many that they are refused on words alone.
	// A store holding one record weighs every term the same, which is the
	// regime a first write is actually compared in.
	t.Run("both writes are decided by the embedding", func(t *testing.T) {
		weights := idf([]Record{{Content: claim}})
		for _, write := range []string{negation, paraphrase} {
			got := sparse(weigh(tokenise(write), weights), weigh(tokenise(claim), weights))
			if got < denseRestatementLexicalFloor || got >= sparseRestatement {
				t.Fatalf("%q shares %.4f of its words with the claim, which is outside (%.2f, %.2f): the case would not be decided by the embedding at all",
					write, got, denseRestatementLexicalFloor, sparseRestatement)
			}
		}
	})

	write := func(t *testing.T, model string, cos float64, content string) (claimID string, err error) {
		t.Helper()
		angles := map[string]float64{claim: 0, content: math.Acos(cos)}
		l := openLocal(t, filepath.Join(t.TempDir(), "facts.json"), angledEmbedder{id: model, angles: angles})
		claimID, err = l.Store(ctx, Record{Content: claim, Scope: scope.Global})
		if err != nil {
			t.Fatalf("store the claim: %v", err)
		}
		_, err = l.Store(ctx, Record{Content: content, Scope: scope.Global})
		return claimID, err
	}
	refused := func(t *testing.T, want string, err error) {
		t.Helper()
		var dup *ErrDuplicateSemantic
		if !errors.As(err, &dup) {
			t.Fatalf("got %v, want ErrDuplicateSemantic", err)
		}
		if dup.Collided != want {
			t.Fatalf("collided with %q, want the stored claim %q", dup.Collided, want)
		}
	}
	kept := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("the write was refused: %v", err)
		}
	}

	const minilm = "all-minilm-l6-v2-f32-v1"
	// One cosine, between the two thresholds, and two verdicts.
	const between = 0.90
	t.Run("a contradiction at 0.90 is refused", func(t *testing.T) {
		id, err := write(t, minilm, between, negation)
		refused(t, id, err)
	})
	t.Run("a restatement at 0.90 is kept", func(t *testing.T) {
		_, err := write(t, minilm, between, paraphrase)
		kept(t, err)
	})
	t.Run("a contradiction below the opposite threshold is kept", func(t *testing.T) {
		_, err := write(t, minilm, minilmDenseRestatementOpposite-0.01, negation)
		kept(t, err)
	})
	t.Run("a restatement above the same-polarity threshold is refused", func(t *testing.T) {
		id, err := write(t, minilm, minilmDenseRestatement+0.01, paraphrase)
		refused(t, id, err)
	})
	// And the pair is per model like every other constant here: the table a
	// model is not in has one number for both polarities, so 0.90 decides
	// nothing under it.
	t.Run("a model with no pair of its own gets the compiled-in table's", func(t *testing.T) {
		for _, content := range []string{negation, paraphrase} {
			_, err := write(t, "some-other-table-v1", between, content)
			kept(t, err)
		}
		id, err := write(t, "some-other-table-v1", denseRestatementOpposite+0.01, negation)
		refused(t, id, err)
	})
}

// End to end on the store and the table that ship: a claim goes in, its
// denial is refused, and the refusal names the claim, which is what lets the
// pipeline supersede it rather than keep both.
func TestLocalRefusesTheDenialOfAStoredClaim(t *testing.T) {
	ctx := context.Background()
	for _, pair := range []struct{ claim, denial string }{
		{"logs are kept for thirty days", "logs are not kept for thirty days"},
		{"we use rebase on shared branches", "we do not use rebase on shared branches"},
		{"releases ship on Fridays", "releases never ship on Fridays"},
		{"the test suite needs network access", "the test suite does not need network access"},
	} {
		t.Run(pair.denial, func(t *testing.T) {
			l := newLocal(t)
			id, err := l.Store(ctx, Record{Content: pair.claim, Scope: scope.Global})
			if err != nil {
				t.Fatalf("store the claim: %v", err)
			}
			_, err = l.Store(ctx, Record{Content: pair.denial, Scope: scope.Global})
			var dup *ErrDuplicateSemantic
			if !errors.As(err, &dup) {
				t.Fatalf("got %v, want ErrDuplicateSemantic: the store would keep the claim and its denial at once", err)
			}
			if dup.Collided != id {
				t.Fatalf("collided with %q, want the contradicted claim %q", dup.Collided, id)
			}
		})
	}
}
