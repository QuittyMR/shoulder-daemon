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

// relayAnswering stands in for a relay answering /readyz with one fixed verdict,
// which is the whole of what the boot script can see. A real daemon is not used
// here because the states that matter - a store that is unreachable, a store
// that was never configured, and a relay too old to have been asked at all -
// are none of them states a healthy daemon will enter on request.
func relayAnswering(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// The two bodies a 503 carries, written exactly as cliapi.ReadyStatus is
// marshalled. They are restated rather than imported because the script reads
// them as text, and a field renamed in Go has to fail here rather than quietly
// stop matching in a shell pattern nobody is testing.
const (
	storeUnreachable  = `{"ok":false,"memory":"unreachable","error":"the store did not answer within 2s"}`
	storeUnconfigured = `{"ok":false,"memory":"none","error":"no memory backend is configured; nothing observed in a session will be kept"}`
	storeReady        = `{"ok":true,"memory":"ok"}`
)

// nothingListening returns an address that was a server and is not one now, so
// a connection to it is refused rather than left hanging.
func nothingListening(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	return addr
}

// production is where the boot script goes when SHOULDER_ADDR is not set, which
// is to say the machine's own relay, with the developer's own memory store
// behind it. A test that reaches it does not fail - it passes, against the
// wrong daemon, having probed a live stack and possibly restarted it. Writing
// the variable is not enough of a defence on its own, because the failure mode
// of forgetting it is silence.
const production = "127.0.0.1:8787"

// real is the guard itself, kept as a predicate so that the thing standing
// between this suite and somebody's own daemon is something a test can assert
// about. An empty address counts, because that is what a missing SHOULDER_ADDR
// looks like by the time it reaches here, and the port alone counts too: the
// host it is written with is not what makes it theirs.
func real(addr string) bool {
	return addr == "" || addr == production || strings.HasSuffix(addr, ":8787")
}

// Nothing else in this file can fail if the guard quietly stops recognising the
// address it guards against, because the tests that would then run against the
// real relay would pass.
func TestTheGuardKnowsTheMachinesOwnRelay(t *testing.T) {
	for _, addr := range []string{"", production, "localhost:8787", "0.0.0.0:8787"} {
		if !real(addr) {
			t.Errorf("%q was not recognised as the machine's own relay", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:41234", "127.0.0.1:8788"} {
		if real(addr) {
			t.Errorf("%q is a stub address and was refused", addr)
		}
	}
}

// boot runs the hook the way the editor does and fails the test if it does not
// exit 0, which it may never do: a non-zero exit from a hook is a message in
// the middle of somebody's session.
func boot(t *testing.T, addr, runtime, started string) string {
	t.Helper()
	if real(addr) {
		t.Fatalf("this would drive the machine's real relay at %q, not a stub", addr)
	}
	// Without a start command of its own a failing probe launches a real
	// shoulderd off PATH, which then binds the real port and outlives the test.
	if started == "" {
		t.Fatal("no SHOULDER_START_CMD: an unready relay would start a real daemon")
	}
	script := filepath.Join("..", "..", "adapters", "claude-code", "scripts", "ensure-daemon.sh")
	cmd := exec.Command(script)
	// clean strips every SHOULDER_ variable the developer's shell carries, so
	// the three set here are the only three the script can see; an inherited
	// SHOULDER_ADDR pointing at the real relay cannot survive into the child.
	cmd.Env = append(clean(os.Environ()),
		"SHOULDER_ADDR="+addr,
		"SHOULDER_START_CMD=touch "+started,
		"XDG_RUNTIME_DIR="+runtime,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the boot script must never fail a hook: %v\n%s", err, out)
	}
	return string(out)
}

// The bug this guards: a relay that cannot reach its store still answers
// /healthz with an ok, so the hook saw health, did nothing, and sessions ran
// for days recalling and writing nothing at all.
func TestTheBootScriptRecoversARelayThatCannotReachItsStore(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")

	boot(t, relayAnswering(t, http.StatusServiceUnavailable, storeUnreachable), t.TempDir(), started)

	if !appears(started, 10*time.Second) {
		t.Fatal("the relay was serving with a dead store and nothing was restarted")
	}
}

// A relay that is ready is the common case, and the whole value of the hook is
// that it costs a single request and then nothing.
func TestTheBootScriptIsQuietWhenTheRelayIsReady(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")

	boot(t, relayAnswering(t, http.StatusOK, storeReady), t.TempDir(), started)

	if appears(started, 3*time.Second) {
		t.Fatal("a ready relay was restarted")
	}
}

// A store that is not coming back answers 503 for as long as the session lasts,
// and this hook runs before every prompt. Without a floor on how often it may
// act, the fix for a broken store is a start command per prompt, forever.
func TestTheBootScriptDoesNotRestartTwiceInsideTheBackoffWindow(t *testing.T) {
	addr := relayAnswering(t, http.StatusServiceUnavailable, storeUnreachable)
	runtime := t.TempDir()
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")

	boot(t, addr, runtime, first)
	if !appears(first, 10*time.Second) {
		t.Fatal("the first prompt did not recover an unready relay")
	}

	boot(t, addr, runtime, second)

	// Long enough that a start command which was going to run has run: the
	// script backgrounds it, so an immediate look proves nothing.
	if appears(second, 3*time.Second) {
		t.Fatal("a second recovery ran within the backoff window; a permanently broken store would start a storm")
	}
}

// Every relay older than /readyz answers 404, and it may be in perfect health.
// Reading that as unready would hand everybody who has not upgraded a start
// command before every prompt they type.
func TestTheBootScriptLeavesARelayWithoutReadyzAlone(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")

	boot(t, relayAnswering(t, http.StatusNotFound, ""), t.TempDir(), started)

	if appears(started, 3*time.Second) {
		t.Fatal("a relay too old to have a readiness endpoint was restarted")
	}
}

// The floor exists for a relay that answers. A relay that is gone has to come
// back on the very next prompt, not half a minute into the work, so two runs in
// a row must both start something.
func TestTheBackoffDoesNotDelayARelayThatIsSimplyGone(t *testing.T) {
	addr := nothingListening(t)
	runtime := t.TempDir()
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")

	boot(t, addr, runtime, first)
	if !appears(first, 10*time.Second) {
		t.Fatal("nothing was listening and the boot script did not start anything")
	}

	boot(t, addr, runtime, second)

	if !appears(second, 10*time.Second) {
		t.Fatal("the next prompt after a failed start did nothing; a dead relay would stay dead")
	}
}

// A relay with no memory backend configured answers 503 like an unreachable one
// does, and no start command can ever fix it: there is no process missing, only
// a setting. Restarting the stack for it would be the same storm the backoff
// floor exists to prevent, slowed to one every thirty seconds and with nothing
// at the end of it. It has to be said instead, and said sparingly.
func TestTheBootScriptNeverRestartsARelayWithNoMemoryBackend(t *testing.T) {
	addr := relayAnswering(t, http.StatusServiceUnavailable, storeUnconfigured)
	runtime := t.TempDir()
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")

	said := boot(t, addr, runtime, first)
	if !strings.Contains(said, "no memory backend configured") {
		t.Fatalf("nothing told the session why it will remember nothing:\n%s", said)
	}
	if !strings.Contains(said, "SHOULDER_MEMORY_URL") {
		t.Fatalf("the message does not say how to fix it:\n%s", said)
	}

	// The next prompt, and every prompt after it: still no start command, and
	// the message not repeated into the middle of somebody's work.
	again := boot(t, addr, runtime, second)
	if strings.Contains(again, "no memory backend configured") {
		t.Fatal("the unconfigured warning is printed before every prompt")
	}
	if appears(first, 3*time.Second) || appears(second, 3*time.Second) {
		t.Fatal("a relay with no memory backend was restarted; no start command can configure one")
	}
}

// The floor and the fault are stored separately, so a machine that is merely
// unconfigured must not leave a recovery stamp behind that would then delay the
// real recovery of a relay that dies a moment later.
func TestAnUnconfiguredRelayLeavesTheRecoveryFloorAlone(t *testing.T) {
	runtime := t.TempDir()
	dir := t.TempDir()
	quiet := filepath.Join(dir, "quiet")
	started := filepath.Join(dir, "started")

	boot(t, relayAnswering(t, http.StatusServiceUnavailable, storeUnconfigured), runtime, quiet)
	boot(t, relayAnswering(t, http.StatusServiceUnavailable, storeUnreachable), runtime, started)

	if !appears(started, 10*time.Second) {
		t.Fatal("an unconfigured relay wrote the recovery stamp and held up a recovery that would have worked")
	}
}
