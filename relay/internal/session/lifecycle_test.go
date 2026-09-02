package session

import (
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
)

func factFor(i int) facts.Fact {
	return facts.Fact{Content: string(rune('a'+i%26)) + " fact", Category: "structure", Scope: "global"}
}

func seen(r *Registry, id string, kind Kind, at time.Time) {
	r.Observe(Event{SessionID: id, Kind: kind, TS: at, Harness: "test", CWD: "/w"})
}

// Where a note lands follows what it is for: context before anything has been
// chosen, a warning at the operation it warns about. Everything else is after
// the fact.
func TestDeliversMatchesLevelToKind(t *testing.T) {
	for _, tc := range []struct {
		kind  Kind
		level AdviceLevel
		want  bool
	}{
		{KindUserPrompt, LevelPlan, true},
		{KindUserPrompt, LevelAction, false},
		{KindToolCall, LevelAction, true},
		{KindToolCall, LevelPlan, false},
		{KindToolResult, LevelPlan, false},
		{KindToolFailure, LevelAction, false},
		{KindAssistantMessage, LevelPlan, false},
		{KindTurnEnd, LevelPlan, false},
		{KindCompact, LevelAction, false},
		{KindSessionEnd, LevelPlan, false},
		// An unset level is context, which is both the common case and the
		// safe one.
		{KindUserPrompt, "", true},
		{KindToolCall, "", false},
	} {
		if got := tc.kind.Delivers(tc.level); got != tc.want {
			t.Errorf("%s.Delivers(%q) = %v, want %v", tc.kind, tc.level, got, tc.want)
		}
	}
}

func TestTurnCountsOnlyCompletedTurns(t *testing.T) {
	r := NewRegistry(10)
	now := time.Now()
	if got := r.Turn("nobody"); got != 0 {
		t.Fatalf("an unknown session is at turn %d", got)
	}
	seen(r, "s1", KindUserPrompt, now)
	seen(r, "s1", KindToolCall, now)
	if got := r.Turn("s1"); got != 0 {
		t.Fatalf("turn %d before the turn ended", got)
	}
	seen(r, "s1", KindTurnEnd, now)
	if got := r.Turn("s1"); got != 1 {
		t.Fatalf("turn %d after one turn", got)
	}
}

// The advisor claim is what stops a slow pass being asked the same question
// several times over while it is still thinking.
func TestOnlyOneAdvisorCallIsClaimedAtATime(t *testing.T) {
	r := NewRegistry(10)
	seen(r, "s1", KindUserPrompt, time.Now())

	if !r.ClaimAdvisor("s1") {
		t.Fatal("the first claim was refused")
	}
	if r.ClaimAdvisor("s1") {
		t.Fatal("a second claim was granted while the first was in flight")
	}
	r.ReleaseAdvisor("s1")
	if !r.ClaimAdvisor("s1") {
		t.Fatal("the claim was not released")
	}
	if r.ClaimAdvisor("nobody") {
		t.Fatal("an unknown session was granted a claim")
	}
}

func TestSnapshotCopiesTheWindow(t *testing.T) {
	r := NewRegistry(10)
	now := time.Now()
	seen(r, "s1", KindUserPrompt, now)
	seen(r, "s1", KindTurnEnd, now)

	events, turn, ok := r.Snapshot("s1")
	if !ok || len(events) != 2 || turn != 1 {
		t.Fatalf("snapshot: %d events, turn %d, ok %v", len(events), turn, ok)
	}
	// Mutating the copy must not reach the registry, which the advisor reads
	// off the hook path while new events keep arriving.
	events[0].SessionID = "tampered"
	again, _, _ := r.Snapshot("s1")
	if again[0].SessionID != "s1" {
		t.Fatal("the snapshot shares its backing array with the registry")
	}
	if _, _, ok := r.Snapshot("nobody"); ok {
		t.Fatal("an unknown session produced a snapshot")
	}
}

