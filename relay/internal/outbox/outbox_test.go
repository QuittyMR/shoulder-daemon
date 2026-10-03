package outbox

import (
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

func note(id string, level session.AdviceLevel, turn uint64) session.Advice {
	return session.Advice{
		ID: id, SessionID: "s1", Kind: session.AdviceNote, Level: level,
		Text: id, CreatedTurn: turn, TTLTurns: 2, CreatedAt: time.Now().UTC(),
	}
}

// A hook that cannot carry a note must pass it over, not consume it: context
// waiting for the next prompt has to survive every tool call in between.
func TestAHookThatCannotCarryANoteLeavesIt(t *testing.T) {
	b := New()
	b.Push(note("plan", session.LevelPlan, 0))

	if _, ok := b.Take("s1", 0, session.KindToolCall, "", ""); ok {
		t.Fatal("a tool call collected context meant for a prompt")
	}
	if b.Depth() != 1 {
		t.Fatalf("the note was consumed by a hook that could not carry it; depth %d", b.Depth())
	}
	got, ok := b.Take("s1", 0, session.KindUserPrompt, "", "")
	if !ok || got.ID != "plan" {
		t.Fatalf("the prompt did not collect it: %+v %v", got, ok)
	}
	if b.Depth() != 0 {
		t.Fatalf("depth %d after collection", b.Depth())
	}
}

// Order is preserved across a skip: the first note a kind can carry wins, and
// the ones behind it stay behind it.
func TestTakeReturnsTheFirstNoteTheKindCanCarry(t *testing.T) {
	b := New()
	b.Push(note("action-1", session.LevelAction, 0))
	b.Push(note("plan-1", session.LevelPlan, 0))
	b.Push(note("action-2", session.LevelAction, 0))

	got, ok := b.Take("s1", 0, session.KindToolCall, "", "")
	if !ok || got.ID != "action-1" {
		t.Fatalf("got %+v, want action-1", got)
	}
	got, ok = b.Take("s1", 0, session.KindToolCall, "", "")
	if !ok || got.ID != "action-2" {
		t.Fatalf("got %+v, want action-2", got)
	}
	got, ok = b.Take("s1", 0, session.KindUserPrompt, "", "")
	if !ok || got.ID != "plan-1" {
		t.Fatalf("the plan note did not survive two tool calls: %+v", got)
	}
}

// Stale advice is worse than none: it describes a turn that has moved on.
func TestExpiredAdviceIsDiscardedOnTheWayPast(t *testing.T) {
	b := New()
	b.Push(note("old", session.LevelPlan, 0))
	b.Push(note("fresh", session.LevelPlan, 9))

	got, ok := b.Take("s1", 9, session.KindUserPrompt, "", "")
	if !ok || got.ID != "fresh" {
		t.Fatalf("got %+v, want the note that is still current", got)
	}
	if b.Depth() != 0 {
		t.Fatalf("the expired note was kept; depth %d", b.Depth())
	}
}

// The queue is bounded because a session nobody collects from would otherwise
// grow one note per turn for as long as it lives.
func TestTheQueueIsBoundedAndDropsTheOldest(t *testing.T) {
	b := New()
	for i := 0; i < maxPerSession+3; i++ {
		b.Push(note(string(rune('a'+i)), session.LevelPlan, 0))
	}
	if b.Depth() != maxPerSession {
		t.Fatalf("depth %d, want %d", b.Depth(), maxPerSession)
	}
	got, _ := b.Take("s1", 0, session.KindUserPrompt, "", "")
	if got.ID == "a" {
		t.Fatal("the newest were dropped instead of the oldest")
	}
}

func TestForgetEmptiesOneSession(t *testing.T) {
	b := New()
	b.Push(note("x", session.LevelPlan, 0))
	b.Push(session.Advice{ID: "y", SessionID: "s2", Level: session.LevelPlan, TTLTurns: 2})

	b.Forget("s1")
	if _, ok := b.Take("s1", 0, session.KindUserPrompt, "", ""); ok {
		t.Fatal("a forgotten session still had advice")
	}
	if b.Depth() != 1 {
		t.Fatalf("another session's advice was dropped; depth %d", b.Depth())
	}
}

func TestTakeOnAnUnknownSessionIsQuiet(t *testing.T) {
	b := New()
	if _, ok := b.Take("nobody", 0, session.KindUserPrompt, "", ""); ok {
		t.Fatal("advice appeared for a session that never had any")
	}
}

// A note addressed to one subagent must wait for that subagent: the main
// thread's next tool call and every other agent's must leave it where it is,
// and a note with no address goes to whoever asks first.
func TestAdviceAddressedToAnAgentWaitsForThatAgent(t *testing.T) {
	b := New()
	for _, a := range []session.Advice{
		{ID: "for-agent-a", SessionID: "s1", Level: session.LevelAction, TTLTurns: 2, AgentID: "agent-a"},
		{ID: "for-anyone", SessionID: "s1", Level: session.LevelAction, TTLTurns: 2},
	} {
		b.Push(a)
	}

	got, ok := b.Take("s1", 0, session.KindToolCall, "", "")
	if !ok || got.ID != "for-anyone" {
		t.Fatalf("the main thread got %+v %v, want the unaddressed note", got, ok)
	}
	if _, taken := b.Take("s1", 0, session.KindToolCall, "agent-b", "explore"); taken {
		t.Fatal("another agent collected a note addressed to agent-a")
	}
	if b.Depth() != 1 {
		t.Fatalf("the addressed note was dropped; depth %d", b.Depth())
	}
	got, ok = b.Take("s1", 0, session.KindToolCall, "agent-a", "explore")
	if !ok || got.ID != "for-agent-a" {
		t.Fatalf("agent-a got %+v %v, want its own note", got, ok)
	}
}

// An unaddressed note is for whoever is working: a subagent's tool call may
// carry it as well as the main thread's.
func TestAnUnaddressedNoteGoesToASubagentToo(t *testing.T) {
	b := New()
	b.Push(note("open", session.LevelAction, 0))
	got, ok := b.Take("s1", 0, session.KindToolCall, "agent-z", "explore")
	if !ok || got.ID != "open" {
		t.Fatalf("got %+v %v", got, ok)
	}
}

// A note written for a subagent that has not been named yet is for a
// subagent of that type and never for the main thread; once the spawn is
// bound to an id, only that agent may take it.
func TestANoteForAnUnnamedSubagentWaitsForOneOfItsType(t *testing.T) {
	b := New()
	b.Push(session.Advice{ID: "spawned", SessionID: "s1", Level: session.LevelAction, TTLTurns: 2, SpawnID: "toolu_1", AgentType: "explore"})

	if _, ok := b.Take("s1", 0, session.KindToolCall, "", ""); ok {
		t.Fatal("the main thread took a note written for a subagent")
	}
	if _, ok := b.Take("s1", 0, session.KindToolCall, "agent-r", "reviewer"); ok {
		t.Fatal("a subagent of another type took the note")
	}
	b.Address("s1", "toolu_1", "agent-a")
	if _, ok := b.Take("s1", 0, session.KindToolCall, "agent-b", "explore"); ok {
		t.Fatal("a sibling of the same type took a note bound to another agent")
	}
	got, ok := b.Take("s1", 0, session.KindAgentStart, "agent-a", "Explore")
	if !ok || got.ID != "spawned" || got.AgentID != "agent-a" {
		t.Fatalf("the agent the note was bound to got %+v %v", got, ok)
	}
}

// A note typed only by agent type goes to the first agent of that type that
// asks, and the main thread never sees it.
func TestANoteTypedByAgentGoesToTheFirstAgentOfThatType(t *testing.T) {
	b := New()
	b.Push(session.Advice{ID: "typed", SessionID: "s1", Level: session.LevelAction, TTLTurns: 2, AgentType: "explore"})
	if _, ok := b.Take("s1", 0, session.KindToolCall, "", ""); ok {
		t.Fatal("the main thread took a note written for subagents")
	}
	got, ok := b.Take("s1", 0, session.KindToolCall, "agent-b", "explore")
	if !ok || got.ID != "typed" {
		t.Fatalf("got %+v %v", got, ok)
	}
}
