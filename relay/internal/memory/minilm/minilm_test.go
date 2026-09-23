package minilm

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// modelDir is where a test finds the model, when it is there at all. The
// download is 91 MB from a third party, so no test performs it: a machine
// without the model in its cache runs only what needs no model.
func modelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("SHOULDER_MODEL_DIR")
	if dir == "" {
		dir = memory.DefaultModelDir()
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(Hub), loadable)); err != nil { //nolint:gosec // G703: the directory is the test's own setting
		t.Skipf("no converted model under %s; fetch one by running the daemon with SHOULDER_EMBEDDING=minilm, or point SHOULDER_MODEL_DIR at one", dir)
	}
	return dir
}

func loaded(t *testing.T, dir string) *Embedder {
	t.Helper()
	e := New(dir, nil)
	select {
	case <-e.Settled():
	case <-time.After(30 * time.Second):
		t.Fatal("the model did not settle")
	}
	if !e.Ready() {
		t.Fatalf("the model on disk did not load: %v", e.Err())
	}
	return e
}

func embed(t *testing.T, e *Embedder, text string) []float32 {
	t.Helper()
	v, err := e.Embed(context.Background(), text)
	if err != nil {
		t.Fatalf("embed %q: %v", text, err)
	}
	return v
}

func dot(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func rssMB(t *testing.T) float64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	m := regexp.MustCompile(`VmRSS:\s*(\d+) kB`).FindSubmatch(b)
	if m == nil {
		return 0
	}
	kb, _ := strconv.Atoi(string(m[1]))
	return float64(kb) / 1024
}

func TestTheModelLoadsAndEmbeds(t *testing.T) {
	dir := modelDir(t)
	before := rssMB(t)
	started := time.Now()
	e := loaded(t, dir)
	t.Logf("loaded in %v; resident memory %.0f MB before, %.0f MB after", time.Since(started).Round(time.Millisecond), before, rssMB(t))

	v := embed(t, e, "the deploy target is the staging cluster")
	if len(v) != 384 {
		t.Fatalf("%d dimensions, want 384", len(v))
	}
	if got := dot(v, v); got < 0.999 || got > 1.001 {
		t.Fatalf("a vector against itself is %.6f, want 1", got)
	}
	if e.ID() != Model || strings.Contains(e.ID(), "glove") {
		t.Fatalf("ID() = %q", e.ID())
	}
}

// The reason to pay for a transformer: a sentence and its paraphrase land
// together, an unrelated one lands apart, and both by a wider margin than a
// mean of word vectors manages.
func TestMeaningBeatsWordsInCommon(t *testing.T) {
	e := loaded(t, modelDir(t))
	cases := []struct{ query, near, far string }{
		{"where does this get deployed", "we ship to the staging cluster", "the office cat is called Biscuit"},
		{"what database do the tests need", "the integration suite requires a running Postgres", "releases are cut on the last Thursday of the month"},
		{"how much detail should the reply have", "prefers terse answers with no preamble", "the CI runner is self-hosted"},
	}
	for _, tc := range cases {
		q := embed(t, e, tc.query)
		near, far := dot(q, embed(t, e, tc.near)), dot(q, embed(t, e, tc.far))
		if near <= far+0.2 {
			t.Errorf("%q: %q scored %.3f and %q scored %.3f", tc.query, tc.near, near, tc.far, far)
		}
	}
}

// Nothing in the library says Encode may be called from two goroutines at
// once, and the store does exactly that: a search during the re-embedding
// pass. The race detector is the test.
func TestEmbedIsSafeToCallConcurrently(t *testing.T) {
	e := loaded(t, modelDir(t))
	want := embed(t, e, "the main branch is called master")
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 3 {
				got, err := e.Embed(context.Background(), "the main branch is called master")
				if err != nil {
					t.Error(err)
					return
				}
				if d := dot(got, want); d < 0.999 {
					t.Errorf("a concurrent embedding of the same text scored %.4f against the first", d)
				}
			}
		}()
	}
	wg.Wait()
}

// A fact longer than the model reads is embedded from its head rather than
// refused; refusing would leave the longest facts, which are the ones people
// write when something went wrong, without a vector at all.
func TestTextBeyondWhatTheModelReadsIsCutNotRefused(t *testing.T) {
	e := loaded(t, modelDir(t))
	head := "The deployment pipeline builds the container and pushes it to the registry."
	long := head + " " + strings.Repeat("Nobody deploys on a Friday afternoon without telling the on-call engineer first. ", 60)
	enc := e.enc.Load()
	if n := len(enc.Tokenizer.Tokenize(strings.ToLower(long))); n <= maxTokens {
		t.Fatalf("the fixture is %d tokens, which the model reads whole", n)
	}
	cut := fit(enc, long)
	if n := len(enc.Tokenizer.Tokenize(cut)) + 2; n > maxTokens {
		t.Fatalf("the cut text is still %d tokens", n)
	}
	if !strings.HasPrefix(cut, strings.ToLower(head)) {
		t.Fatalf("the cut lost the head of the fact: %q", cut)
	}
	if got := fit(enc, head); got != head {
		t.Fatalf("a fact that fits came back changed: %q", got)
	}

	v, err := e.Embed(context.Background(), long)
	if err != nil {
		t.Fatalf("a %d-word fact was refused: %v", len(strings.Fields(long)), err)
	}
	near := dot(v, embed(t, e, "who has to be told before a Friday deploy"))
	far := dot(v, embed(t, e, "the office cat is called Biscuit"))
	if near <= far {
		t.Fatalf("the cut fact scored %.3f against its own subject and %.3f against an unrelated one", near, far)
	}
}

