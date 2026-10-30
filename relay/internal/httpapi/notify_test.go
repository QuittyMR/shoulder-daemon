package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/notify"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

// hookBody is the smallest Claude Code hook payload for one session.
func hookBody(event, sid string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": sid, "hook_event_name": event, "prompt": "hi",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}, "last_assistant_message": "done",
	})
	return string(b)
}

// A notice is the one thing the relay says to the person: it rides on the next
// hook from its session as systemMessage, which the harness shows and never
// puts in front of the model, and the model's own channel stays empty.
func TestNoticesReachThePersonOnTheNextHook(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Notices = notify.New(notify.Defaults())
	h := srv.Handler()

	srv.Notices.Push("s1", notify.Stored, `global (preference): "terse answers"`)
	srv.Notices.Push("s1", notify.Error, "decision failed: gemini status 400")
	srv.Notices.Push("s2", notify.Stored, "somebody else's")

	rec := post(h, "PreToolUse", hookBody("PreToolUse", "s1"))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	msg, _ := out["systemMessage"].(string)
	want := "shoulder-daemon stored: global (preference): \"terse answers\"\nshoulder-daemon error: decision failed: gemini status 400"
	if msg != want {
		t.Fatalf("systemMessage = %q, want %q", msg, want)
	}
	if _, has := out["hookSpecificOutput"]; has {
		t.Fatal("a notice must not open the model's channel")
	}

	// Collected once; the next hook has nothing, and another session's
	// notice stays where it is.
	if body := post(h, "PreToolUse", hookBody("PreToolUse", "s1")).Body.String(); strings.TrimSpace(body) != "{}" {
		t.Fatalf("a drained session answers silence, got %s", body)
	}
	if body := post(h, "UserPromptSubmit", hookBody("UserPromptSubmit", "s2")).Body.String(); !strings.Contains(body, "somebody else") {
		t.Fatalf("the other session's notice was lost: %s", body)
	}
}

// Claude Code discards systemMessage on PreCompact and SessionEnd, so a notice
// handed over there would vanish; it waits for a hook that shows it.
func TestNoticesWaitPastHooksThatDiscardThem(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Notices = notify.New(notify.Defaults())
	h := srv.Handler()
	srv.Notices.Push("s1", notify.Stored, "kept")

	for _, event := range []string{"PreCompact", "SessionEnd"} {
		if body := post(h, event, hookBody(event, "s1")).Body.String(); strings.Contains(body, "systemMessage") {
			t.Fatalf("%s carried a notice the harness would discard: %s", event, body)
		}
	}
	if body := post(h, "UserPromptSubmit", hookBody("UserPromptSubmit", "s1")).Body.String(); !strings.Contains(body, "shoulder-daemon stored: kept") {
		t.Fatalf("the notice did not survive to a hook that shows it: %s", body)
	}
}

// An injection is told in the same response that carries it, with the text
// the model was given, so the person sees what the model sees.
func TestAnInjectionIsNoticedWithItsText(t *testing.T) {
	srv, box := newTestServer(t)
	srv.Notices = notify.New(notify.Defaults())
	h := srv.Handler()
	box.Push(session.Advice{ID: "adv_1", SessionID: "s1", Kind: session.AdviceNote, Text: "the branch is master", CreatedAt: time.Now()})

	rec := post(h, "UserPromptSubmit", hookBody("UserPromptSubmit", "s1"))
	var out struct {
		SystemMessage string         `json:"systemMessage"`
		Hook          map[string]any `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.SystemMessage != `shoulder-daemon injected: "the branch is master"` {
		t.Fatalf("systemMessage = %q", out.SystemMessage)
	}
	if ctx, _ := out.Hook["additionalContext"].(string); !strings.Contains(ctx, "the branch is master") {
		t.Fatalf("the advice itself must still be injected: %v", out.Hook)
	}
}

// Nothing is said unless asked: a server without a box, or with every kind
// off, answers exactly as before.
func TestNoNoticesWithoutAPolicy(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	if body := post(h, "PreToolUse", hookBody("PreToolUse", "s1")).Body.String(); strings.TrimSpace(body) != "{}" {
		t.Fatalf("no box: %s", body)
	}
	srv.Notices = notify.New(notify.Policy{})
	srv.Notices.Push("s1", notify.Stored, "dropped at the door")
	if body := post(h, "PreToolUse", hookBody("PreToolUse", "s1")).Body.String(); strings.TrimSpace(body) != "{}" {
		t.Fatalf("every kind off: %s", body)
	}
}

// The neutral route carries notices as data; the OpenCode plugin shows them.
func TestTheNeutralRouteCarriesNotices(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.Notices = notify.New(notify.Defaults())
	h := srv.Handler()
	srv.Notices.Push("s1", notify.Stored, `global: "x"`)

	body, _ := json.Marshal(map[string]any{"session_id": "s1", "event": "tool_call", "tool_name": "bash"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(string(body))))
	var out struct {
		Advice  any             `json:"advice"`
		Notices []notify.Notice `json:"notices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Advice != nil || len(out.Notices) != 1 || out.Notices[0].Kind != notify.Stored || out.Notices[0].Text != `global: "x"` {
		t.Fatalf("got %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(string(body))))
	if strings.TrimSpace(rec.Body.String()) != `{"advice":null}` {
		t.Fatalf("a drained session answers as before: %s", rec.Body.String())
	}
}
