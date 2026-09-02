package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// held builds a scope of n facts for the tidying pass to read.
func held(n int) []memory.Record {
	out := make([]memory.Record, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, memory.Record{
			ID: fmt.Sprintf("mem_%d", i), Content: fmt.Sprintf("fact %d", i),
			Category: "structure", Scope: scope.Global,
		})
	}
	return out
}

func consolidateStack(t *testing.T, reply string, listed []memory.Record) (*stack, *fakeMemory) {
	t.Helper()
	ts := advisorServer(t, 0, proseBody(t, reply))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{listed: map[scope.Scope][]memory.Record{scope.Global: listed}}
	s.pipe.Memory = mem
	return s, mem
}

// A scope too small to be cluttered is left alone, so the pass costs nothing on
// a store that has barely been written to.
func TestASmallScopeIsNotTidied(t *testing.T) {
	s, mem := consolidateStack(t, `{"drop":["mem_0"],"merge":[]}`, held(consolidateFloor-1))
	dropped, merged, err := s.pipe.Consolidate(context.Background(), scope.Global, "")
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 || merged != 0 {
		t.Fatalf("got %d dropped, %d merged; a scope under the floor is left alone", dropped, merged)
	}
	if len(mem.forgets()) != 0 {
		t.Fatal("nothing should have been read or written")
	}
}

// Forget deletes. An id the pass never saw cannot be checked for scope by the
// boundary either, so a model that invents one must not be able to remove a
// record this pass was never shown.
func TestAnIdThePassNeverSawIsNotDeleted(t *testing.T) {
	s, mem := consolidateStack(t, `{"drop":["mem_1","mem_hallucinated"],"merge":[]}`, held(10))
	dropped, _, err := s.pipe.Consolidate(context.Background(), scope.Global, "")
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Fatalf("got %d dropped, want only the id that was in the listing", dropped)
	}
	if got := mem.forgets(); len(got) != 1 || got[0] != "mem_1" {
		t.Fatalf("forgot %v", got)
	}
}

// A model that misreads the instruction and returns every id would otherwise
// empty a memory that took months to build, in one call, with no way back.
func TestOnePassCannotEmptyTheScope(t *testing.T) {
	recs := held(10)
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, `"`+r.ID+`"`)
	}
	s, mem := consolidateStack(t, `{"drop":[`+strings.Join(ids, ",")+`],"merge":[]}`, recs)

	dropped, _, err := s.pipe.Consolidate(context.Background(), scope.Global, "")
	if err != nil {
		t.Fatal(err)
	}
	ceiling := int(float64(len(recs)) * consolidateCeiling)
	if dropped != ceiling {
		t.Fatalf("dropped %d of %d; the pass must stop at %d", dropped, len(recs), ceiling)
	}
	if len(mem.forgets()) != ceiling {
		t.Fatalf("forgot %v", mem.forgets())
	}
}

// A merge rewrites the record it keeps and removes the ones it replaces, so the
// rule survives in one place rather than three wordings competing in recall.
func TestAMergeKeepsOneRecordAndRemovesTheRest(t *testing.T) {
	s, mem := consolidateStack(t,
		`{"drop":[],"merge":[{"keep":"mem_0","replaces":["mem_1","mem_2"],"content":"one rule"}]}`,
		held(10))

	dropped, merged, err := s.pipe.Consolidate(context.Background(), scope.Global, "")
	if err != nil {
		t.Fatal(err)
	}
	if merged != 1 || dropped != 2 {
		t.Fatalf("got %d merged, %d dropped", merged, dropped)
	}
	stored, superseded, _ := mem.snapshot()
	if len(superseded) != 1 || superseded[0] != "mem_0" {
		t.Fatalf("the kept record must be rewritten, got %v", superseded)
	}
	if len(stored) != 1 || stored[0].Content != "one rule" {
		t.Fatalf("merged content not written: %+v", stored)
	}
}

// An unscoped tidying pass would read one project's knowledge and delete from
// another's.
func TestConsolidateRefusesAnUnscopedRequest(t *testing.T) {
	s, _ := consolidateStack(t, `{"drop":[],"merge":[]}`, held(10))
	if _, _, err := s.pipe.Consolidate(context.Background(), scope.Any, ""); err == nil {
		t.Fatal("a pass with no scope must be refused")
	}
	if _, _, err := s.pipe.Consolidate(context.Background(), scope.Local, ""); err == nil {
		t.Fatal("a local pass with no project must be refused")
	}
}
