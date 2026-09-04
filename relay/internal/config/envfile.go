package config

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// The daemon is started by an editor, and an editor started from a desktop
// launcher has none of the exports from anybody's shell profile. That is the
// single most common way an install ends up watching sessions and never
// speaking: the variables were set, in a shell nothing here ever sees.
//
// So there is one file. Everything that configures the daemon can live in it,
// the CLI and the OpenCode adapter already read it, and this is what makes the
// daemon read it too — a memory service or a model key is then one line in one
// place, rather than a value that has to be threaded into whatever environment
// the harness happens to launch with.
//
// The process environment always wins. A value somebody exported deliberately
// is not overridden by a file they may have forgotten.
var envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Z][A-Z0-9_]*)\s*=\s*(.*)$`)

var (
	envOnce sync.Once
	envVars map[string]string
)

// EnvFilePath is $SHOULDER_ENV_FILE, or the conventional location.
func EnvFilePath() string {
	if path := os.Getenv("SHOULDER_ENV_FILE"); path != "" {
		return path
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "shoulder-daemon", "env")
}

// envFile reads the file once. A missing one is the ordinary case and not an
// error: most installs configure the daemon through its environment.
func envFile() map[string]string {
	envOnce.Do(func() {
		envVars = map[string]string{}
		path := EnvFilePath()
		if path == "" {
			return
		}
		f, err := os.Open(path) //nolint:gosec // G304: SHOULDER_ENV_FILE is the operator's own setting
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			m := envLine.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			envVars[m[1]] = strings.Trim(strings.TrimSpace(m[2]), `"'`)
		}
	})
	return envVars
}

// Setting reads one variable from the environment, falling back to the file.
// It is exported because the CLI resolves the address and the token the same
// way: a terminal that has sourced nothing still has to reach the daemon.
func Setting(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return envFile()[name]
}

// ResetEnvFile drops the cached file. It exists for tests, which write one and
// then expect it read; a daemon reads it once and keeps it.
func ResetEnvFile() {
	envOnce = sync.Once{}
	envVars = nil
}
