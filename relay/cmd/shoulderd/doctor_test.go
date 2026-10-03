package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/cliapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/httpapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// relay stands in for a daemon that has seen the given events, refused the
// given number of hooks, holds a store that answers and runs the model the env
// file asks for. doctor reads the first two off the metrics scrape and asks the
// daemon for the rest.
func relay(t *testing.T, healthy bool, seen []string, unauthorised int) *httptest.Server {
	t.Helper()
	srv := relayRunning(t, healthy, seen, unauthorised,
		&cliapi.MemoryStatus{Name: "mcp-memory-service", Configured: true, OK: true},
		&cliapi.ConfigResponse{Snapshot: settings.Snapshot{Provider: "gemini", Model: "gemini-flash-lite-latest"}})
	envFileSays(t, "SHOULDER_LLM=gemini", "SHOULDER_MEMORY_URL=http://127.0.0.1:8100")
	return srv
}

// relayWithMemory is the same stand-in with the store's answer chosen by the
// caller, and an env file that asks for that store. A nil status is a daemon
// too old to know the route.
func relayWithMemory(t *testing.T, healthy bool, seen []string, unauthorised int, mem *cliapi.MemoryStatus) *httptest.Server {
	t.Helper()
	switch {
	case mem == nil:
		envFileSays(t)
	case mem.Name == "mcp-memory-service":
		envFileSays(t, "SHOULDER_MEMORY_URL=http://127.0.0.1:8100")
	default:
		envFileSays(t, "SHOULDER_MEMORY="+mem.Name)
	}
	return relayRunning(t, healthy, seen, unauthorised, mem, nil)
}

// envFileSays points the env file at one holding lines, and clears the process
// environment of everything doctor compares, so that neither the machine the
// tests run on nor the order they run in decides what is expected.
func envFileSays(t *testing.T, lines ...string) string {
	t.Helper()
	for _, k := range []string{"SHOULDER_LLM", "SHOULDER_LLM_MODEL", "SHOULDER_MEMORY", "SHOULDER_MEMORY_URL", "SHOULDER_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHOULDER_ENV_FILE", path)
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)
	return path
}

// relayRunning is the stand-in with every answer chosen by the caller. A nil
// one is a route the daemon does not know.
func relayRunning(t *testing.T, healthy bool, seen []string, unauthorised int,
	mem *cliapi.MemoryStatus, running *cliapi.ConfigResponse,
) *httptest.Server {
	t.Helper()
	var scrape strings.Builder
	for _, e := range seen {
		scrape.WriteString(`shoulder_hook_latency_seconds_count{event="` + e + `"} 1` + "\n")
	}
	if unauthorised > 0 {
		scrape.WriteString("shoulder_unauthorised_total " + strconv.Itoa(unauthorised) + "\n")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			if !healthy {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/metrics":
			_, _ = io.WriteString(w, scrape.String())
		case "/v1/cli/memory":
			if mem == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(mem)
		case "/v1/cli/config":
			if running == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(running)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// doctor prints straight to the process's stdout; capturing it is what the
// tests below do instead of asking the command to grow a second output.
func stdout(t *testing.T, run func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = io.Copy(&buf, r) }()
	run()
	_ = w.Close()
	os.Stdout = old
	wg.Wait()
	return buf.String()
}

// noRelease keeps doctor off the network: the proxy answers nothing useful.
func noRelease(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	old := latestURL
	latestURL = srv.URL
	t.Cleanup(func() { latestURL = old })
}

func TestDoctorSaysWhenNothingIsListening(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := relay(t, true, nil, 0)
	srv.Close()
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "relay unreachable") || !strings.Contains(out, "fail open") {
		t.Fatalf("output must name the problem and say sessions still work:\n%s", out)
	}
}

func TestDoctorIsCleanWhenEveryRoutineEventHasFired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relay(t, true, httpapi.RoutineEvents(), 0)
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{"relay:   ok", "version: ", "hooks:   all expected events have fired"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDoctorNamesTheEventsThatNeverFired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relay(t, true, []string{"UserPromptSubmit"}, 0)
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "NEVER FIRED") || !strings.Contains(out, "Stop") || strings.Contains(out, "[UserPromptSubmit") {
		t.Fatalf("the missing events, and only those, must be listed:\n%s", out)
	}
}

