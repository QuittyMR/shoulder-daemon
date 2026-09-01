package budget

import "testing"

func note(n int, turn uint64) Candidate {
	return Candidate{Kind: "note", Len: n, CreatedTurn: turn, TTLTurns: 2}
}

func TestGate(t *testing.T) {
	g := Default()

	t.Run("first note passes", func(t *testing.T) {
		if d := g.Allow(State{}, 1, note(100, 1)); !d.Allow {
			t.Fatalf("expected allow, got %v", d)
		}
	})

	t.Run("second note inside the gap is suppressed", func(t *testing.T) {
		st := State{LastInjectTurn: 5, CharsUsed: 100}
		if d := g.Allow(st, 6, note(100, 6)); d.Allow {
			t.Fatal("note only two turns later should be suppressed")
		}
		if d := g.Allow(st, 8, note(100, 8)); !d.Allow {
			t.Fatalf("note three turns later should pass, got %v", d)
		}
	})

	t.Run("warning bypasses the gap but not the session cap", func(t *testing.T) {
		st := State{LastInjectTurn: 5, CharsUsed: 100}
		w := Candidate{Kind: KindWarning, Len: 100, CreatedTurn: 6, TTLTurns: 2}
		if d := g.Allow(st, 6, w); !d.Allow {
			t.Fatalf("warning should bypass the turn gap, got %v", d)
		}
		full := State{CharsUsed: g.SessionMaxChars - 10}
		if d := g.Allow(full, 6, Candidate{Kind: KindWarning, Len: 100, CreatedTurn: 6}); d.Allow {
			t.Fatal("warning must not escape the session character cap")
		}
	})

	t.Run("expired advice is dropped", func(t *testing.T) {
		if d := g.Allow(State{}, 10, note(100, 5)); d.Allow || d.Reason != "expired" {
			t.Fatalf("expected expired, got %v", d)
		}
	})

	t.Run("dry run never injects", func(t *testing.T) {
		dg := Default()
		dg.DryRun = true
		if d := dg.Allow(State{}, 1, note(100, 1)); d.Allow || d.Reason != "dry_run" {
			t.Fatalf("expected dry_run suppression, got %v", d)
		}
	})

	t.Run("empty advice is dropped", func(t *testing.T) {
		if d := g.Allow(State{}, 1, note(0, 1)); d.Allow {
			t.Fatal("empty advice should never inject")
		}
	})
}

func TestRecordAccumulates(t *testing.T) {
	var st State
	st.Record(3, note(200, 3))
	st.Record(9, note(300, 9))
	if st.CharsUsed != 500 || st.LastInjectTurn != 9 {
		t.Fatalf("unexpected state %+v", st)
	}
}
