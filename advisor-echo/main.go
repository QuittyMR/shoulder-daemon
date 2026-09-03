// Command advisor-echo is a reference implementation of the shoulder-daemon
// advisor contract (docs/ADVISOR.md, plan §4.5). It speaks the OpenAI
// `POST /v1/chat/completions` inbound API and answers deterministically
// according to ECHO_MODE, so the relay -> advisor wiring can be exercised
// end to end without a real model behind it.
package main

import (
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// chatMessage is one entry of the OpenAI chat messages array.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the inbound OpenAI-compatible request body.
type chatRequest struct {
	Model       string        `json:"model"`
	User        string        `json:"user"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
}

// chatChoice is one entry of the OpenAI chat response choices array.
type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatUsage is the OpenAI token-usage accounting block.
type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatResponse is the outbound OpenAI-compatible response body.
type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

// config holds the ECHO_* environment knobs.
type config struct {
	addr     string
	mode     string
	text     string
	delay    time.Duration
	failRate float64
}

func loadConfig() config {
	return config{
		addr:     getEnv("ADVISOR_ADDR", ":9090"),
		mode:     getEnv("ECHO_MODE", "noop"),
		text:     getEnv("ECHO_TEXT", "shoulder-daemon advisor: consider double-checking edge cases before proceeding."),
		delay:    time.Duration(getEnvInt("ECHO_DELAY_MS", 0)) * time.Millisecond,
		failRate: getEnvFloat("ECHO_FAIL_RATE", 0),
	}
}

func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getEnvFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// lastUserMessage returns the content of the last message with role "user",
// or "" if there is none.
func lastUserMessage(messages []chatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// truncate cuts s to at most n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// estimateTokensFromChars is a rough, dependency-free stand-in for a real
// tokenizer: ~4 characters per token, minimum 1 for non-empty input.
func estimateTokensFromChars(chars int) int {
	if chars == 0 {
		return 0
	}
	n := chars / 4
	if n < 1 {
		n = 1
	}
	return n
}

// estimateTokens estimates the token count of s.
func estimateTokens(s string) int {
	return estimateTokensFromChars(len(s))
}

// content computes the assistant reply content for the configured mode.
func (c config) content(req chatRequest) string {
	switch c.mode {
	case "fixed":
		return c.text
	case "echo":
		last := lastUserMessage(req.Messages)
		if last == "" {
			return "NOOP"
		}
		return "echo: " + truncate(last, 200)
	default:
		return "NOOP"
	}
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := cryptorand.Read(b); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + fmt.Sprintf("%x", b)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func handleChatCompletions(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"error": map[string]string{"message": "method not allowed"},
			})
			return
		}

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": "invalid request body: " + err.Error()},
			})
			return
		}

		if cfg.delay > 0 {
			time.Sleep(cfg.delay)
		}

		if cfg.failRate > 0 && rand.Float64() < cfg.failRate { //nolint:gosec // G404: fault injection, not a secret
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": map[string]string{"message": "advisor-echo: injected failure (ECHO_FAIL_RATE)"},
			})
			return
		}

		replyContent := cfg.content(req)
		model := req.Model
		if model == "" {
			model = "advisor-echo"
		}

		promptChars := 0
		for _, m := range req.Messages {
			promptChars += len(m.Content)
		}
		promptTokens := estimateTokensFromChars(promptChars)
		completionTokens := estimateTokens(replyContent)

		resp := chatResponse{
			ID:      randomID(),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []chatChoice{
				{
					Index: 0,
					Message: chatMessage{
						Role:    "assistant",
						Content: replyContent,
					},
					FinishReason: "stop",
				},
			},
			Usage: chatUsage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			},
		}

		writeJSON(w, http.StatusOK, resp)
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// runHealthcheck is invoked as `advisor-echo -healthcheck` from the
// container's HEALTHCHECK instruction. The distroless image has no shell,
// wget or curl, so the binary self-checks its own /healthz over loopback and
// exits 0/1 accordingly.
func runHealthcheck(addr string) {
	port := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		port = addr[i+1:]
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/healthz", port)

	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		os.Exit(1)
	}
	healthy := resp.StatusCode == http.StatusOK
	_ = resp.Body.Close()
	if !healthy {
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	cfg := loadConfig()

	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "--healthcheck") {
		runHealthcheck(cfg.addr)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions(cfg))
	mux.HandleFunc("/healthz", handleHealthz)

	log.Printf("advisor-echo listening on %s (mode=%s delay=%s fail_rate=%.2f)",
		cfg.addr, cfg.mode, cfg.delay, cfg.failRate)
	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("advisor-echo: %v", err)
	}
}
