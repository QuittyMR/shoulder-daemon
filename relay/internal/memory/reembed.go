package memory

import (
	"context"
	"errors"
	"fmt"
)

// reembedBatch is how many records are embedded between two takes of the
// lock. A batch is what bounds how long a write waits behind the pass and how
// much work a crash mid-pass throws away; inference happens outside the lock
// either way.
const reembedBatch = 32

// watch runs the one pass a store needs after opening. It waits for an
// embedder that is still loading, because a pass taken before that would
// rewrite every vector into the fallback's space and a second pass would
// rewrite them all again minutes later, on every start.
func (l *Local) watch(ctx context.Context) {
	defer close(l.done)
	if s, ok := l.emb.(Settler); ok {
		select {
		case <-s.Settled():
		case <-ctx.Done():
			return
		}
	}
	n, err := l.Reembed(ctx)
	log := l.logger()
	if log == nil {
		return
	}
	// A pass cut short by Close is the store shutting down, not something
	// going wrong: the next start picks up where it stopped.
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Warn("re-embedding stopped early; the rest is scored on words until the next start",
			"path", l.path, "done", n, "error", err)
		return
	}
	if n > 0 {
		log.Info("re-embedded stored facts with the current model", "path", l.path, "facts", n, "model", l.emb.ID())
	}
}

// Reembed gives every record whose vector is missing or came from another
// model a vector from the current one, and reports how many it rewrote. It is
// safe beside reads and writes: a record is scored on words in common until
// its turn comes, and one that was replaced or forgotten meanwhile is left
// alone.
//
// It stops, rather than failing, when the model changes under it or is not
// ready: the store is still consistent, every vector is tagged with the model
// that made it, and the pass for the model that did arrive is the caller's to
// run.
func (l *Local) Reembed(ctx context.Context) (int, error) {
	if l.emb == nil {
		return 0, nil
	}
	l.reembedMu.Lock()
	defer l.reembedMu.Unlock()

	model := l.emb.ID()
	stale := l.stale(model)
	done := 0
	for start := 0; start < len(stale); start += reembedBatch {
		batch := stale[start:min(start+reembedBatch, len(stale))]
		fresh := make(map[string]vector, len(batch))
		for _, r := range batch {
			if err := ctx.Err(); err != nil {
				// Inference is the expensive part; what it already produced
				// is kept rather than paid for again on the next start.
				n, aerr := l.adopt(model, fresh)
				done += n
				if aerr != nil {
					return done, fmt.Errorf("re-embedding: %w", aerr)
				}
				return done, err
			}
			vec, err := embedTagged(ctx, l.emb, r.Content)
			if errors.Is(err, ErrEmbedderNotReady) {
				// Every vector already in the batch was checked against the
				// model this pass started with, so it is as good as one from a
				// batch that finished.
				n, aerr := l.adopt(model, fresh)
				done += n
				if aerr != nil {
					return done, fmt.Errorf("re-embedding: %w", aerr)
				}
				return done, nil
			}
			if err != nil {
				// One sentence the model refuses is not a reason to leave the
				// rest of the store on the old vectors.
				continue
			}
			if vec == nil {
				continue
			}
			if vec.Model != model {
				// The batch so far is in the old model's space, which is the
				// one the pass for the new model is about to replace.
				return done, nil
			}
			fresh[r.ID] = *vec
		}
		n, err := l.adopt(model, fresh)
		done += n
		if err != nil {
			return done, fmt.Errorf("re-embedding: %w", err)
		}
	}
	return done, nil
}

// stale is a copy of the id and content of every record that needs the pass,
// taken under the read lock so inference can happen without it.
func (l *Local) stale(model string) []Record {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []Record
	for _, r := range l.recs {
		if v, ok := l.vecs[r.ID]; ok && v.Model == model {
			continue
		}
		out = append(out, Record{ID: r.ID, Content: r.Content})
	}
	return out
}

// adopt swaps in one batch of vectors and saves. A vector is kept only for a
// record that is still here and still lacks one from this model, because a
// write that landed during inference already produced the better answer.
func (l *Local) adopt(model string, fresh map[string]vector) (int, error) {
	if len(fresh) == 0 {
		return 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.recs {
		v, ok := fresh[r.ID]
		if !ok {
			continue
		}
		if cur, ok := l.vecs[r.ID]; ok && cur.Model == model {
			continue
		}
		l.vecs[r.ID] = v
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, l.saveLocked()
}
