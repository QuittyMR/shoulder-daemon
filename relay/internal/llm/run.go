package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// Run drives the tool loop and returns the model's final text. A model that
// keeps calling tools must not take the turn down with it, so the step cap
// returns the last text seen instead of an error.
//
// The same rule holds for a provider that fails part way through: text already
// produced is a decision the model has already made, and it outlives the round
// trip that failed. Run therefore reports an error only when it has nothing to
// say, which lets a caller treat a non-empty result as usable without also
// having to inspect the error.
func Run(ctx context.Context, p Provider, system, user string, bindings []Binding, maxSteps int) (string, error) {
	tools := make([]Tool, 0, len(bindings))
	handlers := make(map[string]func(context.Context, json.RawMessage) (string, error), len(bindings))
	for _, b := range bindings {
		tools = append(tools, b.Tool)
		handlers[b.Tool.Name] = b.Handler
	}

	msgs := []Message{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}
	var text string
	for step := 0; step < maxSteps; step++ {
		m, err := p.Chat(ctx, msgs, tools)
		if err != nil {
			if text != "" {
				return text, nil
			}
			return "", err
		}
		if m.Content != "" {
			text = m.Content
		}
		if len(m.ToolCalls) == 0 {
			// text, not m.Content: a step that answers with nothing after an
			// earlier step already produced a decision must not erase it.
			return text, nil
		}
		// The results of these calls could only be handed back on a step that
		// does not exist, so running the handlers would spend real backend work
		// on answers nobody will read. Stopping here rather than answering with
		// no tools offered keeps the cap meaning what it says — maxSteps model
		// round trips — and leaves the last message still asking for a tool,
		// which is how a caller tells "ran out of steps" from "was done".
		if step == maxSteps-1 {
			break
		}
		msgs = append(msgs, m)
		for _, call := range m.ToolCalls {
			msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Content: dispatch(ctx, handlers, call)})
		}
	}
	return text, nil
}

// dispatch answers one call. A failure is information the model can act on —
// it may retry with different arguments — so it goes back as the result.
func dispatch(ctx context.Context, handlers map[string]func(context.Context, json.RawMessage) (string, error), call ToolCall) string {
	h, ok := handlers[call.Name]
	if !ok {
		return fmt.Sprintf("error: no tool named %q", call.Name)
	}
	out, err := h(ctx, call.Args)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}
