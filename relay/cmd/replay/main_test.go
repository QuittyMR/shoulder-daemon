package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

func TestFlattenReadsAStringOrJoinsTextBlocks(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"empty", ``, ""},
		{"string", `"plain"`, "plain"},
		{"blocks", `[{"type":"text","text":"a"},{"type":"tool_use","name":"Bash"},{"type":"text","text":"b"}]`, "ab"},
		{"neither", `{"stdout":"x"}`, `{"stdout":"x"}`},
	}
	for _, tc := range cases {
		if got := flatten(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: flatten(%s) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestBlocksTolerateAContentThatIsNotAList(t *testing.T) {
	if got := blocks(json.RawMessage(`"just a string"`)); got != nil {
		t.Fatalf("a string content must yield no blocks, got %v", got)
	}
	got := blocks(json.RawMessage(`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x"}}]`))
	if len(got) != 1 || got[0].Name != "Read" || got[0].ID != "t1" || string(got[0].Input) != `{"file_path":"/x"}` {
		t.Fatalf("tool_use block not decoded: %+v", got)
	}
}

// Harness housekeeping is written into the transcript as user turns. Replaying
// it as prompts would have the advisor judging text nobody typed.
func TestNoiseIsWhatTheHarnessWroteNotWhatTheUserSaid(t *testing.T) {
	noise := []string{
		"", "   \n",
		"[Request interrupted by user for tool use]",
		"<system-reminder>\nsomething</system-reminder>",
		"  Base directory for this skill: /x",
		"<command-name>/foo</command-name>",
		"Caveat: The messages below were generated",
	}
	for _, s := range noise {
		if !isNoise(s) {
			t.Errorf("%q should be noise", s)
		}
	}
	for _, s := range []string{"fix the bug", "the main branch is master", "a <system-reminder> mid-sentence"} {
		if isNoise(s) {
			t.Errorf("%q is a real prompt", s)
		}
	}
}

func TestParseTSFallsBackToNowRatherThanZero(t *testing.T) {
	want := time.Date(2026, 9, 3, 12, 0, 0, 500, time.UTC)
	if got := parseTS(want.Format(time.RFC3339Nano)); !got.Equal(want) {
		t.Fatalf("parseTS = %v, want %v", got, want)
	}
	before := time.Now()
	got := parseTS("not a timestamp")
	if got.Before(before) || got.After(time.Now()) {
		t.Fatalf("an unparseable stamp must become now, got %v", got)
	}
}

func TestSendPostsANeutralEventAndReturnsTheAdvice(t *testing.T) {
	var got session.Event
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" || r.Method != http.MethodPost {
			t.Errorf("replay posted %s %s", r.Method, r.URL.Path)
		}
		token = r.Header.Get("X-Shoulder-Token")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("body is not an event: %v", err)
		}
		_, _ = io.WriteString(w, `{"advice":{"text":"branch is master"}}`)
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "tok", sessionID: "replay-1", http: http.Client{Timeout: time.Second}}
	adv := c.send(session.Event{Kind: session.KindUserPrompt, Prompt: "rebase onto main"})

	if adv != "branch is master" {
		t.Fatalf("advice = %q", adv)
	}
	if token != "tok" {
		t.Fatalf("token header = %q", token)
	}
	if got.Protocol != 1 || got.Harness != "claude-code-replay" || got.SessionID != "replay-1" || got.Prompt != "rebase onto main" {
		t.Fatalf("event sent was %+v", got)
	}
}

func TestSendIsSilentWhenThereIsNoAdviceOrNoDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"advice":null}`)
	}))
	defer srv.Close()
	c := &client{base: srv.URL, sessionID: "s", http: http.Client{Timeout: time.Second}}
	if adv := c.send(session.Event{Kind: session.KindTurnEnd}); adv != "" {
		t.Fatalf("no advice must be an empty string, got %q", adv)
	}
	srv.Close()
	if adv := c.send(session.Event{Kind: session.KindTurnEnd}); adv != "" {
		t.Fatalf("a dead daemon must be an empty string, got %q", adv)
	}
}
