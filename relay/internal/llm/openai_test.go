package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve returns a provider pointed at a server that replies with body and
// records the request payload it was sent.
func serve(t *testing.T, body string) (*OpenAICompatible, *map[string]any) {
	t.Helper()
	got := new(map[string]any)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, got); err != nil {
			t.Errorf("unparseable request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &OpenAICompatible{Label: "test", BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}, got
}

func TestChatSendsToolsInTheOpenAIShape(t *testing.T) {
	c, got := serve(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	tools := []Tool{{
		Name:        "search_memory",
		Description: "search stored facts",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
	}}
	if _, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "u"}}, tools); err != nil {
		t.Fatal(err)
	}

	list, ok := (*got)["tools"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("tools not sent: %v", (*got)["tools"])
	}
	entry := list[0].(map[string]any)
	if entry["type"] != "function" {
		t.Errorf("type = %v", entry["type"])
	}
	fn := entry["function"].(map[string]any)
	if fn["name"] != "search_memory" || fn["description"] != "search stored facts" {
		t.Errorf("function = %v", fn)
	}
	if params, ok := fn["parameters"].(map[string]any); !ok || params["type"] != "object" {
		t.Errorf("parameters = %v", fn["parameters"])
	}
}

func TestChatOmitsToolsWhenThereAreNone(t *testing.T) {
	c, got := serve(t, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	m, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "u"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "ok" || len(m.ToolCalls) != 0 {
		t.Fatalf("got %+v", m)
	}
	if _, present := (*got)["tools"]; present {
		t.Error("a request with no tools must not carry a tools field")
	}
}

func TestChatSendsToolCallsAndResults(t *testing.T) {
	c, got := serve(t, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	msgs := []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "one", Args: json.RawMessage(`{"q":"x"}`)}}},
		{Role: "tool", ToolCallID: "call_1", Content: "result text"},
	}
	if _, err := c.Chat(context.Background(), msgs, nil); err != nil {
		t.Fatal(err)
	}

	sent := (*got)["messages"].([]any)
	if len(sent) != 4 {
		t.Fatalf("sent %d messages", len(sent))
	}
	assistant := sent[2].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Errorf("tool call = %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "one" || fn["arguments"] != `{"q":"x"}` {
		t.Errorf("function = %v; arguments must be a JSON string", fn)
	}
	result := sent[3].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "call_1" || result["content"] != "result text" {
		t.Errorf("tool result = %v", result)
	}
	if _, present := sent[1].(map[string]any)["tool_call_id"]; present {
		t.Error("a plain message must not carry tool_call_id")
	}
}

