package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// request is what the daemon last received, copied out under the lock: the
// handler runs on the test server's goroutine, not the test's.
type request struct {
	method string
	path   string
	query  url.Values
	body   map[string]any
	token  string
	calls  int
}

// daemon stands in for a running shoulderd. Every test here is about what the
// command line sends and what it prints, so nothing behind the socket is real.
type daemon struct {
	*httptest.Server

	mu   sync.Mutex
	last request

	status int
	reply  string
}

func newDaemon(t *testing.T, reply string) *daemon {
	t.Helper()
	d := &daemon{status: http.StatusOK, reply: reply}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body %q is not JSON: %v", raw, err)
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		d.last = request{
			method: r.Method, path: r.URL.Path, query: r.URL.Query(),
			body: body, token: r.Header.Get("X-Shoulder-Token"), calls: d.last.calls + 1,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(d.status)
		_, _ = io.WriteString(w, d.reply)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *daemon) req() request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

func (d *daemon) field(t *testing.T, name string) string {
	t.Helper()
	v, ok := d.req().body[name]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("field %q is %T, not a string", name, v)
	}
	return s
}

// run drives one command line the way main does, with the streams captured.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	t.Setenv("SHOULDER_TOKEN", "")
	var out, errs bytes.Buffer
	c := &cli{out: &out, err: &errs}
	code = c.dispatch(args[0], args[1:])
	return code, out.String(), errs.String()
}

// withAddr puts --addr where a flag belongs, before the text. A flag typed
// after the text is refused now, so a test may not lean on it being swallowed.
func withAddr(args []string, url string) []string {
	at := 1
	if args[0] == "fact" {
		at = 2
	}
	out := append([]string{}, args[:at]...)
	out = append(out, "--addr", url)
	return append(out, args[at:]...)
}

func TestMessagePrintsTheReplyAndDefaultsToThisProject(t *testing.T) {
	t.Chdir(t.TempDir())
	d := newDaemon(t, `{"reply":"main branch is master","facts":[{"content":"the main branch is master","scope":"local"}]}`)

	code, stdout, stderr := run(t, "message", "--addr", d.URL, "this is my git repository")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "main branch is master\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	// What was recorded belongs on stderr, so a pipe sees the answer alone.
	if !strings.Contains(stderr, "recorded (local): the main branch is master") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got := d.req(); got.path != "/v1/cli/message" || got.method != http.MethodPost {
		t.Fatalf("%s %s", got.method, got.path)
	}
	if got := d.field(t, "scope"); got != "local" {
		t.Fatalf("an unflagged message must read the project it is standing in, sent scope %q", got)
	}
	if d.field(t, "project") == "" {
		t.Fatal("a local message carries no project")
	}
	if got := d.field(t, "update"); got != "auto" {
		t.Fatalf("update = %q", got)
	}
}

func TestMessageUpdateFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		flag string
		want string
	}{
		{"", "auto"},
		{"--update", "force"},
		{"--no-update", "never"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			d := newDaemon(t, `{"reply":"ok"}`)
			args := []string{"message", "--addr", d.URL}
			if c.flag != "" {
				args = append(args, c.flag)
			}
			code, _, stderr := run(t, append(args, "hello")...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if got := d.field(t, "update"); got != c.want {
				t.Fatalf("update = %q, want %q", got, c.want)
			}
		})
	}
}

func TestContradictoryFlagsAreUsageErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := [][]string{
		{"message", "--update", "--no-update", "hello"},
		{"message", "--local", "--global", "hello"},
		{"fact", "add", "--local", "--global", "a fact"},
		{"digest", "--local", "--global"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d := newDaemon(t, `{}`)
			code, _, stderr := run(t, withAddr(args, d.URL)...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, stderr)
			}
			if d.req().calls != 0 {
				t.Fatal("a command that does not parse still reached the daemon")
			}
			if !strings.Contains(stderr, "mutually exclusive") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

func TestFactWritesRefuseToPickAScope(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"fact", "add", "a fact"},
		{"fact", "update", "--id", "x", "a fact"},
	} {
		t.Run(args[1], func(t *testing.T) {
			d := newDaemon(t, `{"id":"mem_1"}`)
			code, _, stderr := run(t, withAddr(args, d.URL)...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, stderr)
			}
			if d.req().calls != 0 {
				t.Fatal("an unscoped write reached the daemon")
			}
			if !strings.Contains(stderr, "--local or --global") {
				t.Fatalf("stderr %q does not name both options", stderr)
			}
		})
	}
}

