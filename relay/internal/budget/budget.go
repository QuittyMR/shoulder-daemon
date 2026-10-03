// Package budget decides whether a piece of advice is allowed to reach the
// model. Volume control is the difference between a useful background observer
// and noise the user switches off in week one.
//
// It deliberately depends on nothing: the gate takes a flat Candidate rather
// than a session type, so the policy stays testable in isolation.
package budget

import "fmt"

const KindWarning = "warning"

type Gate struct {
	MinEventGap     int  // minimum main-thread events between note-kind injections
	MaxChars        int  // per-injection cap
	SessionMaxChars int  // whole-session cap
	DryRun          bool // evaluate and record, inject nothing
}

func Default() Gate {
	return Gate{MinEventGap: 6, MaxChars: 800, SessionMaxChars: 4000}
}

// Candidate is the shape the gate reasons about.
type Candidate struct {
	Kind         string
	Len          int
	CreatedEvent uint64
	TTLEvents    int
}

func (c Candidate) Expired(event uint64) bool {
	if c.TTLEvents <= 0 {
		return false
	}
	return event > c.CreatedEvent+uint64(c.TTLEvents)
}

// State is the counter set for one asker. The caller owns it; the gate is
// pure. A zero LastInjectEvent means nothing has been injected for this asker
// yet: the main thread's first injection lands at count one or later, and a
// subagent's state never carries a count at all.
type State struct {
	LastInjectEvent uint64
	CharsUsed       int
}

type Decision struct {
	Allow  bool
	Reason string
}

// Allow evaluates one candidate. Warnings bypass the event-gap rule but are
// still bound by the session character cap: a noisy advisor cannot escape the
// budget by labelling everything urgent.
func (g Gate) Allow(st State, event uint64, c Candidate) Decision {
	if c.Len == 0 {
		return Decision{false, "empty"}
	}
	if c.Expired(event) {
		return Decision{false, "expired"}
	}
	if st.CharsUsed+c.Len > g.SessionMaxChars {
		return Decision{false, fmt.Sprintf("session_cap:%d", g.SessionMaxChars)}
	}
	if c.Kind != KindWarning && st.LastInjectEvent > 0 && event < st.LastInjectEvent+uint64(g.MinEventGap) { //nolint:gosec // G115: a small positive setting, never near the bound
		return Decision{false, fmt.Sprintf("event_gap:%d", g.MinEventGap)}
	}
	if g.DryRun {
		return Decision{false, "dry_run"}
	}
	return Decision{true, "ok"}
}

// Record updates state after an injection actually happened.
func (st *State) Record(event uint64, c Candidate) {
	st.LastInjectEvent = event
	st.CharsUsed += c.Len
}
