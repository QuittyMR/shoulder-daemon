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

	if _, ok := b.Take("s1", 0, session.KindToolCall); ok {
		t.Fatal("a tool call collected context meant for a prompt")
	}
	if b.Depth() != 1 {
		t.Fatalf("the note was consumed by a hook that could not carry it; depth %d", b.Depth())
	}
	got, ok := b.Take("s1", 0, session.KindUserPrompt)
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

	got, ok := b.Take("s1", 0, session.KindToolCall)
	if !ok || got.ID != "action-1" {
		t.Fatalf("got %+v, want action-1", got)
	}
	got, ok = b.Take("s1", 0, session.KindToolCall)
	if !ok || got.ID != "action-2" {
		t.Fatalf("got %+v, want action-2", got)
	}
	got, ok = b.Take("s1", 0, session.KindUserPrompt)
	if !ok || got.ID != "plan-1" {
		t.Fatalf("the plan note did not survive two tool calls: %+v", got)
	}
}

// Stale advice is worse than none: it describes a turn that has moved on.
func TestExpiredAdviceIsDiscardedOnTheWayPast(t *testing.T) {
	b := New()
	b.Push(note("old", session.LevelPlan, 0))
	b.Push(note("fresh", session.LevelPlan, 9))

	got, ok := b.Take("s1", 9, session.KindUserPrompt)
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
	got, _ := b.Take("s1", 0, session.KindUserPrompt)
	if got.ID == "a" {
		t.Fatal("the newest were dropped instead of the oldest")
	}
}

func TestForgetEmptiesOneSession(t *testing.T) {
	b := New()
	b.Push(note("x", session.LevelPlan, 0))
	b.Push(session.Advice{ID: "y", SessionID: "s2", Level: session.LevelPlan, TTLTurns: 2})

	b.Forget("s1")
	if _, ok := b.Take("s1", 0, session.KindUserPrompt); ok {
		t.Fatal("a forgotten session still had advice")
	}
	if b.Depth() != 1 {
		t.Fatalf("another session's advice was dropped; depth %d", b.Depth())
	}
}

func TestTakeOnAnUnknownSessionIsQuiet(t *testing.T) {
	b := New()
	if _, ok := b.Take("nobody", 0, session.KindUserPrompt); ok {
		t.Fatal("advice appeared for a session that never had any")
	}
}
