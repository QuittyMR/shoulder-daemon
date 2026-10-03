package budget

import "testing"

func note(n int, event uint64) Candidate {
	return Candidate{Kind: "note", Len: n, CreatedEvent: event, TTLEvents: 4}
}

func TestGate(t *testing.T) {
	g := Default()

	t.Run("first note passes", func(t *testing.T) {
		if d := g.Allow(State{}, 1, note(100, 1)); !d.Allow {
			t.Fatalf("expected allow, got %v", d)
		}
	})

	t.Run("second note inside the gap is suppressed", func(t *testing.T) {
		st := State{LastInjectEvent: 5, CharsUsed: 100}
		// Injected at a prompt: the count is odd there and moves by two from
		// one prompt to the next.
		for _, event := range []uint64{6, 7, 9, 10} {
			if d := g.Allow(st, event, note(100, event)); d.Allow || d.Reason != "event_gap:6" {
				t.Fatalf("note at %d, inside the two prompts after the injection, got %v", event, d)
			}
		}
		if d := g.Allow(st, 11, note(100, 11)); !d.Allow {
			t.Fatalf("note at the third prompt after the injection should pass, got %v", d)
		}
	})

	t.Run("warning bypasses the gap but not the session cap", func(t *testing.T) {
		st := State{LastInjectEvent: 5, CharsUsed: 100}
		w := Candidate{Kind: KindWarning, Len: 100, CreatedEvent: 6, TTLEvents: 4}
		if d := g.Allow(st, 6, w); !d.Allow {
			t.Fatalf("warning should bypass the event gap, got %v", d)
		}
		full := State{CharsUsed: g.SessionMaxChars - 10}
		if d := g.Allow(full, 6, Candidate{Kind: KindWarning, Len: 100, CreatedEvent: 6}); d.Allow {
			t.Fatal("warning must not escape the session character cap")
		}
	})

	t.Run("expired advice is dropped", func(t *testing.T) {
		if d := g.Allow(State{}, 9, note(100, 5)); !d.Allow {
			t.Fatalf("a note is current through the second prompt after the one it was written at, got %v", d)
		}
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
	if st.CharsUsed != 500 || st.LastInjectEvent != 9 {
		t.Fatalf("unexpected state %+v", st)
	}
}