// A rejected hook is observed before it is counted, so it looks like a fired
// one. Without the unauthorised check doctor would call this install healthy.
func TestDoctorSeesARelayRefusingHooks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relay(t, true, httpapi.RoutineEvents(), 4)
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "4 REJECTED") || !strings.Contains(out, "SHOULDER_TOKEN") {
		t.Fatalf("a token mismatch must be named as such:\n%s", out)
	}
}

// A container healthcheck asks whether the process serves, not whether a
// coding session has happened to use it yet.
func TestDoctorLivenessIgnoresHooks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := relay(t, true, nil, 9)
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL, "--liveness"}) })
	if code != 0 {
		t.Fatalf("liveness exit %d, want 0", code)
	}
}

func TestDoctorJSONCarriesEveryFinding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relay(t, true, []string{"Stop"}, 1)
	c := &cli{out: io.Discard, err: io.Discard}
	out := stdout(t, func() { c.dispatch("doctor", []string{"--addr=" + srv.URL, "--json"}) })
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	for _, k := range []string{"relay", "version", "origin", "metrics", "events_never_seen", "unauthorised", "plugin"} {
		if _, ok := v[k]; !ok {
			t.Errorf("JSON lacks %q: %v", k, v)
		}
	}
}

func TestDoctorReportsANewerRelease(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Version": "v99.0.0"})
	}))
	defer proxy.Close()
	old := latestURL
	latestURL = proxy.URL
	defer func() { latestURL = old }()
	oldV := buildVersion
	buildVersion = "v0.1.0"
	defer func() { buildVersion = oldV }()

	srv := relay(t, true, httpapi.RoutineEvents(), 0)
	c := &cli{out: io.Discard, err: io.Discard}
	out := stdout(t, func() { c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if !strings.Contains(out, "update:  v99.0.0 is out") {
		t.Fatalf("a newer release must be announced:\n%s", out)
	}
}

func TestDoctorSaysWhenNoStoreIsConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relayWithMemory(t, true, httpapi.RoutineEvents(), 0,
		&cliapi.MemoryStatus{Name: "none"})
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 1 {
		t.Fatalf("exit %d, want 1: a daemon that stores nothing is not healthy", code)
	}
	if !strings.Contains(out, "memory:  NONE") || !strings.Contains(out, "SHOULDER_MEMORY_URL") {
		t.Fatalf("output must name the missing store and how to give it one:\n%s", out)
	}
}

func TestDoctorSaysWhenTheStoreWillNotAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relayWithMemory(t, true, httpapi.RoutineEvents(), 0,
		&cliapi.MemoryStatus{Name: "mcp-memory-service", Configured: true, Error: "status 401"})
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "memory:  UNREACHABLE") || !strings.Contains(out, "status 401") {
		t.Fatalf("output must carry the backend's own reason:\n%s", out)
	}
}

// A daemon that has never heard of the route is old, not broken, and doctor
// says so without making a missing answer a verdict on the store.
func TestDoctorDoesNotCondemnAStoreItCouldNotAskAbout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	srv := relayWithMemory(t, true, httpapi.RoutineEvents(), 0, nil)
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(out, "memory:  unknown") {
		t.Fatalf("output must say it could not ask:\n%s", out)
	}
}

func TestCounterValueReadsOneCounterOutOfAScrape(t *testing.T) {
	scrape := "# TYPE a counter\na 3\nshoulder_unauthorised_total 12\nbroken x\n"
	cases := map[string]int{"a": 3, "shoulder_unauthorised_total": 12, "broken": 0, "absent": 0}
	for name, want := range cases {
		if got := counterValue(scrape, name); got != want {
			t.Errorf("counterValue(%q) = %d, want %d", name, got, want)
		}
	}
}

