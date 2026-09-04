//go:build integration

// Package integration drives a real OpenCode against a real shoulderd.
//
// Everything else in this repository tests the daemon against a description of
// what a harness does. These tests exist because that description has been
// wrong: the adapter posted unauthenticated because the editor's environment is
// not the login shell's, and the daemon was never told a session ended because
// OpenCode does not emit the event the adapter was listening for. Neither is
// visible from inside the Go process, and both left a daemon that looked
// healthy while observing nothing.
//
// They cost a real model call each, so they are behind a build tag:
//
//	go test -tags=integration ./integration/...
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// model is free and small. Anything that can follow a one-line instruction will
// do: these tests assert on what the daemon saw, not on what the model said.
// model is the one the editor is driven with. The default is free, which is the
// right default for a suite anybody can run, and free endpoints stop answering
// for hours at a time — every OpenCode test then skips and the suite proves
// nothing. SHOULDER_IT_MODEL points it at one that is answering today.
var model = envOr("SHOULDER_IT_MODEL", "opencode-go/minimax-m3")

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// runFor bounds one OpenCode invocation, and runTries is how many are worth
// making. A hosted model answers in ten to forty seconds when it is answering,
// and simply never when it is not — measured on one endpoint here, every other
// invocation returned in under 40s and the rest had not answered after three
// minutes. Waiting longer does not help and one more attempt usually does, so
// the budget is spent on a second try rather than on a longer first one.
const (
	runFor   = 90 * time.Second
	runTries = 2
)

