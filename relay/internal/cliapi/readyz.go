package cliapi

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
)

// readyTTL is how long one probe of the store stands in for the next. The
// honest answer — read the store now, every time — is not available here:
// /readyz is asked by an adapter before every user prompt, inside a hook with a
// five second budget, so an honest route would put the store's latency in front
// of somebody's typing several times a minute. Ten seconds is short enough that
// a store which dies is reported within a prompt or two, and long enough that a
// burst of parallel sessions costs one read between all of them.
const readyTTL = 10 * time.Second

// readyProbeTimeout is the whole budget a cache miss may spend. It is far
// shorter than memoryProbeTimeout because the two routes are asked by different
// callers: somebody who typed `shoulderd doctor` will wait for a cold store to
// finish loading an embedding model, and a hook firing before a prompt will
// not. A store still warming up reads as unreachable here, which is the right
// answer to the question actually being asked — it cannot serve this prompt.
const readyProbeTimeout = 2 * time.Second

// ReadyStatus is the /readyz body. The status code is what the adapters act on;
// this is for the person reading a curl and for doctor, which is why the
// backend's own refusal is carried through verbatim rather than flattened into
// the boolean.
type ReadyStatus struct {
	OK     bool   `json:"ok"`
	Memory string `json:"memory"`
	Error  string `json:"error,omitempty"`
}

// readyCache is the last probe and when it was taken. The mutex is held across
// the probe itself, not merely around the fields, so that a burst of hooks
// arriving on a cold cache makes one read between them instead of one each; the
// wait that costs the later ones is bounded by readyProbeTimeout, which is the
// same bound the first of them already accepted.
type readyCache struct {
	mu     sync.Mutex
	at     time.Time
	last   ReadyStatus
	probed bool
}

// handleReady reports whether this daemon can do its job right now. It is
// unauthenticated on purpose, exactly like /healthz: an adapter probes it
// before it has established that it knows the token, and a readiness route that
// answered 401 would report a perfectly good daemon as not ready over the one
// fault this route is not being asked about.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	st := s.ready()
	code := http.StatusOK
	if !st.OK {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, st)
}

// ready serves the cached verdict, taking a new one when it has aged out.
func (s *Server) ready() ReadyStatus {
	s.readyz.mu.Lock()
	defer s.readyz.mu.Unlock()
	now := s.clock()
	if s.readyz.probed && now.Sub(s.readyz.at) < readyTTL {
		return s.readyz.last
	}
	s.readyz.last, s.readyz.at, s.readyz.probed = s.probeReady(), now, true
	return s.readyz.last
}

// probeReady decides the verdict from one read of the store.
func (s *Server) probeReady() ReadyStatus {
	if s.Pipe.Memory.Name() == (memory.Nop{}).Name() {
		// No store at all is not ready. That is the verdict `doctor` already
		// reaches — it exits non-zero on "memory: none" — and readiness is the
		// same question asked by a machine instead of a person, so the two must
		// not disagree. A daemon with nowhere to write watches a whole session
		// and keeps none of it, and an adapter that got a 200 here would have
		// no other moment in which to find that out.
		return ReadyStatus{
			Memory: "none",
			Error:  "no memory backend is configured; nothing observed in a session will be kept",
		}
	}
	if err := s.probeWithin(readyProbeTimeout); err != nil {
		return ReadyStatus{Memory: "unreachable", Error: err.Error()}
	}
	return ReadyStatus{OK: true, Memory: "ok"}
}

// probeWithin runs the store probe under a deadline of its own and returns
// whichever comes first. The read is handed to a goroutine and collected from a
// buffered channel because a deadline only binds a store that honours the
// context it was given; one that does not would otherwise hold this handler,
// and behind it every hook waiting on the cache lock, for as long as it stays
// wedged. The abandoned goroutine ends whenever the store finally does, and the
// buffer is what stops it blocking on a send nobody is waiting for.
//
// The context is rooted in Background rather than in the request, because the
// answer is kept for every later caller: a probe cancelled by the one adapter
// that gave up would otherwise be remembered as a store that refused.
func (s *Server) probeWithin(d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.probeStore(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("the store did not answer within %s", d)
	}
}

// clock is the time the readiness cache ages against.
func (s *Server) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
