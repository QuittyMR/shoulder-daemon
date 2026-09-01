package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scripted replays a fixed sequence of assistant messages and records what it
// was asked, which is the only way to see the tool results the loop appends.
type scripted struct {
	replies []Message
	repeat  bool // keep serving the last reply instead of running out
	calls   int
	seen    [][]Message
	tools   [][]Tool
	err     error
	errAt   int // 1-based call to fail on; zero fails every call
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Complete(context.Context, string, string) (string, error) {
	return "", errors.New("scripted has no Complete")
}

func (s *scripted) Chat(_ context.Context, msgs []Message, tools []Tool) (Message, error) {
	s.calls++
	s.seen = append(s.seen, append([]Message(nil), msgs...))
	s.tools = append(s.tools, tools)
	if s.err != nil && (s.errAt == 0 || s.calls == s.errAt) {
		return Message{}, s.err
	}
	i := s.calls - 1
	if i >= len(s.replies) {
		if !s.repeat || len(s.replies) == 0 {
			return Message{Role: "assistant", Content: "out of script"}, nil
		}
		i = len(s.replies) - 1
	}
	return s.replies[i], nil
}

func echoBinding(name string, out string, err error, hits *int) Binding {
	return Binding{
		Tool: Tool{Name: name, Description: "d", Schema: map[string]any{"type": "object"}},
		Handler: func(_ context.Context, args json.RawMessage) (string, error) {
			if hits != nil {
				*hits++
			}
			if err != nil {
				return "", err
			}
			return out + string(args), nil
		},
	}
}

func TestRunCallsAToolThenAnswers(t *testing.T) {
	p := &scripted{replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "search_memory", Args: json.RawMessage(`{"query":"x"}`)}}},
		{Role: "assistant", Content: "final answer"},
	}}
	var hits int
	got, err := Run(context.Background(), p, "sys", "usr", []Binding{echoBinding("search_memory", "hit:", nil, &hits)}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != "final answer" {
		t.Fatalf("got %q", got)
	}
	if hits != 1 {
		t.Fatalf("handler ran %d times", hits)
	}

	second := p.seen[1]
	if len(second) != 4 {
		t.Fatalf("second call saw %d messages: %+v", len(second), second)
	}
	if second[2].Role != "assistant" || len(second[2].ToolCalls) != 1 {
		t.Errorf("the assistant message with the tool call was not replayed: %+v", second[2])
	}
	if second[3].Role != "tool" || second[3].ToolCallID != "c1" || second[3].Content != `hit:{"query":"x"}` {
		t.Errorf("tool result wrong: %+v", second[3])
	}
}

func TestRunAnswersEveryCallInOneStep(t *testing.T) {
	p := &scripted{replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "a", Name: "one", Args: json.RawMessage(`{}`)},
			{ID: "b", Name: "two", Args: json.RawMessage(`{}`)},
		}},
		{Role: "assistant", Content: "done"},
	}}
	var one, two int
	got, err := Run(context.Background(), p, "sys", "usr", []Binding{
		echoBinding("one", "1:", nil, &one),
		echoBinding("two", "2:", nil, &two),
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != "done" {
		t.Fatalf("got %q", got)
	}
	if one != 1 || two != 1 {
		t.Fatalf("handlers ran %d and %d times", one, two)
	}
	second := p.seen[1]
	if len(second) != 5 {
		t.Fatalf("expected one tool message per call, got %d messages", len(second))
	}
	if second[3].ToolCallID != "a" || second[4].ToolCallID != "b" {
		t.Errorf("tool results not tied to their calls: %+v %+v", second[3], second[4])
	}
}

// A handler that fails tells the model something it can act on; it must not
// end the turn.
func TestRunGivesHandlerErrorsBackToTheModel(t *testing.T) {
	p := &scripted{replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "search_memory", Args: json.RawMessage(`{}`)}}},
		{Role: "assistant", Content: "recovered"},
	}}
	got, err := Run(context.Background(), p, "sys", "usr",
		[]Binding{echoBinding("search_memory", "", errors.New("backend unreachable"), nil)}, 4)
	if err != nil {
		t.Fatalf("a failing tool must not fail the run: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("got %q", got)
	}
	result := p.seen[1][3]
	if !strings.Contains(result.Content, "backend unreachable") {
		t.Errorf("the model was not told what went wrong: %q", result.Content)
	}
}

func TestRunReportsAnUnknownTool(t *testing.T) {
	p := &scripted{replies: []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "nope", Args: json.RawMessage(`{}`)}}},
		{Role: "assistant", Content: "fine"},
	}}
	if _, err := Run(context.Background(), p, "sys", "usr", []Binding{echoBinding("one", "1:", nil, nil)}, 4); err != nil {
		t.Fatal(err)
	}
	if result := p.seen[1][3]; !strings.Contains(result.Content, "nope") {
		t.Errorf("the model was not told the tool does not exist: %q", result.Content)
	}
}