func writePlugin(t *testing.T, root, name, hooks string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), []byte(hooks), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStalePluginsJudgesOnlyOursAndOnlyByWhatTheHarnessLoaded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := writePlugin(t, home, "current", `{"url":"http://127.0.0.1:8787/v1/hooks/claude-code/Stop","headers":{"X-Shoulder-Token":"${SHOULDER_TOKEN}"}}`)
	stale := writePlugin(t, home, "stale", `{"url":"http://127.0.0.1:9999/v1/hooks/claude-code/Stop","headers":{"X-Shoulder-Token":"x"}}`)
	noToken := writePlugin(t, home, "notoken", `{"url":"http://127.0.0.1:8787/v1/hooks/claude-code/Stop"}`)
	other := writePlugin(t, home, "other", `{"url":"http://127.0.0.1:8787/somebody/elses/hook"}`)
	gone := filepath.Join(home, "gone")

	reg := map[string]any{"plugins": map[string]any{
		"current@m": []map[string]string{{"installPath": current}},
		"stale@m":   []map[string]string{{"installPath": stale}},
		"notoken@m": []map[string]string{{"installPath": noToken}},
		"other@m":   []map[string]string{{"installPath": other}},
		"gone@m":    []map[string]string{{"installPath": gone}},
	}}
	raw, _ := json.Marshal(reg)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "plugins"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := stalePlugins("http://127.0.0.1:8787")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"notoken@m at " + noToken, "stale@m at " + stale}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("stale = %v, want %v", got, want)
	}
}

func TestStalePluginsWithNoHarnessInstalledIsNotAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := stalePlugins("http://127.0.0.1:8787")
	if err != nil || got != nil {
		t.Fatalf("got %v, %v; a machine without Claude Code has nothing stale", got, err)
	}
}

func TestStalePluginsRefusesACorruptRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "plugins"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stalePlugins("http://127.0.0.1:8787"); err == nil {
		t.Fatal("a registry that cannot be read must be reported, not treated as empty")
	}
}

func TestBuildStringShowsTheCommitOnlyForAnUntaggedBuild(t *testing.T) {
	dev := build{Version: "devel", Origin: "checkout", Go: "go1.26", Platform: "linux/amd64", Commit: "abc123", Modified: true}
	if got := dev.String(); got != "shoulderd devel abc123+dirty (go1.26, linux/amd64, checkout)" {
		t.Fatalf("devel build = %q", got)
	}
	rel := build{Version: "v0.1.0", Origin: "release", Go: "go1.26", Platform: "linux/amd64", Commit: "abc123"}
	if got := rel.String(); got != "shoulderd v0.1.0 (go1.26, linux/amd64, release)" {
		t.Fatalf("release build = %q", got)
	}
}

func TestProviderNameOfNothingIsNone(t *testing.T) {
	if got := providerName(nil); got != "none" {
		t.Fatalf("providerName(nil) = %q", got)
	}
}

// For the docs store doctor says where the global facts are and, from the
// directory it was typed in, whether this checkout holds any yet; and it
// says when the setting lost to a memory service.
func TestDoctorDescribesTheDocsStoreFromWhereItWasTyped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "docs", "DECISIONS.shoulder.md"), []byte("# Decisions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	srv := relayWithMemory(t, true, httpapi.RoutineEvents(), 0,
		&cliapi.MemoryStatus{Name: "docs", Configured: true, OK: true, GlobalDocs: "/srv/facts", DocsDir: "docs"})
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "memory:  ok (docs)") || !strings.Contains(out, "global facts: /srv/facts") ||
		!strings.Contains(out, "holds 1 shoulder file") {
		t.Fatalf("output must name the global directory and this checkout's files:\n%s", out)
	}

	srv = relayWithMemory(t, true, httpapi.RoutineEvents(), 0, &cliapi.MemoryStatus{
		Name: "mcp-memory-service", Configured: true, OK: true,
		Overridden: "SHOULDER_MEMORY=docs is ignored while SHOULDER_MEMORY_URL is set",
	})
	out = stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL, "--json"}) })
	if code != 0 || !strings.Contains(out, `"memory_overridden": "SHOULDER_MEMORY=docs is ignored`) {
		t.Fatalf("exit %d; the JSON must carry the precedence note:\n%s", code, out)
	}
}