var (
	shoulderd string
	// editorConfig is a configuration directory of the suite's own for the
	// editor to load. Without it a run loads whatever the person running the
	// suite has installed globally, and their plugins are then part of every
	// test: one here answered a turn with a tool call the provider refused, so
	// the editor exited non-zero and the daemon was blamed for it. Built once,
	// because the first run against an empty one costs a minute.
	editorConfig string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shoulder-it")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	editorConfig = filepath.Join(dir, "config")
	if err := os.MkdirAll(editorConfig, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	shoulderd = filepath.Join(dir, "shoulderd")
	build := exec.Command("go", "build", "-o", shoulderd, "./cmd/shoulderd")
	build.Dir = ".."
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building shoulderd:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// opencodeOrSkip keeps the suite runnable on a machine that does not have the
// editor installed, which is most machines that run the rest of the tests.
func opencodeOrSkip(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Skip("opencode is not on PATH")
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type daemon struct {
	t     *testing.T
	addr  string
	token string
	facts string
	cmd   *exec.Cmd
	log   *strings.Builder
	// stopped is closed, not sent on: both the cleanup and a test waiting for
	// the daemon to exit by itself have to see it, and a value would be taken
	// by whichever got there first and leave the other waiting for ever.
	stopped chan struct{}
}

// startDaemon runs a daemon of this test's own on a port of its own, so a run
// never reads or disturbs whatever the developer has running.
func startDaemon(t *testing.T, extra ...string) *daemon {
	t.Helper()
	d := &daemon{
		t:       t,
		addr:    fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		token:   fmt.Sprintf("it-token-%d", time.Now().UnixNano()),
		log:     &strings.Builder{},
		stopped: make(chan struct{}),
	}

	d.cmd = exec.Command(shoulderd)
	// A clean environment, not the developer's: an inherited SHOULDER_MEMORY_URL
	// would have these tests writing into a real memory store, and the store the
	// daemon keeps itself defaults to one under HOME, which is the same leak by
	// another route. Both are pointed at a directory of this test's own.
	//
	// Not t.TempDir(): the daemon writes its store while it shuts down, and the
	// framework removes that directory the moment the test ends, so the two
	// race and the loser is reported as a failure of whatever test happened to
	// be running. This one is removed after the daemon has actually exited.
	factsDir, err := os.MkdirTemp("", "shoulder-facts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(factsDir) })
	d.facts = filepath.Join(factsDir, "facts.json")
	d.cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"SHOULDER_ADDR=" + d.addr,
		"SHOULDER_TOKEN=" + d.token,
		"SHOULDER_MEMORY_PATH=" + d.facts,
		"LOG_LEVEL=DEBUG",
	}, extra...)
	d.cmd.Stdout = d.log
	d.cmd.Stderr = d.log
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = d.cmd.Wait()
		close(d.stopped)
	}()
	t.Cleanup(func() {
		_ = d.cmd.Process.Kill()
		<-d.stopped
		if t.Failed() {
			t.Logf("daemon log:\n%s", d.log.String())
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := http.Get("http://" + d.addr + "/healthz"); err == nil {
			res.Body.Close()
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon never answered on %s:\n%s", d.addr, d.log.String())
	return nil
}

func (d *daemon) get(path string) string {
	d.t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+d.addr+path, nil)
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("X-Shoulder-Token", d.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		d.t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		d.t.Fatal(err)
	}
	return string(body)
}

type observedSession struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	CWD     string `json:"cwd"`
	Turn    uint64 `json:"turn"`
	Seq     uint64 `json:"seq"`
}

func (d *daemon) sessions() []observedSession {
	d.t.Helper()
	var out []observedSession
	body := d.get("/v1/sessions")
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		d.t.Fatalf("session listing is not JSON: %v\n%s", err, body)
	}
	return out
}

var metricLine = regexp.MustCompile(`^([a-z_]+)(\{[^}]*\})?\s+([0-9.]+)$`)

// metric sums every series of one counter, labels and all.
func (d *daemon) metric(name string) float64 { return d.series(name, "") }

// series sums the series of one metric whose labels contain want. Metrics are
// the only evidence that outlives a session: the listing is the live registry,
// and a session that ended correctly is not in it.
func (d *daemon) series(name, want string) float64 {
	d.t.Helper()
	total := 0.0
	for _, line := range strings.Split(d.get("/metrics"), "\n") {
		m := metricLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || m[1] != name || !strings.Contains(m[2], want) {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			d.t.Fatalf("unparseable metric %q: %v", line, err)
		}
		total += v
	}
	return total
}

// observed reports how many hooks of one kind reached the daemon.
func (d *daemon) observed(kind string) float64 {
	return d.series("shoulder_hook_latency_seconds_count", `event="`+kind+`"`)
}

// watch samples the session listing until stop is called, because a session is
// only in it while it is open. It exists to catch what a session was - its
// harness and its directory - which nothing records once it ends.
func (d *daemon) watch() (stop func() []observedSession) {
	done := make(chan struct{})
	out := make(chan []observedSession, 1)
	go func() {
		byID := map[string]observedSession{}
		for {
			select {
			case <-done:
				seen := make([]observedSession, 0, len(byID))
				for _, s := range byID {
					seen = append(seen, s)
				}
				out <- seen
				return
			default:
			}
			// The daemon stops when its last session ends, so a refused
			// connection here is an outcome rather than a fault.
			func() {
				defer func() { _ = recover() }()
				for _, s := range d.sessions() {
					byID[s.ID] = s
				}
			}()
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return func() []observedSession {
		close(done)
		return <-out
	}
}

// pin holds one session of the test's own open, so the daemon outlives the
// editor and can still be asked what it saw. A daemon whose last session ends
// stops, which is the behaviour under test elsewhere in this file and would
// otherwise make every assertion after a run a connection refused.
//
// It keeps posting, because holding a session open is not the same as opening
// one: a session silent for a minute is evicted when any other session ends,
// and an editor answering a slow model is silent for exactly that long. Without
// the heartbeat this pin holds nothing whenever the model is having a bad day,
// which is when a run takes long enough for it to matter.
func (d *daemon) pin() {
	d.t.Helper()
	d.event(`{"session_id":"integration-pin","event":"session_start","cwd":"/"}`)

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				// A tool result, not a prompt: it keeps the session alive
				// without asking the decision model anything, so a test that
				// reads what the model was sent reads its own turns only.
				d.event(`{"session_id":"integration-pin","event":"tool_result","tool_name":"Read"}`)
			}
		}
	}()
	d.t.Cleanup(func() {
		close(done)
		<-stopped
		d.event(`{"session_id":"integration-pin","event":"session_end","cwd":"/"}`)
	})
}

func (d *daemon) event(body string) {
	d.t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+d.addr+"/v1/events", strings.NewReader(body))
	if err != nil {
		d.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Shoulder-Token", d.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// Only reachable at cleanup, once the daemon has already gone.
		return
	}
	res.Body.Close()
}

// exited reports whether the daemon process has stopped, waiting up to within.
func (d *daemon) exited(within time.Duration) bool {
	select {
	case <-d.stopped:
		return true
	case <-time.After(within):
		return false
	}
}

