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
	// Extra is provider state attached to this call, replayed verbatim.
	Extra json.RawMessage
	ID    string
	Name  string
	Args  json.RawMessage
}

type Message struct {
	Role       string // "system" | "user" | "assistant" | "tool"
	Content    string
	ToolCalls  []ToolCall // assistant only
	ToolCallID string     // tool only
	// Extra is provider state attached to this message, replayed verbatim.
	Extra json.RawMessage
}

// Provider keeps Complete because the digest and the CLI message path ask one
// question and want one answer; Chat is for the turn that may need to look
// things up before it can answer.
type Provider interface {
	Name() string
	Complete(ctx context.Context, system, user string) (string, error)
	Chat(ctx context.Context, msgs []Message, tools []Tool) (Message, error)
}

// Named is a provider that can say which model it sends to. It is separate from
// Provider because nothing on the decision path needs it: it exists so
// `shoulderd config` can report what is actually in use rather than what was
// asked for, which are different the moment a preset default fills in a blank.
type Named interface {
	ModelID() string
}

// ModelOf reports the model a provider sends to, and "" for one that does not
// say and for no provider at all.
func ModelOf(p Provider) string {
	if n, ok := p.(Named); ok {
		return n.ModelID()
	}
	return ""
}

// Binding is a tool plus the code that answers it.
type Binding struct {
	Tool    Tool
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}
