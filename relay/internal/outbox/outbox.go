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

// Push queues advice. The queue is bounded; the oldest entry is discarded
// rather than allowed to grow, because stale advice is worthless anyway.
func (b *Box) Push(a session.Advice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[a.SessionID]
	if len(q) >= maxPerSession {
		q = q[1:]
	}
	b.pending[a.SessionID] = append(q, a)
}

// Take pops the first advice for the session that has not expired at this turn.
// Expired entries are discarded on the way past.
func (b *Box) Take(sessionID string, turn uint64) (session.Advice, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[sessionID]
	for len(q) > 0 {
		a := q[0]
		q = q[1:]
		if a.Expired(turn) {
			continue
		}
		b.pending[sessionID] = q
		return a, true
	}
	delete(b.pending, sessionID)
	return session.Advice{}, false
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
