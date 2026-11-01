package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Set through -ldflags by the release and container builds, which compile from
// a tarball or a COPY and so have no VCS or module metadata to read. `go
// install` sets neither: the module version in the build info is the tag, and
// that is the whole story.
var (
	buildVersion string
	buildOrigin  string
)

// latestURL is the module proxy's answer to "what is the newest tag". Asking
// the proxy rather than a forge means the answer is the same whichever remote
// the user cloned, and needs no token.
var latestURL = "https://proxy.golang.org/gitlab.com/quittymr/shoulder-daemon/relay/@latest"

// listURL is the proxy's every tag, one per line, candidates included.
var listURL = "https://proxy.golang.org/gitlab.com/quittymr/shoulder-daemon/relay/@v/list"

// Under this directory the binary was put there by an editor plugin, not by
// the user, which decides whether an update means `go install` or a restart.
var pluginBinDir = func() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "shoulder-daemon", "bin")
}

type build struct {
	Version  string `json:"version"`
	Origin   string `json:"origin"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
	Commit   string `json:"commit,omitempty"`
	Modified bool   `json:"modified,omitempty"`
}

func currentBuild() build {
	b := build{
		Version:  buildVersion,
		Origin:   buildOrigin,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	info, ok := debug.ReadBuildInfo()
	if ok {
		if b.Version == "" {
			b.Version = info.Main.Version
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) > 12 {
					s.Value = s.Value[:12]
				}
				b.Commit = s.Value
			case "vcs.modified":
				b.Modified, _ = strconv.ParseBool(s.Value)
			}
		}
	}
	if b.Version == "" || b.Version == "(devel)" {
		b.Version = "devel"
	}
	if b.Origin == "" {
		switch {
		case isTagged(b.Version):
			b.Origin = "go install"
		default:
			b.Origin = "checkout"
		}
	}
	if exe, err := os.Executable(); err == nil {
		if rel, err := filepath.Rel(pluginBinDir(), exe); err == nil && !strings.HasPrefix(rel, "..") {
			b.Origin = "plugin"
		}
	}
	return b
}

func (b build) String() string {
	s := "shoulderd " + b.Version
	if b.Commit != "" && !isTagged(b.Version) {
		s += " " + b.Commit
		if b.Modified {
			s += "+dirty"
		}
	}
	return s + " (" + b.Go + ", " + b.Platform + ", " + b.Origin + ")"
}

const versionUsage = `usage: shoulderd version [--json]

Print which build this is and where it came from.
`

func (c *cli) version(args []string) int {
	fs := c.flags("version", versionUsage)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	b := currentBuild()
	if *asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(b)
		return 0
	}
	fmt.Fprintln(c.out, b)
	return 0
}

// latestRelease asks the module proxy for the newest tag. Nothing here may
// hold up doctor for long: the proxy is not the thing being diagnosed.
func latestRelease(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("proxy answered %s", resp.Status)
	}
	var v struct{ Version string }
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if !isTagged(v.Version) {
		return "", fmt.Errorf("no tagged release yet; newest is %s", v.Version)
	}
	return v.Version, nil
}

// latestCandidate asks the module proxy for every tag and returns the newest,
// release candidates included. The proxy's own "latest" never names a
// candidate while a release exists, which is right for everyone who did not
// ask for one.
func latestCandidate(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("proxy answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	best := ""
	for _, v := range strings.Fields(string(body)) {
		if isTagged(v) && (best == "" || newer(v, best)) {
			best = v
		}
	}
	if best == "" {
		return "", errors.New("no tagged release yet")
	}
	return best, nil
}

// isTagged is true for a release, vX.Y.Z, or a release candidate, vX.Y.Z-rc
// with an optional number. A pseudo-version carries a timestamp and a hash
// after the patch number and is not one.
func isTagged(v string) bool {
	_, ok := parseTag(v)
	return ok
}

// tag is one release in comparable form. rc is 0 for a release and the
// candidate's number otherwise; a bare -rc counts as the first.
type tag struct {
	n  [3]int
	rc int
}

// parseTag reads vX.Y.Z or vX.Y.Z-rc[N], or nothing for anything else.
func parseTag(v string) (tag, bool) {
	var t tag
	nums, rc, isRC := strings.Cut(strings.TrimPrefix(v, "v"), "-rc")
	if isRC {
		t.rc = 1
		if rc != "" {
			x, err := strconv.Atoi(rc)
			if err != nil || x < 1 {
				return t, false
			}
			t.rc = x
		}
	}
	return t.numbers(nums)
}

func (t tag) numbers(v string) (tag, bool) {
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return t, false
	}
	for i, f := range fields {
		x, err := strconv.Atoi(f)
		if err != nil || x < 0 {
			return t, false
		}
		t.n[i] = x
	}
	return t, true
}

// newer reports whether candidate is a release later than current. Anything
// that is not a tagged release on either side is not comparable, and a build
// that cannot be compared is not told to update. A release is later than
// every candidate for the same numbers; among candidates the number decides.
func newer(candidate, current string) bool {
	a, ok := parseTag(candidate)
	if !ok {
		return false
	}
	b, ok := parseTag(current)
	if !ok {
		return false
	}
	if a.n != b.n {
		return a.n[0] > b.n[0] || a.n[0] == b.n[0] && (a.n[1] > b.n[1] || a.n[1] == b.n[1] && a.n[2] > b.n[2])
	}
	if a.rc == 0 || b.rc == 0 {
		return a.rc == 0 && b.rc != 0
	}
	return a.rc > b.rc
}

// updateHint is the one command that brings this build forward, which depends
// on how it got here.
func updateHint(origin string) string {
	switch origin {
	case "plugin", "release", "go install":
		return "run: shoulderd update"
	case "container":
		return "pull the newer image and recreate the container"
	}
	return "git pull && make update"
}
