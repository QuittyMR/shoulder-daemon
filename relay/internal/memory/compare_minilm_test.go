//go:build compare && minilm

// This file is what lets the benchmark measure the downloaded model:
//
//	SHOULDER_EMBEDDING=minilm go test -tags compare,minilm \
//	  ./internal/memory/ -run TestCompare -v
//
// It is behind a tag of its own because the model runtime is the daemon's only
// heavy dependency, and a run that measures the compiled-in table should not
// have to build it.
package memory_test

import (
	"os"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/minilm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
)

func init() {
	embedders["minilm"] = func(t *testing.T) memory.Embedder {
		t.Helper()
		dir := os.Getenv("SHOULDER_MODEL_DIR")
		if dir == "" {
			dir = memory.DefaultModelDir()
		}
		e := minilm.New(dir, nil)
		select {
		case <-e.Settled():
		case <-time.After(fetchAndLoad):
			t.Fatalf("minilm: still loading after %v", fetchAndLoad)
		}
		if err := e.Err(); err != nil {
			t.Fatalf("minilm: %v", err)
		}
		return e
	}
	// The composite is what an install gets, and it is not the model: the
	// first writes of a run land while the model is still loading and are
	// embedded with the compiled-in table, then re-embedded behind the store.
	// Measuring it separately is the only way to see whether a store that
	// caught up answers like one that never had to.
	embedders["fallback"] = func(t *testing.T) memory.Embedder {
		t.Helper()
		dir := os.Getenv("SHOULDER_MODEL_DIR")
		if dir == "" {
			dir = memory.DefaultModelDir()
		}
		return memory.NewFallback(minilm.New(dir, nil), vectors.Embedder{})
	}
}
