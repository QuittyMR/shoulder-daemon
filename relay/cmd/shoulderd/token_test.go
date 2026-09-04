package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

// quiet keeps the token machinery from narrating into the test output, and
// isolates every path it writes to from the machine running the tests.
func quiet(t *testing.T) *slog.Logger {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("SHOULDER_TOKEN", "")
	t.Setenv("SHOULDER_ENV_FILE", "")
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Nobody types this value, so the daemon has to invent it. Without this an
// install that is not a git checkout runs with no authentication at all.
func TestTokenIsGeneratedAndKept(t *testing.T) {
	log := quiet(t)
	first, generated := ensureToken(log)
	if !generated || first == "" {
		t.Fatalf("no token was generated: %q", first)
	}
	if len(first) != 2*tokenBytes {
		t.Errorf("token is %d characters, want %d", len(first), 2*tokenBytes)
	}

	// The daemon exits when the last session ends and starts again for the
	// next one; a token that changed each time would reject every hook.
	second, _ := ensureToken(log)
	if second != first {
		t.Errorf("a restart produced a different token: %q then %q", first, second)
	}

	info, err := os.Stat(tokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %o, want 600", perm)
	}
}

func TestAnOperatorsTokenIsLeftAlone(t *testing.T) {
	log := quiet(t)
	t.Setenv("SHOULDER_TOKEN", "chosen-by-hand")
	tok, generated := ensureToken(log)
	if tok != "chosen-by-hand" {
		t.Errorf("got %q, want the value from the environment", tok)
	}
	if generated {
		t.Error("a token from the environment is not one the daemon invented")
	}
	if _, err := os.Stat(tokenPath()); err == nil {
		t.Error("the daemon wrote a token file over an operator's own setting")
	}
}

// The point of the whole exercise: the harness ends up holding the value
// without anybody being asked to do anything.
func TestTheHarnessConfigurationLearnsTheToken(t *testing.T) {
	log := quiet(t)
	settings := filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil { //nolint:gosec // G703: a path this test built from t.TempDir
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"model": "opus", "env": {"FOO": "bar"}}`), 0o600); err != nil { //nolint:gosec // G703: a path this test built from t.TempDir
		t.Fatal(err)
	}

	tok, _ := ensureToken(log)

	raw, err := os.ReadFile(settings) //nolint:gosec // G703: a path this test built from t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model string            `json:"model"`
		Env   map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the settings file is no longer valid JSON: %v\n%s", err, raw)
	}
	if got.Env["SHOULDER_TOKEN"] != tok {
		t.Errorf("the harness holds %q, the daemon holds %q", got.Env["SHOULDER_TOKEN"], tok)
	}
	if got.Model != "opus" || got.Env["FOO"] != "bar" {
		t.Errorf("somebody else's settings were changed: %s", raw)
	}
}

// The file belongs to the person using it. A one-value edit that reorders their
// keys reads as the daemon having rewritten everything.
func TestTheHarnessConfigurationKeepsItsOwnShape(t *testing.T) {
	quiet(t)
	settings := filepath.Join(t.TempDir(), "settings.json")
	const before = `{
  "zeta": 1,
  "alpha": {
    "nested": [1, 2, 3]
  },
  "env": {
    "ZZZ": "last",
    "AAA": "first"
  }
}`
	if err := os.WriteFile(settings, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncClaudeSettings(settings, "abc123"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if strings.Index(got, `"zeta"`) > strings.Index(got, `"alpha"`) {
		t.Errorf("the top-level keys were reordered:\n%s", got)
	}
	if strings.Index(got, `"ZZZ"`) > strings.Index(got, `"AAA"`) {
		t.Errorf("the env keys were reordered:\n%s", got)
	}
	if !strings.Contains(got, `"SHOULDER_TOKEN": "abc123"`) {
		t.Errorf("the token is not in the file:\n%s", got)
	}
	var check map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("the result is not valid JSON: %v\n%s", err, got)
	}
}

func TestTheHarnessConfigurationGrowsAnEnvBlockWhenItHasNone(t *testing.T) {
	quiet(t)
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{"model": "opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncClaudeSettings(settings, "abc123"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, raw)
	}
	if got.Env["SHOULDER_TOKEN"] != "abc123" {
		t.Errorf("the token is missing: %s", raw)
	}
}

// A settings file nobody can parse is a settings file this daemon must not
// rewrite: the alternative is replacing it with two keys of its own.
func TestABrokenHarnessConfigurationIsLeftUntouched(t *testing.T) {
	quiet(t)
	settings := filepath.Join(t.TempDir(), "settings.json")
	const broken = "{ this was never JSON"
	if err := os.WriteFile(settings, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncClaudeSettings(settings, "abc123"); err == nil {
		t.Fatal("an unparseable settings file must be an error")
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != broken {
		t.Errorf("the file was changed:\n%s", raw)
	}
}

func TestTheEnvFileLearnsTheTokenWithoutLosingWhatWasThere(t *testing.T) {
	quiet(t)
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("SHOULDER_LLM=\"gemini\"\nSHOULDER_TOKEN=\"old\"\nGEMINI_API_KEY=\"secret\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncEnvFile(path, "new"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, `SHOULDER_TOKEN="new"`) || strings.Contains(got, `"old"`) {
		t.Errorf("the token was not replaced:\n%s", got)
	}
	for _, want := range []string{`SHOULDER_LLM="gemini"`, `GEMINI_API_KEY="secret"`} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was lost:\n%s", want, got)
		}
	}
}

// A terminal that has sourced nothing still has to be able to talk to the
// daemon it can see running.
func TestTheCLIFindsTheGeneratedToken(t *testing.T) {
	log := quiet(t)
	tok, _ := ensureToken(log)
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)
	if got := setting("SHOULDER_TOKEN"); got != tok {
		t.Errorf("the CLI resolved %q, the daemon generated %q", got, tok)
	}
}

// The checkout installer writes a token to the env file before any daemon has
// run. Generating a second one there would have the two overwrite each other's
// value on every start.
func TestAnExistingEnvFileTokenIsAdoptedRatherThanReplaced(t *testing.T) {
	log := quiet(t)
	envPath := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envPath, []byte("SHOULDER_TOKEN=\"from-the-installer\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHOULDER_ENV_FILE", envPath)
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)

	tok, generated := ensureToken(log)
	if tok != "from-the-installer" {
		t.Errorf("got %q, want the value already in the env file", tok)
	}
	if !generated {
		t.Error("a token the daemon owns is one it keeps in step, however it first arrived")
	}
	raw, err := os.ReadFile(tokenPath())
	if err != nil {
		t.Fatalf("the adopted token was not kept: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "from-the-installer" {
		t.Errorf("the token file holds %q", strings.TrimSpace(string(raw)))
	}
}