// daemonRuns is what the stand-in daemon reports it runs and where each value
// came from. An empty envFile is the file doctor itself reads.
type daemonRuns struct {
	provider, providerSource, model, modelSource string
	store, storeSource, openError, envFile       string
	triage                                       string
}

// doctorAgainst runs doctor with args against a daemon that runs d, with the
// env file holding lines.
func doctorAgainst(t *testing.T, d daemonRuns, args []string, lines ...string) (int, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	noRelease(t)
	path := envFileSays(t, lines...)
	if d.envFile == "" {
		d.envFile = path
	}
	srv := relayRunning(t, true, httpapi.RoutineEvents(), 0,
		&cliapi.MemoryStatus{
			Name: d.store, Configured: d.store != "none", OK: d.store != "none",
			Source: d.storeSource, EnvFile: d.envFile, OpenError: d.openError,
		},
		&cliapi.ConfigResponse{
			Snapshot:       settings.Snapshot{Provider: d.provider, Model: d.model},
			ProviderSource: d.providerSource, ModelSource: d.modelSource, EnvFile: d.envFile,
			Triage: d.triage,
		})
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", append([]string{"--addr=" + srv.URL}, args...)) })
	return code, out
}

// fromFile is a daemon that started on the env file and runs what fileAsks
// asks for.
var fromFile = daemonRuns{
	provider: "gemini", providerSource: config.SourceFile, model: "gemini-flash-lite-latest", modelSource: config.SourceDefault,
	store: "mcp-memory-service", storeSource: config.SourceFile,
}

var fileAsks = []string{"SHOULDER_LLM=gemini", "SHOULDER_MEMORY_URL=http://127.0.0.1:8100"}

func TestDoctorIsCleanWhenTheDaemonRunsWhatTheEnvFileAsksFor(t *testing.T) {
	code, out := doctorAgainst(t, fromFile, nil, fileAsks...)
	if code != 0 || strings.Contains(out, "MISMATCH") || strings.Contains(out, "note:") ||
		!strings.Contains(out, "llm:     gemini (gemini-flash-lite-latest)") {
		t.Fatalf("exit %d, want 0 and the running model named:\n%s", code, out)
	}

	chain := fromFile
	chain.provider = "gemini→openrouter"
	code, out = doctorAgainst(t, chain, nil, "SHOULDER_LLM=Gemini, openrouter", "SHOULDER_MEMORY_URL=http://127.0.0.1:8100")
	if code != 0 || strings.Contains(out, "MISMATCH") {
		t.Fatalf("exit %d; a chain is named the way the daemon names it:\n%s", code, out)
	}
}

// A daemon with no decision model observes and says nothing, and looked
// healthy to every other line of this report for weeks.
func TestDoctorFailsWithoutADecisionModel(t *testing.T) {
	d := fromFile
	d.provider, d.providerSource, d.model = "none", config.SourceDefault, ""
	code, out := doctorAgainst(t, d, nil, "SHOULDER_MEMORY_URL=http://127.0.0.1:8100")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "llm:     NONE") || !strings.Contains(out, "SHOULDER_LLM") || strings.Contains(out, "MISMATCH") {
		t.Fatalf("output must name the missing model, and nothing it disagrees with:\n%s", out)
	}
}

