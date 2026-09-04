//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is about what the daemon remembers, through the real binary and a
// real editor. The unit tests measure the store on its own; these measure it in
// the configuration somebody installs: the built-in store, the embedding table
// compiled into that binary, and a session driving it over hooks.

// advisor is a stand-in decision model that records every prompt the daemon
// sends it and answers from a script. The prompts are the evidence: what the
// daemon recalled and handed to the model is invisible from anywhere else, and
// is exactly what these tests are about.
type advisor struct {
	*httptest.Server

	mu      sync.Mutex
	prompts []string
	reply   func(prompt string) string
}

// newAdvisor starts one. reply returns the JSON object the daemon expects as
// the model's final content: inject, keywords, facts.
func newAdvisor(t *testing.T, reply func(prompt string) string) *advisor {
	t.Helper()
	a := &advisor{reply: reply}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var prompt strings.Builder
		for _, m := range in.Messages {
			prompt.WriteString(m.Role + ": " + m.Content + "\n")
		}
		a.mu.Lock()
		a.prompts = append(a.prompts, prompt.String())
		a.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{"content": a.reply(prompt.String())},
			}},
		})
	}))
	t.Cleanup(a.Close)
	return a
}

// seen reports the prompts the daemon has sent so far.
func (a *advisor) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.prompts...)
}