// Loading needs the converted weights, the vocabulary and the tokenizer
// settings, and not the 91 MB PyTorch file the conversion started from, which
// is why fetch deletes it.
func TestTheModelLoadsWithoutTheOriginalWeights(t *testing.T) {
	src := filepath.Join(modelDir(t), filepath.FromSlash(Hub))
	dir := t.TempDir()
	dst := filepath.Join(dir, filepath.FromSlash(Hub))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{loadable, "vocab.txt", "tokenizer_config.json", "config.json"} {
		if err := os.Symlink(filepath.Join(src, name), filepath.Join(dst, name)); err != nil {
			t.Fatal(err)
		}
	}
	e := loaded(t, dir)
	if _, err := e.Embed(context.Background(), "prefers terse answers"); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedBeforeTheModelIsReadyIsNotReady(t *testing.T) {
	e := &Embedder{settled: make(chan struct{})}
	_, err := e.Embed(context.Background(), "anything")
	if !errors.Is(err, memory.ErrEmbedderNotReady) {
		t.Fatalf("Embed() before loading = %v, want ErrEmbedderNotReady", err)
	}
	if e.Err() != nil {
		t.Fatal("Err() reported a failure before loading had settled")
	}
}

// A download that does not match its checksum, or does not arrive, leaves the
// cache exactly as it was: no directory a later start could mistake for a
// model, and a daemon that is not ready rather than one that is wrong.
func TestARuinedDownloadLeavesNothingBehind(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve http.HandlerFunc
	}{
		{"wrong bytes", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not the model")) }},
		{"missing", func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.serve)
			defer srv.Close()
			dir := t.TempDir()
			e := start(dir, srv.URL, nil)
			select {
			case <-e.Settled():
			case <-time.After(10 * time.Second):
				t.Fatal("did not settle")
			}
			if e.Ready() || e.Err() == nil {
				t.Fatalf("ready=%v err=%v after a download that could not have worked", e.Ready(), e.Err())
			}
			if _, err := e.Embed(context.Background(), "anything"); !errors.Is(err, memory.ErrEmbedderNotReady) {
				t.Fatalf("Embed() = %v, want ErrEmbedderNotReady", err)
			}
			if _, err := os.Stat(e.Dir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a model directory was left behind: %v", err)
			}
			if stale, _ := filepath.Glob(filepath.Join(dir, ".staging-*")); len(stale) > 0 {
				t.Fatalf("staging left behind: %v", stale)
			}
		})
	}
}

func TestTheModelDirectoryIsUnderTheCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/somewhere/cache")
	if got := memory.DefaultModelDir(); got != "/somewhere/cache/shoulder-daemon/models" {
		t.Fatalf("DefaultModelDir() = %q", got)
	}
	e := &Embedder{dir: "/models"}
	if got := e.Dir(); got != filepath.Join("/models", "sentence-transformers", "all-MiniLM-L6-v2") {
		t.Fatalf("Dir() = %q", got)
	}
}

