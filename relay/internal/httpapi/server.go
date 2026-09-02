// Package httpapi is the hook-facing surface. It is the hot path: a handler may
// only touch in-memory structures, never the advisor, the store, or the disk.
// That rule is what keeps a hook under a millisecond even when the advisor is
// wedged, and it is enforced by TestHotPathHasNoSlowDependencies.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/metrics"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

const maxBodyBytes = 8 << 20

type Server struct {
	Registry *session.Registry
	Outbox   *outbox.Box
	Metrics  *metrics.Metrics
	Queue    chan session.Event
	Token    string
	Budget   budget.Gate
	Now      func() time.Time

	// Log is optional. It is used for one line, on the first rejected request.
	Log *slog.Logger

	saidDenied sync.Once
}

func New(reg *session.Registry, box *outbox.Box, queue chan session.Event, token string, gate budget.Gate) *Server {
	m := metrics.New()
	m.SetGauges(func() int { return len(queue) }, box.Depth, reg.Len)
	return &Server{Registry: reg, Outbox: box, Metrics: m, Queue: queue, Token: token, Budget: gate, Now: time.Now}
}

// Handler builds the hook routes. It returns the mux rather than an
// http.Handler so the CLI routes, which live in a package this one may not
// import, can be mounted on the same address and the same token.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, s.Metrics.Render())
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorised(r) {
			s.deny(w)
			return
		}
		writeJSON(w, s.Registry.Sessions())
	})
	mux.HandleFunc("/v1/hooks/claude-code/", s.handleClaudeCode)
	mux.HandleFunc("/v1/events", s.handleNeutral)
	return mux
}

func (s *Server) authorised(r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got := r.Header.Get("X-Shoulder-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

// deny answers an unauthenticated caller. It still returns a well-formed empty
// hook response: a rejected request must never break the session that sent it.
func (s *Server) deny(w http.ResponseWriter) {
	s.Metrics.Inc("shoulder_unauthorised_total")
	// A rejected hook still gets a well-formed empty answer, so a
	// misconfigured token costs the session nothing and shows the person
	// nothing either. That silence is the point of failing open and it is also
	// how an install sits dead for days, so it is said out loud exactly once:
	// the condition is persistent, and logging per request would put a syscall
	// on the path this package exists to keep free of them.
	s.saidDenied.Do(func() {
		if s.Log != nil {
			s.Log.Warn("hook rejected: the caller's X-Shoulder-Token does not match this daemon's; nothing is being observed",
				"hint", "SHOULDER_TOKEN must hold the same value here and wherever the harness runs")
		}
	})
	writeRaw(w, silentJSON)
}

func (s *Server) handleClaudeCode(w http.ResponseWriter, r *http.Request) {
	start := s.Now()
	event := strings.TrimPrefix(r.URL.Path, "/v1/hooks/claude-code/")
	defer func() { s.Metrics.ObserveHook(event, s.Now().Sub(start)) }()

	if !s.authorised(r) {
		s.deny(w)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		s.Metrics.Inc("shoulder_malformed_total")
		writeRaw(w, silentJSON)
		return
	}

	if s.Log != nil {
		s.Log.Debug("hook received", "event", event, "bytes", len(body))
	}

	ev, ok := parseClaudeCode(event, body, s.Now())
	if !ok {
		s.Metrics.Inc("shoulder_unmapped_event_total")
		writeRaw(w, silentJSON)
		return
	}

	s.ingest(ev)

	{
		if a, ok := s.collect(ev.SessionID, ev.Kind); ok {
			s.saidAdvice(ev.SessionID, event, a)
			writeJSON(w, inject(event, a))
			return
		}
	}
	writeRaw(w, silentJSON)
}

// saidAdvice records the moment advice actually reaches a harness. The queued
// side is logged by the pipeline; this is the half that says it was delivered
// rather than suppressed, which is the distinction the counters alone lose.
func (s *Server) saidAdvice(sessionID, event string, a session.Advice) {
	if s.Log == nil {
		return
	}
	s.Log.Info("advice injected", "id", a.ID, "session", sessionID,
		"event", event, "text", a.Text)
}

func (s *Server) handleNeutral(w http.ResponseWriter, r *http.Request) {
	start := s.Now()
	if !s.authorised(r) {
		s.deny(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeRaw(w, noAdviceJSON)
		return
	}
	if s.Log != nil {
		s.Log.Debug("hook received", "event", "neutral", "bytes", len(body))
	}
	var ev session.Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.SessionID == "" {
		s.Metrics.Inc("shoulder_malformed_total")
		writeRaw(w, noAdviceJSON)
		return
	}
	if ev.TS.IsZero() {
		ev.TS = s.Now()
	}
	if ev.Harness == "" {
		ev.Harness = "unknown"
	}
	defer func() { s.Metrics.ObserveHook(string(ev.Kind), s.Now().Sub(start)) }()

	s.ingest(ev)

	{
		if a, ok := s.collect(ev.SessionID, ev.Kind); ok {
			s.saidAdvice(ev.SessionID, string(ev.Kind), a)
			writeJSON(w, map[string]any{"advice": a})
			return
		}
	}
	writeRaw(w, noAdviceJSON)
}

// ingest records the event and hands it to the background worker. The send is
// non-blocking: if the worker is behind, the event is dropped and counted
// rather than allowed to stall a hook.
func (s *Server) ingest(ev session.Event) {
	_, ev.Seq = s.Registry.Observe(ev)
	s.Metrics.Inc("shoulder_events_total")
	select {
	case s.Queue <- ev:
	default:
		s.Metrics.Inc("shoulder_queue_dropped_total")
	}
	// SessionEnd deliberately does NOT destroy state. A session can be resumed
	// with --continue or --resume under the same id, and `claude -p` fires
	// SessionEnd at the end of every invocation; dropping the outbox here would
	// discard advice a fraction of a second before the next turn collects it.
	// Eviction is idle-time based and lives in the pipeline janitor.
}

// collect pops one piece of advice if the budget gate allows it at this turn.
func (s *Server) collect(sessionID string, kind session.Kind) (session.Advice, bool) {
	turn := s.Registry.Turn(sessionID)
	a, ok := s.Outbox.Take(sessionID, turn, kind)
	if !ok {
		return session.Advice{}, false
	}
	d := s.Budget.Allow(s.Registry.BudgetState(sessionID), turn, a.Candidate())
	if !d.Allow {
		s.Metrics.Inc("shoulder_advice_suppressed_total")
		s.Metrics.Inc("shoulder_advice_suppressed_" + suppressLabel(d.Reason) + "_total")
		return session.Advice{}, false
	}
	s.Registry.RecordInjection(sessionID, turn, a)
	s.Metrics.Inc("shoulder_advice_emitted_total")
	return a, true
}

func suppressLabel(reason string) string {
	if i := strings.IndexByte(reason, ':'); i >= 0 {
		return reason[:i]
	}
	return reason
}

// writeRaw answers with a response that never varies. Most hook requests take
// this path — there is usually no advice pending — so they cost no marshalling.
func writeRaw(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeRaw(w, silentJSON)
		return
	}
	writeRaw(w, b)
}