func TestChatParsesToolCalls(t *testing.T) {
	c, _ := serve(t, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"search_memory","arguments":"{\"query\":\"x\"}"}},
		{"id":"call_2","type":"function","function":{"name":"session_history","arguments":""}}]}}]}`)
	m, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "u"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Role != "assistant" || m.Content != "" {
		t.Errorf("got %+v", m)
	}
	if len(m.ToolCalls) != 2 {
		t.Fatalf("got %d tool calls", len(m.ToolCalls))
	}
	if m.ToolCalls[0].ID != "call_1" || m.ToolCalls[0].Name != "search_memory" {
		t.Errorf("first call = %+v", m.ToolCalls[0])
	}
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(m.ToolCalls[0].Args, &args); err != nil || args.Query != "x" {
		t.Errorf("arguments = %s (%v)", m.ToolCalls[0].Args, err)
	}
	// A zero-argument tool must still hand the handler something parseable.
	if err := json.Unmarshal(m.ToolCalls[1].Args, &struct{}{}); err != nil {
		t.Errorf("empty arguments = %q: %v", m.ToolCalls[1].Args, err)
	}
}

// A server that ignores the tools field just answers, and that has to keep
// working.
func TestChatWithAProviderThatIgnoresTools(t *testing.T) {
	c, _ := serve(t, `{"choices":[{"message":{"role":"assistant","content":"plain"}}]}`)
	m, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "u"}},
		[]Tool{{Name: "one", Schema: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "plain" || len(m.ToolCalls) != 0 {
		t.Fatalf("got %+v", m)
	}
}

func TestChatOnAnEmptyChoiceList(t *testing.T) {
	c, _ := serve(t, `{"choices":[]}`)
	m, err := c.Chat(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "" || len(m.ToolCalls) != 0 {
		t.Fatalf("got %+v", m)
	}
}

// Complete is the digest and CLI path and must go on sending exactly what it
// sent before.
func TestCompleteStillSendsTwoPlainMessages(t *testing.T) {
	c, got := serve(t, `{"choices":[{"message":{"content":"answer"}}]}`)
	c.MaxTokens = 1200
	c.Temperature = 0.2
	c.Extra = map[string]any{"thinking": map[string]string{"type": "disabled"}}

	out, err := c.Complete(context.Background(), "sys", "usr")
	if err != nil || out != "answer" {
		t.Fatalf("got %q err %v", out, err)
	}
	if (*got)["model"] != "m" || (*got)["max_tokens"] != float64(1200) || (*got)["temperature"] != 0.2 {
		t.Errorf("payload = %v", *got)
	}
	if (*got)["thinking"] == nil {
		t.Error("provider Extra must survive")
	}
	if _, present := (*got)["tools"]; present {
		t.Error("Complete must never send tools")
	}
	sent := (*got)["messages"].([]any)
	if len(sent) != 2 {
		t.Fatalf("sent %d messages", len(sent))
	}
	for i, want := range []struct{ role, content string }{{"system", "sys"}, {"user", "usr"}} {
		m := sent[i].(map[string]any)
		if m["role"] != want.role || m["content"] != want.content {
			t.Errorf("message %d = %v", i, m)
		}
	}
}

func TestCompleteReportsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()
	c := &OpenAICompatible{Label: "test", BaseURL: srv.URL, Model: "m", HTTP: srv.Client()}
	if _, err := c.Complete(context.Background(), "s", "u"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := c.Chat(context.Background(), nil, nil); err == nil {
		t.Fatal("expected an error")
	}
}

// Gemini attaches a thought signature to every tool call it makes and answers
// 400 if the next request does not carry it back, so a turn that calls a tool
// fails on its second step and nowhere else.
func TestProviderStateOnAToolCallIsReplayedVerbatim(t *testing.T) {
	const sig = `{"google":{"thought_signature":"abc123"}}`

	var second []byte
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			second, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"search_memory","arguments":"{}"},
			 "extra_content":` + sig + `}]}}]}`))
	}))
	defer ts.Close()

	c := &OpenAICompatible{Label: "test", BaseURL: ts.URL, Model: "m", HTTP: ts.Client()}
	got, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "go"}}, []Tool{{Name: "search_memory"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolCalls) != 1 || string(got.ToolCalls[0].Extra) != sig {
		t.Fatalf("the provider's state was dropped on the way in: %+v", got.ToolCalls)
	}

	if _, err := c.Chat(context.Background(), []Message{
		{Role: "user", Content: "go"},
		got,
		{Role: "tool", ToolCallID: "c1", Content: "nothing"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), `"thought_signature":"abc123"`) {
		t.Fatalf("the provider's state was not sent back:\n%s", second)
	}
}

// A connector without its own client must get one that closes a connection
// whose peer has gone quiet, or a dropped HTTP/2 stream costs every later call
// the full timeout.
func TestDefaultClientDetectsDeadConnections(t *testing.T) {
	tr, ok := defaultClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", defaultClient.Transport)
	}
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatalf("HTTP/2 health pings not configured: %+v", tr.HTTP2)
	}
	if defaultClient.Timeout == 0 {
		t.Fatal("client has no timeout")
	}
	if tr.HTTP2.SendPingTimeout+tr.HTTP2.PingTimeout >= defaultClient.Timeout {
		t.Fatalf("a dead connection is noticed after %v, later than the %v client timeout", tr.HTTP2.SendPingTimeout+tr.HTTP2.PingTimeout, defaultClient.Timeout)
	}
}
