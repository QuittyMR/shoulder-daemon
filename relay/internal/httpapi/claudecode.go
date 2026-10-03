package httpapi

import (
	"encoding/json"
	"sort"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/render"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

// claudeEvent is what the relay knows about one Claude Code hook event.
type claudeEvent struct {
	kind session.Kind
	// routine marks the events that fire in every healthy session, which is a
	// smaller set than the events the relay accepts: PreCompact only fires
	// around a context compaction and PostToolUseFailure only when a tool
	// errors, so never having seen either proves nothing about the install.
	routine bool
	// agent marks an event that only ever fires from inside a subagent.
	agent bool
}

// claudeEvents is the one place a Claude Code event name appears in the relay.
// The parser reads it to pick a neutral kind, the handler reads that kind to
// decide whether a response may carry advice, and doctor reads the routine
// names to check the plugin is wired up — a new event is added here and nowhere
// else, except in the plugin's hooks.json which must register the same set.
//
// SessionStart is absent on purpose: Claude Code refuses HTTP hooks for it, so
// the session opens lazily on whichever event arrives first.
var claudeEvents = map[string]claudeEvent{
	"UserPromptSubmit":   {kind: session.KindUserPrompt, routine: true},
	"PreToolUse":         {kind: session.KindToolCall, routine: true},
	"PostToolUse":        {kind: session.KindToolResult, routine: true},
	"PostToolUseFailure": {kind: session.KindToolFailure},
	"Stop":               {kind: session.KindTurnEnd, routine: true},
	// Neither subagent event is routine: a session that never spawns an agent
	// never fires them, and the main thread's Stop proves the install on its
	// own. SubagentStart is the first event to carry the id Claude Code gave
	// the agent, and the one place context reaches it before its first step.
	"SubagentStart": {kind: session.KindAgentStart, agent: true},
	"SubagentStop":  {kind: session.KindTurnEnd, agent: true},
	"PreCompact":    {kind: session.KindCompact},
	"SessionEnd":    {kind: session.KindSessionEnd, routine: true},
}

// RoutineEvents lists, in a stable order, the events doctor expects to have
// seen at least once on a working install.
func RoutineEvents() []string {
	out := make([]string, 0, len(claudeEvents))
	for name, e := range claudeEvents {
		if e.routine {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// claudeHook is the union of the Claude Code hook payload fields this relay
// consumes. Every field here was observed on the wire from Claude Code 2.1.251
// during Phase 0; the recorded fixtures live in testdata/hook-payloads/2.1.251/.
type claudeHook struct {
	SessionID     string          `json:"session_id"`
	HookEventName string          `json:"hook_event_name"`
	CWD           string          `json:"cwd"`
	Prompt        string          `json:"prompt"`
	ToolName      string          `json:"tool_name"`
	ToolUseID     string          `json:"tool_use_id"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolResponse  json.RawMessage `json:"tool_response"`

	// Every event; read on Stop. The JSONL session file, which holds every
	// text block of the turn where last_assistant_message holds the last.
	TranscriptPath string `json:"transcript_path"`

	// Stop, SubagentStop
	LastAssistantMessage string `json:"last_assistant_message"`

	// Every event fired from inside a subagent, and SubagentStart, which
	// fires before the agent's first own event. The subagent's prompt arrives
	// before any of them, as the input of the Agent tool call that spawns it:
	// SubagentStart carries the id and the type, never the prompt.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`

	// SessionEnd
	Reason string `json:"reason"`
}

// agentToolName is the Claude Code tool that spawns a subagent.
const agentToolName = "Agent"

// agentInput is the part of the Agent tool's input the relay reads: the prompt
// the subagent is given, and what kind of subagent it is.
type agentInput struct {
	Prompt       string `json:"prompt"`
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
}

// parseClaudeCode maps one Claude Code hook payload onto neutral events. The
// first is the hook's own; a PreToolUse that spawns a subagent also yields the
// subagent's prompt, which no hook of its own carries, as a user_prompt from
// the agent. The decoded payload comes with them, for the fields that inform
// the handler without being part of an event.
func parseClaudeCode(event string, body []byte, now time.Time) ([]session.Event, claudeHook, bool) {
	var h claudeHook
	mapped, ok := claudeEvents[event]
	if !ok {
		return nil, h, false
	}
	kind := mapped.kind
	if err := json.Unmarshal(body, &h); err != nil || h.SessionID == "" {
		return nil, h, false
	}

	ev := session.Event{
		Protocol:  1,
		Harness:   "claude-code",
		SessionID: h.SessionID,
		TS:        now,
		Kind:      kind,
		CWD:       h.CWD,
		ToolName:  h.ToolName,
		ToolUseID: h.ToolUseID,
		ToolInput: h.ToolInput,
		AgentID:   h.AgentID,
		AgentType: h.AgentType,
	}
	if h.AgentID != "" || mapped.agent {
		ev.Origin = session.OriginAgent
	}

	switch kind {
	case session.KindUserPrompt:
		ev.Prompt = h.Prompt
	case session.KindToolResult, session.KindToolFailure:
		// Clipped on the way in, not on the way out: the registry holds
		// hundreds of these per session for up to an hour, and no reader ever
		// sees more than this many bytes of one.
		ev.ToolResult = textutil.Clip(flatten(h.ToolResponse), render.ToolResultClip)
	case session.KindTurnEnd:
		ev.Assistant = h.LastAssistantMessage
	case session.KindSessionEnd:
		ev.StopReason = h.Reason
	}
	events := []session.Event{ev}
	if kind == session.KindToolCall && h.ToolName == agentToolName {
		if prompt, ok := agentPrompt(ev, h.ToolInput); ok {
			events = append(events, prompt)
		}
	}
	return events, h, true
}

// agentPrompt builds the subagent's prompt event from the Agent tool call that
// spawns it. The subagent has no id yet: Claude Code assigns one when it
// starts and reports it on SubagentStart, so the event carries its type and
// the spawning call's id, which is what the start is later matched to. A call
// with no prompt and no description spawns nothing worth reading and yields
// no event.
func agentPrompt(spawn session.Event, input json.RawMessage) (session.Event, bool) {
	var in agentInput
	if err := json.Unmarshal(input, &in); err != nil {
		return session.Event{}, false
	}
	text := in.Prompt
	if text == "" {
		text = in.Description
	}
	if text == "" {
		return session.Event{}, false
	}
	return session.Event{
		Protocol:  spawn.Protocol,
		Harness:   spawn.Harness,
		SessionID: spawn.SessionID,
		TS:        spawn.TS,
		Kind:      session.KindUserPrompt,
		CWD:       spawn.CWD,
		ToolUseID: spawn.ToolUseID,
		Prompt:    text,
		Origin:    session.OriginAgent,
		AgentType: in.SubagentType,
	}, true
}

// flatten reduces a tool_response to text. Claude Code sends either a string or
// an object whose useful content sits under a handful of well-known keys.
func flatten(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		for _, k := range []string{"stdout", "content", "output", "result", "text"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		if v, ok := m["stderr"].(string); ok && v != "" {
			return v
		}
	}
	return string(raw)
}