func TestFactAddSendsWhatWasTyped(t *testing.T) {
	d := newDaemon(t, `{"id":"mem_7"}`)
	code, stdout, stderr := run(t, "fact", "add", "--addr", d.URL, "--global",
		"--category", "preference", "--tag", "style", "--tag", "prose", "prefers terse answers")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "mem_7\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if got := d.req().method; got != http.MethodPost {
		t.Fatalf("method = %s", got)
	}
	if got := d.field(t, "scope"); got != "global" {
		t.Fatalf("scope = %q", got)
	}
	if got := d.field(t, "project"); got != "" {
		t.Fatalf("a global fact was sent with project %q", got)
	}
	if got := d.field(t, "content"); got != "prefers terse answers" {
		t.Fatalf("content = %q", got)
	}
	if got := d.field(t, "category"); got != "preference" {
		t.Fatalf("category = %q", got)
	}
	tags, _ := d.req().body["tags"].([]any)
	if len(tags) != 2 || tags[0] != "style" || tags[1] != "prose" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestFactAddLocalCarriesTheProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	d := newDaemon(t, `{"id":"mem_1"}`)

	code, _, stderr := run(t, "fact", "add", "--addr", d.URL, "--local", "the tests need docker")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := d.field(t, "scope"); got != "local" {
		t.Fatalf("scope = %q", got)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.field(t, "project"); got != want && got != dir {
		t.Fatalf("project = %q, want the directory the command ran in (%s)", got, dir)
	}
}

func TestFactUpdateNeedsAnID(t *testing.T) {
	d := newDaemon(t, `{"id":"mem_1"}`)
	code, _, stderr := run(t, "fact", "update", "--addr", d.URL, "--global", "a fact")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--id") {
		t.Fatalf("stderr = %q", stderr)
	}
	if d.req().calls != 0 {
		t.Fatal("an update with nothing to update reached the daemon")
	}
}

func TestFactUpdatePatches(t *testing.T) {
	d := newDaemon(t, `{"id":"mem_9"}`)
	code, stdout, stderr := run(t, "fact", "update", "--addr", d.URL, "--global",
		"--id", "mem_2", "prefers terse answers")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := d.req().method; got != http.MethodPatch {
		t.Fatalf("method = %s", got)
	}
	if got := d.field(t, "id"); got != "mem_2" {
		t.Fatalf("id = %q", got)
	}
	if stdout != "mem_9\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestFactList(t *testing.T) {
	d := newDaemon(t, `{"facts":[{"id":"a","content":"one","category":"structure"},{"id":"b","content":"two"}]}`)
	code, stdout, stderr := run(t, "fact", "list", "--addr", d.URL, "--global", "--limit", "3")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	got := d.req()
	if got.method != http.MethodGet {
		t.Fatalf("method = %s", got.method)
	}
	if got.query.Get("scope") != "global" || got.query.Get("limit") != "3" {
		t.Fatalf("query = %v", got.query)
	}
	if got.query.Get("project") != "" {
		t.Fatalf("a global list asked about project %q", got.query.Get("project"))
	}
	want := "a  (structure) one\nb  two\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestFactListOfAnEmptyScopeSaysSo(t *testing.T) {
	d := newDaemon(t, `{"facts":[]}`)
	code, stdout, stderr := run(t, "fact", "list", "--addr", d.URL, "--global")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "nothing stored\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestDigestAsksAboutBothScopesUnlessTold(t *testing.T) {
	t.Chdir(t.TempDir())
	d := newDaemon(t, `{"digest":"You work mostly on one Go relay."}`)

	code, stdout, stderr := run(t, "digest", "--addr", d.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := d.field(t, "scope"); got != "" {
		t.Fatalf("a bare digest asked for scope %q", got)
	}
	if d.field(t, "project") == "" {
		t.Fatal("a digest run inside a project must say which one")
	}
	if stdout != "You work mostly on one Go relay.\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestDigestGlobalSendsNoProject(t *testing.T) {
	t.Chdir(t.TempDir())
	d := newDaemon(t, `{"digest":"..."}`)
	if code, _, stderr := run(t, "digest", "--addr", d.URL, "--global"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := d.field(t, "project"); got != "" {
		t.Fatalf("project = %q", got)
	}
}

func TestUnknownSubcommandPrintsEveryCommand(t *testing.T) {
	code, _, stderr := run(t, "remember", "something")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	for _, want := range []string{"remember", "doctor", "message", "fact add", "fact update", "fact list", "digest"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, stderr)
		}
	}
}

func TestUnreachableDaemonIsOneLineAndExitOne(t *testing.T) {
	d := newDaemon(t, `{}`)
	d.Close()

	code, _, stderr := run(t, "digest", "--addr", d.URL, "--global")
	if code != 1 {
		t.Fatalf("exit %d, want 1: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no daemon answering") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
		t.Fatalf("the message is more than one line:\n%s", stderr)
	}
}

func TestServerRefusalsBecomeExitCodes(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{http.StatusBadRequest, 2},
		{http.StatusInternalServerError, 1},
		{http.StatusUnauthorized, 1},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			d := newDaemon(t, `{"error":"no scope: pass --local or --global"}`)
			d.status = c.status
			code, _, stderr := run(t, "digest", "--addr", d.URL, "--global")
			if code != c.want {
				t.Fatalf("exit %d, want %d", code, c.want)
			}
			if !strings.Contains(stderr, "no scope: pass --local or --global") {
				t.Fatalf("the daemon's own reason was lost: %q", stderr)
			}
		})
	}
}