func TestRunStopsAtTheStepCap(t *testing.T) {
	p := &scripted{
		repeat: true,
		replies: []Message{{
			Role:      "assistant",
			Content:   "still looking",
			ToolCalls: []ToolCall{{ID: "c", Name: "one", Args: json.RawMessage(`{}`)}},
		}},
	}
	got, err := Run(context.Background(), p, "sys", "usr", []Binding{echoBinding("one", "1:", nil, nil)}, 4)
	if err != nil {
		t.Fatalf("the step cap must not be an error: %v", err)
	}
	if p.calls != 4 {
		t.Fatalf("model was called %d times, want 4", p.calls)
	}
	if got != "still looking" {
		t.Fatalf("the text the model had produced was lost: %q", got)
	}
}

func TestRunWithoutToolsIsOneRoundTrip(t *testing.T) {
	p := &scripted{replies: []Message{{Role: "assistant", Content: "plain answer"}}}
	got, err := Run(context.Background(), p, "sys", "usr", nil, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain answer" {
		t.Fatalf("got %q", got)
	}
	if p.calls != 1 {
		t.Fatalf("model was called %d times", p.calls)
	}
	if len(p.tools[0]) != 0 {
		t.Errorf("no bindings must mean no tools offered, got %+v", p.tools[0])
	}
	if len(p.seen[0]) != 2 || p.seen[0][0].Role != "system" || p.seen[0][1].Role != "user" {
		t.Errorf("unexpected opening messages: %+v", p.seen[0])
	}
}

func TestRunReturnsProviderErrors(t *testing.T) {
	p := &scripted{err: errors.New("502 bad gateway")}
	if _, err := Run(context.Background(), p, "sys", "usr", nil, 4); err == nil {
		t.Fatal("a provider failure is the caller's problem, not silence")
	}
}

// A provider that dies half way through cannot un-produce the advice the model
// already wrote. Losing it costs the session a finished injection to report a
// failure it can do nothing about.
func TestRunKeepsTextProducedBeforeAProviderFailure(t *testing.T) {
	p := &scripted{
		err:   errors.New("context deadline exceeded"),
		errAt: 2,
		replies: []Message{{
			Role:      "assistant",
			Content:   `{"inject":"you already decided to use sqlite"}`,
			ToolCalls: []ToolCall{{ID: "c1", Name: "search_memory", Args: json.RawMessage(`{}`)}},
		}},
	}
	got, err := Run(context.Background(), p, "sys", "usr", []Binding{echoBinding("search_memory", "hit:", nil, nil)}, 4)
	if err != nil {
		t.Fatalf("a failure after usable text must not be the caller's problem: %v", err)
	}
	if !strings.Contains(got, "you already decided to use sqlite") {
		t.Fatalf("the advice the model had already produced was lost: %q", got)
	}
}

// Nothing usable was produced, so there is nothing to prefer over the error.
func TestRunReturnsProviderErrorsWhenNoTextWasProduced(t *testing.T) {
	p := &scripted{
		err:   errors.New("502 bad gateway"),
		errAt: 2,
		replies: []Message{{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "c1", Name: "one", Args: json.RawMessage(`{}`)}},
		}},
	}
	got, err := Run(context.Background(), p, "sys", "usr", []Binding{echoBinding("one", "1:", nil, nil)}, 4)
	if err == nil {
		t.Fatal("a provider failure with nothing to show for it is the caller's problem, not silence")
	}
	if got != "" {
		t.Fatalf("got %q alongside the error", got)
	}
}

// Tool results can only be read on a later step. On the last one there is no
// later step, so the handlers must not run at all.
func TestRunDoesNotAnswerToolCallsItCannotHandBack(t *testing.T) {
	p := &scripted{
		repeat: true,
		replies: []Message{{
			Role:      "assistant",
			Content:   "still looking",
			ToolCalls: []ToolCall{{ID: "c", Name: "one", Args: json.RawMessage(`{}`)}},
		}},
	}
	var hits int
	got, err := Run(context.Background(), p, "sys", "usr", []Binding{echoBinding("one", "1:", nil, &hits)}, 4)
	if err != nil {
		t.Fatalf("the step cap must not be an error: %v", err)
	}
	if p.calls != 4 {
		t.Fatalf("model was called %d times, want 4", p.calls)
	}
	if hits != 3 {
		t.Fatalf("handler ran %d times, want one per step whose results the model still gets", hits)
	}
	if got != "still looking" {
		t.Fatalf("the text the model had produced was lost: %q", got)
	}
	// What was gathered up to the last step still reached the model.
	last := p.seen[3]
	if last[len(last)-1].Role != "tool" {
		t.Errorf("the previous step's tool results were not replayed: %+v", last)
	}
}
