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

// Take pops the first advice for the session that this event kind may carry and
// that has not expired at this turn.
//
// Advice the kind cannot carry is left in the queue rather than dropped: a note
// meant for the next prompt is passed over by every tool call in between, and
// is still there when the prompt arrives. Expired entries are discarded on the
// way past.
func (b *Box) Take(sessionID string, turn uint64, kind session.Kind) (session.Advice, bool) {
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
		case a.Expired(turn):
		case kind.Delivers(a.Level):
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
