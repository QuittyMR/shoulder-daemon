package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

func TestValidateAcceptsTheTwoLegalShapes(t *testing.T) {
	local := Record{Content: "the main branch is master", Scope: scope.Local, Project: "/srv/app"}
	if err := Validate(local); err != nil {
		t.Errorf("local record with a project: %v", err)
	}
	global := Record{Content: "prefers terse answers", Scope: scope.Global}
	if err := Validate(global); err != nil {
		t.Errorf("global record without a project: %v", err)
	}
}

func TestValidateRejectsEmptyContent(t *testing.T) {
	if err := Validate(Record{Scope: scope.Global}); err == nil {
		t.Fatal("a record with nothing in it must be rejected")
	}
}

// The whole point of the type: knowledge that arrived without a decision is
// refused, not filed somewhere plausible.
func TestValidateRejectsAnUnsetScopeWithErrUnscoped(t *testing.T) {
	for name, r := range map[string]Record{
		"unset":   {Content: "x"},
		"any":     {Content: "x", Scope: scope.Any},
		"unknown": {Content: "x", Scope: scope.Scope("team")},
	} {
		if err := Validate(r); !errors.Is(err, ErrUnscoped) {
			t.Errorf("%s scope: got %v, want ErrUnscoped", name, err)
		}
	}
}

func TestValidateTiesLocalToAProjectAndGlobalToNone(t *testing.T) {
	err := Validate(Record{Content: "x", Scope: scope.Local})
	if err == nil {
		t.Error("a local record with no project cannot be recalled in the right place")
	}
	if errors.Is(err, ErrUnscoped) {
		t.Error("a missing project is not a missing scope; the errors must stay distinguishable")
	}

	err = Validate(Record{Content: "x", Scope: scope.Global, Project: "/srv/app"})
	if err == nil {
		t.Fatal("a global record naming a project would be recalled in one project only")
	}
	if !strings.Contains(err.Error(), "/srv/app") {
		t.Errorf("the error should name the offending project, got %q", err)
	}
}

func TestValidateQueryAllowsAnUnfilteredScopeButNotAProjectlessLocalOne(t *testing.T) {
	if err := ValidateQuery(Query{Text: "q", Scope: scope.Any}); err != nil {
		t.Errorf("Any means do not filter, which is a legal query: %v", err)
	}
	if err := ValidateQuery(Query{Text: "q", Scope: scope.Global}); err != nil {
		t.Errorf("global query: %v", err)
	}
	if err := ValidateQuery(Query{Text: "q", Scope: scope.Local, Project: "/srv/app"}); err != nil {
		t.Errorf("local query with a project: %v", err)
	}
	if err := ValidateQuery(Query{Text: "q", Scope: scope.Local}); err == nil {
		t.Error("a local query with no project would read some other project's memory")
	}
	if err := ValidateQuery(Query{Text: "q", Scope: scope.Scope("team")}); err == nil {
		t.Error("an unknown scope must not be treated as no filter")
	}
}

// spy is a connector that records what reached it, so a test can assert that a
// rejected call reached nothing at all.
type spy struct {
	calls []string
	err   error
	// held is what List returns. A legal supersede names a record that exists in
	// the replacement's scope, so a spy that lists nothing cannot model one.
	held []Record
}

func (s *spy) Name() string { return "spy" }

func (s *spy) Search(context.Context, Query) ([]Record, error) {
	s.calls = append(s.calls, "Search")
	return nil, s.err
}

func (s *spy) List(context.Context, Query) ([]Record, error) {
	s.calls = append(s.calls, "List")
	return s.held, s.err
}

func (s *spy) Store(context.Context, Record) (string, error) {
	s.calls = append(s.calls, "Store")
	return "id", s.err
}

func (s *spy) Supersede(context.Context, string, Record) (string, error) {
	s.calls = append(s.calls, "Supersede")
	return "id", s.err
}

func (s *spy) Forget(context.Context, string, Query) error {
	s.calls = append(s.calls, "Forget")
	return s.err
}

func TestCheckedKeepsUnscopedWritesAwayFromTheBackend(t *testing.T) {
	ctx := context.Background()

	t.Run("store", func(t *testing.T) {
		inner := &spy{}
		id, err := Checked(inner).Store(ctx, Record{Content: "x"})
		if !errors.Is(err, ErrUnscoped) {
			t.Fatalf("got id=%q err=%v, want ErrUnscoped", id, err)
		}
		if len(inner.calls) != 0 {
			t.Fatalf("the unscoped record reached the backend: %v", inner.calls)
		}
	})

	t.Run("supersede", func(t *testing.T) {
		inner := &spy{}
		if _, err := Checked(inner).Supersede(ctx, "old", Record{Content: "x"}); !errors.Is(err, ErrUnscoped) {
			t.Fatalf("got %v, want ErrUnscoped", err)
		}
		if len(inner.calls) != 0 {
			t.Fatalf("the unscoped record reached the backend: %v", inner.calls)
		}
	})

	t.Run("local store without a project", func(t *testing.T) {
		inner := &spy{}
		if _, err := Checked(inner).Store(ctx, Record{Content: "x", Scope: scope.Local}); err == nil {
			t.Fatal("want an error")
		}
		if len(inner.calls) != 0 {
			t.Fatalf("the record reached the backend: %v", inner.calls)
		}
	})
}

