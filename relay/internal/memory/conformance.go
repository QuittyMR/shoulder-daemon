package memory

import (
	"context"
	"errors"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// Projects used by the suite. They are absolute paths that exist nowhere, so a
// connector talking to a real service cannot collide with a real checkout.
const (
	conformanceProjectA = "/shoulder-daemon/conformance-a"
	conformanceProjectB = "/shoulder-daemon/conformance-b"
)

// TestConnector runs the behaviour a Connector must have to be safe in this
// system. It lives in the package proper rather than in a test file so that
// somebody writing a second connector, in their own repository, can run it
// against theirs.
//
// It checks the rules that are load-bearing and invisible in the signatures:
// that a scoped read never sees another scope's records, that a list without a
// scope is refused, that a superseded record never comes back, that a supersede
// keeps the replacement where the original was, and that a query which never
// mentioned Kind is answered with facts alone. Connectors are exercised through
// Checked, because that is how they are used in production; a connector tested
// bare would be measured against a contract it does not actually ship under.
//
// newConnector must return an empty store each time it is called. A connector
// with no backend — one whose writes report ErrNoBackend — skips the parts that
// need a write rather than passing them vacuously.
func TestConnector(t *testing.T, newConnector func() Connector) {
	t.Helper()
	ctx := context.Background()
	open := func() Connector { return Checked(newConnector()) }

	t.Run("a list without a scope is refused", func(t *testing.T) {
		got, err := open().List(ctx, Query{Scope: scope.Any})
		if !errors.Is(err, ErrUnscopedList) {
			t.Fatalf("got %+v, %v; a digest must never read every project at once, and callers match the refusal with errors.Is", got, err)
		}
	})

	t.Run("Store round-trips scope", func(t *testing.T) {
		c := open()
		local := Record{Content: "the integration tests need a live Postgres", Scope: scope.Local, Project: conformanceProjectA}
		global := Record{Content: "prefers terse answers", Scope: scope.Global}
		conformanceStore(ctx, t, c, local)
		conformanceStore(ctx, t, c, global)

		got := conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA})
		rec, ok := conformanceFind(got, local.Content)
		if !ok {
			t.Fatalf("the stored local record was not listed back: %+v", got)
		}
		if rec.Scope != scope.Local {
			t.Errorf("scope did not survive the round trip: got %q", rec.Scope)
		}
		if rec.ProjectKey() != scope.Key(conformanceProjectA) {
			t.Errorf("project did not survive the round trip: got %q", rec.Project)
		}

		got = conformanceList(ctx, t, c, Query{Scope: scope.Global})
		rec, ok = conformanceFind(got, global.Content)
		if !ok {
			t.Fatalf("the stored global record was not listed back: %+v", got)
		}
		if rec.Scope != scope.Global {
			t.Errorf("scope did not survive the round trip: got %q", rec.Scope)
		}
		if rec.Project != "" {
			t.Errorf("a global record must name no project, got %q", rec.Project)
		}
	})

	t.Run("a scoped read sees only its own scope", func(t *testing.T) {
		c := open()
		const (
			inA      = "conformance: the deploy script lives in bin/ship"
			inB      = "conformance: the deploy script lives in tools/release"
			inGlobal = "conformance: the deploy script is always run by hand"
		)
		conformanceStore(ctx, t, c, Record{Content: inA, Scope: scope.Local, Project: conformanceProjectA})
		conformanceStore(ctx, t, c, Record{Content: inB, Scope: scope.Local, Project: conformanceProjectB})
		conformanceStore(ctx, t, c, Record{Content: inGlobal, Scope: scope.Global})

		for _, read := range conformanceReads(ctx, c, "conformance: the deploy script") {
			inspect := func(q Query, want string, forbidden ...string) {
				got, err := read.fn(q)
				if err != nil {
					t.Fatalf("%s %v: %v", read.name, q, err)
				}
				for _, f := range forbidden {
					if _, found := conformanceFind(got, f); found {
						t.Errorf("%s under %s/%s returned another scope's record %q",
							read.name, q.Scope, scope.Label(q.Project), f)
					}
				}
				// Only List is exhaustive; a search may legitimately rank the
				// wanted record out of the answer, so its absence proves
				// nothing and is not asserted here.
				if read.exhaustive {
					if _, found := conformanceFind(got, want); !found {
						t.Errorf("%s under %s/%s lost its own record: %+v",
							read.name, q.Scope, scope.Label(q.Project), got)
					}
				}
			}
			inspect(Query{Scope: scope.Local, Project: conformanceProjectA}, inA, inB, inGlobal)
			inspect(Query{Scope: scope.Local, Project: conformanceProjectB}, inB, inA, inGlobal)
			inspect(Query{Scope: scope.Global}, inGlobal, inA, inB)
		}
	})

	t.Run("a superseded record never comes back", func(t *testing.T) {
		c := open()
		const (
			stale   = "conformance: releases ship on Fridays"
			current = "conformance: releases never ship on Fridays"
		)
		old := Record{Content: stale, Scope: scope.Local, Project: conformanceProjectA}
		oldID := conformanceStore(ctx, t, c, old)
		if oldID == "" {
			t.Fatal("Store returned no id: nothing can ever supersede this record")
		}
		if _, err := c.Supersede(ctx, oldID, Record{Content: current, Scope: scope.Local, Project: conformanceProjectA}); err != nil {
			t.Fatalf("supersede: %v", err)
		}

		for _, read := range conformanceReads(ctx, c, "conformance: releases ship") {
			got, err := read.fn(Query{Scope: scope.Local, Project: conformanceProjectA})
			if err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			if _, found := conformanceFind(got, stale); found {
				t.Errorf("%s still returns the superseded content; every later turn will supersede it again", read.name)
			}
			for _, r := range got {
				if r.ID == oldID {
					t.Errorf("%s still returns the superseded id %q", read.name, oldID)
				}
			}
		}
		if got := conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA}); len(got) > 0 {
			if _, found := conformanceFind(got, current); !found {
				t.Errorf("the replacement is not listed where the original was: %+v", got)
			}
		}
	})

	t.Run("Supersede carries scope and project through", func(t *testing.T) {
		c := open()
		const (
			stale   = "conformance: the main branch is called trunk"
			current = "conformance: the main branch is called master"
		)
		oldID := conformanceStore(ctx, t, c, Record{Content: stale, Scope: scope.Local, Project: conformanceProjectA})
		if oldID == "" {
			t.Fatal("Store returned no id: nothing can ever supersede this record")
		}
		if _, err := c.Supersede(ctx, oldID, Record{Content: current, Scope: scope.Local, Project: conformanceProjectA}); err != nil {
			t.Fatalf("supersede: %v", err)
		}

		got := conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA})
		rec, ok := conformanceFind(got, current)
		if !ok {
			t.Fatalf("the replacement is not in the scope the original was in: %+v", got)
		}
		if rec.Scope != scope.Local || rec.ProjectKey() != scope.Key(conformanceProjectA) {
			t.Errorf("the replacement landed elsewhere: scope %q project %q", rec.Scope, rec.Project)
		}
		for _, q := range []Query{
			{Scope: scope.Local, Project: conformanceProjectB},
			{Scope: scope.Global},
		} {
			if _, found := conformanceFind(conformanceList(ctx, t, c, q), current); found {
				t.Errorf("the replacement leaked into %s/%s", q.Scope, scope.Label(q.Project))
			}
		}
	})
	// The working notes of one session are written to the same store as the
	// knowledge, under the same scope, and are meaningless a day later. Every
	// caller that existed before them asks for facts by asking for nothing, so
	// the cost of a connector ignoring Kind is a digest, a fact list or a
	// recall quietly filling with keyword soup.
	t.Run("a session record stays out of an answer that did not ask for one", func(t *testing.T) {
		c := open()
		const (
			fact    = "conformance: the release rota lives in docs/rota.md"
			working = "conformance session keywords: release, rota, docs"
		)
		conformanceStore(ctx, t, c, Record{Content: fact, Scope: scope.Local, Project: conformanceProjectA})
		conformanceStore(ctx, t, c, Record{
			Content: working, Kind: KindSession, Scope: scope.Local, Project: conformanceProjectA,
		})

		for _, read := range conformanceReads(ctx, c, "conformance release rota") {
			got, err := read.fn(Query{Scope: scope.Local, Project: conformanceProjectA})
			if err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			if _, found := conformanceFind(got, working); found {
				t.Errorf("%s answered a query that never mentioned Kind with a session record", read.name)
			}
			if read.exhaustive {
				if _, found := conformanceFind(got, fact); !found {
					t.Errorf("%s lost the fact while filtering: %+v", read.name, got)
				}
			}

			got, err = read.fn(Query{Scope: scope.Local, Project: conformanceProjectA, Kind: KindSession})
			if err != nil {
				t.Fatalf("%s for session records: %v", read.name, err)
			}
			if _, found := conformanceFind(got, fact); found {
				t.Errorf("%s for session records returned a fact; Kind is matched, not widened", read.name)
			}
			if read.exhaustive {
				rec, found := conformanceFind(got, working)
				if !found {
					t.Fatalf("%s cannot read back the session record it stored: %+v", read.name, got)
				}
				if rec.Kind != KindSession {
					t.Errorf("kind did not survive the round trip: got %q", rec.Kind)
				}
			}
		}
	})

	// A floor exists so the agent can search again more widely than the search
	// it was handed, and still not be told about everything the store holds.
	t.Run("MinScore drops what the backend scored below it and spares what it did not score", func(t *testing.T) {
		c := open()
		const (
			near = "conformance: the cache is invalidated on deploy"
			far  = "conformance: lunch is at one"
		)
		conformanceStore(ctx, t, c, Record{Content: near, Scope: scope.Local, Project: conformanceProjectA})
		conformanceStore(ctx, t, c, Record{Content: far, Scope: scope.Local, Project: conformanceProjectA})

		const floor = 0.5
		q := Query{Text: "conformance cache invalidated on deploy", Limit: 20, Scope: scope.Local, Project: conformanceProjectA}
		unfiltered, err := c.Search(ctx, q)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		q.MinScore = floor
		filtered, err := c.Search(ctx, q)
		if err != nil {
			t.Fatalf("search with a floor: %v", err)
		}

		for _, r := range filtered {
			if r.Score != 0 && r.Score < floor {
				t.Errorf("%q scored %v, below the floor of %v", r.Content, r.Score, floor)
			}
		}
		// A backend with no notion of similarity leaves Score at zero, and must
		// not read as having scored everything zero and answered with nothing.
		for _, r := range unfiltered {
			if r.Score != 0 {
				continue
			}
			if _, found := conformanceFind(filtered, r.Content); !found {
				t.Errorf("the floor dropped %q, which the backend never scored", r.Content)
			}
		}
	})

	// The running note of a session is one record rewritten every turn, so this
	// is the supersede that happens most often: same scope, same project, same
	// kind, new content.
	t.Run("a session record supersedes itself", func(t *testing.T) {
		c := open()
		const (
			before = "conformance session keywords: migration"
			after  = "conformance session keywords: migration, rollback"
		)
		rec := Record{Content: before, Kind: KindSession, Scope: scope.Local, Project: conformanceProjectA}
		oldID := conformanceStore(ctx, t, c, rec)
		if oldID == "" {
			t.Fatal("Store returned no id: the next turn has nothing to supersede")
		}
		rec.Content = after
		newID, err := c.Supersede(ctx, oldID, rec)
		if err != nil {
			t.Fatalf("supersede: %v", err)
		}
		if newID == "" {
			t.Fatal("Supersede returned no id: the turn after this one has nothing to supersede")
		}

		got := conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA, Kind: KindSession})
		if _, found := conformanceFind(got, before); found {
			t.Error("the superseded keywords are still listed; the note grows a second copy every turn")
		}
		for _, r := range got {
			if r.ID == oldID {
				t.Errorf("the superseded id %q is still listed", oldID)
			}
		}
		current, found := conformanceFind(got, after)
		if !found {
			t.Fatalf("the replacement is not listed where the original was: %+v", got)
		}
		if current.ID != newID {
			t.Errorf("the listed replacement is %q but Supersede reported %q; the next turn would supersede the wrong record", current.ID, newID)
		}
		if _, found := conformanceFind(conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA}), after); found {
			t.Error("the replacement is a fact as far as a default list is concerned")
		}
	})

	// Working notes are the one thing this system writes that is worth nothing
	// tomorrow, and there is one per session per project. Superseding replaces
	// rather than removes, so without deletion the population grows for the
	// life of the store and buries the knowledge it sits beside.
	t.Run("a forgotten record never comes back", func(t *testing.T) {
		c := open()
		const gone = "conformance: the staging cluster is rebuilt nightly"
		id := conformanceStore(ctx, t, c, Record{Content: gone, Scope: scope.Local, Project: conformanceProjectA})
		if id == "" {
			t.Fatal("Store returned no id: nothing can ever be forgotten")
		}
		if err := c.Forget(ctx, id, Query{Scope: scope.Local, Project: conformanceProjectA}); err != nil {
			t.Fatalf("forget: %v", err)
		}
		for _, read := range conformanceReads(ctx, c, "conformance staging cluster rebuilt") {
			got, err := read.fn(Query{Scope: scope.Local, Project: conformanceProjectA})
			if err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			if _, found := conformanceFind(got, gone); found {
				t.Errorf("%s still returns the forgotten record", read.name)
			}
		}
		// Asking twice is what a janitor does after a crash, and the second
		// answer must not read as a fault.
		if err := c.Forget(ctx, id, Query{Scope: scope.Local, Project: conformanceProjectA}); err != nil {
			t.Errorf("forgetting an id that is already gone must succeed, got %v", err)
		}
	})

	t.Run("Forget will not reach into another scope", func(t *testing.T) {
		c := open()
		const mine = "conformance: this project pins Go 1.26"
		id := conformanceStore(ctx, t, c, Record{Content: mine, Scope: scope.Local, Project: conformanceProjectA})
		if id == "" {
			t.Skip("connector returns no ids")
		}
		// Deletion is the one verb that cannot be undone, so naming the wrong
		// place must be a no-op rather than a loss.
		if err := c.Forget(ctx, id, Query{Scope: scope.Local, Project: conformanceProjectB}); err != nil {
			t.Fatalf("forget in the wrong scope should be a no-op, got %v", err)
		}
		got := conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA})
		if _, found := conformanceFind(got, mine); !found {
			t.Fatal("a record was deleted by a caller naming a different project")
		}
	})

	t.Run("Forget without an id is refused", func(t *testing.T) {
		if err := open().Forget(ctx, "", Query{Scope: scope.Local, Project: conformanceProjectA}); !errors.Is(err, ErrForgetUnidentified) {
			t.Fatalf("got %v, want ErrForgetUnidentified; an empty id is a caller that lost track of what it meant to delete", err)
		}
	})

	// The id of a working note lives in the memory of one process, and that
	// process is restarted routinely. The mark in the store is what lets the
	// session find its own note again instead of writing a second one.
	t.Run("the session mark survives the round trip", func(t *testing.T) {
		c := open()
		const (
			note  = "conformance session keywords: indexer, backfill"
			owner = "conformance-session-7"
		)
		conformanceStore(ctx, t, c, Record{
			Content: note, Kind: KindSession, Session: owner,
			Scope: scope.Local, Project: conformanceProjectA,
		})
		got := conformanceList(ctx, t, c, Query{Scope: scope.Local, Project: conformanceProjectA, Kind: KindSession})
		rec, found := conformanceFind(got, note)
		if !found {
			t.Fatalf("the note was not listed back: %+v", got)
		}
		if rec.Session != owner {
			t.Errorf("the session mark did not survive: got %q, want %q; a session that restarts cannot find its own note", rec.Session, owner)
		}
	})

	// A supersede can half-land: the backend commits the replacement and the
	// caller never learns the new id, so it keeps naming a record that is no
	// longer current. The write is still refused — a record no read can see
	// must not be rewritten from here — but the refusal has to say that nothing
	// was found rather than that it belongs to somebody else, because that is
	// the only signal by which the caller can recognise its own lost record and
	// write again.
	t.Run("a refused supersede does not invent a scope for a record it never saw", func(t *testing.T) {
		c := open()
		const (
			lost    = "conformance session keywords: indexer"
			wanted  = "conformance session keywords: indexer, backfill"
			project = conformanceProjectA
		)
		rec := Record{Content: lost, Kind: KindSession, Scope: scope.Local, Project: project}
		deadID := conformanceStore(ctx, t, c, rec)
		if deadID == "" {
			t.Fatal("Store returned no id")
		}
		if err := c.Forget(ctx, deadID, Query{Scope: scope.Local, Project: conformanceProjectA, Kind: KindSession}); err != nil {
			t.Fatalf("forget: %v", err)
		}

		rec.Content = wanted
		_, err := c.Supersede(ctx, deadID, rec)
		var cross *ErrCrossScopeSupersede
		if !errors.As(err, &cross) {
			t.Fatalf("superseding a record that is gone must be refused as such, got %v", err)
		}
		if cross.Elsewhere {
			t.Error("nothing was seen in another scope; a caller that cannot tell its own lost record from somebody else's never writes again")
		}
	})
}