// project is a git worktree with the adapter installed into it, which is where
// OpenCode looks for a plugin scoped to one project.
func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	plugins := filepath.Join(dir, ".opencode", "plugins")
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := os.ReadFile(filepath.Join("..", "..", "adapters", "opencode", "shoulder-daemon.js"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugins, "shoulder-daemon.js"), adapter, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// run drives one OpenCode invocation to completion, and gives up on the test
// when the model behind the editor will not answer.
func run(t *testing.T, dir, prompt string, env ...string) {
	t.Helper()
	if err := tryRun(t, dir, prompt, env...); err != nil {
		t.Skipf("%v", err)
	}
}

// runContinuing drives a second turn of the session the last run left behind.
// Two invocations are two sessions otherwise, and advice is delivered to the
// session that earned it on its next turn — so a test that wants to see advice
// arrive has to give that session another turn rather than start a new one.
func runContinuing(t *testing.T, dir, prompt string, env ...string) {
	t.Helper()
	var err error
	for range runTries {
		if err = runOnce(t, dir, prompt, []string{"--continue"}, env...); err == nil {
			return
		}
	}
	t.Skipf("%v", err)
}

// tryRun is the same thing for a caller that cannot skip: a skip from a
// goroutine ends that goroutine alone, so a test that ran two editors at once
// and skipped inside them went on to assert on sessions that never happened
// and reported the model's silence as a leak between projects.
//
// env is added to a copy of the caller's, because OpenCode needs its own
// credentials from the real HOME.
func tryRun(t *testing.T, dir, prompt string, env ...string) error {
	t.Helper()
	var err error
	for range runTries {
		if err = runOnce(t, dir, prompt, nil, env...); err == nil {
			return nil
		}
	}
	return err
}

func runOnce(t *testing.T, dir, prompt string, flags []string, env ...string) error {
	t.Helper()
	bin := opencodeOrSkip(t)

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	args := append([]string{"run", "--dir", dir, "-m", model}, flags...)
	cmd := exec.CommandContext(ctx, bin, append(args, prompt)...)
	cmd.Dir = dir
	// The editor keeps the caller's environment, because it needs the real HOME
	// for its own credentials, but not one variable of ours. Whoever is running
	// the suite almost certainly has a token and an address exported for their
	// own daemon, and the adapter prefers its process environment to any file -
	// so an inherited SHOULDER_TOKEN silently overrides what a test is trying
	// to set up and the test reports on the developer's machine instead.
	cmd.Env = append(clean(os.Environ()),
		"SHOULDER_ENV_FILE=/dev/null",
		// After the copy, so it wins: the editor loads the suite's plugins and
		// not the developer's.
		"XDG_CONFIG_HOME="+editorConfig,
	)
	cmd.Env = append(cmd.Env, env...)
	// Killing the editor does not close the pipes its children inherited, so
	// without this a hung model holds CombinedOutput open long past the
	// deadline and takes the whole suite's budget with it.
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("opencode did not finish within %s; the model endpoint is not answering:\n%s", runFor, out)
	}
	if err != nil {
		t.Fatalf("opencode run: %v\n%s", err, out)
	}
	return nil
}

// clean drops every SHOULDER_ variable from an environment.
func clean(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, "SHOULDER_") {
			out = append(out, kv)
		}
	}
	return out
}

