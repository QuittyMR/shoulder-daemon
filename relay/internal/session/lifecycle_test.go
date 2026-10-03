package session

import (
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
)

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
		{KindAgentStart, LevelPlan, true},
		{KindAgentStart, LevelAction, true},
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

// The keyword lock is held by one writer at a time, and belongs to the state:
// a session that is gone has none, and one reopened under the same id has its
// own.
func TestTheKeywordLockIsHeldByOneWriterAndGoesWithTheSession(t *testing.T) {
	r := NewRegistry(10)
	seen(r, "s1", KindUserPrompt, time.Now())

	if _, ok := r.LockKeywords("nobody"); ok {
		t.Fatal("an unknown session was given a lock")
	}
	unlock, ok := r.LockKeywords("s1")
	if !ok {
		t.Fatal("a live session was refused its lock")
	}
	second := make(chan bool, 1)
	go func() {
		release, ok := r.LockKeywords("s1")
		if ok {
			release()
		}
		second <- ok
	}()
	select {
	case <-second:
		t.Fatal("a second writer was let in while the first held the lock")
	default:
	}
	r.CloseSession("s1")
	seen(r, "s1", KindUserPrompt, time.Now())
	unlock()
	if !<-second {
		t.Fatal("the writer that waited was refused the reopened session's lock")
	}

	r.CloseSession("s1")
	if _, ok := r.LockKeywords("s1"); ok {
		t.Fatal("a closed session still has a lock")
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
	r.SetKeywordRecord("s1", "/w", "/w", "mem_1", "a, b")

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
		r.SetKeywordRecord(id, "/w", "/w", "mem_"+id, "kw")
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
	if got := r.BudgetState("nobody", ""); got != (budget.State{}) {
		t.Fatalf("an unknown session has budget state %+v", got)
	}

	r.RecordInjection("s1", 3, "", Advice{ID: "a", Text: "twelve chars", TTLTurns: 2})
	got := r.BudgetState("s1", "")
	if got.LastInjectTurn != 3 || got.CharsUsed == 0 {
		t.Fatalf("the injection was not recorded: %+v", got)
	}
}

// A subagent shares the session's characters and nothing else: what it is
// handed does not open the main thread's turn gap, and the main thread's gap
// does not close the agent's.
func TestASubagentsInjectionIsChargedToTheSessionNotToTheTurnGap(t *testing.T) {
	r := NewRegistry(10)
	seen(r, "s1", KindUserPrompt, time.Now())

	r.RecordInjection("s1", 3, "agent-1", Advice{ID: "a", Text: "twelve chars", TTLTurns: 2})
	if got := r.BudgetState("s1", ""); got.LastInjectTurn != 0 || got.CharsUsed != len("twelve chars") {
		t.Fatalf("the main thread's state after an agent's injection: %+v", got)
	}
	r.RecordInjection("s1", 3, "", Advice{ID: "b", Text: "twelve chars", TTLTurns: 2})
	if got := r.BudgetState("s1", "agent-1"); got.LastInjectTurn != 0 || got.CharsUsed != 2*len("twelve chars") {
		t.Fatalf("the agent's state after the main thread's injection: %+v", got)
	}
}

// A subagent's prompt is seen under the tool call that spawned it, before the
// harness has named the agent; the start binds the oldest unbound spawn of
// the type to the id, and the stop forgets it.
func TestSpawnsAreBoundToAgentIdsInOrderOfType(t *testing.T) {
	r := NewRegistry(10)
	spawn := func(toolUse, agentType string) {
		r.Observe(Event{SessionID: "s1", Kind: KindUserPrompt, Origin: OriginAgent, ToolUseID: toolUse, AgentType: agentType, Prompt: "p"})
	}
	start := func(agentID, agentType string) {
		r.Observe(Event{SessionID: "s1", Kind: KindAgentStart, Origin: OriginAgent, AgentID: agentID, AgentType: agentType})
	}
	spawn("toolu_1", "explore")
	spawn("toolu_2", "reviewer")
	spawn("toolu_3", "explore")
	if got := r.AgentOf("s1", "toolu_1"); got != "" {
		t.Fatalf("a spawn had an id before its agent started: %q", got)
	}

	start("agent-r", "Reviewer")
	start("agent-a", "explore")
	start("agent-b", "explore")
	for spawnID, want := range map[string]string{"toolu_1": "agent-a", "toolu_2": "agent-r", "toolu_3": "agent-b"} {
		if got := r.AgentOf("s1", spawnID); got != want {
			t.Errorf("AgentOf(%s) = %q, want %q", spawnID, got, want)
		}
	}
	if got := r.SpawnOf("s1", "agent-b"); got != "toolu_3" {
		t.Fatalf("SpawnOf(agent-b) = %q", got)
	}

	r.Observe(Event{SessionID: "s1", Kind: KindTurnEnd, Origin: OriginAgent, AgentID: "agent-a", AgentType: "explore", Assistant: "done"})
	if got := r.SpawnOf("s1", "agent-a"); got != "" {
		t.Fatalf("a stopped agent is still bound: %q", got)
	}
	if got := r.AgentOf("s1", "toolu_3"); got != "agent-b" {
		t.Fatalf("another agent's binding was lost: %q", got)
	}
}

// A harness that names a type one way in the spawn and another on the start
// still gets its id bound, to the oldest spawn still waiting for one.
func TestAStartOfAnUnknownTypeBindsTheOldestUnboundSpawn(t *testing.T) {
	r := NewRegistry(10)
	r.Observe(Event{SessionID: "s1", Kind: KindUserPrompt, Origin: OriginAgent, ToolUseID: "toolu_1", AgentType: "reviewer", Prompt: "p"})
	r.Observe(Event{SessionID: "s1", Kind: KindAgentStart, Origin: OriginAgent, AgentID: "agent-x", AgentType: "my-plugin:reviewer"})
	if got := r.AgentOf("s1", "toolu_1"); got != "agent-x" {
		t.Fatalf("AgentOf = %q, want agent-x", got)
	}
	r.Observe(Event{SessionID: "s1", Kind: KindAgentStart, Origin: OriginAgent, AgentID: "agent-internal", AgentType: ""})
	if got := r.SpawnOf("s1", "agent-internal"); got != "" {
		t.Fatalf("an agent nobody spawned was bound to %q", got)
	}
}
