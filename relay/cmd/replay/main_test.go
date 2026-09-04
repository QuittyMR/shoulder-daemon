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
