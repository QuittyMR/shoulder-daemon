package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = "testdata/session.jsonl"

func TestTurnAssistantTextCollectsWholeTurn(t *testing.T) {
	got, err := TurnAssistantText(fixture, DefaultTail)
	if err != nil {
		t.Fatal(err)
	}
	want := "Looking at the client first.\n\nThe client never notices a dropped connection.\n\nDone."
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "first") && strings.Contains(got, "Answer to the first") {
		t.Fatal("earlier turn leaked in")
	}
}

// A tool result sits in a user entry but is not a prompt; text before it
// belongs to the same turn. A subagent's text is a different conversation.
func TestTurnAssistantTextBoundaries(t *testing.T) {
	got, err := TurnAssistantText(fixture, DefaultTail)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Looking at the client first.") {
		t.Fatalf("text before the tool result dropped: %q", got)
	}
	if strings.Contains(got, "subagent") {
		t.Fatalf("sidechain text included: %q", got)
	}
}

func TestTurnAssistantTextTailDropsPartialLine(t *testing.T) {
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// A tail that starts inside the second prompt's line: the cut line is
	// dropped, so the boundary is lost and the tail yields the rest whole.
	cut := strings.Index(string(raw), "second question") + 3
	got, err := TurnAssistantText(fixture, len(raw)-cut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "Looking at the client first.") || !strings.HasSuffix(got, "Done.") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "Answer to the first") {
		t.Fatalf("text before the tail included: %q", got)
	}
}

func TestTurnAssistantTextEmptyAndMissing(t *testing.T) {
	if _, err := TurnAssistantText(filepath.Join(t.TempDir(), "none.jsonl"), DefaultTail); err == nil {
		t.Fatal("missing file did not error")
	}
	p := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(p, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := TurnAssistantText(p, DefaultTail)
	if err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestPromptRejectsNoise(t *testing.T) {
	for _, line := range []string{
		`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`,
		`{"type":"user","isMeta":true,"message":{"content":"x"}}`,
		`{"type":"user","message":{"content":"[Request interrupted by user]"}}`,
		`{"type":"user","isSidechain":true,"message":{"content":"x"}}`,
	} {
		e, m, ok := Parse([]byte(line))
		if !ok {
			t.Fatalf("parse failed: %s", line)
		}
		if _, isPrompt := Prompt(e, m); isPrompt {
			t.Fatalf("accepted as prompt: %s", line)
		}
	}
	e, m, _ := Parse([]byte(`{"type":"user","message":{"content":"real"}}`))
	if s, ok := Prompt(e, m); !ok || s != "real" {
		t.Fatalf("real prompt rejected: %q %v", s, ok)
	}
}

func TestFlattenReadsAStringOrJoinsTextBlocks(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"empty", ``, ""},
		{"string", `"plain"`, "plain"},
		{"blocks", `[{"type":"text","text":"a"},{"type":"tool_use","name":"Bash"},{"type":"text","text":"b"}]`, "ab"},
		{"neither", `{"stdout":"x"}`, `{"stdout":"x"}`},
	}
	for _, tc := range cases {
		if got := Flatten(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: Flatten(%s) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestBlocksTolerateAContentThatIsNotAList(t *testing.T) {
	if got := Blocks(json.RawMessage(`"just a string"`)); got != nil {
		t.Fatalf("a string content must yield no blocks, got %v", got)
	}
	got := Blocks(json.RawMessage(`[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/x"}}]`))
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
		if !IsNoise(s) {
			t.Errorf("%q should be noise", s)
		}
	}
	for _, s := range []string{"fix the bug", "the main branch is master", "a <system-reminder> mid-sentence"} {
		if IsNoise(s) {
			t.Errorf("%q is a real prompt", s)
		}
	}
}

func TestIsSessionFile(t *testing.T) {
	for _, p := range []string{
		"/home/u/.claude/projects/-home-u-p/11111111-1111-4111-8111-111111111111.jsonl",
		"/root/.claude/projects/x/y.jsonl",
	} {
		if !IsSessionFile(p) {
			t.Errorf("%q rejected", p)
		}
	}
	for _, p := range []string{
		"", "relative/.claude/projects/x.jsonl",
		"/home/u/.claude/projects/../../.ssh/id_ed25519",
		"/home/u/.claude/projects/x/y.jsonl/../../../../etc/passwd",
		"/etc/passwd", "/home/u/.claude/projects/x/y.json",
		"/home/u/.claude/projects/x/y.jsonl/",
	} {
		if IsSessionFile(p) {
			t.Errorf("%q accepted", p)
		}
	}
}
