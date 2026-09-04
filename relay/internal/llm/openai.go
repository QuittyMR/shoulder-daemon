package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

const maxResponseBytes = 1 << 20

// defaultClient is shared by every connector that does not bring its own. The
// transport is not http.DefaultTransport because that one never notices a
// silently dropped HTTP/2 connection: every later request queues on the dead
// stream and burns the whole client timeout, one after another, until the
// process restarts. Pinging an idle connection and closing it when the ping
// goes unanswered turns that into one failed call and a fresh dial.
var defaultClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 10 * time.Second,
			PingTimeout:     5 * time.Second,
		},
	},
}

// OpenAICompatible talks to any server implementing POST {base}/chat/completions.
// BaseURL must already include the provider's version segment, because every
// provider documents it differently: OpenRouter ends /api/v1, Gemini's
// compatibility layer ends /v1beta/openai, Z.ai ends /api/paas/v4.
type OpenAICompatible struct {
	Label       string
	BaseURL     string
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float64
	Headers     map[string]string
	Extra       map[string]any
	HTTP        *http.Client
}

func (c *OpenAICompatible) Name() string {
	if c.Label != "" {
		return c.Label
	}
	return "openai-compatible"
}

func (c *OpenAICompatible) ModelID() string { return c.Model }

// wireMessage is the chat completions shape of a Message in both directions.
type wireMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Extra      json.RawMessage `json:"extra_content,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// The arguments are a JSON document carried as a string, not an object.
		Arguments string `json:"arguments"`
	} `json:"function"`
	// Extra is whatever the provider hangs off a tool call that is not part of
	// the OpenAI shape, kept opaque and echoed back untouched. Gemini puts a
	// thought signature here and rejects the next request with 400 if it does
	// not come back, so dropping it makes every tool-using turn fail on the
	// second step and only there.
	Extra json.RawMessage `json:"extra_content,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message wireMessage `json:"message"`
	} `json:"choices"`
}

func (c *OpenAICompatible) payload(msgs []wireMessage) map[string]any {
	p := map[string]any{
		"model":       c.Model,
		"temperature": c.Temperature,
		"messages":    msgs,
	}
	if c.MaxTokens > 0 {
		p["max_tokens"] = c.MaxTokens
	}
	for k, v := range c.Extra {
		p[k] = v
	}
	return p
}

func (c *OpenAICompatible) post(ctx context.Context, payload map[string]any) (chatResponse, error) {
	var out chatResponse

	body, err := json.Marshal(payload)
	if err != nil {
		return out, err
	}

	url := strings.TrimRight(c.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	client := c.HTTP
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return out, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, fmt.Errorf("%s status %d: %s", c.Name(), resp.StatusCode, textutil.Clip(string(raw), 200))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("%s returned unparseable JSON: %w", c.Name(), err)
	}
	return out, nil
}

func (c *OpenAICompatible) Complete(ctx context.Context, system, user string) (string, error) {
	out, err := c.post(ctx, c.payload([]wireMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}))
	if err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", nil
	}
	return out.Choices[0].Message.Content, nil
}

func (c *OpenAICompatible) Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error) {
	wire := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		w := wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Extra: m.Extra}
		for _, tc := range m.ToolCalls {
			var t wireToolCall
			t.ID, t.Type = tc.ID, "function"
			t.Function.Name = tc.Name
			t.Function.Arguments = string(tc.Args)
			t.Extra = tc.Extra
			w.ToolCalls = append(w.ToolCalls, t)
		}
		wire = append(wire, w)
	}

	payload := c.payload(wire)
	if len(tools) > 0 {
		specs := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			specs = append(specs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Schema,
				},
			})
		}
		payload["tools"] = specs
	}

	out, err := c.post(ctx, payload)
	if err != nil {
		return Message{}, err
	}
	if len(out.Choices) == 0 {
		return Message{Role: "assistant"}, nil
	}

	got := out.Choices[0].Message
	m := Message{Role: "assistant", Content: got.Content, Extra: got.Extra}
	for _, tc := range got.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 {
			// A tool with no parameters comes back with the arguments field
			// empty rather than as an empty object on several providers.
			args = json.RawMessage("{}")
		}
		m.ToolCalls = append(m.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: args, Extra: tc.Extra})
	}
	return m, nil
}
