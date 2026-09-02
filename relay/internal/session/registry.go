package session

import (
	"strings"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
)

// maxPendingFacts bounds explicit record_fact calls held for one turn.
const maxPendingFacts = 32

// maxSessionKeywords bounds the running note a session accumulates. The
// per-turn cap bounds one turn; nothing bounded the sum, and a long session is
// hundreds of turns. The note is written to the store and read back into every
// later prompt, so an unbounded sum is a record and a prompt that grow all day
// and are largest exactly when the session can least afford it.
//
// The most recent are the ones kept: the note exists so that a bare "do it" can
// be read against what just happened, and what happened two hundred turns ago
// is not that.
const maxSessionKeywords = 200

// State is everything the relay knows about one live session. It lives in
// memory only; nothing on the hook path touches disk.
//
// The json tags are explicit because this struct is serialised whole into the
// diagnostic session listing, and a field added here would otherwise join that
// listing by accident. What belongs there is who the session is and whether it
// is alive; what does not is the content the daemon is reasoning over, which is
// the user's work rather than the daemon's health.
type State struct {
	ID        string    `json:"id"`
	Harness   string    `json:"harness,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	GitBranch string    `json:"git_branch,omitempty"`
	OpenedAt  time.Time `json:"opened_at"`
	LastSeen  time.Time `json:"last_seen"`

	// Project is the scope this session's note is filed under.
	Project string `json:"project,omitempty"`

	Seq  uint64 `json:"seq"`
	Turn uint64 `json:"turn"`

	// Events is the window the advisor reads. It carries prompts and tool
	// output verbatim, so it is never part of a listing.
	Events []Event      `json:"-"`
	Budget budget.State `json:"budget"`

	// PendingFacts are explicit record_fact calls awaiting reconciliation.
	PendingFacts []facts.Fact `json:"pending_facts,omitempty"`

	// Keywords is what every turn of this session has been about so far, and
	// KeywordRecord is the memory record holding it. They are kept together
	// because the note is rewritten in place: each turn supersedes the record
	// the last one wrote, and a list with no id would be stored twice.
	//
	// Neither is published. The note is the session's own working vocabulary,
	// and the record id is a handle to a store nobody reading a health check
	// can act on.
	Keywords      []string `json:"-"`
	KeywordRecord string   `json:"-"`

	// KeywordsWritten is the note as the store last accepted it. A turn that
	// names nothing the session has not already named leaves Keywords exactly
	// as it was, and rewriting a record with the content it already holds is
	// not a no-op at a backend that deduplicates: it is refused, and the
	// refusal is indistinguishable in the log from losing the turn.
	KeywordsWritten string `json:"-"`

	// AdvisorInFlight prevents a slow advisor from being asked the same
	// question several times while it is still thinking.
	AdvisorInFlight bool `json:"advisor_in_flight"`
}

// Registry owns all session state behind one mutex. Every mutation is O(1) and
// allocation-light so a hook handler can complete in microseconds.
type Registry struct {
	mu        sync.Mutex
	sessions  map[string]*State
	maxEvents int
	lastSeen  time.Time
	born      time.Time
}

func NewRegistry(maxEvents int) *Registry {
	if maxEvents <= 0 {
		maxEvents = 200
	}
	return &Registry{sessions: map[string]*State{}, maxEvents: maxEvents, born: time.Now()}
}

// Observe records an event, opening the session lazily if this is the first one
// seen. Claude Code refuses HTTP hooks for SessionStart, so there is no
// explicit open: whichever event arrives first creates the session.
func (r *Registry) Observe(e Event) (turn uint64, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.sessions[e.SessionID]
	if !ok {
		st = &State{ID: e.SessionID, Harness: e.Harness, OpenedAt: e.TS}
		r.sessions[e.SessionID] = st
	}
	st.Seq++
	st.LastSeen = e.TS
	if e.CWD != "" {
		st.CWD = e.CWD
	}
	if e.GitBranch != "" {
		st.GitBranch = e.GitBranch
	}

	e.Seq = st.Seq
	st.Events = append(st.Events, e)
	// Slide the window in place rather than reslicing forward: reslicing leaves
	// the evicted events reachable through the backing array, so their tool
	// output stays live until the whole session is dropped.
	if drop := len(st.Events) - r.maxEvents; drop > 0 {
		kept := copy(st.Events, st.Events[drop:])
		for i := kept; i < len(st.Events); i++ {
			st.Events[i] = Event{}
		}
		st.Events = st.Events[:kept]
	}
	if e.Kind == KindTurnEnd {
		st.Turn++
	}
	r.lastSeen = e.TS
	return st.Turn, st.Seq
}

// Turn reports the current turn number without mutating anything.
func (r *Registry) Turn(sessionID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		return st.Turn
	}
	return 0
}

// Snapshot copies the event window for a session so the advisor can be called
// off the hook path without holding the lock.
func (r *Registry) Snapshot(sessionID string) (events []Event, turn uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return nil, 0, false
	}
	out := make([]Event, len(st.Events))
	copy(out, st.Events)
	return out, st.Turn, true
}

// ClaimAdvisor returns true if the caller now owns the right to ask the advisor
// about this session. It is released with ReleaseAdvisor.
func (r *Registry) ClaimAdvisor(sessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok || st.AdvisorInFlight {
		return false
	}
	st.AdvisorInFlight = true
	return true
}

func (r *Registry) ReleaseAdvisor(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		st.AdvisorInFlight = false
	}
}

// BudgetState returns a copy for the gate to evaluate against.
func (r *Registry) BudgetState(sessionID string) budget.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		return st.Budget
	}
	return budget.State{}
}

func (r *Registry) RecordInjection(sessionID string, turn uint64, a Advice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		st.Budget.Record(turn, a.Candidate())
	}
}

// Evicted is what one dropped session leaves behind elsewhere. The id clears
// the outbox; the keyword record is a row in a memory backend that nothing else
// will ever have a reason to remove, since the note is superseded rather than
// deleted for as long as the session lives.
type Evicted struct {
	ID string
	// Project is carried out with the eviction because deleting the note needs
	// to name the scope it is deleting from, and the state that knew it is
	// gone by the time the caller acts.
	Project       string
	KeywordRecord string
}

// CloseSession drops one session and reports how many are left. It is what a
// harness saying goodbye means: this editor is finished, and if it was the last
// one there is nothing for the daemon to stay up for.
func (r *Registry) CloseSession(sessionID string) (Evicted, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return Evicted{}, len(r.sessions)
	}
	gone := Evicted{ID: sessionID, Project: st.Project, KeywordRecord: st.KeywordRecord}
	delete(r.sessions, sessionID)
	return gone, len(r.sessions)
}

// Drain empties the registry and returns what every session was holding, for a
// daemon on its way out. The notes it hands back are the only record of
// themselves that survives this process.
func (r *Registry) Drain() []Evicted {
	r.mu.Lock()
	defer r.mu.Unlock()
	gone := make([]Evicted, 0, len(r.sessions))
	for id, st := range r.sessions {
		gone = append(gone, Evicted{ID: id, Project: st.Project, KeywordRecord: st.KeywordRecord})
		delete(r.sessions, id)
	}
	return gone
}

// Evict drops sessions untouched for longer than idleFor and returns what they
// left behind, so the caller can clean up the state this registry does not own.
// Time, not a SessionEnd event, is what makes a session dead: SessionEnd fires
// on every `claude -p` invocation and on every resume boundary.
func (r *Registry) Evict(idleFor time.Duration, now time.Time) []Evicted {
	r.mu.Lock()
	defer r.mu.Unlock()
	var gone []Evicted
	for id, st := range r.sessions {
		if now.Sub(st.LastSeen) > idleFor {
			gone = append(gone, Evicted{ID: id, Project: st.Project, KeywordRecord: st.KeywordRecord})
			delete(r.sessions, id)
		}
	}
	return gone
}

// Idle reports how long since any session sent anything, and whether any
// session is still open. A registry that has never been touched is idle from
// the moment it was built, which is what lets a daemon nobody used exit.
func (r *Registry) Idle(now time.Time) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastSeen.IsZero() {
		return now.Sub(r.born), len(r.sessions) == 0
	}
	return now.Sub(r.lastSeen), len(r.sessions) == 0
}

func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Sessions returns a shallow summary of every live session, for doctor output.
func (r *Registry) Sessions() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]State, 0, len(r.sessions))
	for _, st := range r.sessions {
		c := *st
		c.Events = nil
		out = append(out, c)
	}
	return out
}

// AddFact records a fact the agent asked to store explicitly, via the
// record_fact tool. It is held until the turn ends, so it can be reconciled
// against whatever the decision model deduced from the same turn's prose.
func (r *Registry) AddFact(sessionID string, f facts.Fact) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		st = &State{ID: sessionID, OpenedAt: time.Now(), LastSeen: time.Now()}
		r.sessions[sessionID] = st
	}
	if len(st.PendingFacts) >= maxPendingFacts {
		return
	}
	st.PendingFacts = append(st.PendingFacts, f)
}

// AddKeywords folds this turn's keywords into the session's running note and
// returns the accumulated list along with the id of the record that currently
// holds it, empty on the first turn. Repeats are dropped: a session that works
// on one file for an hour would otherwise name it in every turn.
func (r *Registry) AddKeywords(sessionID string, words []string) (recordID, note string, unchanged bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return "", "", false
	}
	seen := make(map[string]bool, len(st.Keywords)+len(words))
	for _, w := range st.Keywords {
		seen[strings.ToLower(w)] = true
	}
	for _, w := range words {
		k := strings.ToLower(w)
		if seen[k] {
			continue
		}
		seen[k] = true
		st.Keywords = append(st.Keywords, w)
	}
	// Slide the window in place rather than reslicing forward, for the same
	// reason the event window does: a reslice leaves the dropped strings
	// reachable through the backing array for as long as the session lives.
	if drop := len(st.Keywords) - maxSessionKeywords; drop > 0 {
		kept := copy(st.Keywords, st.Keywords[drop:])
		for i := kept; i < len(st.Keywords); i++ {
			st.Keywords[i] = ""
		}
		st.Keywords = st.Keywords[:kept]
	}
	note = strings.Join(st.Keywords, ", ")
	return st.KeywordRecord, note, st.KeywordRecord != "" && note == st.KeywordsWritten
}

// SetKeywordRecord points the session at where its note now lives. Superseding
// returns a new id, so the value from the previous turn is dead the moment the
// write lands.
// SetKeywordRecord records the note and the project it was filed under. The
// project is kept because deleting the note later has to name the scope it is
// deleting from, and by then the caller has only an eviction to go on.
func (r *Registry) SetKeywordRecord(sessionID, project, recordID, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		st.KeywordRecord = recordID
		st.KeywordsWritten = note
		st.Project = project
	}
}

// Keywords returns what the session has been about, for the tool that answers
// the decision model when a turn does not explain itself.
func (r *Registry) Keywords(sessionID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return nil
	}
	return append([]string(nil), st.Keywords...)
}

// TakeFacts drains the explicitly recorded facts for a session.
func (r *Registry) TakeFacts(sessionID string) []facts.Fact {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok || len(st.PendingFacts) == 0 {
		return nil
	}
	out := st.PendingFacts
	st.PendingFacts = nil
	return out
}