func TestTokenIsSentWhenConfigured(t *testing.T) {
	d := newDaemon(t, `{"digest":"..."}`)
	t.Setenv("SHOULDER_TOKEN", "s3cret")
	var out, errs bytes.Buffer
	c := &cli{out: &out, err: &errs}
	if code := c.dispatch("digest", []string{"--addr", d.URL, "--global"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if got := d.req().token; got != "s3cret" {
		t.Fatalf("token header = %q", got)
	}
}

func TestAskingForHelpIsNotAUsageError(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h", "-help"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := run(t, arg)
			if code != 0 {
				t.Fatalf("exit %d, want 0: %s", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("an answered question wrote to stderr: %q", stderr)
			}
			for _, want := range []string{"fact add", "fact list", "digest", "message", "--local"} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("help does not mention %q:\n%s", want, stdout)
				}
			}
		})
	}
}

func TestSubcommandHelpTeachesTheScopeContract(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"fact", "--help"}, []string{"fact add", "fact update", "fact list"}},
		{[]string{"fact", "add", "--help"}, []string{"--local", "--global", "required", `"content"`}},
		{[]string{"fact", "update", "--help"}, []string{"--id ID", "required", "never moves it"}},
		{[]string{"fact", "list", "--help"}, []string{"(default)", "--global"}},
		{[]string{"message", "--help"}, []string{"(default)", "--no-update"}},
		{[]string{"digest", "--help"}, []string{"covers both"}},
		{[]string{"doctor", "--help"}, []string{"--liveness"}},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			code, stdout, stderr := run(t, c.args...)
			if code != 0 {
				t.Fatalf("exit %d, want 0: %s", code, stderr)
			}
			if stderr != "" {
				t.Fatalf("help went to stderr: %q", stderr)
			}
			for _, want := range c.want {
				if !strings.Contains(stdout, want) {
					t.Fatalf("%v help does not mention %q:\n%s", c.args, want, stdout)
				}
			}
			// A project outside a repository is still a project, and only the
			// scoped subcommands have to say so.
			if c.args[0] != "doctor" && !strings.Contains(stdout, "git worktree") {
				t.Fatalf("%v help does not say what a project is:\n%s", c.args, stdout)
			}
		})
	}
}

func TestAMistypedFlagIsStillAUsageError(t *testing.T) {
	code, stdout, stderr := run(t, "message", "--nope", "hello")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if stdout != "" {
		t.Fatalf("a rejection wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "not defined") || !strings.Contains(stderr, "usage:") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestAFlagAfterTheTextIsRefused(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := [][]string{
		{"message", "hello", "--no-update"},
		{"fact", "add", "--global", "a fact", "--tag=x"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d := newDaemon(t, `{"reply":"never asked"}`)
			code, _, stderr := run(t, withAddr(args, d.URL)...)
			if code != 2 {
				t.Fatalf("exit %d, want 2: %s", code, stderr)
			}
			if d.req().calls != 0 {
				t.Fatal("the flag was swallowed into the text and sent anyway")
			}
			if !strings.Contains(stderr, "flags go first") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

func TestAWriteWithNoBackendFails(t *testing.T) {
	d := newDaemon(t, `{"error":"nothing was written: no memory backend is configured, so set SHOULDER_MEMORY_URL to an mcp-memory-service base URL and restart the daemon"}`)
	d.status = http.StatusServiceUnavailable

	code, stdout, stderr := run(t, "fact", "add", "--addr", d.URL, "--global", "prefers terse answers")
	if code == 0 {
		t.Fatal("a fact that went nowhere reported success")
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "SHOULDER_MEMORY_URL") {
		t.Fatalf("stderr does not say how to fix it: %q", stderr)
	}
}

func TestAFactAlreadyStoredIsSuccess(t *testing.T) {
	d := newDaemon(t, `{"id":"","already_known":true}`)
	code, stdout, stderr := run(t, "fact", "add", "--addr", d.URL, "--global", "prefers terse answers")
	if code != 0 {
		t.Fatalf("exit %d: running the same fact add twice must not fail: %s", code, stderr)
	}
	if stdout != "already known\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestADaemonThatNeverHeardOfTheRouteNamesItself(t *testing.T) {
	d := newDaemon(t, "404 page not found\n")
	d.status = http.StatusNotFound

	code, _, stderr := run(t, "fact", "add", "--addr", d.URL, "--global", "a fact")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	for _, want := range []string{d.URL, "404", "older", "/v1/cli/facts"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr does not mention %q: %q", want, stderr)
		}
	}
}

func TestFactListWithNoFlagSaysWhichHalfItRead(t *testing.T) {
	t.Chdir(t.TempDir())
	d := newDaemon(t, `{"facts":[]}`)

	code, stdout, stderr := run(t, "fact", "list", "--addr", d.URL)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if stdout != "nothing stored\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "this project only") || !strings.Contains(stderr, "--global") {
		t.Fatalf("an empty project list must say it never looked at the other half: %q", stderr)
	}

	scoped := newDaemon(t, `{"facts":[]}`)
	if _, _, errs := run(t, "fact", "list", "--addr", scoped.URL, "--global"); strings.Contains(errs, "this project only") {
		t.Fatalf("a list that was told which half still explained itself: %q", errs)
	}
}
