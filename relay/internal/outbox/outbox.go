// Package outbox holds advice waiting to be collected by whichever hook fires
// next. It is in-memory and lock-guarded: the hook path must never touch disk.
package outbox

import (
	"sync"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

const maxPerSession = 8

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
func (b *Box) Take(sessionID string, event uint64, kind session.Kind, agentID, agentType string) (session.Advice, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[sessionID]
	kept := q[:0:0]
	var found session.Advice
	ok := false
	for _, a := range q {
		switch {
		case ok:
			kept = append(kept, a)
		case a.Expired(event):
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
