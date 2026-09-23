package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// loadingEmbedder stands in for a model that arrives after the store opened.
// It answers with a vector whose every value is one, in as many dimensions as
// it was given, so a test can tell which model produced what by width alone.
type loadingEmbedder struct {
	id   string
	dims int

	mu      sync.Mutex
	ready   bool
	fail    error
	settled chan struct{}
	// onEmbed runs inside Embed, between the two reads of ID a caller makes,
	// which is where a model flipping is hardest to handle.
	onEmbed func()
}

func newLoadingEmbedder(id string, dims int) *loadingEmbedder {
	return &loadingEmbedder{id: id, dims: dims, settled: make(chan struct{})}
}

func (e *loadingEmbedder) ID() string { return e.id }

func (e *loadingEmbedder) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

func (e *loadingEmbedder) Settled() <-chan struct{} { return e.settled }

// arrive makes the model ready and tells whoever is waiting.
func (e *loadingEmbedder) arrive() {
	e.mu.Lock()
	e.ready = true
	e.mu.Unlock()
	close(e.settled)
}

// giveUp settles without ever becoming ready, which is a download that failed.
func (e *loadingEmbedder) giveUp() { close(e.settled) }

func (e *loadingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if e.onEmbed != nil {
		e.onEmbed()
	}
	e.mu.Lock()
	ready, fail := e.ready, e.fail
	e.mu.Unlock()
	if !ready {
		return nil, ErrEmbedderNotReady
	}
	if fail != nil {
		return nil, fail
	}
	out := make([]float32, e.dims)
	for i := range out {
		out[i] = 1
	}
	return out, nil
}

// fixedEmbedder is the model that ships: always there, always the same.
type fixedEmbedder struct {
	id      string
	dims    int
	onEmbed func()
}

func (e fixedEmbedder) ID() string { return e.id }

func (e fixedEmbedder) Embed(context.Context, string) ([]float32, error) {
	if e.onEmbed != nil {
		e.onEmbed()
	}
	out := make([]float32, e.dims)
	for i := range out {
		out[i] = 1
	}
	return out, nil
}

func TestFallbackAnswersWithWhicheverModelIsReady(t *testing.T) {
	ctx := context.Background()
	primary := newLoadingEmbedder("big-model", 8)
	f := NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2})

	if got := f.ID(); got != "small-model" {
		t.Fatalf("before the model arrives ID() = %q, want the fallback", got)
	}
	v, err := f.Embed(ctx, "the deploy target is staging")
	if err != nil || len(v) != 2 {
		t.Fatalf("before the model arrives Embed() = %d dims, %v; want the fallback's 2", len(v), err)
	}

	primary.arrive()
	if got := f.ID(); got != "big-model" {
		t.Fatalf("after the model arrives ID() = %q, want the primary", got)
	}
	v, err = f.Embed(ctx, "the deploy target is staging")
	if err != nil || len(v) != 8 {
		t.Fatalf("after the model arrives Embed() = %d dims, %v; want the primary's 8", len(v), err)
	}
}

// A sentence the primary refuses must come back as a refusal. Handing it to
// the fallback would produce a vector from another space that the store then
// files under the primary's name.
func TestFallbackDoesNotMaskAPrimaryFailure(t *testing.T) {
	primary := newLoadingEmbedder("big-model", 8)
	primary.arrive()
	primary.fail = errors.New("input sequence too long")
	f := NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2})

	v, err := f.Embed(context.Background(), "a very long fact")
	if err == nil || v != nil {
		t.Fatalf("Embed() = %v, %v; want the primary's error and no vector", v, err)
	}
	if errors.Is(err, ErrEmbedderNotReady) {
		t.Fatal("a refused sentence was reported as a model that is not ready")
	}
}

func TestFallbackSettlesWithItsPrimary(t *testing.T) {
	primary := newLoadingEmbedder("big-model", 8)
	f := NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2})
	select {
	case <-f.Settled():
		t.Fatal("settled before the primary did")
	default:
	}
	primary.giveUp()
	select {
	case <-f.Settled():
	default:
		t.Fatal("the primary settled and the composite did not")
	}
	if got := f.ID(); got != "small-model" {
		t.Fatalf("a model that never arrived is still being named: ID() = %q", got)
	}
}

// The store reads the model's name, embeds, and reads the name again. When a
// model arrives between the two, the vector belongs to neither name a caller
// can trust, and the write must not be tagged with either.
func TestAVectorIsNeverTaggedWithAModelThatDidNotProduceIt(t *testing.T) {
	primary := newLoadingEmbedder("big-model", 8)
	// The fallback is asked first because the primary is not ready; the
	// primary arrives while the fallback is answering.
	f := NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2, onEmbed: func() {
		if !primary.Ready() {
			primary.arrive()
		}
	}})

	v, err := embedTagged(context.Background(), f, "the deploy target is staging")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if v == nil || v.Model != "big-model" || len(v.Values) != 8 {
		t.Fatalf("got %+v; want the retry to land on the primary with its own 8 dimensions", v)
	}
}
