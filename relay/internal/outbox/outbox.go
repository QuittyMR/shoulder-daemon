// Package outbox holds advice waiting to be collected by whichever hook fires
// next. It is in-memory and lock-guarded: the hook path must never touch disk.
package outbox

import (
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

const maxPerSession = 8

// PlanMaxAge is how long context written for "the next prompt" stays valid.
// A plan note is about the prompt the session was on when it was written;
// the prompt that arrives a quarter of an hour later is about something else.
const PlanMaxAge = 2 * time.Minute

type Box struct {
	mu      sync.Mutex
	pending map[string][]session.Advice
}

func New() *Box { return &Box{pending: map[string][]session.Advice{}} }

// Push queues advice and reports whether it did. Advice whose text is
// already pending for the same session and addressee is refused: consults of
// one session run concurrently over overlapping windows and reach the same
// conclusion. The queue is bounded; the oldest entry is discarded rather than
// allowed to grow, because stale advice is worthless anyway.
func (b *Box) Push(a session.Advice) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[a.SessionID]
	for _, held := range q {
		if held.Text == a.Text && held.AgentID == a.AgentID && held.SpawnID == a.SpawnID &&
			held.AgentType == a.AgentType && !held.Expired(a.CreatedEvent) {
			return false
		}
	}
	if len(q) >= maxPerSession {
		q = q[1:]
	}
	b.pending[a.SessionID] = append(q, a)
	return true
}

// Take pops the first advice for the session that this event kind may carry,
// that is meant for the asker and that has not expired at this count. agentID
// and agentType name the subagent asking, both empty for the main thread.
//
// Advice the kind cannot carry, or that is addressed to someone else, is left
// in the queue rather than dropped: a note meant for the next prompt is passed
// over by every tool call in between, and is still there when the prompt
// arrives. Expired entries are discarded on the way past.
//
// A prompt is the one event plan-level context was written for, so when the
// main thread's prompt arrives, every plan note it does not take is dropped
// with it: the prompt they were about has passed. A plan note older than
// PlanMaxAge is dropped the same way, whatever event asks.
func (b *Box) Take(sessionID string, event uint64, now time.Time, kind session.Kind, agentID, agentType string) (session.Advice, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[sessionID]
	kept := q[:0:0]
	var found session.Advice
	ok := false
	mainPrompt := kind == session.KindUserPrompt && agentID == ""
	for _, a := range q {
		plan := a.Level != session.LevelAction && a.AgentID == "" && a.SpawnID == "" && a.AgentType == ""
		switch {
		case a.Expired(event):
		case plan && stale(a, now):
		case ok && plan && mainPrompt:
		case ok:
			kept = append(kept, a)
		case kind.Delivers(a.Level) && a.For(agentID, agentType):
			found, ok = a, true
		default:
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		delete(b.pending, sessionID)
	} else {
		b.pending[sessionID] = kept
	}
	return found, ok
}

func stale(a session.Advice, now time.Time) bool {
	return !a.CreatedAt.IsZero() && now.Sub(a.CreatedAt) > PlanMaxAge
}

// Address binds every note written for the subagent spawned by spawnID to
// the id the harness has since given it, so a sibling of the same type that
// asks first is passed over.
func (b *Box) Address(sessionID, spawnID, agentID string) {
	if spawnID == "" || agentID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[sessionID]
	for i := range q {
		if q[i].AgentID == "" && q[i].SpawnID == spawnID {
			q[i].AgentID = agentID
		}
	}
}

func (b *Box) Forget(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, sessionID)
}

func (b *Box) Depth() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, q := range b.pending {
		n += len(q)
	}
	return n
}
