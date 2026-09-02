// Package scope decides where a piece of knowledge belongs.
//
// shoulder-daemon holds two kinds of thing. Some of it is about one codebase —
// "the main branch is called master", "the integration tests need a live
// Postgres" — and is noise everywhere else. The rest is about the person or the
// way they work — "prefers terse answers", "always runs the linter before
// pushing" — and follows them between repositories.
//
// There is no third option and no default. A caller that has not decided is a
// caller that will eventually pollute one project's memory with another's, so
// every entry point rejects an unset scope rather than guessing.
package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

type Scope string

const (
	// Local knowledge belongs to one project and is recalled only there.
	Local Scope = "local"
	// Global knowledge follows the user across every project.
	Global Scope = "global"
	// Any is not a storage scope. It is only meaningful as a query filter,
	// meaning "do not filter by scope".
	Any Scope = ""
)

// Parse converts a caller-supplied scope. An empty string is an error: the
// choice between local and global is never made implicitly.
func Parse(s string) (Scope, error) {
	switch Scope(strings.ToLower(strings.TrimSpace(s))) {
	case Local:
		return Local, nil
	case Global:
		return Global, nil
	case Any:
		return Any, fmt.Errorf("scope is required: pass local or global")
	}
	return Any, fmt.Errorf("unknown scope %q: expected local or global", s)
}

// Valid reports whether s is a scope a record may actually be stored under.
func (s Scope) Valid() bool { return s == Local || s == Global }

// Project identifies the project a local record belongs to.
//
// It is the root of the git worktree containing dir, so that every directory
// inside one checkout shares a single memory, and the absolute path of dir
// itself when dir is not in a repository.
func Project(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// Not checked for existence. The directory belongs to whichever machine the
	// session runs on, and the daemon is routinely somewhere else - a container,
	// another user, another host - where that path is real but unreachable.
	// Requiring it to resolve locally means every fact from a containerised
	// daemon is dropped for having no project to file it under.
	cmd := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel")
	if out, err := cmd.Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root, nil
		}
	}
	return abs, nil
}

// Key is the stable identifier a backend stores for a project. It is a hash
// rather than the path itself so that a memory backend shared between machines
// does not leak local directory layout, and so the value is safe to use as a
// tag in backends that restrict tag characters.
func Key(project string) string {
	if project == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(project)))
	return hex.EncodeToString(sum[:])[:12]
}

// Label is a short human-readable name for a project, for digests and logs. It
// is not an identifier; two projects can share one.
func Label(project string) string {
	if project == "" {
		return ""
	}
	return filepath.Base(filepath.Clean(project))
}
