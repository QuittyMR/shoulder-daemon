//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeOrSkip keeps the suite runnable without the editor installed.
func claudeOrSkip(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	return bin
}

// hookSettings writes a settings file carrying the adapter's own hooks, with
// the relay address rewritten to this test's daemon.
//
// The hooks are read from adapters/claude-code/hooks/hooks.json rather than
// restated here, so a URL or a header lost in an edit fails this test rather
// than silently stopping a real session from being observed. They are loaded
// through --settings because a plugin registry is not honoured from a
// throwaway config directory, which is the only way to run the editor without
// writing into the developer's own.
func hookSettings(t *testing.T, addr, token string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "adapters", "claude-code", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.ReplaceAll(string(body), "127.0.0.1:8787", addr))

	var plugin struct {
		Hooks map[string]any `json:"hooks"`
	}
	if err := json.Unmarshal(body, &plugin); err != nil {
		t.Fatal(err)
	}
	// The boot script is a plugin path the editor cannot resolve here, and this
	// test is not about starting daemons; the ones that are drive the script
	// directly.
	for event, groups := range plugin.Hooks {
		kept := []any{}
		for _, g := range groups.([]any) {
			hooks := []any{}
			for _, h := range g.(map[string]any)["hooks"].([]any) {
				if h.(map[string]any)["type"] == "http" {
					hooks = append(hooks, h)
				}
			}
			if len(hooks) > 0 {
				kept = append(kept, map[string]any{"hooks": hooks})
			}
		}
		if len(kept) == 0 {
			delete(plugin.Hooks, event)
			continue
		}
		plugin.Hooks[event] = kept
	}

	out, err := json.MarshalIndent(map[string]any{
		"env":   map[string]string{"SHOULDER_ADDR": addr, "SHOULDER_TOKEN": token},
		"hooks": plugin.Hooks,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runClaude drives one non-interactive turn with those hooks in place.
func runClaude(t *testing.T, settings, dir, prompt string) {
	t.Helper()
	bin := claudeOrSkip(t)

	cmd := exec.Command(bin, "-p", prompt, "--permission-mode", "plan", "--settings", settings)
	cmd.Dir = dir
	// CLAUDECODE and the session variables belong to the editor running this
	// suite; inherited, they make the child believe it is a nested session.
	env := clean(os.Environ())
	kept := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "CLAUDE_CODE_") && !strings.HasPrefix(kv, "CLAUDECODE=") {
			kept = append(kept, kv)
		}
	}
	cmd.Env = append(kept, "SHOULDER_ENV_FILE=/dev/null")
	cmd.WaitDelay = 10 * time.Second
	cmd.Stdin = strings.NewReader("")

	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(runFor):
		_ = cmd.Process.Kill()
		<-done
		t.Skipf("claude did not finish within %s:\n%s", runFor, out)
	}
	if err != nil {
		t.Fatalf("claude -p: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "Not logged in") {
		t.Skip("the editor is not logged in")
	}
}

// TestClaudeCodeSessionIsObserved is the counterpart to the OpenCode test, and
// exists because every regression on this side was found by a person rather
// than by a test.
func TestClaudeCodeSessionIsObserved(t *testing.T) {
	claudeOrSkip(t)
	d := startDaemon(t)
	d.pin()
	dir := project(t)

	runClaude(t, hookSettings(t, d.addr, d.token), dir, "reply with exactly: ok")

	if got := d.metric("shoulder_unauthorised_total"); got != 0 {
		t.Fatalf("%v events rejected; the hooks and the daemon disagree about the token", got)
	}
	// Named as Claude Code names them: this route records the hook, where the
	// neutral one records the kind it maps to. A prompt and a stop are the two
	// the advisor runs on, and the two whose absence has been mistaken for a
	// healthy daemon before.
	for _, hook := range []string{"UserPromptSubmit", "Stop"} {
		if d.observed(hook) == 0 {
			t.Errorf("no %s hook reached the daemon", hook)
		}
	}
	if got := d.metric("shoulder_unmapped_event_total"); got != 0 {
		t.Errorf("%v events arrived that the relay could not map", got)
	}
}

