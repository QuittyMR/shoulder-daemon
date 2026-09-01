// Package render turns a window of session events into the text the advisor
// reads. It is deliberately lossy: tool inputs are summarised and results are
// clipped, because the advisor needs the shape of the work, not a transcript.
package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

const (
	// ToolResultClip is how much of a tool result ever reaches the advisor. It
	// is exported because it also bounds what is worth retaining upstream:
	// storing more than this is storing bytes nothing will ever read.
	ToolResultClip = 1500

	commandClip  = 120
	rawInputClip = 200
	proseClip    = 4000
	recallClip   = 600
)

// Window renders the most recent events that fit inside maxChars, oldest first.
func Window(events []session.Event, maxEvents, maxChars int) string {
	if maxEvents > 0 && len(events) > maxEvents {
		events = events[len(events)-maxEvents:]
	}

	lines := make([]string, 0, len(events))
	for _, e := range events {
		if s := line(e); s != "" {
			lines = append(lines, s)
		}
	}

	// Walk back from the newest line to find the oldest one that still fits,
	// then join once. Joining inside the loop would rebuild the whole window on
	// every dropped line.
	if maxChars > 0 && len(lines) > 1 {
		first := len(lines) - 1
		total := len(lines[first])
		for i := first - 1; i >= 0; i-- {
			next := total + 1 + len(lines[i])
			if next > maxChars {
				break
			}
			total = next
			first = i
		}
		lines = lines[first:]
	}
	return strings.Join(lines, "\n")
}

func line(e session.Event) string {
	switch e.Kind {
	case session.KindUserPrompt:
		return "<user>" + clip(e.Prompt, proseClip) + "</user>"
	case session.KindAssistantMessage, session.KindTurnEnd:
		if strings.TrimSpace(e.Assistant) == "" {
			return ""
		}
		s := "<assistant>" + clip(e.Assistant, proseClip) + "</assistant>"
		if e.Thinking != "" {
			s = "<thinking>" + clip(e.Thinking, proseClip/2) + "</thinking>\n" + s
		}
		return s
	case session.KindToolCall:
		return fmt.Sprintf("<tool name=%q>%s</tool>", e.ToolName, ToolSummary(e.ToolInput))
	case session.KindToolResult:
		return fmt.Sprintf("<result name=%q>%s</result>", e.ToolName, clip(e.ToolResult, ToolResultClip))
	case session.KindToolFailure:
		return fmt.Sprintf("<result name=%q error=\"true\">%s</result>", e.ToolName, clip(e.ToolResult, ToolResultClip))
	case session.KindCompact:
		return "<compact/>"
	}
	return ""
}

// summaryKeys is the order in which a tool input's fields identify what the
// call touched, with the length worth keeping of each. It is keyed on argument
// names rather than tool names so that a tool this relay has never seen — an
// MCP tool, or one from another harness — still summarises usefully.
var summaryKeys = []struct {
	name string
	clip int
}{
	{"command", commandClip},
	{"file_path", rawInputClip},
	{"url", rawInputClip},
	{"query", rawInputClip},
	{"description", rawInputClip},
	{"path", rawInputClip},
}

// ToolSummary reduces a tool's arguments to the one detail that identifies what
// it touched. Arguments it cannot recognise fall back to raw JSON, clipped.
func ToolSummary(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return clip(string(input), rawInputClip)
	}
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	// A search names both what it looked for and where, and the pair is the
	// summary; every other argument stands alone.
	if p := str("pattern"); p != "" {
		if path := str("path"); path != "" {
			return p + " in " + path
		}
		return p
	}
	for _, k := range summaryKeys {
		if v := str(k.name); v != "" {
			return clip(v, k.clip)
		}
	}
	return clip(string(input), rawInputClip)
}

func clip(s string, n int) string { return textutil.Clip(strings.TrimSpace(s), n) }

// RecallQuery builds the text used to search long-term memory. It deliberately
// ignores tool calls and results: they are the bulk of a window but they drag a
// semantic search towards whichever files were touched, not towards what was
// actually said or decided.
func RecallQuery(events []session.Event) string {
	var parts []string
	for i := len(events) - 1; i >= 0 && len(parts) < 4; i-- {
		e := events[i]
		switch e.Kind {
		case session.KindUserPrompt:
			if s := strings.TrimSpace(e.Prompt); s != "" {
				parts = append(parts, clip(s, recallClip))
			}
		case session.KindTurnEnd, session.KindAssistantMessage:
			if s := strings.TrimSpace(e.Assistant); s != "" {
				parts = append(parts, clip(s, recallClip))
			}
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "\n")
}