// The incident: the file asks for a model, and the daemon was started from an
// environment that never saw it.
func TestDoctorFailsWhenTheDaemonStartedWithoutTheModelTheFileAsksFor(t *testing.T) {
	d := fromFile
	d.provider, d.providerSource, d.model = "none", config.SourceDefault, ""
	code, out := doctorAgainst(t, d, nil, fileAsks...)
	if code != 1 || !strings.Contains(out, "asks for the model provider gemini; the daemon started with nothing set and runs none") {
		t.Fatalf("exit %d; output must say what the file asks for and that nothing set what runs:\n%s", code, out)
	}
}

func TestDoctorNamesWhichFileAStaleDaemonStartedFrom(t *testing.T) {
	d := fromFile
	d.provider = "openrouter"
	code, out := doctorAgainst(t, d, nil, fileAsks...)
	if code != 1 || !strings.Contains(out, "started from this file, since edited and runs openrouter") ||
		!strings.Contains(out, "restart it") {
		t.Fatalf("exit %d; a file edited since the start must be named as such:\n%s", code, out)
	}

	d.envFile = "/elsewhere/env"
	code, out = doctorAgainst(t, d, nil, fileAsks...)
	if code != 1 || !strings.Contains(out, "started from /elsewhere/env, not this file") {
		t.Fatalf("exit %d; a daemon started on another file must name it:\n%s", code, out)
	}
}

// A value the daemon's own environment or `config set` gave it beat the file
// on purpose, so it is said and not failed.
func TestDoctorOnlyNotesAModelChosenOverTheFile(t *testing.T) {
	d := fromFile
	d.provider, d.providerSource = "openrouter", config.SourceEnvironment
	code, out := doctorAgainst(t, d, nil, fileAsks...)
	if code != 0 || strings.Contains(out, "MISMATCH") || !strings.Contains(out, "note: the model provider openrouter comes from the daemon's process environment") {
		t.Fatalf("exit %d, want 0 and a note naming the environment:\n%s", code, out)
	}

	d.providerSource = cliapi.SourceConfigSet
	code, out = doctorAgainst(t, d, nil, fileAsks...)
	if code != 0 || !strings.Contains(out, "was set with `config set`") || !strings.Contains(out, "a restart returns to") {
		t.Fatalf("exit %d, want 0 and a note naming config set:\n%s", code, out)
	}
}

// The model is compared only where the file names one; otherwise the daemon
// runs the preset's default and there is nothing to disagree with.
func TestDoctorComparesTheModelOnlyWhereTheFileNamesOne(t *testing.T) {
	code, out := doctorAgainst(t, fromFile, nil, append(fileAsks, "SHOULDER_LLM_MODEL=gemini-2.5-flash")...)
	if code != 1 || !strings.Contains(out, "asks for the model gemini-2.5-flash; the daemon started with nothing set and runs gemini-flash-lite-latest") {
		t.Fatalf("exit %d; a model other than the file's must fail:\n%s", code, out)
	}

	d := fromFile
	d.model, d.modelSource = "gemini-2.5-flash", config.SourceFile
	code, out = doctorAgainst(t, d, nil, append(fileAsks, "SHOULDER_LLM_MODEL=gemini-2.5-flash")...)
	if code != 0 || strings.Contains(out, "MISMATCH") {
		t.Fatalf("exit %d; the model the file names is the clean case:\n%s", code, out)
	}
}

func TestDoctorFailsWhenTheDaemonKeepsAnotherStoreThanTheEnvFileAsksFor(t *testing.T) {
	d := fromFile
	d.store, d.storeSource = "local", config.SourceDefault
	code, out := doctorAgainst(t, d, nil, fileAsks...)
	if code != 1 || !strings.Contains(out, "asks for the store mcp-memory-service; the daemon started with nothing set and runs local") {
		t.Fatalf("exit %d; output must say which store the file asks for and which runs:\n%s", code, out)
	}

	d.store, d.storeSource = "mcp-memory-service", config.SourceFile
	code, out = doctorAgainst(t, d, nil, "SHOULDER_LLM=gemini", "SHOULDER_MEMORY=docs")
	if code != 1 || !strings.Contains(out, "asks for the store docs;") {
		t.Fatalf("exit %d; a service the file no longer names is stale too:\n%s", code, out)
	}

	d.storeSource = config.SourceEnvironment
	code, out = doctorAgainst(t, d, nil, "SHOULDER_LLM=gemini")
	if code != 0 || !strings.Contains(out, "note: the store mcp-memory-service comes from the daemon's process environment") {
		t.Fatalf("exit %d; a store from the environment is a note:\n%s", code, out)
	}
}

