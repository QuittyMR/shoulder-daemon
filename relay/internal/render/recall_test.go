package render

import (
	"encoding/json"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

// A recall query is what was said, not what was touched: tool traffic is the
// bulk of a window and it pulls a semantic search towards file names.
func TestRecallQueryIsTheLastFewThingsSaidInOrder(t *testing.T) {
	events := []session.Event{
		{Kind: session.KindUserPrompt, Prompt: "one"},
		{Kind: session.KindTurnEnd, Assistant: "two"},
		{Kind: session.KindToolCall, ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"rm -rf build"}`)},
		{Kind: session.KindToolResult, ToolName: "Bash", ToolResult: "removed"},
		{Kind: session.KindUserPrompt, Prompt: "  three  "},
		{Kind: session.KindAssistantMessage, Assistant: ""},
		{Kind: session.KindTurnEnd, Assistant: "four"},
		{Kind: session.KindUserPrompt, Prompt: "five"},
	}
	got := RecallQuery(events)
	if got != "two\nthree\nfour\nfive" {
		t.Fatalf("RecallQuery = %q", got)
	}
	if strings.Contains(got, "rm -rf") || strings.Contains(got, "removed") {
		t.Fatal("tool traffic leaked into the recall query")
	}
}

func TestRecallQueryOfNothingSaidIsEmpty(t *testing.T) {
	events := []session.Event{
		{Kind: session.KindToolCall, ToolName: "Read"},
		{Kind: session.KindUserPrompt, Prompt: "   "},
	}
	if got := RecallQuery(events); got != "" {
		t.Fatalf("RecallQuery = %q, want empty", got)
	}
}

func TestRecallQueryClipsEachUtterance(t *testing.T) {
	long := strings.Repeat("w", recallClip*3)
	got := RecallQuery([]session.Event{{Kind: session.KindUserPrompt, Prompt: long}})
	if len(got) > recallClip+len("…") {
		t.Fatalf("one utterance rendered as %d bytes, bound is %d", len(got), recallClip)
	}
}

func TestEveryEventKindRendersAsItsOwnTag(t *testing.T) {
	cases := []struct {
		name string
		ev   session.Event
		want string
	}{
		{"prompt", session.Event{Kind: session.KindUserPrompt, Prompt: " hi "}, "<user>hi</user>"},
		{"silent assistant", session.Event{Kind: session.KindAssistantMessage, Assistant: "  "}, ""},
		{"thinking precedes the answer", session.Event{Kind: session.KindTurnEnd, Assistant: "done", Thinking: "hmm"}, "<thinking>hmm</thinking>\n<assistant>done</assistant>"},
		{"tool call", session.Event{Kind: session.KindToolCall, ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"/a"}`)}, `<tool name="Read">/a</tool>`},
		{"tool result", session.Event{Kind: session.KindToolResult, ToolName: "Bash", ToolResult: "ok"}, `<result name="Bash">ok</result>`},
		{"tool failure is marked", session.Event{Kind: session.KindToolFailure, ToolName: "Bash", ToolResult: "boom"}, `<result name="Bash" error="true">boom</result>`},
		{"compact", session.Event{Kind: session.KindCompact}, "<compact/>"},
		{"unknown kind is nothing", session.Event{Kind: session.KindSessionEnd}, ""},
	}
	for _, tc := range cases {
		if got := line(tc.ev); got != tc.want {
			t.Errorf("%s: line = %q, want %q", tc.name, got, tc.want)
		}
	}
}