func TestCheckedKeepsProjectlessLocalReadsAwayFromTheBackend(t *testing.T) {
	ctx := context.Background()

	inner := &spy{}
	if _, err := Checked(inner).Search(ctx, Query{Text: "q", Scope: scope.Local}); err == nil {
		t.Fatal("want an error")
	}
	if len(inner.calls) != 0 {
		t.Fatalf("the query reached the backend: %v", inner.calls)
	}

	inner = &spy{}
	if _, err := Checked(inner).List(ctx, Query{Scope: scope.Local}); err == nil {
		t.Fatal("want an error")
	}
	if len(inner.calls) != 0 {
		t.Fatalf("the query reached the backend: %v", inner.calls)
	}
}

func TestCheckedPassesLegalCallsThrough(t *testing.T) {
	ctx := context.Background()
	inner := &spy{held: []Record{{ID: "old", Content: "x", Scope: scope.Local, Project: "/srv/app"}}}
	c := Checked(inner)

	if _, err := c.Store(ctx, Record{Content: "x", Scope: scope.Global}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Supersede(ctx, "old", Record{Content: "x", Scope: scope.Local, Project: "/srv/app"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(ctx, Query{Text: "q", Scope: scope.Any}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(ctx, Query{Scope: scope.Local, Project: "/srv/app"}); err != nil {
		t.Fatal(err)
	}
	// The List before Supersede is the boundary confirming the record being
	// replaced is in the scope the replacement claims.
	want := []string{"Store", "List", "Supersede", "Search", "List"}
	if strings.Join(inner.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", inner.calls, want)
	}
	if c.Name() != inner.Name() {
		t.Errorf("the wrapper must not rename the backend: %q", c.Name())
	}
}

// Without a backend, recalling nothing is the truth and is reported as such,
// while a write has to fail: a caller told its fact was stored will never write
// it again.
func TestNopRecallsNothingAndRefusesToPretendItStored(t *testing.T) {
	var c Connector = Nop{}
	ctx := context.Background()

	got, err := c.Search(ctx, Query{Text: "q", Scope: scope.Global})
	if err != nil || got != nil {
		t.Fatalf("nop search: %v %v", got, err)
	}
	if got, err := c.List(ctx, Query{Scope: scope.Any}); err != nil || got != nil {
		t.Fatalf("nop list: %v %v", got, err)
	}
	if id, err := c.Store(ctx, Record{Content: "x", Scope: scope.Global}); !errors.Is(err, ErrNoBackend) || id != "" {
		t.Fatalf("nop store: got %q, %v; want ErrNoBackend and no id", id, err)
	}
	if id, err := c.Supersede(ctx, "old", Record{Content: "x", Scope: scope.Global}); !errors.Is(err, ErrNoBackend) || id != "" {
		t.Fatalf("nop supersede: got %q, %v; want ErrNoBackend and no id", id, err)
	}
	// Unlike the other writes, forgetting succeeds: what the caller wants gone
	// was never there, so a janitor tidying up after a session on a daemon with
	// no memory service has nothing to report.
	if err := c.Forget(ctx, "old", Query{Scope: scope.Local, Project: "/srv/app", Kind: KindSession}); err != nil {
		t.Fatalf("nop forget: got %v, want success", err)
	}
	if c.Name() == "" {
		t.Error("even the no-backend connector must name itself for logs")
	}
}

// A digest reads a scope whole. Unscoped it would read every project the user
// has ever worked in, so the wrapper refuses before any backend is asked.
func TestCheckedRefusesAnUnscopedList(t *testing.T) {
	inner := &spy{}
	got, err := Checked(inner).List(context.Background(), Query{Scope: scope.Any})
	// Matched rather than merely non-nil: a caller has to tell "you never said
	// which knowledge you wanted" apart from "the store is broken", and answer
	// each differently.
	if !errors.Is(err, ErrUnscopedList) {
		t.Fatalf("got %+v, %v; want ErrUnscopedList", got, err)
	}
	if len(inner.calls) != 0 {
		t.Fatalf("the query reached the backend: %v", inner.calls)
	}
	// Search is the read that may go unfiltered: its results are chosen by the
	// text at hand rather than returned wholesale.
	if _, err := Checked(inner).Search(context.Background(), Query{Text: "q", Scope: scope.Any}); err != nil {
		t.Fatalf("unfiltered search: %v", err)
	}
}

// A record read back may carry either form of project, and callers compare
// projects through ProjectKey precisely so they do not have to know which.
func TestProjectKeyIsTheSameWhicheverFormCameBack(t *testing.T) {
	const project = "/srv/app"
	written := Record{Content: "x", Scope: scope.Local, Project: project}
	read := Record{Content: "x", Scope: scope.Local, Project: scope.Key(project)}

	if written.ProjectKey() != scope.Key(project) {
		t.Errorf("from the path: got %q, want %q", written.ProjectKey(), scope.Key(project))
	}
	if read.ProjectKey() != scope.Key(project) {
		t.Errorf("from the key: got %q, want %q; hashing an already-hashed key loses the project", read.ProjectKey(), scope.Key(project))
	}
	if global := (Record{Content: "x", Scope: scope.Global}); global.ProjectKey() != "" {
		t.Errorf("a global record belongs to no project, got key %q", global.ProjectKey())
	}
}

// fake is the smallest store that can satisfy the conformance suite, so the
// suite is exercised here rather than only in whatever a future connector
// author writes. It keeps projects as keys, the way a backend receives them,
// and scores nothing, which is how it stands in for a backend MinScore cannot
// filter.
type fake struct {
	mu   sync.Mutex
	next int
	recs map[string]Record
}

func newFake() *fake { return &fake{recs: map[string]Record{}} }

func (f *fake) Name() string { return "fake" }

func (f *fake) inScope(r Record, q Query) bool {
	switch q.Scope {
	case scope.Any:
		return true
	case scope.Global:
		return r.Scope == scope.Global
	default:
		return r.Scope == scope.Local && r.ProjectKey() == scope.Key(q.Project)
	}
}

func (f *fake) selected(q Query) []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []Record
	for _, r := range f.recs {
		if f.inScope(r, q) && r.Kind == q.Kind {
			got = append(got, r)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
	if q.Limit > 0 && len(got) > q.Limit {
		got = got[:q.Limit]
	}
	return got
}

// Ranking is a backend's own business; answering with everything it holds in
// scope is a legal, and unhelpful, way to rank.
func (f *fake) Search(_ context.Context, q Query) ([]Record, error) { return f.selected(q), nil }

func (f *fake) List(_ context.Context, q Query) ([]Record, error) { return f.selected(q), nil }

func (f *fake) Store(_ context.Context, r Record) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	r.ID = fmt.Sprintf("fake-%d", f.next)
	r.Project = scope.Key(r.Project)
	f.recs[r.ID] = r
	return r.ID, nil
}

func (f *fake) Supersede(ctx context.Context, oldID string, r Record) (string, error) {
	f.mu.Lock()
	delete(f.recs, oldID)
	f.mu.Unlock()
	return f.Store(ctx, r)
}

func (f *fake) Forget(_ context.Context, id string, _ Query) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.recs, id)
	return nil
}

func TestFakeConnectorConformance(t *testing.T) {
	TestConnector(t, func() Connector { return newFake() })
}

func TestNopConnectorConformance(t *testing.T) {
	TestConnector(t, func() Connector { return Nop{} })
}

// Both refusals are correct, and they are not the same event. A record seen in
// another scope was named by mistake; a record seen nowhere may simply have
// been replaced already, which is what a supersede that committed and then
// failed on the way home leaves behind. The caller that wrote the id is the
// only party that can tell those apart, so the boundary has to hand it the
// difference instead of one indistinguishable refusal.
func TestARefusedSupersedeSaysWhetherTheTargetWasSeenElsewhere(t *testing.T) {
	ctx := context.Background()
	c := Checked(newFake())

	globalID, err := c.Store(ctx, Record{Content: "prefers terse answers", Scope: scope.Global})
	if err != nil {
		t.Fatal(err)
	}
	local := Record{Content: "prefers terse answers", Scope: scope.Local, Project: "/srv/app"}

	var cross *ErrCrossScopeSupersede
	if _, err := c.Supersede(ctx, globalID, local); !errors.As(err, &cross) {
		t.Fatalf("a local record must not be able to swallow a global one, got %v", err)
	}
	if cross.OldID != globalID {
		t.Errorf("the refusal must name the record it is about, got %q", cross.OldID)
	}
	if !cross.Elsewhere {
		t.Error("the target was read, current, in the global scope; the refusal must say so")
	}
	if !strings.Contains(cross.Error(), "current in another one") {
		t.Errorf("the message must state what was observed: %q", cross.Error())
	}

	// The record a session wrote for itself, then lost: superseded at the
	// backend by a write whose reply never arrived, so its id is a handle to
	// nothing. Nothing about another scope was established here.
	rec := Record{Content: "session keywords: parser", Kind: KindSession, Scope: scope.Local, Project: "/srv/app"}
	goneID, err := c.Store(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Forget(ctx, goneID, Query{Scope: scope.Local, Project: "/srv/app", Kind: KindSession}); err != nil {
		t.Fatal(err)
	}
	rec.Content = "session keywords: parser, loader"
	if _, err := c.Supersede(ctx, goneID, rec); !errors.As(err, &cross) {
		t.Fatalf("the write must still be refused rather than rewriting an unseen record, got %v", err)
	}
	if cross.Elsewhere {
		t.Error("nothing was seen in another scope, and saying otherwise sends a reader hunting a placement bug that never happened")
	}
	if msg := cross.Error(); !strings.Contains(msg, "already replaced") {
		t.Errorf("the message must allow for the record having gone: %q", msg)
	}
	if got, _ := c.List(ctx, Query{Scope: scope.Global}); len(got) != 1 {
		t.Errorf("the global record must be untouched, got %+v", got)
	}
}
