package httpapi

import (
	"context"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

// stubPrompter answers every prompt with the same text and records what it
// was asked.
type stubPrompter struct {
	text  string
	asked []string
}

func (p *stubPrompter) PromptAdvice(_ context.Context, sid, prompt string, count uint64) (session.Advice, bool) {
	p.asked = append(p.asked, prompt)
	if p.text == "" {
		return session.Advice{}, false
	}
	return session.Advice{
		ID: "adv_p", SessionID: sid, Kind: session.AdviceNote, Level: session.LevelPlan,
		Text: p.text, CreatedEvent: count, TTLEvents: 4,
	}, true
}

func promptBody(sid, text string) string {
	return `{"session_id":"` + sid + `","hook_event_name":"UserPromptSubmit","prompt":"` + text + `"}`
}

// The main thread's prompt is answered by the prompter in the same hook
// response, before the assistant has read the prompt.
func TestThePromptHookCarriesThePromptersAnswer(t *testing.T) {
	s, _ := newTestServer(t)
	p := &stubPrompter{text: "Stored: when the user asks a question, answer it first."}
	s.Prompter = p
	h := s.Handler()

	got := post(h, "UserPromptSubmit", promptBody("p1", "why does it fail?")).Body.String()
	if !strings.Contains(got, p.text) {
		t.Fatalf("the prompt hook did not carry the prompter's answer: %s", got)
	}
	if len(p.asked) != 1 || p.asked[0] != "why does it fail?" {
		t.Fatalf("the prompter was asked %q", p.asked)
	}
	if got := post(h, "PreToolUse", `{"session_id":"p1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`).Body.String(); got != "{}" {
		t.Fatalf("a tool call asked the prompter or carried its answer again: %s", got)
	}
}

// The prompter's answer counts as said: the same words are not carried again
// for the cooldown, by the prompter or by the outbox, however the gate is set.
func TestThePromptersAnswerStartsTheCooldown(t *testing.T) {
	s, box := newTestServer(t)
	s.Budget = budget.Gate{MinEventGap: 0, MaxChars: 800, SessionMaxChars: 4000}
	p := &stubPrompter{text: "Stored: when the user asks a question, answer it first."}
	s.Prompter = p
	h := s.Handler()

	if got := post(h, "UserPromptSubmit", promptBody("p2", "why?")).Body.String(); !strings.Contains(got, p.text) {
		t.Fatalf("first prompt: %s", got)
	}
	post(h, "Stop", `{"session_id":"p2","hook_event_name":"Stop","last_assistant_message":"because"}`)
	if got := post(h, "UserPromptSubmit", promptBody("p2", "and why that?")).Body.String(); strings.Contains(got, p.text) {
		t.Fatalf("the same words were carried again two events later: %s", got)
	}
	if r := box.Push(session.Advice{
		ID: "adv_bg", SessionID: "p2", Kind: session.AdviceNote, Level: session.LevelPlan,
		Text: "stored: When the user asks a question, answer it first.", CreatedEvent: s.Registry.MainEvents("p2"), TTLEvents: 4,
	}); r != outbox.Cooling {
		t.Fatalf("the background consult queued the words the prompter had said: %v", r)
	}
}

// A subagent's prompt and a session with no prompter are answered as before.
func TestOnlyTheMainThreadsPromptAsksThePrompter(t *testing.T) {
	s, _ := newTestServer(t)
	p := &stubPrompter{text: "Stored: something."}
	s.Prompter = p
	h := s.Handler()

	got := postNeutral(h, `{"session_id":"p3","event":"user_prompt","prompt":"look","agent_id":"agent-1"}`).Body.String()
	if strings.Contains(got, p.text) || len(p.asked) != 0 {
		t.Fatalf("a subagent's prompt asked the prompter: %s %q", got, p.asked)
	}
	got = postNeutral(h, `{"session_id":"p3","event":"user_prompt","prompt":"look"}`).Body.String()
	if !strings.Contains(got, p.text) {
		t.Fatalf("the neutral endpoint's main-thread prompt did not carry the answer: %s", got)
	}

	s.Prompter = nil
	if got := post(h, "UserPromptSubmit", promptBody("p4", "hello")).Body.String(); got != "{}" {
		t.Fatalf("with no prompter the prompt hook answered %s", got)
	}
}