// The store, the composite and the model together: facts written while the
// model was loading, and facts on disk from before it existed, end up in its
// space without anybody asking.
func TestAStoreCatchesUpOnceTheModelArrives(t *testing.T) {
	dir := modelDir(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "facts.json")
	first, err := memory.NewLocal(path, vectors.Embedder{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	old, err := first.Store(ctx, memory.Record{Content: "the main branch is called master", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatal(err)
	}

	l, err := memory.NewLocal(path, memory.NewFallback(New(dir, nil), vectors.Embedder{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	fresh, err := l.Store(ctx, memory.Record{Content: "we ship every build to staging first", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		models := storedModels(t, path)
		if models[old] == Model && models[fresh] == Model {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 30s the store holds %v", models)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := l.Search(ctx, memory.Query{Text: "which branch do I rebase onto", Scope: scope.Local, Project: "/tmp/project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].ID != old {
		t.Fatalf("search after the pass returned %+v", got)
	}
}

// storedModels reads which model each record's vector on disk came from,
// through the file rather than the store, because that is what the next start
// will read.
func storedModels(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Vectors map[string]struct {
			Model string `json:"model"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for id, v := range f.Vectors {
		out[id] = v.Model
	}
	return out
}

// The whole first start, against huggingface.co: 91 MB down, converted,
// moved into place, loaded. It is behind an environment variable because it
// needs the network and a third party, and it writes into the directory it is
// given rather than the cache so that a run leaves nothing behind that a
// later test would take for granted.
func TestTheFirstStartFetchesAndConvertsTheModel(t *testing.T) {
	dir := os.Getenv("SHOULDER_MINILM_FETCH_DIR")
	if dir == "" {
		t.Skip("set SHOULDER_MINILM_FETCH_DIR to an empty directory to download the model from huggingface.co")
	}
	started := time.Now()
	e := New(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	select {
	case <-e.Settled():
	case <-time.After(fetchTimeout):
		t.Fatal("the fetch did not settle")
	}
	if !e.Ready() {
		t.Fatalf("not ready after %v: %v", time.Since(started), e.Err())
	}
	t.Logf("fetched, converted and loaded in %v", time.Since(started).Round(time.Millisecond))
	if _, err := os.Stat(filepath.Join(e.Dir(), weights)); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // G703: the directory is the test's own setting
		t.Fatalf("the original weights were kept: %v", err)
	}
	if stale, _ := filepath.Glob(filepath.Join(dir, ".staging-*")); len(stale) > 0 {
		t.Fatalf("staging left behind: %v", stale)
	}
	v := embed(t, e, "the deploy target is the staging cluster")
	if len(v) != 384 {
		t.Fatalf("%d dimensions", len(v))
	}
}

// TestTheModelIdIsTheOneTheStoreKeepsAFloorFor pins the string rather than the
// constant. The memory package keys two per-model tables on this id — the
// search floor and the pair of restatement thresholds a write is refused by —
// and cannot import this package to spell it, because the dependency runs the
// other way. A rename here would silently put MiniLM back on both numbers
// measured against GloVe: every paraphrase the model does recall would be
// dropped as too weak to return, and a claim written twice in different words
// would be kept twice instead of superseding.
func TestTheModelIdIsTheOneTheStoreKeepsAFloorFor(t *testing.T) {
	if Model != "all-minilm-l6-v2-f32-v1" {
		t.Fatalf("Model = %q; memory.minScores and memory.restatements are keyed on %q and have to be changed with it",
			Model, "all-minilm-l6-v2-f32-v1")
	}
}

// The opposite-polarity threshold, against the model it was measured on. The
// pair here is the reason the threshold exists: "releases ship on Fridays" and
// "releases never ship on Fridays" score 0.9176 by the model and share 0.89 of
// their words, so neither the same-polarity threshold of 0.95 nor the words
// alone at 0.90 refuse the denial, and without the collision the store keeps
// the claim and its denial and answers with whichever ranks higher.
//
// The control is what keeps the threshold honest. "the frontend is not
// deployed to Cloudflare" against "the backend is deployed to Cloudflare" is
// the strongest pair of genuinely different facts measured on this model at
// 0.8590, and a refusal is a supersede, so a store that collided them would
// delete one of the two. It has to be kept.
func TestADenialOfAStoredClaimIsRefusedByTheModel(t *testing.T) {
	dir := modelDir(t)
	ctx := context.Background()
	store := func(t *testing.T, first, second string) (id string, err error) {
		t.Helper()
		l, openErr := memory.NewLocal(filepath.Join(t.TempDir(), "facts.json"), loaded(t, dir))
		if openErr != nil {
			t.Fatal(openErr)
		}
		t.Cleanup(func() { _ = l.Close() })
		id, err = l.Store(ctx, memory.Record{Content: first, Scope: scope.Global})
		if err != nil {
			t.Fatalf("store %q: %v", first, err)
		}
		_, err = l.Store(ctx, memory.Record{Content: second, Scope: scope.Global})
		return id, err
	}

	t.Run("the denial collides with the claim", func(t *testing.T) {
		id, err := store(t, "releases ship on Fridays", "releases never ship on Fridays")
		var dup *memory.ErrDuplicateSemantic
		if !errors.As(err, &dup) {
			t.Fatalf("got %v, want ErrDuplicateSemantic naming %s", err, id)
		}
		if dup.Collided != id {
			t.Fatalf("collided with %q, want the contradicted claim %q", dup.Collided, id)
		}
	})
	t.Run("two different facts with a negator between them are both kept", func(t *testing.T) {
		if _, err := store(t, "the backend is deployed to Cloudflare", "the frontend is not deployed to Cloudflare"); err != nil {
			t.Fatalf("the second fact was refused: %v; a refusal is a supersede, so one of the two would be deleted", err)
		}
	})
}

// TestTheModelReportsNoVocabulary pins a decision rather than an omission.
//
// The store will ask an embedder which words it could not look up, and refuse
// to let its cosine declare a restatement when the two texts differ in one of
// them. That guard exists for the compiled-in table, which has a fixed list of
// 40,000 words and embeds "the frontend is deployed to Cloudflare" and "the
// backend is deployed to Cloudflare" to the identical vector because it holds
// none of the three. A WordPiece tokeniser has no such word: "cloudflare" is
// spelled out of pieces it does hold, the vector moves, and the same pair
// scores 0.9384 here. Implementing memory.Vocabulary would mean inventing an
// answer, so this model does not, and the store takes it to have seen
// everything.
func TestTheModelReportsNoVocabulary(t *testing.T) {
	if _, ok := any(&Embedder{}).(memory.Vocabulary); ok {
		t.Fatal("this model reports a vocabulary; a WordPiece tokeniser has no word outside one, so whatever it reports was not measured")
	}
}