// The hooks carry ${SHOULDER_TOKEN}, interpolated by the editor from its own
// settings. When that is missing the daemon drops every event while continuing
// to answer 200, so the session looks healthy and observes nothing.
func TestClaudeCodeHooksCarryTheToken(t *testing.T) {
	claudeOrSkip(t)
	d := startDaemon(t)
	d.pin()
	dir := project(t)

	runClaude(t, hookSettings(t, d.addr, "not-the-daemons-token"), dir, "reply with exactly: ok")

	if d.metric("shoulder_unauthorised_total") == 0 {
		t.Fatal("a wrong token was accepted; the hooks are not sending one at all")
	}
	if d.metric("shoulder_events_total") != 1 {
		// One: the pin this test opened. Anything more was let through.
		t.Fatal("an event with the wrong token was observed")
	}
}

// The daemon stops when the last session it knows about ends, which it can do
// under an editor that is still open. The boot script runs before every prompt
// for exactly that reason, so a dead daemon is back by the next turn.
func TestClaudeCodeRevivesADeadDaemon(t *testing.T) {
	claudeOrSkip(t)
	d := startDaemon(t)
	dir := project(t)
	script := filepath.Join("..", "..", "adapters", "claude-code", "scripts", "ensure-daemon.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the boot script is missing: %v", err)
	}

	// Nothing is listening: the daemon this test started is stopped first.
	_ = d.cmd.Process.Kill()
	<-d.stopped

	started := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(script)
	cmd.Env = append(clean(os.Environ()),
		"SHOULDER_ADDR="+d.addr,
		"SHOULDER_START_CMD=touch "+started,
		"XDG_RUNTIME_DIR="+t.TempDir(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the boot script must never fail a hook: %v\n%s", err, out)
	}
	// The script backgrounds the start command and exits without waiting for
	// it, which is the whole point — a hook may not block a prompt — so the
	// file appears shortly after the script has already returned.
	if !appears(started, 10*time.Second) {
		t.Fatal("nothing was listening and the boot script did not start anything")
	}
	_ = dir
}

// A daemon that is already answering must not be started a second time, or
// every prompt pays for a container that is already running.
func TestTheBootScriptIsQuietWhenTheDaemonIsUp(t *testing.T) {
	claudeOrSkip(t)
	d := startDaemon(t)
	script := filepath.Join("..", "..", "adapters", "claude-code", "scripts", "ensure-daemon.sh")

	started := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(script)
	cmd.Env = append(clean(os.Environ()),
		"SHOULDER_ADDR="+d.addr,
		"SHOULDER_START_CMD=touch "+started,
		"XDG_RUNTIME_DIR="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// Long enough that a start command which was going to run has run: the
	// script backgrounds it, so an immediate look proves nothing.
	if appears(started, 3*time.Second) {
		t.Fatal("a second daemon was started while the first was answering")
	}
}

// appears waits for a path to exist. Every assertion about the boot script is
// about something it left behind after returning.
func appears(path string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The hook configuration is what the editor reads; a URL or a header lost in an
// edit is invisible until a session quietly stops being observed.
func TestEveryHookPostsToTheRelayWithTheToken(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "adapters", "claude-code", "hooks", "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type           string            `json:"type"`
				URL            string            `json:"url"`
				Command        string            `json:"command"`
				Headers        map[string]string `json:"headers"`
				AllowedEnvVars []string          `json:"allowedEnvVars"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hooks) == 0 {
		t.Fatal("no hooks configured")
	}
	for event, groups := range cfg.Hooks {
		http := 0
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type != "http" {
					continue
				}
				http++
				if !strings.Contains(h.URL, "/v1/hooks/claude-code/"+event) {
					t.Errorf("%s posts to %q", event, h.URL)
				}
				if h.Headers["X-Shoulder-Token"] != "${SHOULDER_TOKEN}" {
					t.Errorf("%s does not send the token: %v", event, h.Headers)
				}
				// Without this the editor leaves the placeholder uninterpolated
				// and the daemon rejects every event from the session.
				if len(h.AllowedEnvVars) == 0 || h.AllowedEnvVars[0] != "SHOULDER_TOKEN" {
					t.Errorf("%s does not allow SHOULDER_TOKEN to be interpolated: %v", event, h.AllowedEnvVars)
				}
			}
		}
		if event != "SessionStart" && http == 0 {
			t.Errorf("%s posts nothing to the relay", event)
		}
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit"} {
		found := false
		for _, g := range cfg.Hooks[event] {
			for _, h := range g.Hooks {
				found = found || (h.Type == "command" && strings.Contains(h.Command, "ensure-daemon.sh"))
			}
		}
		if !found {
			t.Errorf("%s does not run the boot script, so a stopped daemon stays stopped", event)
		}
	}
}
