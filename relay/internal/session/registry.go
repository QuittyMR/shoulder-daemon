package session

import (
	"strings"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
)

// maxSpawns bounds the subagent spawns a session remembers while waiting for
// the harness to name them. A session fans out a handful at a time; one that
// holds more has agents that never reported starting.
const maxSpawns = 16

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

	// Project is the scope this session's note is filed under, and ProjectDir
	// the directory it was resolved from. The directory is kept beside it
	// rather than read from CWD when it is needed, because CWD follows every
	// event: a session that has moved would otherwise hand a cleanup the
	// project of one checkout with the path of another, and a backend that
	// files local facts with the code takes that pair as the truth about where
	// the project is.
	Project    string `json:"project,omitempty"`
	ProjectDir string `json:"-"`

	Seq  uint64 `json:"seq"`
	Turn uint64 `json:"turn"`

	// Events is the window the advisor reads. It carries prompts and tool
	// output verbatim, so it is never part of a listing.
	Events []Event      `json:"-"`
	Budget budget.State `json:"budget"`

	// Spawns are the subagents this session has started, in order, each
	// bound to its harness id once the harness has reported it. A spawn is
	// known by the tool call that made it, because that is all a subagent's
	// prompt carries; the id arrives later, on the agent's first own event.
	Spawns []Spawn `json:"-"`

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

	// keywordWrite serialises the rewrite of the keyword record. It lives
	// here so that it goes when the session does, with nothing to clean up.
	keywordWrite *sync.Mutex
}

// Spawn is one subagent the session started: the tool call that spawned it,
// its type, and the id the harness gave it, empty until it has.
type Spawn struct {
	SpawnID   string
	AgentType string
	AgentID   string
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
		st = &State{ID: e.SessionID, Harness: e.Harness, OpenedAt: e.TS, keywordWrite: &sync.Mutex{}}
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
	// A turn is the user's: everything a subagent does happens inside the
	// main thread's current turn, and advice is aged and budgeted in turns.
	if e.Kind == KindTurnEnd && e.Origin == OriginUser {
		st.Turn++
	}
	st.observeSpawn(e)
	r.lastSeen = e.TS
	return st.Turn, st.Seq
}

// observeSpawn keeps the spawn list current: a subagent's prompt opens a
// spawn under its tool call, the agent's start binds the oldest unbound spawn
// of its type - or of any type, when the harness names types differently in
// the two places - to the id, and its stop closes it.
func (st *State) observeSpawn(e Event) {
	if e.Origin != OriginAgent {
		return
	}
	switch e.Kind {
	case KindUserPrompt:
		if e.AgentID != "" || e.ToolUseID == "" {
			return
		}
		if len(st.Spawns) >= maxSpawns {
			st.Spawns = st.Spawns[1:]
		}
		st.Spawns = append(st.Spawns, Spawn{SpawnID: e.ToolUseID, AgentType: e.AgentType})
	case KindAgentStart:
		if e.AgentID == "" {
			return
		}
		unbound := -1
		for i, sp := range st.Spawns {
			if sp.AgentID != "" {
				continue
			}
			if strings.EqualFold(sp.AgentType, e.AgentType) {
				st.Spawns[i].AgentID = e.AgentID
				return
			}
			if unbound < 0 {
				unbound = i
			}
		}
		if unbound >= 0 {
			st.Spawns[unbound].AgentID = e.AgentID
		}
	case KindTurnEnd:
		for i, sp := range st.Spawns {
			if sp.AgentID == e.AgentID {
				st.Spawns = append(st.Spawns[:i], st.Spawns[i+1:]...)
				return
			}
		}
	}
}

// AgentOf returns the id the harness gave the subagent spawned by spawnID,
// empty while it has not reported starting.
func (r *Registry) AgentOf(sessionID, spawnID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		for _, sp := range st.Spawns {
			if sp.SpawnID == spawnID {
				return sp.AgentID
			}
		}
	}
	return ""
}

// SpawnOf returns the tool call that spawned the subagent agentID, empty when
// the session never saw its prompt.
func (r *Registry) SpawnOf(sessionID, agentID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		for _, sp := range st.Spawns {
			if sp.AgentID == agentID {
				return sp.SpawnID
			}
		}
	}
	return ""
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

// LockKeywords holds the session's keyword record for one rewrite: reading
// the list, writing the record and noting where it now lives. Consults of
// one session run concurrently, and two of them superseding the same record
// would leave two. It reports false for a session that is gone.
func (r *Registry) LockKeywords(sessionID string) (unlock func(), ok bool) {
	for {
		mu := r.keywordWrite(sessionID)
		if mu == nil {
			return nil, false
		}
		mu.Lock()
		// A session closed and reopened while this waited has a new state,
		// and the lock that guards it is the new one.
		if r.keywordWrite(sessionID) == mu {
			return mu.Unlock, true
		}
		mu.Unlock()
	}
}

func (r *Registry) keywordWrite(sessionID string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		return st.keywordWrite
	}
	return nil
}

// BudgetState returns a copy for the gate to evaluate against, for the asker
// named by agentID. The session's character cap is shared by everyone in it;
// the turn gap is the main thread's alone. A subagent lives inside one turn,
// so a gap keyed on it would either never pass for the agent or, counted
// against the main thread, silence the person's next turns because an agent
// was told something.
func (r *Registry) BudgetState(sessionID, agentID string) budget.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return budget.State{}
	}
	if agentID != "" {
		return budget.State{CharsUsed: st.Budget.CharsUsed}
	}
	return st.Budget
}

// RecordInjection charges an injection to the session. One handed to a
// subagent counts against the characters and leaves the turn gap alone.
func (r *Registry) RecordInjection(sessionID string, turn uint64, agentID string, a Advice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[sessionID]
	if !ok {
		return
	}
	if agentID != "" {
		st.Budget.CharsUsed += a.Candidate().Len
		return
	}
	st.Budget.Record(turn, a.Candidate())
}

// Evicted is what one dropped session leaves behind elsewhere. The id clears
// the outbox; the keyword record is a row in a memory backend that nothing else
// will ever have a reason to remove, since the note is superseded rather than
// deleted for as long as the session lives.
type Evicted struct {
	ID string
	// Project is carried out with the eviction because deleting the note needs
	// to name the scope it is deleting from, and the state that knew it is
	// gone by the time the caller acts. Dir is the directory that project was
	// resolved from, for a backend that has to find the checkout to delete
	// from it; the two travel together and are never taken from different
	// turns.
	Project       string
	Dir           string
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
	gone := Evicted{ID: sessionID, Project: st.Project, Dir: st.ProjectDir, KeywordRecord: st.KeywordRecord}
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
		gone = append(gone, Evicted{ID: id, Project: st.Project, Dir: st.ProjectDir, KeywordRecord: st.KeywordRecord})
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
			gone = append(gone, Evicted{ID: id, Project: st.Project, Dir: st.ProjectDir, KeywordRecord: st.KeywordRecord})
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
// SetKeywordRecord records the note, the project it was filed under and the
// directory that project was resolved from. The project is kept because
// deleting the note later has to name the scope it is deleting from, and by
// then the caller has only an eviction to go on; the directory is kept with it
// so the two cannot come from different turns.
func (r *Registry) SetKeywordRecord(sessionID, project, dir, recordID, note string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.sessions[sessionID]; ok {
		st.KeywordRecord = recordID
		st.KeywordsWritten = note
		st.Project = project
		st.ProjectDir = dir
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
