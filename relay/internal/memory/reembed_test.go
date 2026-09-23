package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// writeStore puts records on disk with vectors from a model the test names, or
// none when it names nothing, which is what a store written before any model
// was configured looks like.
func writeStore(t *testing.T, path, model string, contents ...string) []string {
	t.Helper()
	f := file{Version: fileVersion, Vectors: map[string]vector{}}
	var ids []string
	for i, c := range contents {
		id := contentID(c)
		ids = append(ids, id)
		f.Records = append(f.Records, Record{
			ID: id, Content: c, Scope: scope.Local, Project: scope.Key("/tmp/project"),
			CreatedAt: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC),
		})
		if model != "" {
			f.Vectors[id] = vector{Model: model, Values: []float32{1, 2, 3}}
		}
	}
	body, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return ids
}

func vectorModel(t *testing.T, l *Local, id string) (string, int) {
	t.Helper()
	l.mu.RLock()
	defer l.mu.RUnlock()
	v, ok := l.vecs[id]
	if !ok {
		return "", 0
	}
	return v.Model, len(v.Values)
}

func waitForModel(t *testing.T, l *Local, id, model string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := vectorModel(t, l, id); got == model {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := vectorModel(t, l, id)
	t.Fatalf("record %s is still tagged %q, want %q", id[:8], got, model)
}

func TestReembedRewritesVectorsFromAnotherModel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	ids := writeStore(t, path, "old-model",
		"the main branch is called master",
		"we ship every build to staging first",
		"prefers terse answers",
	)
	// Ready but never settled, so the pass the store starts on its own waits
	// and the one below is the only one that runs.
	emb := newLoadingEmbedder("new-model", 4)
	emb.ready = true
	l, err := NewLocal(path, emb)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Plus one record with no vector at all.
	l.mu.Lock()
	delete(l.vecs, ids[2])
	l.mu.Unlock()

	n, err := l.Reembed(ctx)
	if err != nil {
		t.Fatalf("reembed: %v", err)
	}
	if n != 3 {
		t.Fatalf("re-embedded %d records, want 3", n)
	}
	for _, id := range ids {
		if model, dims := vectorModel(t, l, id); model != "new-model" || dims != 4 {
			t.Errorf("record %s is tagged %q with %d dims", id[:8], model, dims)
		}
	}

	// The pass saved: a restart sees the new vectors, and every fact.
	again, err := NewLocal(path, emb)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if again.Len() != 3 {
		t.Fatalf("%d facts after the pass, want 3", again.Len())
	}
	for _, id := range ids {
		if model, _ := vectorModel(t, again, id); model != "new-model" {
			t.Errorf("record %s was not saved with the new model: %q", id[:8], model)
		}
	}
	if n, err := again.Reembed(ctx); err != nil || n != 0 {
		t.Fatalf("a second pass did %d records with error %v; want nothing to do", n, err)
	}
}

// The pass is what makes a model that arrives after the store opened count
// for the facts already in it. It is not started until the model has settled,
// because before then it would rewrite everything into the fallback's space
// and then rewrite it all again.
func TestOpeningAStoreReembedsOnceTheModelArrives(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	ids := writeStore(t, path, "old-model", "the main branch is called master", "we ship every build to staging first")

	primary := newLoadingEmbedder("big-model", 8)
	l, err := NewLocal(path, NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A write before the model arrives goes through the fallback.
	written, err := l.Store(ctx, Record{Content: "prefers terse answers", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if model, _ := vectorModel(t, l, written); model != "small-model" {
		t.Fatalf("a write before the model arrived is tagged %q", model)
	}
	time.Sleep(20 * time.Millisecond)
	if model, _ := vectorModel(t, l, ids[0]); model != "old-model" {
		t.Fatalf("the pass ran before the model settled and tagged the record %q", model)
	}

	primary.arrive()
	for _, id := range append(ids, written) {
		waitForModel(t, l, id, "big-model")
	}
	// The search kept answering throughout; afterwards it answers from the
	// new vectors and the words together.
	got, err := l.Search(ctx, Query{Text: "which branch is main", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || got[0].Content != "the main branch is called master" {
		t.Fatalf("search after the pass returned %+v", got)
	}
}

// A model that never arrives leaves the store exactly as the fallback wrote
// it; the pass has nothing to rewrite and must not wait forever either.
func TestAModelThatNeverArrivesChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "facts.json")
	ids := writeStore(t, path, "small-model", "the main branch is called master")
	primary := newLoadingEmbedder("big-model", 8)
	l, err := NewLocal(path, NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	primary.giveUp()
	n, err := l.Reembed(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("pass did %d with %v; want nothing", n, err)
	}
	if model, _ := vectorModel(t, l, ids[0]); model != "small-model" {
		t.Fatalf("record is tagged %q", model)
	}
}

// A record replaced while its old content was being embedded must not get the
// old content's vector: the write that replaced it already embedded the new
// content with the same model.
func TestReembedLeavesARecordReplacedDuringThePassAlone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	ids := writeStore(t, path, "old-model", "the main branch is called master")
	// Ready but not settled, so the only pass is the one below.
	emb := newLoadingEmbedder("new-model", 4)
	emb.ready = true
	l, err := NewLocal(path, emb)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	replaced := ""
	var once bool
	emb.onEmbed = func() {
		if once {
			return
		}
		once = true
		id, err := l.Supersede(ctx, ids[0], Record{Content: "the main branch is called main after all", Scope: scope.Local, Project: "/tmp/project"})
		if err != nil {
			t.Errorf("supersede during the pass: %v", err)
		}
		replaced = id
	}
	if _, err := l.Reembed(ctx); err != nil {
		t.Fatalf("reembed: %v", err)
	}
	if model, _ := vectorModel(t, l, ids[0]); model != "" {
		t.Fatalf("the superseded record still has a vector tagged %q", model)
	}
	if model, _ := vectorModel(t, l, replaced); model != "new-model" {
		t.Fatalf("the replacement is tagged %q", model)
	}
}

// The pass gives up when the model is taken away mid-way, rather than filing
// vectors under a name that no longer answers.
func TestReembedStopsWhenTheModelIsNotReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "facts.json")
	writeStore(t, path, "old-model", "one fact", "another fact", "a third fact")
	emb := newLoadingEmbedder("new-model", 4)
	l, err := NewLocal(path, emb)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	n, err := l.Reembed(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("pass on a model that is not ready did %d with %v", n, err)
	}
}

// A store that has no file yet is every install's first start. Its watcher
// has to run too, or everything the first session writes keeps the fallback's
// vector and is scored on words against the model's queries until the next
// start.
func TestAFreshStoreCatchesUpOnceTheModelArrives(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	primary := newLoadingEmbedder("big-model", 8)
	l, err := NewLocal(path, NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	written, err := l.Store(ctx, Record{Content: "prefers terse answers", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if model, _ := vectorModel(t, l, written); model != "small-model" {
		t.Fatalf("a write before the model arrived is tagged %q", model)
	}
	primary.arrive()
	waitForModel(t, l, written, "big-model")
}
