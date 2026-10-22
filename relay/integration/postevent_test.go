//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// post runs the per-event hook the way the editor does: the event on stdin, the
// relay address and token in the environment, and a start command of its own
// so an unreachable relay cannot launch a real daemon off PATH.
func post(t *testing.T, addr, token, event, body, started string) string {
	t.Helper()
	if real(addr) {
		t.Fatalf("this would post to the machine's real relay at %q, not a stub", addr)
	}
	if started == "" {
		t.Fatal("no SHOULDER_START_CMD: an unreachable relay would start a real daemon")
	}
	script := filepath.Join("..", "..", "adapters", "claude-code", "scripts", "post-event.sh")
	cmd := exec.Command(script, event) //nolint:gosec // G204: script is a literal path in this repository
	cmd.Env = append(clean(os.Environ()),
		"SHOULDER_ADDR="+addr,
		"SHOULDER_TOKEN="+token,
		"SHOULDER_START_CMD=touch "+started,
		"XDG_RUNTIME_DIR="+t.TempDir(),
	)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the post script must never fail a hook: %v", err)
	}
	return string(out)
}

// relayRecording stands in for a relay's hook route: it keeps what arrived and
// answers with a fixed body, so the test can see both sides of the post.
func relayRecording(t *testing.T, status int, reply string) (addr string, got *http.Request, body *string) {
	t.Helper()
	var seen http.Request
	var text string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r
		b, _ := readAll(r)
		text = b
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &seen, &text
}

func readAll(r *http.Request) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String(), nil
		}
	}
}

// The whole reason this script exists: an HTTP hook that cannot connect prints
// an error and nothing acts on it, so a relay that died under a working session
// stayed dead until somebody typed. Here the failed post is what starts it.
func TestAFailedPostRestartsTheRelay(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")

	out := post(t, nothingListening(t), "tok", "PreToolUse", `{"hook_event_name":"PreToolUse"}`, started)

	if out != "" {
		t.Errorf("a failed post printed %q to the harness; it must print nothing", out)
	}
	if !appears(started, 10*time.Second) {
		t.Fatal("the relay was unreachable and nothing was restarted")
	}
}

// A relay that answers is the common case: the event reaches the right route
// with the token, its answer is printed as-is, and nothing is started.
func TestAPostThatIsAnsweredIsPrintedAndStartsNothing(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	reply := `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"x"}}`
	addr, got, body := relayRecording(t, http.StatusOK, reply)

	out := post(t, addr, "tok", "Stop", `{"hook_event_name":"Stop"}`, started)

	if strings.TrimSpace(out) != reply {
		t.Errorf("printed %q, want the relay's answer %q", out, reply)
	}
	if got.URL.Path != "/v1/hooks/claude-code/Stop" {
		t.Errorf("posted to %q", got.URL.Path)
	}
	if got.Header.Get("X-Shoulder-Token") != "tok" {
		t.Errorf("token header was %q", got.Header.Get("X-Shoulder-Token"))
	}
	if *body != `{"hook_event_name":"Stop"}` {
		t.Errorf("the relay received %q, not the event on stdin", *body)
	}
	if appears(started, 2*time.Second) {
		t.Fatal("an answering relay was restarted")
	}
}

// A relay that answers anything but 200 - a token it rejects, a route it does
// not know - said nothing the harness should read as a hook response.
func TestAPostRefusedByTheRelayPrintsNothing(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")
	addr, _, _ := relayRecording(t, http.StatusUnauthorized, `{"error":"token"}`)

	out := post(t, addr, "wrong", "Stop", `{}`, started)

	if out != "" {
		t.Errorf("a refused post printed %q to the harness", out)
	}
	if appears(started, 2*time.Second) {
		t.Fatal("a relay that is up and refusing was restarted")
	}
}
