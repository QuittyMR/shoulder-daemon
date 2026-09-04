package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// newLocal builds the store as the daemon ships it: with the embedding table
// compiled into the binary. A test against a store scoring some other way is a
// test of something nobody runs.
func newLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(filepath.Join(t.TempDir(), "facts.json"), vectors.Embedder{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return l
}

// newLexicalLocal is the store with no embedding model, which is what a build
// whose table failed to load falls back to.
func newLexicalLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(filepath.Join(t.TempDir(), "facts.json"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return l
}

// The store that ships is held to the same contract as the one that talks to a
// service, because it is the one almost everybody will actually run.
func TestLocalConformance(t *testing.T) {
	dir := t.TempDir()
	n := 0
	TestConnector(t, func() Connector {
		n++
		l, err := NewLocal(filepath.Join(dir, "facts-"+strings.Repeat("x", n)+".json"), vectors.Embedder{})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return l
	})
}

func TestLocalKeepsFactsAcrossARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	first, err := NewLocal(path, vectors.Embedder{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	const fact = "the integration tests need a live Postgres"
	id, err := first.Store(ctx, Record{Content: fact, Category: "structure", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	// The daemon exits when the last session ends and is started again by the
	// editor, so this is the ordinary case, not a disaster case.
	second, err := NewLocal(path, vectors.Embedder{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
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
	if _, err := NewLocal(path, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
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
	l, openErr := NewLocal(path, stubEmbedder{id: "stub-v1"})
	if openErr != nil {
		t.Fatalf("open: %v", openErr)
	}
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
	first, openErr := NewLocal(path, stubEmbedder{id: "stub-v1"})
	if openErr != nil {
		t.Fatalf("open: %v", openErr)
	}
	if _, err := first.Store(ctx, Record{Content: "the deploy target is staging", Scope: scope.Global}); err != nil {
		t.Fatalf("store: %v", err)
	}

	second, err := NewLocal(path, stubEmbedder{id: "stub-v2"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := second.Search(ctx, Query{Text: "what is the deploy target", Limit: 5, Scope: scope.Global})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Scored lexically, which is the point: the old vector was not used, and
	// the record was not lost either.
	if len(got) != 1 {
		t.Fatalf("the record was lost when the model changed: %+v", got)
	}
}

// An embedder that is down must not take the daemon's memory down with it.
type brokenEmbedder struct{}

func (brokenEmbedder) ID() string { return "broken" }
func (brokenEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("no route to host")
}

func TestLocalStoresAndRecallsWhenTheEmbedderFails(t *testing.T) {
	ctx := context.Background()
	l, openErr := NewLocal(filepath.Join(t.TempDir(), "facts.json"), brokenEmbedder{})
	if openErr != nil {
		t.Fatalf("open: %v", openErr)
	}
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
