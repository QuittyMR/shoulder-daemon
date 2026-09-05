package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const sampleLog = `{"time":"2026-09-05T10:00:00.000Z","level":"INFO","msg":"shoulderd listening","addr":"127.0.0.1:8787"}
{"time":"2026-09-05T10:00:01.000Z","level":"DEBUG","msg":"hook received","event":"UserPromptSubmit"}
{"time":"2026-09-05T10:00:02.000Z","level":"INFO","msg":"fact stored","id":"mem_1","origin":"s1","scope":"local","project":"repo","category":"structure","content":"main branch is master"}
{"time":"2026-09-05T10:00:03.000Z","level":"INFO","msg":"advice queued","id":"adv_1","session":"0123456789abcdef","turn":4,"text":"the branch is master"}
{"time":"2026-09-05T10:00:04.000Z","level":"INFO","msg":"advice injected","id":"adv_1","session":"0123456789abcdef","event":"UserPromptSubmit","text":"the branch is master"}
{"time":"2026-09-05T10:00:05.000Z","level":"INFO","msg":"fact superseded","origin":"cli","scope":"global","project":"","supersedes":"mem_2","category":"preference","content":"terse answers"}
{"time":"2026-09-05T10:00:06.000Z","level":"INFO","msg":"facts merged","scope":"local","project":"repo","kept":"mem_5","replaced":"mem_7,mem_9","content":"one rule"}
{"time":"2026-09-05T10:00:07.000Z","level":"INFO","msg":"fact dropped as no longer a rule","scope":"local","project":"repo","id":"mem_8","content":"old rule"}
{"time":"2026-09-05T10:00:08.000Z","level":"WARN","msg":"fact dropped: no scope was decided","origin":"s1","content":"unplaced"}
{"time":"2026-09-05T10:00:09.000Z","level":"WARN","msg":"decision failed; session unaffected","session":"s1","err":"timeout"}
`

func TestMonitorFiltersToMovements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shoulderd.log")
	if err := os.WriteFile(path, []byte(sampleLog), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	c := &cli{out: &out, err: &errOut}
	if code := c.dispatch("monitor", []string{"--log=" + path, "--no-follow"}); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("want 7 movements, got %d:\n%s", len(lines), out.String())
	}
	for _, unwanted := range []string{"listening", "hook received", "decision failed"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("%q leaked through the filter:\n%s", unwanted, out.String())
		}
	}
	for i, want := range []string{
		`stored      local repo                (structure) "main branch is master"  id=mem_1`,
		`queued      session 01234567 turn 4   "the branch is master"  id=adv_1`,
		`injected    session 01234567 UserPromptSubmit  "the branch is master"  id=adv_1`,
		`superseded  global                   [cli]  (preference) "terse answers"  supersedes=mem_2`,
		`merged      local repo                "one rule"  kept=mem_5  replaced=mem_7,mem_9`,
		`dropped     local repo                "old rule"  id=mem_8`,
		`refused      "unplaced"  (fact dropped: no scope was decided)`,
	} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d\n got %q\nwant it to contain %q", i, lines[i], want)
		}
	}
}

func TestMonitorTailAndAll(t *testing.T) {
	var b strings.Builder
	for i := 0; i < monitorTail+5; i++ {
		b.WriteString(`{"time":"2026-09-05T10:00:00Z","level":"INFO","msg":"fact stored","id":"mem_` + string(rune('a'+i)) + `","scope":"global","content":"x"}` + "\n")
	}
	path := filepath.Join(t.TempDir(), "shoulderd.log")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &cli{out: &out, err: &out}
	if code := c.dispatch("monitor", []string{"--log=" + path, "--no-follow"}); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if n := strings.Count(out.String(), "\n"); n != monitorTail {
		t.Errorf("default shows %d lines, want %d", n, monitorTail)
	}
	out.Reset()
	if code := c.dispatch("monitor", []string{"--log=" + path, "--no-follow", "--all"}); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if n := strings.Count(out.String(), "\n"); n != monitorTail+5 {
		t.Errorf("--all shows %d lines, want %d", n, monitorTail+5)
	}
}

func TestMonitorJSONPassesRecordsThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shoulderd.log")
	if err := os.WriteFile(path, []byte(sampleLog), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &cli{out: &out, err: &out}
	if code := c.dispatch("monitor", []string{"--log=" + path, "--no-follow", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.HasPrefix(line, `{"time":`) || !strings.Contains(line, `"msg":`) {
			t.Errorf("not a raw record: %q", line)
		}
	}
}

func TestMonitorFollowsAppendsAndWaitsForFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shoulderd.log")
	done := make(chan struct{})
	var out, errOut lockedBuffer
	c := &cli{out: &out, err: &errOut, done: done}
	finished := make(chan int)
	go func() { finished <- c.dispatch("monitor", []string{"--log=" + path}) }()

	waitFor(t, func() bool { return strings.Contains(errOut.String(), "waiting for") })
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	first := `{"time":"2026-09-05T10:00:02Z","level":"INFO","msg":"fact stored","id":"mem_1","scope":"global","content":"first"}` + "\n"
	if _, err := f.WriteString(first); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), `"first"`) })

	// A record split across two writes must not be shown half-parsed.
	second := `{"time":"2026-09-05T10:00:03Z","level":"INFO","msg":"fact stored","id":"mem_2","scope":"global","content":"second"}` + "\n"
	if _, err := f.WriteString(second[:30]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * monitorPoll)
	if strings.Contains(out.String(), "mem_2") {
		t.Fatalf("half a record was shown:\n%s", out.String())
	}
	if _, err := f.WriteString(second[30:]); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), `"second"`) })
	_ = f.Close()

	close(done)
	select {
	case code := <-finished:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not stop")
	}
	if n := strings.Count(out.String(), "\n"); n != 2 {
		t.Errorf("want 2 lines, got %d:\n%s", n, out.String())
	}
}

func TestMonitorRefusesWhenNoFile(t *testing.T) {
	t.Setenv("SHOULDER_LOG", "stderr")
	t.Setenv("SHOULDER_ENV_FILE", filepath.Join(t.TempDir(), "absent"))
	var out bytes.Buffer
	c := &cli{out: &out, err: &out}
	if code := c.dispatch("monitor", []string{"--no-follow"}); code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "stderr") {
		t.Errorf("the reason should name the setting: %s", out.String())
	}
}

func TestNewLoggerWritesFileAndCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep", "er", "shoulderd.log")
	log := newLogger(path, nil)
	log.Info("fact stored", "id", "mem_1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("log file not written: %v", err)
	}
	if !strings.Contains(string(raw), `"msg":"fact stored"`) {
		t.Errorf("record missing from file: %s", raw)
	}
}

func TestOpenLogRotatesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shoulderd.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), logRotateBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if st, err := os.Stat(path); err != nil || st.Size() != 0 {
		t.Errorf("fresh file expected after rotation, got size %v err %v", st, err)
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != logRotateBytes+1 {
		t.Errorf("old file not moved aside: %v %v", st, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// lockedBuffer is a bytes.Buffer the monitor goroutine writes while the test
// reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
