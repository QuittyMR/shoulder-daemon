// Command replay feeds a recorded Claude Code transcript through a running
// shoulder-daemon relay, turn by turn, as if the session were happening live.
//
// It exists to answer "what would shoulder-daemon have remembered from this
// work?" without re-running the work.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/transcript"
)

func main() {
	var (
		path      = flag.String("transcript", "", "path to a Claude Code transcript .jsonl")
		relay     = flag.String("relay", "http://127.0.0.1:8787", "relay base URL")
		sessionID = flag.String("session", "", "session id to replay under (default: derived from the file name)")
		settle    = flag.Duration("settle", 6*time.Second, "pause after each turn so the decision pass can finish")
		maxTurns  = flag.Int("max-turns", 0, "stop after N turns (0 = all)")
		verbose   = flag.Bool("v", false, "print every event")
	)
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "replay: -transcript is required")
		os.Exit(2)
	}
	if *sessionID == "" {
		base := *path
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		*sessionID = "replay-" + strings.TrimSuffix(base, ".jsonl")
	}

	raw, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}

	c := &client{base: strings.TrimRight(*relay, "/"), token: os.Getenv("SHOULDER_TOKEN"), sessionID: *sessionID, verbose: *verbose}

	toolNames := map[string]string{}
	var pendingText strings.Builder
	turns, prompts, tools, injections := 0, 0, 0, 0

	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		e, m, ok := transcript.Parse([]byte(line))
		if !ok || e.IsSidechain {
			continue
		}
		ts := parseTS(e.Timestamp)

		switch e.Type {
		case "user":
			// A real prompt has content as a plain string; tool results and
			// injected context arrive as an array of blocks.
			if s, ok := transcript.Prompt(e, m); ok {
				prompts++
				c.send(session.Event{TS: ts, Kind: session.KindUserPrompt, Prompt: s})
				continue
			}
			if _, isString := prompt(m); isString {
				continue
			}
			for _, b := range transcript.Blocks(m.Content) {
				if b.Type != "tool_result" {
					continue
				}
				kind := session.KindToolResult
				if b.IsError {
					kind = session.KindToolFailure
				}
				c.send(session.Event{
					TS: ts, Kind: kind, ToolUseID: b.ToolUseID,
					ToolName: toolNames[b.ToolUseID], ToolResult: transcript.Flatten(b.Content),
				})
			}

		case "assistant":
			for _, b := range transcript.Blocks(m.Content) {
				switch b.Type {
				case "text":
					pendingText.WriteString(b.Text)
				case "tool_use":
					tools++
					toolNames[b.ID] = b.Name
					c.send(session.Event{TS: ts, Kind: session.KindToolCall, ToolName: b.Name, ToolUseID: b.ID, ToolInput: b.Input})
				}
			}
			if m.StopReason == "end_turn" || m.StopReason == "stop_sequence" {
				turns++
				adv := c.send(session.Event{TS: ts, Kind: session.KindTurnEnd, Assistant: strings.TrimSpace(pendingText.String())})
				pendingText.Reset()
				if adv != "" {
					injections++
					fmt.Printf("  turn %d injected: %s\n", turns, adv)
				}
				fmt.Printf("turn %d (%d prompts, %d tool calls so far)\n", turns, prompts, tools)
				time.Sleep(*settle)
				if *maxTurns > 0 && turns >= *maxTurns {
					fmt.Printf("stopping at %d turns\n", turns)
					goto done
				}
			}
		}
	}
done:
	fmt.Printf("\nreplayed %d turns, %d prompts, %d tool calls, %d injections\n", turns, prompts, tools, injections)
}

type client struct {
	base      string
	token     string
	sessionID string
	verbose   bool
	http      http.Client
}

// send posts one event and returns any advice the relay handed back.
func (c *client) send(e session.Event) string {
	e.Protocol = 1
	e.Harness = "claude-code-replay"
	e.SessionID = c.sessionID
	body, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("X-Shoulder-Token", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "  send failed:", err)
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if c.verbose {
		fmt.Printf("  -> %s %s\n", e.Kind, textutil.Clip(strings.ReplaceAll(string(raw), "\n", " "), 120))
	}
	var out struct {
		Advice *struct {
			Text string `json:"text"`
		} `json:"advice"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Advice != nil {
		return out.Advice.Text
	}
	return ""
}

func parseTS(s string) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Now()
}

// prompt reports whether the content is a plain string, which a user entry
// has only when it was typed; anything typed but unusable is skipped, not
// scanned for blocks.
func prompt(m transcript.Message) (string, bool) {
	var s string
	if json.Unmarshal(m.Content, &s) != nil {
		return "", false
	}
	return s, true
}