func TestCloseSessionReportsWhatIsLeft(t *testing.T) {
	r := NewRegistry(10)
	now := time.Now()
	seen(r, "s1", KindUserPrompt, now)
	seen(r, "s2", KindUserPrompt, now)
	r.SetKeywordRecord("s1", "/w", "mem_1", "a, b")

	gone, left := r.CloseSession("s1")
	if left != 1 {
		t.Fatalf("left %d with another session open", left)
	}
	if gone.ID != "s1" || gone.KeywordRecord != "mem_1" || gone.Project != "/w" {
		t.Fatalf("the note handle did not come out with the session: %+v", gone)
	}
	if _, left = r.CloseSession("s1"); left != 1 {
		t.Fatalf("closing a closed session changed the count to %d", left)
	}
	if _, left = r.CloseSession("s2"); left != 0 {
		t.Fatalf("left %d after the last session", left)
	}
}

// A note's id lives in memory and dies with the process, so a daemon on its way
// out has to hand back every one it holds or the records are orphaned.
func TestDrainHandsBackEverySessionsNote(t *testing.T) {
	r := NewRegistry(10)
	now := time.Now()
	for _, id := range []string{"s1", "s2"} {
		seen(r, id, KindUserPrompt, now)
		r.SetKeywordRecord(id, "/w", "mem_"+id, "kw")
	}

	gone := r.Drain()
	if len(gone) != 2 {
		t.Fatalf("drained %d of 2", len(gone))
	}
	for _, g := range gone {
		if g.KeywordRecord == "" {
			t.Fatalf("a note was left behind: %+v", g)
		}
	}
	if r.Len() != 0 {
		t.Fatalf("%d sessions survived the drain", r.Len())
	}
}

// An editor that is killed, crashes, or loses the machine under it never says
// goodbye, so time is what makes those sessions dead.
func TestEvictDropsOnlyTheIdleOnes(t *testing.T) {
	r := NewRegistry(10)
	now := time.Now()
	seen(r, "old", KindUserPrompt, now.Add(-2*time.Hour))
	seen(r, "new", KindUserPrompt, now)

	gone := r.Evict(time.Hour, now)
	if len(gone) != 1 || gone[0].ID != "old" {
		t.Fatalf("evicted %+v", gone)
	}
	if r.Len() != 1 {
		t.Fatalf("%d sessions left, want the live one", r.Len())
	}
}

func TestIdleReportsWhetherAnythingIsOpen(t *testing.T) {
	r := NewRegistry(10)
	now := time.Now()
	if _, empty := r.Idle(now); !empty {
		t.Fatal("a fresh registry is not empty")
	}
	seen(r, "s1", KindUserPrompt, now)
	idle, empty := r.Idle(now.Add(time.Minute))
	if empty {
		t.Fatal("a registry with a live session reported empty")
	}
	if idle < time.Minute {
		t.Fatalf("idle %v, want at least a minute", idle)
	}
}

func TestBudgetStateFollowsInjections(t *testing.T) {
	r := NewRegistry(10)
	seen(r, "s1", KindUserPrompt, time.Now())
	if got := r.BudgetState("nobody"); got != (budget.State{}) {
		t.Fatalf("an unknown session has budget state %+v", got)
	}

	r.RecordInjection("s1", 3, Advice{ID: "a", Text: "twelve chars", TTLTurns: 2})
	got := r.BudgetState("s1")
	if got.LastInjectTurn != 3 || got.CharsUsed == 0 {
		t.Fatalf("the injection was not recorded: %+v", got)
	}
}

// Facts named explicitly during a turn are held until the turn is reconciled,
// and the queue is bounded so one runaway turn cannot grow it without limit.
func TestPendingFactsAreHeldPerTurnAndBounded(t *testing.T) {
	r := NewRegistry(10)
	seen(r, "s1", KindUserPrompt, time.Now())

	for i := 0; i < maxPendingFacts+5; i++ {
		r.AddFact("s1", factFor(i))
	}
	got := r.TakeFacts("s1")
	if len(got) != maxPendingFacts {
		t.Fatalf("held %d facts, want the cap of %d", len(got), maxPendingFacts)
	}
	if again := r.TakeFacts("s1"); len(again) != 0 {
		t.Fatalf("taking twice returned %d facts", len(again))
	}
	if len(r.TakeFacts("nobody")) != 0 {
		t.Fatal("an unknown session had pending facts")
	}
}