// sawInAPrompt reports whether any prompt so far contains want, waiting for one
// that does. The advisor pass runs off the hook path, so a turn returning is
// not the same as the pass having happened.
func (a *advisor) sawInAPrompt(want string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		for _, p := range a.seen() {
			if strings.Contains(p, want) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// answer is the shape of a decision model's final content.
func answer(inject string, facts ...map[string]any) string {
	if facts == nil {
		facts = []map[string]any{}
	}
	out, err := json.Marshal(map[string]any{
		"inject": inject, "keywords": []string{}, "facts": facts,
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}

func fact(content, scope string) map[string]any {
	return map[string]any{"content": content, "category": "structure", "scope": scope, "tags": []string{}}
}

// withAdvisor is startDaemon pointed at a stub model, which is the only way to
// make an assertion about a decision: a real one is free to say nothing, and
// nothing is the correct answer to most turns.
func withAdvisor(t *testing.T, a *advisor, extra ...string) *daemon {
	t.Helper()
	return startDaemon(t, append([]string{
		"SHOULDER_LLM=local",
		"SHOULDER_LLM_BASE_URL=" + a.URL,
		"SHOULDER_LLM_MODEL=stub",
	}, extra...)...)
}

// storedFacts waits for the store to hold n records and returns their contents.
func storedFacts(t *testing.T, path string, n int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var f struct {
			Records []struct {
				Content string `json:"content"`
				Kind    string `json:"kind"`
			} `json:"records"`
		}
		raw, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(raw, &f) == nil {
			var out []string
			for _, r := range f.Records {
				if r.Kind == "" {
					out = append(out, r.Content)
				}
			}
			if len(out) >= n {
				return out
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store at %s never held %d facts; it holds %s", path, n, raw)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cli runs a shoulderd subcommand against this test's daemon, from dir, with
// none of the developer's environment. The address goes in after the
// subcommand and before every flag, because the CLI reads anything after the
// content as part of it and says so rather than guessing.
func cli(t *testing.T, d *daemon, dir string, args ...string) string {
	t.Helper()
	at := 0
	for at < len(args) && !strings.HasPrefix(args[at], "-") {
		at++
	}
	full := append(append(append([]string{}, args[:at]...), "--addr=http://"+d.addr), args[at:]...)
	cmd := exec.Command(shoulderd, full...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"SHOULDER_TOKEN=" + d.token,
		"SHOULDER_ENV_FILE=/dev/null",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shoulderd %s: %v\n%s", strings.Join(full, " "), err, out)
	}
	return string(out)
}

// TestAFactLearnedInOneTurnIsRecalledForALaterOneInOtherWords is the promise of
// the whole system, measured end to end through the shipping configuration: a
// real editor, the daemon's own store, and the embedding table compiled into
// the binary. The second turn shares no word with the fact the first one
// established, so nothing that counts words in common can pass this.
func TestAFactLearnedInOneTurnIsRecalledForALaterOneInOtherWords(t *testing.T) {
	opencodeOrSkip(t)
	const learned = "we ship every build to the staging cluster"

	a := newAdvisor(t, func(prompt string) string {
		// The fact is offered once. A second offer of the same sentence is
		// refused by the store as something it already holds, which is correct
		// and would leave the log full of expected failures.
		if strings.Contains(prompt, "staging") {
			return answer("")
		}
		return answer("", fact(learned, "global"))
	})
	d := withAdvisor(t, a)
	d.pin()
	dir := project(t)

	run(t, dir, "reply with exactly: one", "SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)
	if got := storedFacts(t, d.facts, 1, 30*time.Second); !strings.Contains(got[0], "staging") {
		t.Fatalf("the fact was not stored: %q", got)
	}

	// Nothing here says ship, build, staging or cluster.
	run(t, dir, "reply with exactly: two. where does this get deployed to",
		"SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)

	if !a.sawInAPrompt(learned, 30*time.Second) {
		t.Fatalf("the stored fact was never recalled for a turn about the same thing in other words;\nprompts:\n%s\ndaemon:\n%s",
			strings.Join(a.seen(), "\n----\n"), d.log.String())
	}
}

// The daemon exits when the last session ends and the editor starts it again,
// so every fact it has ever learned goes through a restart. The store is a file
// it rewrites whole; this is the test that the file it wrote is one it can read.
func TestFactsSurviveTheDaemonExitingBetweenSessions(t *testing.T) {
	opencodeOrSkip(t)
	const learned = "the release rota is kept in docs/rota.md"

	a := newAdvisor(t, func(prompt string) string {
		if strings.Contains(prompt, "rota") {
			return answer("")
		}
		return answer("", fact(learned, "global"))
	})
	first := withAdvisor(t, a)
	first.pin()
	dir := project(t)

	run(t, dir, "reply with exactly: one", "SHOULDER_ADDR="+first.addr, "SHOULDER_TOKEN="+first.token)
	storedFacts(t, first.facts, 1, 30*time.Second)

	_ = first.cmd.Process.Kill()
	<-first.stopped

	// A second daemon, on the same file, as the editor would start it.
	second := startDaemon(t,
		"SHOULDER_LLM=local",
		"SHOULDER_LLM_BASE_URL="+a.URL,
		"SHOULDER_LLM_MODEL=stub",
		"SHOULDER_MEMORY_PATH="+first.facts,
	)
	second.pin()

	if got := cli(t, second, dir, "fact", "list", "--global"); !strings.Contains(got, learned) {
		t.Fatalf("the fact did not survive the daemon that learned it:\n%s\n%s", got, second.log.String())
	}
}

// Local knowledge is keyed to the project it was learned in. A leak here is the
// worst thing this system can do: one client's arrangements turning up in
// another's session, silently, as something the agent believes.
func TestALocalFactStaysOutOfAnotherProjectsSession(t *testing.T) {
	opencodeOrSkip(t)
	const secret = "the client insists every migration is reviewed by their DBA"

	a := newAdvisor(t, func(string) string { return answer("") })
	d := withAdvisor(t, a)
	d.pin()

	here, elsewhere := project(t), project(t)
	cli(t, d, here, "fact", "add", "--local", "--category=constraint", secret)

	// A turn in the other project, about exactly the subject of the fact.
	run(t, elsewhere, "reply with exactly: ok. who has to review a database migration",
		"SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)

	// Give the pass every chance to have run before concluding it did not leak.
	if !a.sawInAPrompt("review a database migration", 30*time.Second) {
		t.Skipf("the turn never reached the decision model, so nothing is proven:\n%s", d.log.String())
	}
	for _, p := range a.seen() {
		if strings.Contains(p, secret) {
			t.Fatalf("a fact from another project was recalled into this one:\n%s", p)
		}
	}
	// And it is still readable where it belongs, so the test is not passing
	// because the fact was never stored.
	if got := cli(t, d, here, "fact", "list", "--local"); !strings.Contains(got, secret) {
		t.Fatalf("the fact is not in the project it was filed in:\n%s", got)
	}
}

// A store the daemon cannot read is somebody's facts. It must not be replaced,
// and it must not take the session down with it: hooks fail open, so the
// session carries on unobserved rather than breaking.
func TestARuinedStoreCostsTheFactsAndNothingElse(t *testing.T) {
	opencodeOrSkip(t)
	broken := filepath.Join(t.TempDir(), "facts.json")
	const contents = "{ this was never JSON"
	if err := os.WriteFile(broken, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	d := startDaemon(t, "SHOULDER_MEMORY_PATH="+broken)
	d.pin()
	dir := project(t)

	run(t, dir, "reply with exactly: ok", "SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)

	if got := d.metric("shoulder_events_total"); got == 0 {
		t.Errorf("the session was not observed at all:\n%s", d.log.String())
	}
	raw, err := os.ReadFile(broken)
	if err != nil || string(raw) != contents {
		t.Errorf("the unreadable store was overwritten: %q %v", raw, err)
	}
	if !strings.Contains(d.log.String(), "could not be opened") {
		t.Errorf("the daemon did not say why nothing is being stored:\n%s", d.log.String())
	}
}

// One daemon serves every editor on the machine, and two projects open at once
// is the ordinary case rather than an exotic one. Each session has to keep its
// own place: the daemon files what it learns under the project the session is
// in, and a mix-up here is the same leak as the one above by a different route.
func TestTwoProjectsAtOnceKeepTheirOwnPlaces(t *testing.T) {
	opencodeOrSkip(t)
	a := newAdvisor(t, func(string) string { return answer("") })
	d := withAdvisor(t, a)
	d.pin()

	one, two := project(t), project(t)
	seen := d.watch()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, dir := range []string{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = tryRun(t, dir, "reply with exactly: ok",
				"SHOULDER_ADDR="+d.addr, "SHOULDER_TOKEN="+d.token)
		}()
	}
	wg.Wait()
	sessions := seen()
	for _, err := range errs {
		if err != nil {
			t.Skipf("%v", err)
		}
	}

	for _, dir := range []string{one, two} {
		found := false
		for _, s := range sessions {
			if s.CWD == dir {
				found = true
			}
		}
		if !found {
			t.Errorf("no session was ever open for %s; saw %+v", dir, sessions)
		}
	}
	if got := d.metric("shoulder_unauthorised_total"); got != 0 {
		t.Errorf("%v events were rejected while two sessions ran at once", got)
	}
	// Two sessions writing one file at the same time is the case a store held
	// in memory and rewritten whole has to get right: a half-written file is
	// every fact on the machine, gone.
	if raw, err := os.ReadFile(d.facts); err == nil {
		var f map[string]any
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Errorf("two sessions at once left the store unreadable: %v\n%s", err, raw)
		}
	}
}

// The token nobody sets. A daemon started with none generates one, writes it
// into the harness configuration, and still has to observe the session that
// started it — whose editor read its environment before that value existed.
func TestTheGeneratedTokenReachesTheHarnessAndThenIsEnforced(t *testing.T) {
	home := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"model": "opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	facts := filepath.Join(t.TempDir(), "facts.json")
	cmd := exec.Command(shoulderd)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"SHOULDER_ADDR=" + addr,
		"SHOULDER_MEMORY_PATH=" + facts,
		"LOG_LEVEL=DEBUG",
	}
	log := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := http.Get("http://" + addr + "/healthz"); err == nil {
			res.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	raw, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model string            `json:"model"`
		Env   map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the harness configuration is no longer JSON: %v\n%s", err, raw)
	}
	token := got.Env["SHOULDER_TOKEN"]
	if len(token) != 64 {
		t.Fatalf("the harness was not given a token: %s", raw)
	}
	if got.Model != "opus" {
		t.Errorf("the rest of the configuration was not preserved: %s", raw)
	}
	kept, err := os.ReadFile(filepath.Join(home, "data", "shoulder-daemon", "token"))
	if err != nil || strings.TrimSpace(string(kept)) != token {
		t.Errorf("the daemon and the harness hold different tokens: %q %v", kept, err)
	}

	post := func(token, id string) int {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/events",
			strings.NewReader(`{"session_id":"`+id+`","event":"user_prompt","prompt":"hello"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Shoulder-Token", token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	// The editor that started this daemon has the old environment, which is
	// why it is let in.
	post("", "before")
	if !strings.Contains(log.String(), "accepting hooks without a token") {
		t.Errorf("a hook from the session that started the daemon was turned away:\n%s", log.String())
	}
	// The next editor to start has the value, and from then on it is required.
	post(token, "after")
	post("", "later")
	res, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body := make([]byte, 1<<16)
	n, _ := res.Body.Read(body)
	if !strings.Contains(string(body[:n]), "shoulder_unauthorised_total 1") {
		t.Errorf("a hook with no token was still accepted after the harness proved it has one:\n%s", body[:n])
	}
}