// TestOpenCodeSessionIsObserved is the whole point of the adapter: a real
// editor, doing a real turn, showing up in the daemon as the session it is.
func TestOpenCodeSessionIsObserved(t *testing.T) {
	d := startDaemon(t)
	d.pin()
	dir := project(t)

	seen := d.watch()
	run(t, dir, "reply with exactly: ok",
		"SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)
	sessions := seen()

	if got := d.metric("shoulder_unauthorised_total"); got != 0 {
		t.Fatalf("%v events were rejected; the adapter and the daemon disagree about the token", got)
	}
	// One assertion per kind, because each is a separate mapping in the adapter
	// and a missing one is invisible in a total. session_end in particular was
	// mapped to an event OpenCode does not emit for a run.
	for _, kind := range []string{"session_start", "user_prompt", "turn_end", "session_end"} {
		if d.observed(kind) == 0 {
			t.Errorf("no %s hook reached the daemon", kind)
		}
	}

	found := false
	for _, s := range sessions {
		if s.CWD != dir {
			continue
		}
		found = true
		if s.Harness != "opencode" {
			t.Errorf("harness is %q, want opencode", s.Harness)
		}
	}
	if !found {
		t.Fatalf("no session for %s was ever open; saw %+v", dir, sessions)
	}
}

// TestOpenCodeAuthenticatesFromTheEnvFile covers the failure that is invisible
// from both ends: an editor launched from a desktop entry has none of the shell
// exports, so the adapter posts with no token and the daemon drops every event
// while continuing to answer 200 and look healthy.
func TestOpenCodeAuthenticatesFromTheEnvFile(t *testing.T) {
	d := startDaemon(t)
	d.pin()
	dir := project(t)

	envFile := filepath.Join(t.TempDir(), "env")
	body := fmt.Sprintf("SHOULDER_ADDR=%s\nSHOULDER_TOKEN=%s\n", d.addr, d.token)
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Deliberately not in the environment: the file is the only place the
	// adapter can learn where the daemon is and how to authenticate to it.
	run(t, dir, "reply with exactly: ok", "SHOULDER_ENV_FILE="+envFile)

	if got := d.metric("shoulder_unauthorised_total"); got != 0 {
		t.Errorf("%v events rejected: the adapter did not read the token from %s", got, envFile)
	}
	if got := d.metric("shoulder_events_total"); got == 0 {
		t.Fatalf("nothing arrived: the adapter did not read the address from %s", envFile)
	}
}

// TestOpenCodeSessionEndStopsTheDaemon is the lifecycle requirement. OpenCode
// emits session.deleted only for a session that is explicitly discarded, so
// leaving the daemon to that event alone means it is told a session began and
// never that it ended - and it stays up for the rest of the day.
func TestOpenCodeSessionEndStopsTheDaemon(t *testing.T) {
	d := startDaemon(t)
	dir := project(t)

	run(t, dir, "reply with exactly: ok",
		"SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)

	if !d.exited(30 * time.Second) {
		t.Fatalf("the daemon is still running after its only session ended:\n%s", d.log.String())
	}
}

// TestOpenCodeReceivesAdvice checks the return path. The daemon is pointed at a
// stub decision model so the advice is fixed, which is the only way to assert
// on it: a real model is free to say nothing, and saying nothing is the correct
// and most common answer.
func TestOpenCodeReceivesAdvice(t *testing.T) {
	const marker = "SHOULDER-INTEGRATION-MARKER"

	advisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{"inject": marker, "facts": []any{}, "keywords": []any{}})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": string(inner)}}},
		})
	}))
	defer advisor.Close()

	d := startDaemon(t,
		"SHOULDER_LLM=local",
		"SHOULDER_LLM_BASE_URL="+advisor.URL,
		"SHOULDER_LLM_MODEL=stub",
	)
	d.pin()
	dir := project(t)

	// Two turns of one session: the first produces the advice, the second is
	// the one it can be delivered on, because advice is never returned to the
	// turn that caused it. Two separate invocations would be two sessions, and
	// the advice the first earned would have nowhere to go.
	run(t, dir, "reply with exactly: one", "SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)
	runContinuing(t, dir, "reply with exactly: two", "SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)

	if got := d.metric("shoulder_advice_emitted_total"); got == 0 {
		t.Fatalf("the daemon produced advice but never handed it to the adapter:\n%s", d.log.String())
	}
}

// The daemon stops when the last session it knows about ends, which it can do
// under an editor that is still open. Claude Code recovers by running the boot
// script before every prompt; OpenCode has no such hook, so the adapter starts
// one itself the first time a post finds nothing listening.
func TestOpenCodeRevivesADeadDaemon(t *testing.T) {
	opencodeOrSkip(t)
	d := startDaemon(t)
	dir := project(t)

	// Nothing is listening on the address the adapter is about to use.
	_ = d.cmd.Process.Kill()
	<-d.stopped

	started := filepath.Join(t.TempDir(), "started")
	run(t, dir, "reply with exactly: ok",
		"SHOULDER_ADDR="+d.addr,
		"SHOULDER_TOKEN="+d.token,
		"SHOULDER_START_CMD=touch "+started)

	if _, err := os.Stat(started); err != nil {
		t.Fatalf("the adapter left the session unobserved rather than starting a daemon: %v", err)
	}
}
