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
	MinTurnGap      int  // minimum turns between note-kind injections
	MaxChars        int  // per-injection cap
	SessionMaxChars int  // whole-session cap
	DryRun          bool // evaluate and record, inject nothing
}

func Default() Gate {
	return Gate{MinTurnGap: 3, MaxChars: 800, SessionMaxChars: 4000}
}

// Candidate is the shape the gate reasons about.
type Candidate struct {
	Kind        string
	Len         int
	CreatedTurn uint64
	TTLTurns    int
}

func (c Candidate) Expired(turn uint64) bool {
	if c.TTLTurns <= 0 {
		return false
	}
	return turn > c.CreatedTurn+uint64(c.TTLTurns)
}

// State is the per-session counter set. The caller owns it; the gate is pure.
// A zero LastInjectTurn means nothing has been injected: advice is only ever
// created at a turn end, which has already advanced the turn past zero.
type State struct {
	LastInjectTurn uint64
	CharsUsed      int
}

type Decision struct {
	Allow  bool
	Reason string
}

// Allow evaluates one candidate. Warnings bypass the turn-gap rule but are
// still bound by the session character cap: a noisy advisor cannot escape the
// budget by labelling everything urgent.
func (g Gate) Allow(st State, turn uint64, c Candidate) Decision {
	if c.Len == 0 {
		return Decision{false, "empty"}
	}
	if c.Expired(turn) {
		return Decision{false, "expired"}
	}
	if st.CharsUsed+c.Len > g.SessionMaxChars {
		return Decision{false, fmt.Sprintf("session_cap:%d", g.SessionMaxChars)}
	}
	if c.Kind != KindWarning && st.LastInjectTurn > 0 && turn < st.LastInjectTurn+uint64(g.MinTurnGap) { //nolint:gosec // G115: a small positive setting, never near the bound
		return Decision{false, fmt.Sprintf("turn_gap:%d", g.MinTurnGap)}
	}
	if g.DryRun {
		return Decision{false, "dry_run"}
	}
	return Decision{true, "ok"}
}

// Record updates state after an injection actually happened.
func (st *State) Record(turn uint64, c Candidate) {
	st.LastInjectTurn = turn
	st.CharsUsed += c.Len
}
