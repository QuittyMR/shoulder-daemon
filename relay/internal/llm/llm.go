// Package llm is the model boundary for the decision step. Most providers speak
// the OpenAI chat completions wire format, so they differ only by base URL,
// auth header and model id.
package llm

import (
	"context"
	"encoding/json"
)

// Tool is one capability offered to the model.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema for the parameters object
}

// ToolCall is the model asking for one of them. ID ties the answer back to the
// request, because a single step may ask for several at once.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

type Message struct {
	Role       string // "system" | "user" | "assistant" | "tool"
	Content    string
	ToolCalls  []ToolCall // assistant only
	ToolCallID string     // tool only
}

// Provider keeps Complete because the digest and the CLI message path ask one
// question and want one answer; Chat is for the turn that may need to look
// things up before it can answer.
type Provider interface {
	Name() string
	Complete(ctx context.Context, system, user string) (string, error)
	Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error)
}

// Binding is a tool plus the code that answers it.
type Binding struct {
	Tool    Tool
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}