// A store that could not be opened is not the wrong store: the file may be
// right, and sending somebody to edit it would be sending them the wrong way.
func TestDoctorSaysWhenTheStoreFailedToOpen(t *testing.T) {
	d := fromFile
	d.store, d.storeSource, d.openError = "none", config.SourceDefault, "the local store at /x/facts.json: permission denied"
	code, out := doctorAgainst(t, d, nil, "SHOULDER_LLM=gemini")
	if code != 1 || !strings.Contains(out, "FAILED TO OPEN: the local store at /x/facts.json: permission denied") ||
		strings.Contains(out, "MISMATCH") {
		t.Fatalf("exit %d; output must carry the reason and no mismatch:\n%s", code, out)
	}
}

func TestDoctorJSONCarriesTheSourcesAndTheMismatch(t *testing.T) {
	d := fromFile
	d.provider, d.providerSource, d.model = "none", config.SourceDefault, ""
	d.store, d.storeSource = "local", config.SourceDefault
	code, out := doctorAgainst(t, d, []string{"--json"}, fileAsks...)
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if code != 1 || v["llm"] != "none" || v["llm_source"] != config.SourceDefault || v["memory_source"] != config.SourceDefault {
		t.Fatalf("exit %d; JSON must carry what runs and where it came from: %v", code, v)
	}
	for _, k := range []string{"llm_mismatch", "memory_mismatch"} {
		if msg, _ := v[k].(string); !strings.HasPrefix(msg, "MISMATCH: ") {
			t.Errorf("JSON %s = %v", k, v[k])
		}
	}
}

// The healthcheck must not make a container unhealthy over its configuration:
// restarting it would not change the environment it was created with.
func TestDoctorLivenessIgnoresAMismatch(t *testing.T) {
	d := fromFile
	d.provider, d.providerSource = "none", config.SourceDefault
	code, _ := doctorAgainst(t, d, []string{"--liveness"}, fileAsks...)
	if code != 0 {
		t.Fatalf("liveness exit %d, want 0", code)
	}
}

// Triage with no decision model is a supported way to run, not a missing one.
func TestDoctorAcceptsATriageWithoutADecisionModel(t *testing.T) {
	d := fromFile
	d.provider, d.providerSource, d.model, d.triage = "none", config.SourceDefault, "", "jev"
	code, out := doctorAgainst(t, d, nil, "SHOULDER_MEMORY_URL=http://127.0.0.1:8100", "SHOULDER_TRIAGE=jev")
	if code != 0 || !strings.Contains(out, "llm:     none (triage only: jev)") || strings.Contains(out, "NONE") {
		t.Fatalf("exit %d; a triage-only daemon is healthy:\n%s", code, out)
	}
}

// The start command runs where nobody reads it, so a relay it failed to start
// has to show its last words here.
func TestDoctorShowsWhatTheStartCommandSaidWhenNothingListens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := relay(t, true, nil, 0)
	srv.Close()
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "shoulder-daemon"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := "one\ntwo\nthree\nfour\nfive\nsix\nError: no such image\n"
	if err := os.WriteFile(filepath.Join(dir, "shoulder-daemon", "up.log"), []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &cli{out: io.Discard, err: io.Discard}
	var code int
	out := stdout(t, func() { code = c.dispatch("doctor", []string{"--addr=" + srv.URL}) })
	if code != 1 || !strings.Contains(out, "Error: no such image") || strings.Contains(out, "one\n") {
		t.Fatalf("exit %d; the last lines of the start command's output must be shown:\n%s", code, out)
	}
}
