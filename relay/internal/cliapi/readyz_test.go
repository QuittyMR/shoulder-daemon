package cliapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/metrics"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/pipeline"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// newReadyServer builds the server rather than only its mux, because the
// readiness tests need the clock the cache ages against.
func newReadyServer(t *testing.T, token string, mem memory.Connector) *Server {
	t.Helper()
	cfg := config.Load()
	cfg.Budget = budget.Default()
	pipe := &pipeline.Pipeline{
		Cfg:      cfg,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:  metrics.New(),
		Settings: settings.ForProvider(nil),
		Memory:   memory.Checked(mem),
	}
	return New(pipe, token)
}

// getReady drives one probe through the whole mux, so that what is tested is
// the route as the adapters reach it and not the handler in isolation.
func getReady(t *testing.T, s *Server, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Mount(mux)
	return do(t, mux, http.MethodGet, "/readyz", "", headers...)
}

// probes reports how many times the store has been read. The lock is the
// fake's own, so this is safe to call while a handler is running.
func (f *fakeMemory) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func TestReadyReportsTheStore(t *testing.T) {
	refusing := newFakeMemory()
	refusing.searchErr = errors.New("status 401: authorization_required")

	local, err := memory.NewLocal(filepath.Join(t.TempDir(), "store.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })

	cases := []struct {
		name    string
		mem     memory.Connector
		code    int
		ok      bool
		memory  string
		because string
	}{
		{
			name: "a store that answers", mem: newFakeMemory(),
			code: http.StatusOK, ok: true, memory: "ok",
		},
		{
			name: "a store that refuses", mem: refusing,
			code: http.StatusServiceUnavailable, memory: "unreachable", because: "401",
		},
		{
			// The default install. It keeps its records in a file this process
			// owns, so there is nothing to be unreachable, and a relay that
			// reported the ordinary case as not ready would be useless.
			name: "the built-in file store", mem: local,
			code: http.StatusOK, ok: true, memory: "ok",
		},
		{
			name: "no store at all", mem: memory.Nop{},
			code: http.StatusServiceUnavailable, memory: "none", because: "configured",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getReady(t, newReadyServer(t, "", tc.mem))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body)
			}
			got := decode[ReadyStatus](t, rec)
			if got.OK != tc.ok || got.Memory != tc.memory {
				t.Fatalf("status = %+v, want ok=%v memory=%q", got, tc.ok, tc.memory)
			}
			if tc.because == "" {
				if got.Error != "" {
					t.Fatalf("a ready relay explains nothing: %+v", got)
				}
				return
			}
			if !strings.Contains(got.Error, tc.because) {
				t.Fatalf("error %q does not say why (%q)", got.Error, tc.because)
			}
		})
	}
}

// The route exists so that an adapter can find out whether anything works
// before it has established that it holds the right token. Requiring the header
// would answer "not ready" to a perfectly good daemon.
func TestReadyNeedsNoToken(t *testing.T) {
	s := newReadyServer(t, "the-secret", newFakeMemory())
	rec := getReady(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := decode[ReadyStatus](t, rec); !got.OK {
		t.Fatalf("status = %+v", got)
	}
	// The token still guards everything else on the same mux.
	if rec := getReady(t, s); rec.Code != http.StatusOK {
		t.Fatalf("a second unauthenticated probe: %d", rec.Code)
	}
	mux := http.NewServeMux()
	s.Mount(mux)
	if rec := do(t, mux, http.MethodGet, "/v1/cli/memory", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/cli/memory without a token: %d", rec.Code)
	}
}

// The hook that calls this route fires before every user prompt, so the cost of
// answering has to be a cached read almost every time.
func TestReadyCachesTheProbe(t *testing.T) {
	mem := newFakeMemory()
	s := newReadyServer(t, "", mem)
	now := time.Now()
	s.Now = func() time.Time { return now }

	for i := range 3 {
		if rec := getReady(t, s); rec.Code != http.StatusOK {
			t.Fatalf("probe %d: status %d", i, rec.Code)
		}
	}
	if n := mem.probes(); n != 1 {
		t.Fatalf("three calls inside the TTL read the store %d times, want 1", n)
	}

	now = now.Add(readyTTL - time.Millisecond)
	getReady(t, s)
	if n := mem.probes(); n != 1 {
		t.Fatalf("a call just inside the TTL read the store again: %d reads", n)
	}

	now = now.Add(2 * time.Millisecond)
	getReady(t, s)
	if n := mem.probes(); n != 2 {
		t.Fatalf("a call past the TTL read the store %d times, want 2", n)
	}
}

// A verdict that has aged out must not be answered from more than one goroutine
// at a time, or a burst of parallel sessions costs a store read each.
func TestReadyProbesOnceUnderConcurrency(t *testing.T) {
	mem := newFakeMemory()
	s := newReadyServer(t, "", mem)
	now := time.Now()
	s.Now = func() time.Time { return now }

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.ready()
		}()
	}
	wg.Wait()
	if n := mem.probes(); n != 1 {
		t.Fatalf("32 concurrent callers read the store %d times, want 1", n)
	}
}

// wedgedMemory never returns and never looks at the context it was handed,
// which is the case the deadline alone does not cover.
type wedgedMemory struct {
	*fakeMemory
	release chan struct{}
}

func (w wedgedMemory) Search(context.Context, memory.Query) ([]memory.Record, error) {
	<-w.release
	return nil, nil
}

func TestReadyAnswersWhileTheStoreIsWedged(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := newReadyServer(t, "", wedgedMemory{fakeMemory: newFakeMemory(), release: release})

	done := make(chan ReadyStatus, 1)
	go func() { done <- s.ready() }()

	select {
	case got := <-done:
		if got.OK || got.Memory != "unreachable" {
			t.Fatalf("a wedged store is unreachable, not %+v", got)
		}
		if !strings.Contains(got.Error, "did not answer") {
			t.Fatalf("error %q does not say the store never answered", got.Error)
		}
	case <-time.After(4 * readyProbeTimeout):
		t.Fatal("the handler is still waiting on the store")
	}
}
