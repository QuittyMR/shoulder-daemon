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
// It is the repository's root commit, with the checkout's directory name in
// front of it for anyone reading a log. The commit is what the identity is
// actually made of: a path changes when the directory is renamed or the
// repository is cloned somewhere else, and a path-derived identity silently
// orphans every fact filed under the old one - the store still holds them and
// no read can see them. The root commit is the same in every clone and survives
// both.
//
// Two repositories collide only if they share a root commit, which means one
// was cloned or forked from the other - in which case they are arguably the
// same history anyway. A directory that is not a repository, or a repository
// with no commits yet, falls back to its absolute path. There is nothing more stable to use, and one
// project keyed by path is better than one project with no key at all.
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
	root := gitOutput(abs, "rev-parse", "--show-toplevel")
	if root == "" {
		return abs, nil
	}
	born := gitOutput(abs, "rev-list", "--max-parents=0", "HEAD")
	if born == "" {
		return abs, nil
	}
	// Several root commits means a repository with grafted or merged histories.
	// The first is stable for a given repository, which is all that is asked.
	if i := strings.IndexAny(born, " \n"); i >= 0 {
		born = born[:i]
	}
	return filepath.Base(root) + identitySep + born, nil
}

// identitySep divides the human-readable half of a project identity from the
// half that identifies it. Only the second half is hashed into a key, so
// renaming a checkout changes what a log calls it and nothing else.
const identitySep = "@"

func gitOutput(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output() //nolint:gosec // G204: git with literal arguments in the session's own directory
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Key is the stable identifier a backend stores for a project. It is a hash
// rather than the path itself so that a memory backend shared between machines
// does not leak local directory layout, and so the value is safe to use as a
// tag in backends that restrict tag characters.
func Key(project string) string {
	if project == "" {
		return ""
	}
	// Only the identifying half is hashed. The name in front of it is for
	// people, and hashing it would put a renamed checkout in a different
	// project from the one it was a moment ago.
	if i := strings.LastIndex(project, identitySep); i >= 0 {
		project = project[i+1:]
	} else {
		project = filepath.Clean(project)
	}
	sum := sha256.Sum256([]byte(project))
	return hex.EncodeToString(sum[:])[:12]
}

// Label is a short human-readable name for a project, for digests and logs. It
// is not an identifier; two projects can share one.
func Label(project string) string {
	if project == "" {
		return ""
	}
	if i := strings.LastIndex(project, identitySep); i >= 0 {
		return project[:i]
	}
	return filepath.Base(filepath.Clean(project))
}
