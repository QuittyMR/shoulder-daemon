package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const updateUsage = `usage: shoulderd update [--check] [--json]

Bring this binary forward to the newest release. What that takes depends on
where this build came from, which 'shoulderd version' prints:

  plugin, release   the release binary for this platform is downloaded,
                    checked against the published SHA256SUMS, and put in
                    place of this one
  go install        go install ...@<tag> is run
  container         nothing is downloaded: the image has to be pulled and the
                    container recreated, and the command says so
  checkout          git pull && make update, and the command says so

The daemon that is running keeps the old code until it is restarted; the
command says how. --check only asks: exit 0 when this is the newest release,
3 when a newer one is out, 1 when the module proxy cannot be asked.

  --check      report whether a newer release exists and change nothing
  --json       machine-readable output
`

// exitNewer is what --check returns when a newer release is out. It is not 1,
// which is "could not ask", and not 2, which is a command line to fix: a shell
// script branches on it.
const exitNewer = 3

// releaseBase is where a release's files live, with the tag appended. The
// plugin's start script reads the same files from the "latest" alias; here
// the tag the proxy named is pinned so that the check and the download cannot
// disagree about which release is being installed. SHOULDER_RELEASE_BASE
// replaces the whole base, tag included, for a mirror.
var releaseBase = "https://github.com/QuittyMR/shoulder-daemon/releases/download"

// executable is os.Executable, replaceable so a test can update a file of its
// own rather than the test binary.
var executable = os.Executable

// goInstall runs go install for a tag, replaceable in tests.
var goInstall = func(ctx context.Context, tag string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "go", "install", goModule+"@"+tag) //nolint:gosec // G204: the tag is a vX.Y.Z the proxy named, checked by isTagged
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

const goModule = "gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd"

// updateReport is the --json shape, and what the text output is written from.
type updateReport struct {
	Current string `json:"current"`
	Latest  string `json:"latest"`
	Newer   bool   `json:"newer"`
	Origin  string `json:"origin"`
	// Action is what was done: "none", "replaced", "go install", or "told",
	// the last being an origin this command cannot bring forward itself.
	Action string `json:"action"`
	Path   string `json:"path,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (c *cli) update(args []string) int {
	fs := c.flags("update", updateUsage)
	check := fs.Bool("check", false, "report whether a newer release exists and change nothing")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("update takes no arguments, got %q", fs.Arg(0)))
	}

	b := currentBuild()
	rep := updateReport{Current: b.Version, Origin: b.Origin, Action: "none"}
	latest, err := latestRelease(context.Background())
	if err != nil {
		rep.Error = "could not ask the module proxy which release is newest: " + err.Error()
		return c.updateDone(rep, 1, *asJSON)
	}
	rep.Latest = latest
	// newer is false for a build that is not a release, so a checkout or a
	// pseudo-version is never told to update, and never told it is current.
	rep.Newer = newer(latest, b.Version)

	if *check {
		if rep.Newer {
			return c.updateDone(rep, exitNewer, *asJSON)
		}
		return c.updateDone(rep, 0, *asJSON)
	}
	if !rep.Newer {
		return c.updateDone(rep, 0, *asJSON)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch b.Origin {
	case "plugin", "release":
		path, err := replaceBinary(ctx, latest)
		if err != nil {
			rep.Error = err.Error()
			return c.updateDone(rep, 1, *asJSON)
		}
		rep.Action, rep.Path = "replaced", path
	case "go install":
		if err := goInstall(ctx, latest, c.err); err != nil {
			rep.Error = "go install failed: " + err.Error()
			return c.updateDone(rep, 1, *asJSON)
		}
		rep.Action = "go install"
	default:
		rep.Action = "told"
		return c.updateDone(rep, 1, *asJSON)
	}
	return c.updateDone(rep, 0, *asJSON)
}

// updateDone prints the report and returns the code. The text form says what
// happened and, when the binary changed, that the daemon is still the old one.
func (c *cli) updateDone(rep updateReport, code int, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return code
	}
	switch {
	case rep.Error != "" && rep.Action != "told":
		fmt.Fprintln(c.err, "shoulderd update:", rep.Error)
	case rep.Latest == "":
	case !isTagged(rep.Current):
		fmt.Fprintf(c.out, "this is a %s build, %s, not a release; the newest release is %s\n", rep.Origin, rep.Current, rep.Latest)
	case !rep.Newer:
		fmt.Fprintf(c.out, "shoulderd %s is the newest release\n", rep.Current)
	case rep.Action == "none":
		fmt.Fprintf(c.out, "shoulderd %s is out; this is %s\n", rep.Latest, rep.Current)
	case rep.Action == "told":
		fmt.Fprintf(c.out, "shoulderd %s is out; this is %s, a %s build, which this command cannot replace: %s\n",
			rep.Latest, rep.Current, rep.Origin, updateHint(rep.Origin))
	case rep.Action == "replaced":
		fmt.Fprintf(c.out, "shoulderd %s is now at %s\n", rep.Latest, rep.Path)
		fmt.Fprintln(c.out, restartNote)
	case rep.Action == "go install":
		fmt.Fprintf(c.out, "shoulderd %s is installed\n", rep.Latest)
		fmt.Fprintln(c.out, restartNote)
	}
	return code
}

const restartNote = `The daemon that is running is still the old build. It exits by itself once no
session has used it for a while, and the next session starts the new one; to
switch now, stop it with 'pkill -x shoulderd' and the next hook event starts
it again.`

// replaceBinary downloads the release binary for this platform, checks it
// against the published checksums, and renames it over the running one. The
// download lands in the executable's own directory so the rename is atomic,
// and a failure at any step leaves the running binary exactly as it was.
func replaceBinary(ctx context.Context, tag string) (string, error) {
	exe, err := executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return "", errors.New("a running Windows binary cannot be replaced; download the release and copy it over " + exe + " while nothing runs it")
	}
	name := "shoulderd_" + runtime.GOOS + "_" + runtime.GOARCH
	base := os.Getenv("SHOULDER_RELEASE_BASE")
	if base == "" {
		base = releaseBase + "/" + tag
	}

	sums, err := fetch(ctx, base+"/SHA256SUMS", 15*time.Second)
	if err != nil {
		return "", fmt.Errorf("checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == name {
			want = f[0]
		}
	}
	if want == "" {
		return "", fmt.Errorf("the release carries no checksum for %s", name)
	}

	body, err := fetch(ctx, base+"/"+name, 90*time.Second)
	if err != nil {
		return "", fmt.Errorf("binary: %w", err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("checksum mismatch for %s: the release says %s, the download is %s; nothing was replaced", name, want, got)
	}

	tmp, err := os.CreateTemp(filepath.Dir(exe), ".shoulderd-update-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //nolint:gosec // G302: a program has to be executable
		return "", err
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		return "", err
	}
	return exe, nil
}

// fetch reads one URL whole under its own deadline.
func fetch(ctx context.Context, url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: the release host, or the mirror the operator named in SHOULDER_RELEASE_BASE
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: as above
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
