package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// proxySays stands in for the module proxy naming one newest release.
func proxySays(t *testing.T, version string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Version": version})
	}))
	t.Cleanup(srv.Close)
	old := latestURL
	latestURL = srv.URL
	t.Cleanup(func() { latestURL = old })
}

// thisBuildIs makes the test binary report as one release from one origin.
func thisBuildIs(t *testing.T, version, origin string) {
	t.Helper()
	oldV, oldO := buildVersion, buildOrigin
	buildVersion, buildOrigin = version, origin
	t.Cleanup(func() { buildVersion, buildOrigin = oldV, oldO })
}

// releaseServes stands in for the release files: the binary for this platform
// with the given content, and a checksum file that says sum of it.
func releaseServes(t *testing.T, tag, content, sum string) {
	t.Helper()
	name := "shoulderd_" + runtime.GOOS + "_" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tag + "/SHA256SUMS":
			_, _ = io.WriteString(w, sum+"  "+name+"\n"+"0000  shoulderd_other_arch\n")
		case "/" + tag + "/" + name:
			_, _ = io.WriteString(w, content)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releaseBase
	releaseBase = srv.URL
	t.Cleanup(func() { releaseBase = old })
	t.Setenv("SHOULDER_RELEASE_BASE", "")
}

// ownBinary gives the command a file of its own to replace.
func ownBinary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shoulderd")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // G306: a program
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return path, nil }
	t.Cleanup(func() { executable = old })
	return path
}

func runUpdate(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	c := &cli{out: &out, err: &errOut}
	code := c.dispatch("update", args)
	return code, out.String(), errOut.String()
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The check is what the session-start hook runs once a day: its exit code is
// the whole answer, so each of the three has to be the documented one.
func TestUpdateCheckAnswersByExitCode(t *testing.T) {
	thisBuildIs(t, "v0.5.0", "release")

	proxySays(t, "v0.5.2")
	code, out, _ := runUpdate(t, "--check")
	if code != exitNewer || !strings.Contains(out, "v0.5.2 is out; this is v0.5.0") {
		t.Fatalf("newer release: exit %d, %q", code, out)
	}

	proxySays(t, "v0.5.0")
	code, out, _ = runUpdate(t, "--check")
	if code != 0 || !strings.Contains(out, "v0.5.0 is the newest release") {
		t.Fatalf("current: exit %d, %q", code, out)
	}

	old := latestURL
	latestURL = "http://127.0.0.1:1/"
	t.Cleanup(func() { latestURL = old })
	code, _, errOut := runUpdate(t, "--check")
	if code != 1 || !strings.Contains(errOut, "module proxy") {
		t.Fatalf("proxy unreachable: exit %d, %q", code, errOut)
	}
}

func TestUpdateReplacesAReleaseBinaryInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a running Windows binary cannot be replaced")
	}
	thisBuildIs(t, "v0.5.0", "plugin")
	proxySays(t, "v0.5.2")
	releaseServes(t, "v0.5.2", "the new binary", sha("the new binary"))
	path := ownBinary(t, "the old binary")

	code, out, errOut := runUpdate(t)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if err != nil || string(got) != "the new binary" {
		t.Fatalf("the binary was not replaced: %q, %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode()&0o111 == 0 {
		t.Fatal("the new binary is not executable")
	}
	if !strings.Contains(out, "v0.5.2 is now at "+path) || !strings.Contains(out, "still the old build") {
		t.Fatalf("the report must name the file and say the daemon is unchanged:\n%s", out)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".shoulderd-update-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestUpdateRefusesAChecksumMismatchAndKeepsTheBinary(t *testing.T) {
	thisBuildIs(t, "v0.5.0", "release")
	proxySays(t, "v0.5.2")
	releaseServes(t, "v0.5.2", "tampered", sha("the real binary"))
	path := ownBinary(t, "the old binary")

	code, _, errOut := runUpdate(t)
	if code != 1 || !strings.Contains(errOut, "checksum mismatch") {
		t.Fatalf("exit %d, %q", code, errOut)
	}
	got, _ := os.ReadFile(path) //nolint:gosec // G304: the test's own file
	if string(got) != "the old binary" {
		t.Fatalf("the binary was touched: %q", got)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".shoulderd-update-*"))
	if len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestUpdateRunsGoInstallForAGoInstallBuild(t *testing.T) {
	thisBuildIs(t, "v0.5.0", "go install")
	proxySays(t, "v0.5.2")
	var asked string
	old := goInstall
	goInstall = func(_ context.Context, tag string, _ io.Writer) error { asked = tag; return nil }
	t.Cleanup(func() { goInstall = old })

	code, out, _ := runUpdate(t)
	if code != 0 || asked != "v0.5.2" || !strings.Contains(out, "v0.5.2 is installed") {
		t.Fatalf("exit %d, installed %q, out %q", code, asked, out)
	}
}

func TestUpdateTellsACheckoutWhatToRun(t *testing.T) {
	thisBuildIs(t, "v0.5.0", "checkout")
	proxySays(t, "v0.5.2")

	code, out, _ := runUpdate(t)
	if code != 1 || !strings.Contains(out, "git pull && make update") {
		t.Fatalf("exit %d, %q", code, out)
	}
}

// A checkout has no release number to compare, so it is neither told to
// update nor told it is current.
func TestUpdateCheckNamesAnUnreleasedBuild(t *testing.T) {
	thisBuildIs(t, "", "checkout")
	proxySays(t, "v0.5.2")
	code, out, _ := runUpdate(t, "--check")
	if code != 0 || !strings.Contains(out, "not a release; the newest release is v0.5.2") {
		t.Fatalf("exit %d, %q", code, out)
	}
}

func TestUpdateDoesNothingWhenCurrent(t *testing.T) {
	thisBuildIs(t, "v0.5.2", "plugin")
	proxySays(t, "v0.5.2")
	path := ownBinary(t, "the current binary")

	code, out, _ := runUpdate(t, "--json")
	if code != 0 {
		t.Fatalf("exit %d, %q", code, out)
	}
	var rep updateReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Newer || rep.Action != "none" {
		t.Fatalf("report %+v, %v", rep, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "the current binary" { //nolint:gosec // G304: the test's own file
		t.Fatal("a current binary was replaced")
	}
}
