// Package transcript reads the JSONL session files Claude Code writes beside a
// session. The Stop hook carries only the last text block of a turn; the file
// holds every one, so it is the only place the text an assistant wrote between
// tool calls can be read from.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTail is how much of the end of a transcript is read for one turn. A
// turn rarely runs past a few hundred kilobytes; a file is read whole only
// when it is smaller than this.
const DefaultTail = 4 << 20

// Entry is one line of the file. Sidechain entries belong to subagents and
// are not part of the conversation the user sees.
type Entry struct {
	Type        string          `json:"type"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	Timestamp   string          `json:"timestamp"`
	Message     json.RawMessage `json:"message"`
}

// Message is the API message inside an Entry.
type Message struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
}

// Block is one content block of a Message.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Parse decodes one line. ok is false for blank lines, lines that are not
// entries, and entries without a message.
func Parse(line []byte) (e Entry, m Message, ok bool) {
	if len(bytes.TrimSpace(line)) == 0 {
		return e, m, false
	}
	if json.Unmarshal(line, &e) != nil || len(e.Message) == 0 {
		return e, m, false
	}
	if json.Unmarshal(e.Message, &m) != nil {
		return e, m, false
	}
	return e, m, true
}

// Blocks decodes a content array; nil when the content is a plain string.
func Blocks(raw json.RawMessage) []Block {
	var bs []Block
	if json.Unmarshal(raw, &bs) != nil {
		return nil
	}
	return bs
}

// Flatten reduces a tool_result payload to text.
func Flatten(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []Block
	if json.Unmarshal(raw, &bs) == nil {
		var out strings.Builder
		for _, b := range bs {
			out.WriteString(b.Text)
		}
		return out.String()
	}
	return string(raw)
}

// Prompt returns what the user typed when the entry is a real prompt. Tool
// results and injected context arrive as an array of blocks and are not one;
// neither is meta or harness-generated text.
func Prompt(e Entry, m Message) (string, bool) {
	if e.Type != "user" || e.IsSidechain || e.IsMeta {
		return "", false
	}
	var s string
	if json.Unmarshal(m.Content, &s) != nil || IsNoise(s) {
		return "", false
	}
	return s, true
}

// IsNoise reports text that sits in a user slot but was never typed.
func IsNoise(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	for _, p := range []string{"[Request interrupted", "<system-reminder>", "Base directory for this skill:", "<command-name>", "Caveat: The messages below"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// IsSessionFile reports whether a path is shaped like a Claude Code transcript:
// absolute, under a `.claude/projects` directory, a `.jsonl` file, no `..`.
// The path arrives in a hook payload, and anything that can post a hook can
// name a file; this is the whole of what the daemon will open on its say-so.
func IsSessionFile(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	return strings.Contains(path, "/.claude/projects/") && strings.HasSuffix(path, ".jsonl")
}

// TurnAssistantText returns every text block the assistant wrote since the
// last real prompt, in order, separated by blank lines. Only the last tail
// bytes of the file are read; a turn longer than that yields what fits.
func TurnAssistantText(path string, tail int) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: callers gate on IsSessionFile; the hook names the file
	if err != nil {
		return "", err
	}
	defer f.Close()

	var r io.Reader = f
	if st, err := f.Stat(); err == nil && tail > 0 && st.Size() > int64(tail) {
		if _, err := f.Seek(st.Size()-int64(tail), io.SeekStart); err != nil {
			return "", err
		}
		br := bufio.NewReader(f)
		if _, err := br.ReadBytes('\n'); err != nil {
			return "", nil
		}
		r = br
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(nil, DefaultTail)
	var texts []string
	for sc.Scan() {
		e, m, ok := Parse(sc.Bytes())
		if !ok || e.IsSidechain {
			continue
		}
		if _, isPrompt := Prompt(e, m); isPrompt {
			texts = texts[:0]
			continue
		}
		if e.Type != "assistant" {
			continue
		}
		for _, b := range Blocks(m.Content) {
			if b.Type == "text" {
				if t := strings.TrimSpace(b.Text); t != "" {
					texts = append(texts, t)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return strings.Join(texts, "\n\n"), nil
}
