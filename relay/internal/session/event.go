// Package session defines the harness-neutral event vocabulary that every
// adapter maps onto, plus the advisory records the relay hands back.
package session

import (
	"encoding/json"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
)

type Kind string

const (
	KindUserPrompt       Kind = "user_prompt"
	KindToolCall         Kind = "tool_call"
	KindToolResult       Kind = "tool_result"
	KindToolFailure      Kind = "tool_failure"
	KindAssistantMessage Kind = "assistant_message"
	KindTurnEnd          Kind = "turn_end"
	KindCompact          Kind = "compact"
	KindSessionEnd       Kind = "session_end"
)

// AdviceLevel says where in a turn a piece of advice is still worth delivering.
type AdviceLevel string

const (
	// LevelPlan is context: something the assistant should know before it
	// decides what to do. It is only useful before it has decided, so it is
	// delivered at the prompt and nowhere else.
	LevelPlan AdviceLevel = "plan"

	// LevelAction is about an operation that is about to happen. It is
	// delivered at a tool call, which is the last point anything can be
	// stopped.
	LevelAction AdviceLevel = "action"
)

// Delivers reports whether a response to this event may carry advice of this
// level.
//
// Advice used to drain on whichever hook fired first, which put context in
// front of an assistant that had already chosen its next several tool calls -
// too late to change the plan, and unable to recall the calls already in
// flight. Where a note lands has to follow what the note is for: context is
// only actionable before the assistant has committed to anything, and a warning
// about an operation is only actionable at the operation. Everything else - a
// tool result, an assistant message, a turn end - is after the fact, and
// spending a note there is the same as discarding it.
func (k Kind) Delivers(level AdviceLevel) bool {
	switch level {
	case LevelAction:
		return k == KindToolCall
	default:
		return k == KindUserPrompt
	}
}

// Event is one observation from a coding session. Adapters translate their
// harness's native payload into this shape; nothing downstream knows which
// harness produced it.
type Event struct {
	Protocol       int             `json:"protocol"`
	Harness        string          `json:"harness"`
	HarnessVersion string          `json:"harness_version,omitempty"`
	SessionID      string          `json:"session_id"`
	Seq            uint64          `json:"seq"`
	TS             time.Time       `json:"ts"`
	Kind           Kind            `json:"event"`
	CWD            string          `json:"cwd,omitempty"`
	GitBranch      string          `json:"git_branch,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResult     string          `json:"tool_result,omitempty"`
	Assistant      string          `json:"assistant,omitempty"`
	StopReason     string          `json:"stop_reason,omitempty"`

	// Thinking carries verbatim reasoning text. It is always empty on the
	// Claude Code hook path: every thinking block Claude Code persists has an
	// empty body (verified across 17,200 blocks in 708 transcripts). Only the
	// OpenCode adapter's ReasoningPart and the opt-in capture proxy fill it.
	Thinking       string `json:"thinking,omitempty"`
	ThinkingTokens int    `json:"thinking_tokens,omitempty"`
}

// AdviceKind controls how hard the budget gate pushes back.
type AdviceKind string

const (
	AdviceNote    AdviceKind = "note"
	AdviceWarning AdviceKind = "warning"
	AdviceRecall  AdviceKind = "recall"
)

// Advice is one pending advisory message for a session.
type Advice struct {
	ID          string      `json:"id"`
	SessionID   string      `json:"session_id"`
	Kind        AdviceKind  `json:"kind"`
	Level       AdviceLevel `json:"level,omitempty"`
	Text        string      `json:"text"`
	CreatedTurn uint64      `json:"created_turn"`
	TTLTurns    int         `json:"ttl_turns"`
	CreatedAt   time.Time   `json:"created_at"`
}

// Expired reports whether the advice has sat unclaimed for longer than its TTL.
// Stale observations are dropped rather than injected late. It defers to the
// budget gate's rule so the outbox and the gate cannot disagree about what has
// gone stale.
func (a Advice) Expired(turn uint64) bool { return a.Candidate().Expired(turn) }

// Candidate projects the advice onto the flat shape the budget gate evaluates.
func (a Advice) Candidate() budget.Candidate {
	return budget.Candidate{
		Kind:        string(a.Kind),
		Len:         len(a.Text),
		CreatedTurn: a.CreatedTurn,
		TTLTurns:    a.TTLTurns,
	}
}
