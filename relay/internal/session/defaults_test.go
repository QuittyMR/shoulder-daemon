package session

import "testing"

// A registry with no cap would keep every event of a day-long session in
// memory; a nonsense cap must not be honoured as "unbounded".
func TestARegistryWithNoCapGetsTheDefaultOne(t *testing.T) {
	for _, n := range []int{0, -5} {
		if got := NewRegistry(n).maxEvents; got != 200 {
			t.Errorf("NewRegistry(%d) keeps %d events, want the default 200", n, got)
		}
	}
	if got := NewRegistry(7).maxEvents; got != 7 {
		t.Errorf("NewRegistry(7) keeps %d events", got)
	}
}

// The outbox and the budget gate must agree about what has gone stale, so
// Advice defers to the gate's rule rather than carrying its own.
func TestAdviceExpiresByTheGatesRule(t *testing.T) {
	a := Advice{Text: "x", CreatedTurn: 3, TTLTurns: 2}
	if a.Expired(3) {
		t.Fatal("advice created this turn is not stale")
	}
	if !a.Expired(100) {
		t.Fatal("advice from ninety-seven turns ago is stale")
	}
	if a.Expired(3) != a.Candidate().Expired(3) || a.Expired(100) != a.Candidate().Expired(100) {
		t.Fatal("Advice.Expired and the gate's Candidate.Expired disagree")
	}
}

func TestAdviceLevelsAreDeliveredOnTheEventThatCanStillUseThem(t *testing.T) {
	if !KindUserPrompt.Delivers(LevelPlan) || KindToolCall.Delivers(LevelPlan) {
		t.Fatal("plan-level advice belongs on the prompt, before the assistant chooses")
	}
	if !KindToolCall.Delivers(LevelAction) || KindUserPrompt.Delivers(LevelAction) {
		t.Fatal("action-level advice belongs on the tool call it is about")
	}
	if !KindUserPrompt.Delivers("") {
		t.Fatal("advice with no level is plan-level")
	}
}
