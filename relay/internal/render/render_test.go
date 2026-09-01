package render

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

func TestToolSummary(t *testing.T) {
	cases := []struct{ tool, input, want string }{
		{"Bash", `{"command":"go test ./...","description":"run tests"}`, "go test ./..."},
		{"Read", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"Edit", `{"file_path":"/a/b.go","old_string":"x","new_string":"y"}`, "/a/b.go"},
		{"Grep", `{"pattern":"func main","path":"/src"}`, "func main in /src"},
		{"WebFetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"Unknown", `{"a":1}`, `{"a":1}`},
	}
	for _, tc := range cases {
		if got := ToolSummary(json.RawMessage(tc.input)); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.tool, got, tc.want)
		}
	}
}

func TestToolSummaryClipsLongBashCommands(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := ToolSummary(json.RawMessage(`{"command":"` + long + `"}`))
	if len(got) > commandClip+len("…") {
		t.Fatalf("expected a clipped command, got %d chars", len(got))
	}
}

func TestToolSummaryHandlesMalformedInput(t *testing.T) {
	if got := ToolSummary(json.RawMessage(`{not json`)); got == "" {
		t.Fatal("malformed input should degrade to raw text, not vanish")
	}
}

func TestWindowKeepsMostRecentAndRespectsCharBudget(t *testing.T) {
	var events []session.Event
	for i := 0; i < 100; i++ {
		events = append(events, session.Event{
			Kind: session.KindUserPrompt, TS: time.Now(),
			Prompt: strings.Repeat("q", 50) + string(rune('A'+i%26)),
		})
	}
	events = append(events, session.Event{Kind: session.KindUserPrompt, Prompt: "THE-LAST-ONE"})

	out := Window(events, 40, 1200)
	if len(out) > 1200 {
		t.Fatalf("window exceeded char budget: %d", len(out))
	}
	if !strings.Contains(out, "THE-LAST-ONE") {
		t.Fatal("the most recent event must survive truncation")
	}
}

func TestWindowSkipsEmptyAssistantTurns(t *testing.T) {
	out := Window([]session.Event{
		{Kind: session.KindTurnEnd, Assistant: "   "},
		{Kind: session.KindUserPrompt, Prompt: "hello"},
	}, 40, 4000)
	if strings.Contains(out, "<assistant>") {
		t.Fatalf("an empty assistant turn should not be rendered: %q", out)
	}
}

func TestWindowRendersThinkingWhenPresent(t *testing.T) {
	out := Window([]session.Event{
		{Kind: session.KindTurnEnd, Assistant: "done", Thinking: "considered X then Y"},
	}, 40, 4000)
	if !strings.Contains(out, "considered X then Y") {
		t.Fatalf("thinking should be rendered when an adapter supplies it: %q", out)
	}
}
