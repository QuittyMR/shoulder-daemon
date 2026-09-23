package cliapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// heldMemory holds the first write until the test releases it, then honours
// whatever the context says by then - the way a real backend's write would,
// mirroring internal/pipeline's own heldStore for the pipeline's writes.
type heldMemory struct {
	fakeMemory
	entered chan struct{}
	proceed chan struct{}

	mu    sync.Mutex
	calls int
}

func newHeldMemory() *heldMemory {
	return &heldMemory{
		fakeMemory: *newFakeMemory(),
		entered:    make(chan struct{}, 1),
		proceed:    make(chan struct{}),
	}
}

func (h *heldMemory) storeCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func (h *heldMemory) Store(ctx context.Context, r memory.Record) (string, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	h.entered <- struct{}{}
	<-h.proceed
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return h.fakeMemory.Store(ctx, r)
}

func (h *heldMemory) Supersede(ctx context.Context, old string, r memory.Record) (string, error) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	h.entered <- struct{}{}
	<-h.proceed
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return h.fakeMemory.Supersede(ctx, old, r)
}

// serve runs the handler over req on its own goroutine and reports when it
// has returned, so a test can hold a write open while the request's own
// context is cancelled out from under it.
func serve(h http.Handler, req *http.Request) (*httptest.ResponseRecorder, <-chan struct{}) {
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	return rec, done
}

func awaitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never reached the store", what)
	}
}

func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned")
	}
}

// cancelWellWithinGrace cancels ctx and waits long enough that a write
// released right after really was released before pipeline.Decided's grace
// (2s) could have ended it, without spending anywhere near that budget.
func cancelWellWithinGrace(cancel context.CancelFunc) {
	cancel()
	time.Sleep(100 * time.Millisecond)
}

// A fact typed at the terminal is the caller's own words; the daemon commits
// it even if the request that carried it is cancelled mid-write, for as long
// as pipeline.Decided's grace allows.
func TestFactAddFinishesAWriteInFlightWhenTheRequestIsCancelled(t *testing.T) {
	mem := newHeldMemory()
	h, _ := newTestServerWith(t, "", &fakeLLM{}, mem)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/cli/facts",
		strings.NewReader(`{"content":"a fact","scope":"global"}`)).WithContext(ctx)
	rec, done := serve(h, req)

	awaitEntered(t, mem.entered, "the write")
	cancelWellWithinGrace(cancel)
	close(mem.proceed)
	awaitDone(t, done)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: a write already in flight must still succeed once its request is cancelled: %s",
			rec.Code, rec.Body.String())
	}
	if got := decode[FactResponse](t, rec).ID; got != "mem_1" {
		t.Fatalf("id = %q", got)
	}
	writes := mem.writes()
	if len(writes) != 1 || writes[0].Content != "a fact" {
		t.Fatalf("stored %+v", writes)
	}
}

// Same as above for a PATCH: the correction is the caller's own too, and a
// supersede held open by the store finishes rather than being lost to the
// cancellation of the request that asked for it.
func TestFactUpdateFinishesASupersedeInFlightWhenTheRequestIsCancelled(t *testing.T) {
	mem := newHeldMemory()
	mem.held[scope.Global] = []memory.Record{{ID: "old", Content: "the main branch is main", Scope: scope.Global}}
	h, _ := newTestServerWith(t, "", &fakeLLM{}, mem)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPatch, "/v1/cli/facts",
		strings.NewReader(`{"id":"old","content":"the main branch is master","scope":"global"}`)).WithContext(ctx)
	rec, done := serve(h, req)

	awaitEntered(t, mem.entered, "the supersede")
	cancelWellWithinGrace(cancel)
	close(mem.proceed)
	awaitDone(t, done)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: a supersede already in flight must still succeed once its request is cancelled: %s",
			rec.Code, rec.Body.String())
	}
	if len(mem.superseded) != 1 || mem.superseded[0] != "old" {
		t.Fatalf("superseded %v", mem.superseded)
	}
}

// A migration that is cancelled while its first destination write is held
// still finishes that write, starts no others, and reports every record it
// never started as failed: r.Context().Err() is checked by the loop between
// records, not inside pipeline.Decided's grace.
func TestMigrateFinishesTheHeldWriteAndStartsNoMoreWhenTheRequestIsCancelled(t *testing.T) {
	unsetMemoryEnv(t)
	source := filepath.Join(t.TempDir(), "old-facts.json")
	first := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	oldStore(t, source,
		memory.Record{Content: "first fact", Scope: scope.Global, CreatedAt: first},
		memory.Record{Content: "second fact", Scope: scope.Global, CreatedAt: first.Add(time.Hour)},
		memory.Record{Content: "third fact", Scope: scope.Global, CreatedAt: first.Add(2 * time.Hour)},
	)
	mem := newHeldMemory()
	h, _ := newTestServerWith(t, "", nil, mem)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/cli/migrate",
		strings.NewReader(`{"scope":"global","from":"`+source+`"}`)).WithContext(ctx)
	rec, done := serve(h, req)

	awaitEntered(t, mem.entered, "the first write")
	cancelWellWithinGrace(cancel)
	close(mem.proceed)
	awaitDone(t, done)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[MigrateResponse](t, rec)
	if got.Stored != 1 {
		t.Fatalf("%+v: exactly the record already held by the store must be reported stored", got)
	}
	if got.Failed != 2 || len(got.Facts) != 3 {
		t.Fatalf("%+v: the two records never started must be reported failed, or the CLI exits 0 on a partial migration", got)
	}
	for _, f := range got.Facts[1:] {
		if f.Outcome != MigrateFailed || !strings.Contains(f.Error, "stopped") {
			t.Fatalf("%+v: a record never started must say the daemon stopped before copying it", f)
		}
	}
	if got.Facts[1].Content != "second fact" || got.Facts[2].Content != "third fact" {
		t.Fatalf("%+v: unstarted records must be named oldest first", got.Facts)
	}
	if n := mem.storeCalls(); n != 1 {
		t.Fatalf("the store was asked to write %d records; a cancelled request must start none beyond the one already held", n)
	}
	writes := mem.writes()
	if len(writes) != 1 || writes[0].Content != "first fact" {
		t.Fatalf("stored %+v, want only the oldest record", writes)
	}
}
