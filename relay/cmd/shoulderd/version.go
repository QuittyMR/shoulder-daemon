package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// isTagged is true for a real release, vX.Y.Z. A pseudo-version carries a
// timestamp and a hash after the patch number and is not one.
func isTagged(v string) bool {
	_, ok := semver(v)
	return ok
}

// semver returns the three numbers of vX.Y.Z, or nothing for anything else.
func semver(v string) ([3]int, bool) {
	var n [3]int
	v = strings.TrimPrefix(v, "v")
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return n, false
	}
	for i, f := range fields {
		x, err := strconv.Atoi(f)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// newer reports whether candidate is a release later than current. Anything
// that is not a tagged release on either side is not comparable, and a build
// that cannot be compared is not told to update.
func newer(candidate, current string) bool {
	a, ok := semver(candidate)
	if !ok {
		return false
	}
	b, ok := semver(current)
	if !ok {
		return false
	}
	return a != b && (a[0] > b[0] || a[0] == b[0] && (a[1] > b[1] || a[1] == b[1] && a[2] > b[2]))
}

// updateHint is the one command that brings this build forward, which depends
// on how it got here.
func updateHint(origin string) string {
	switch origin {
	case "plugin":
		return "the plugin fetches it on the next editor start once the daemon has exited"
	case "container":
		return "pull the newer image and recreate the container"
	case "go install":
		return "go install gitlab.com/quittymr/shoulder-daemon/relay/cmd/shoulderd@latest"
	case "release":
		return "download the newer release binary"
	}
	return "git pull && make update"
}