// conformanceStore writes a record the suite depends on, and gives up on the
// whole subtest when there is no backend to write to: a connector that stores
// nothing cannot demonstrate anything about what it stored.
func conformanceStore(ctx context.Context, t *testing.T, c Connector, r Record) string {
	t.Helper()
	id, err := c.Store(ctx, r)
	if errors.Is(err, ErrNoBackend) {
		t.Skipf("%s has no backend to write to", c.Name())
	}
	if err != nil {
		t.Fatalf("store %q: %v", r.Content, err)
	}
	return id
}

func conformanceList(ctx context.Context, t *testing.T, c Connector, q Query) []Record {
	t.Helper()
	got, err := c.List(ctx, q)
	if err != nil {
		t.Fatalf("list %s/%s: %v", q.Scope, scope.Label(q.Project), err)
	}
	return got
}

func conformanceFind(recs []Record, content string) (Record, bool) {
	for _, r := range recs {
		if r.Content == content {
			return r, true
		}
	}
	return Record{}, false
}

// conformanceRead is one of the two read surfaces. Search is not exhaustive —
// it may rank a matching record out of the answer — so only List is held to
// returning the record it should have.
type conformanceRead struct {
	name       string
	exhaustive bool
	fn         func(Query) ([]Record, error)
}

// conformanceReads pairs the surfaces so every scope rule is asserted on both.
func conformanceReads(ctx context.Context, c Connector, text string) []conformanceRead {
	return []conformanceRead{
		{"search", false, func(q Query) ([]Record, error) {
			q.Text = text
			q.Limit = 20
			return c.Search(ctx, q)
		}},
		{"list", true, func(q Query) ([]Record, error) {
			return c.List(ctx, q)
		}},
	}
}
