package memory

import (
	"context"
	"errors"
)

// Loadable is an embedder that may not be able to answer yet: its model is
// fetched or loaded after construction, and until then Embed returns
// ErrEmbedderNotReady. Settled is closed once that has finished one way or the
// other, so that whoever holds vectors from it knows when the answer stopped
// changing.
type Loadable interface {
	Embedder
	Ready() bool
	Settled() <-chan struct{}
}

// Settler is the part of Loadable a store cares about. It is a separate
// interface so a store can ask any embedder it was given whether its ID is
// still going to change, without knowing which model it is.
type Settler interface {
	Settled() <-chan struct{}
}

// Fallback answers with the primary embedder when it is ready and with the
// fallback until then. It exists because the better model arrives some time
// after the daemon starts, and a store with no embedding in the meantime is
// worse than one with the model that ships in the binary.
//
// ID reports whichever embedder would answer at that moment, because a vector
// is only comparable to vectors from the same model: tagging a fallback vector
// with the primary's name would have the store compare a hundred dimensions of
// one space against three hundred of another and call the result a score.
type Fallback struct {
	primary  Loadable
	fallback Embedder
}

func NewFallback(primary Loadable, fallback Embedder) *Fallback {
	return &Fallback{primary: primary, fallback: fallback}
}

func (f *Fallback) ID() string {
	if f.primary.Ready() {
		return f.primary.ID()
	}
	return f.fallback.ID()
}

// Embed hands the text to the primary if it is ready, and to the fallback only
// when the primary says it is not. Any other failure is returned as it is: a
// sentence the primary could not embed must not come back as a vector from
// another space under the primary's name.
func (f *Fallback) Embed(ctx context.Context, text string) ([]float32, error) {
	if f.primary.Ready() {
		v, err := f.primary.Embed(ctx, text)
		if !errors.Is(err, ErrEmbedderNotReady) {
			return v, err
		}
	}
	return f.fallback.Embed(ctx, text)
}

func (f *Fallback) Settled() <-chan struct{} { return f.primary.Settled() }

// Knows forwards to whichever embedder Embed would hand the text to, because
// the words a vector is blind to are a property of the model that produced it
// and this composite is two models. An embedder that cannot say answers that
// it knows the word, which is the same default a store applies to an embedder
// that does not implement Vocabulary at all: the guard is inert rather than
// wrong.
//
// Answering from the embedder that is current, rather than from the one that
// produced some particular vector, is right because the caller compares
// vectors from one model at a time — a record embedded by the other is scored
// on words alone and never reaches the question.
func (f *Fallback) Knows(word string) bool {
	if f.primary.Ready() {
		return knows(f.primary, word)
	}
	return knows(f.fallback, word)
}

func knows(emb Embedder, word string) bool {
	v, ok := emb.(Vocabulary)
	return !ok || v.Knows(word)
}
