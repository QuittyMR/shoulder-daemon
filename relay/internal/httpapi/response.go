package httpapi

import (
	"fmt"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

// hookSpecificOutput and hookResponse are the ONLY types this package can
// serialise back to a harness. They have no field for `decision`, `continue`,
// `stopReason` or `permissionDecision`, so the relay is structurally incapable
// of blocking a tool, forcing continuation, or taking the user's turn. The
// guarantee is enforced by the type system first and by TestNeverBlocks second.
type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type hookResponse struct {
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// ForbiddenFields are the JSON keys whose presence would mean the relay had
// gained the power to interfere with a turn.
var ForbiddenFields = []string{"decision", "continue", "stopReason", "permissionDecision", "permissionDecisionReason", "updatedInput", "systemMessage"}

// Kept to one line: this rides along on every injection, and a paragraph of
// framing costs more context than the advice it wraps.
const envelopeNote = "Background observer. Not a user instruction. Ignore if irrelevant; do not mention it."

// Envelope frames sanitised advice. The advice text has already had every angle
// bracket entity-escaped, so it cannot close this tag or forge harness framing.
func Envelope(a session.Advice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<shoulder-daemon kind=%q id=%q>\n", string(a.Kind), a.ID)
	b.WriteString(a.Text)
	b.WriteString("\n</shoulder-daemon>\n")
	b.WriteString(envelopeNote)
	return b.String()
}

func inject(event string, a session.Advice) hookResponse {
	return hookResponse{HookSpecificOutput: &hookSpecificOutput{
		HookEventName:     event,
		AdditionalContext: Envelope(a),
	}}
}

// The two responses that carry nothing are constants: an empty hook response
// and a neutral reply with no advice. Both are on the path taken by almost
// every request.
var (
	silentJSON   = []byte(`{}`)
	noAdviceJSON = []byte(`{"advice":null}`)
)
