package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

// The shell scripts write the env file through this command rather than with
// sed, so a value goes in once, comes back as itself, and leaves the rest of
// the file alone.
func TestEnvSetsAndReadsTheFileThroughItsGrammar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("# mine\nexport KEEP=\"x # y\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHOULDER_ENV_FILE", path)
	run := func(in string, args ...string) (int, string) {
		t.Helper()
		config.ResetEnvFile()
		t.Cleanup(config.ResetEnvFile)
		old := stdin
		stdin = strings.NewReader(in)
		t.Cleanup(func() { stdin = old })
		var out bytes.Buffer
		c := &cli{out: &out, err: &out}
		return c.dispatch("env", args), out.String()
	}

	if code, out := run("", "path"); code != 0 || strings.TrimSpace(out) != path {
		t.Fatalf("path: %d %q", code, out)
	}
	if code, _ := run("", "set", "SHOULDER_LLM", "gemini"); code != 0 {
		t.Fatalf("set: %d", code)
	}
	if code, out := run("it's # \\secret\n", "set", "GEMINI_API_KEY"); code != 0 || out != "" {
		t.Fatalf("set from stdin: %d %q; nothing may be printed", code, out)
	}
	for name, want := range map[string]string{"SHOULDER_LLM": "gemini", "GEMINI_API_KEY": `it's # \secret`, "KEEP": "x # y"} {
		if code, out := run("", "get", name); code != 0 || out != want+"\n" {
			t.Errorf("get %s: %d %q, want %q", name, code, out, want)
		}
	}
	if code, _ := run("", "unset", "SHOULDER_LLM"); code != 0 {
		t.Fatalf("unset: %d", code)
	}
	if code, out := run("", "get", "SHOULDER_LLM"); code != 0 || out != "" {
		t.Fatalf("get after unset: %d %q", code, out)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# mine\nexport KEEP=\"x # y\"\n") {
		t.Fatalf("the rest of the file changed:\n%s", raw)
	}
	if code, _ := run("", "set", "X", "${HOME}"); code != 1 {
		t.Fatalf("a value the grammar would expand was written: %d", code)
	}
	if code, _ := run("", "frobnicate"); code != 2 {
		t.Fatalf("unknown verb: %d", code)
	}
}
