package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func doRequest(t *testing.T, cfg config, body chatRequest) (*http.Response, chatResponse) {
	t.Helper()

	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	srv := httptest.NewServer(handleChatCompletions(cfg))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	var out chatResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp, out
}

func TestHandleChatCompletions_Modes(t *testing.T) {
	req := chatRequest{
		Model:       "shoulder",
		User:        "session-123",
		MaxTokens:   300,
		Temperature: 0.3,
		Messages: []chatMessage{
			{Role: "system", Content: "you are an advisor"},
			{Role: "user", Content: "the quick brown fox jumps over the lazy dog"},
		},
	}

	cases := []struct {
		name       string
		mode       string
		echoText   string
		wantExact  string
		wantPrefix string
	}{
		{name: "noop default", mode: "noop", wantExact: "NOOP"},
		{name: "fixed", mode: "fixed", echoText: "please add tests", wantExact: "please add tests"},
		{name: "echo", mode: "echo", wantPrefix: "echo: the quick brown fox"},
		{name: "unknown mode falls back to noop", mode: "bogus", wantExact: "NOOP"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config{mode: tc.mode, text: tc.echoText}
			resp, out := doRequest(t, cfg, req)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if out.Object != "chat.completion" {
				t.Errorf("object = %q, want chat.completion", out.Object)
			}
			if out.ID == "" {
				t.Errorf("id is empty")
			}
			if out.Created == 0 {
				t.Errorf("created is zero")
			}
			if out.Model != req.Model {
				t.Errorf("model = %q, want %q", out.Model, req.Model)
			}
			if len(out.Choices) != 1 {
				t.Fatalf("choices len = %d, want 1", len(out.Choices))
			}
			choice := out.Choices[0]
			if choice.Index != 0 {
				t.Errorf("choice.index = %d, want 0", choice.Index)
			}
			if choice.Message.Role != "assistant" {
				t.Errorf("choice.message.role = %q, want assistant", choice.Message.Role)
			}
			if choice.FinishReason != "stop" {
				t.Errorf("choice.finish_reason = %q, want stop", choice.FinishReason)
			}
			content := choice.Message.Content
			if tc.wantExact != "" && content != tc.wantExact {
				t.Errorf("content = %q, want exactly %q", content, tc.wantExact)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(content, tc.wantPrefix) {
				t.Errorf("content = %q, want prefix %q", content, tc.wantPrefix)
			}
			if out.Usage.TotalTokens != out.Usage.PromptTokens+out.Usage.CompletionTokens {
				t.Errorf("usage.total_tokens = %d, want prompt+completion = %d",
					out.Usage.TotalTokens, out.Usage.PromptTokens+out.Usage.CompletionTokens)
			}
		})
	}
}

func TestHandleChatCompletions_EchoTruncatesTo200Chars(t *testing.T) {
	long := strings.Repeat("a", 500)
	req := chatRequest{
		Messages: []chatMessage{{Role: "user", Content: long}},
	}
	cfg := config{mode: "echo"}
	resp, out := doRequest(t, cfg, req)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	content := out.Choices[0].Message.Content
	if !strings.HasPrefix(content, "echo: ") {
		t.Fatalf("content missing echo prefix: %q", content)
	}
	if got := len([]rune(strings.TrimPrefix(content, "echo: "))); got != 200 {
		t.Errorf("echoed length = %d, want 200", got)
	}
}

func TestHandleChatCompletions_EchoModeNoUserMessageIsNoop(t *testing.T) {
	req := chatRequest{
		Messages: []chatMessage{{Role: "system", Content: "no user message here"}},
	}
	cfg := config{mode: "echo"}
	_, out := doRequest(t, cfg, req)

	if out.Choices[0].Message.Content != "NOOP" {
		t.Errorf("content = %q, want NOOP when there is no user message", out.Choices[0].Message.Content)
	}
}

func TestHandleChatCompletions_DelayIsApplied(t *testing.T) {
	cfg := config{mode: "noop", delay: 30 * time.Millisecond}
	req := chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}

	start := time.Now()
	resp, _ := doRequest(t, cfg, req)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed < cfg.delay {
		t.Errorf("elapsed = %v, want at least %v", elapsed, cfg.delay)
	}
}

func TestHandleChatCompletions_FailRateAlwaysFails(t *testing.T) {
	cfg := config{mode: "noop", failRate: 1.0}
	req := chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}

	resp, _ := doRequest(t, cfg, req)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 with fail_rate=1.0", resp.StatusCode)
	}
}

func TestHandleChatCompletions_FailRateZeroNeverFails(t *testing.T) {
	cfg := config{mode: "noop", failRate: 0}
	req := chatRequest{Messages: []chatMessage{{Role: "user", Content: "hi"}}}

	for i := 0; i < 20; i++ {
		resp, _ := doRequest(t, cfg, req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 with fail_rate=0", resp.StatusCode)
		}
	}
}

func TestHandleChatCompletions_InvalidBody(t *testing.T) {
	cfg := config{mode: "noop"}
	srv := httptest.NewServer(handleChatCompletions(cfg))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for invalid JSON", resp.StatusCode)
	}
}

func TestHandleChatCompletions_MethodNotAllowed(t *testing.T) {
	cfg := config{mode: "noop"}
	srv := httptest.NewServer(handleChatCompletions(cfg))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for GET", resp.StatusCode)
	}
}

func TestHandleHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(handleHealthz))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "ok" {
		t.Errorf("status field = %q, want ok", out["status"])
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg := loadConfig()
	if cfg.addr != ":9090" {
		t.Errorf("default addr = %q, want :9090", cfg.addr)
	}
	if cfg.mode != "noop" {
		t.Errorf("default mode = %q, want noop", cfg.mode)
	}
	if cfg.delay != 0 {
		t.Errorf("default delay = %v, want 0", cfg.delay)
	}
	if cfg.failRate != 0 {
		t.Errorf("default fail rate = %v, want 0", cfg.failRate)
	}
}
