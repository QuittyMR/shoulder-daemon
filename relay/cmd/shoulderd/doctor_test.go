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

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/httpapi"
)

// relay stands in for a daemon that has seen the given events and turned away
// the given number of hooks. doctor reads both off the metrics scrape.
func relay(t *testing.T, healthy bool, seen []string, unauthorised int) *httptest.Server {
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
func TestDoctorSeesARelayTurningHooksAway(t *testing.T) {
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
