package main

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// setting reads one SHOULDER_ variable, falling back to the daemon's env file
// when this process does not have it.
//
// The daemon is configured from that file and the OpenCode adapter already
// reads it. Without this the CLI is the one piece that insists on an export,
// which means a terminal that has not sourced anything cannot talk to a daemon
// running perfectly well on the same machine - and the error it gives says the
// token is missing rather than that it is sitting in a file nobody read.
func setting(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return envFile()[name]
}

var (
	envOnce sync.Once
	envVars map[string]string
	envLine = regexp.MustCompile(`^\s*(?:export\s+)?(SHOULDER_[A-Z0-9_]+)\s*=\s*(.*)$`)
)

// envFile is the same file the daemon is configured from: $SHOULDER_ENV_FILE,
// or the conventional location.
func envFile() map[string]string {
	envOnce.Do(func() {
		envVars = map[string]string{}
		path := os.Getenv("SHOULDER_ENV_FILE")
		if path == "" {
			dir := os.Getenv("XDG_CONFIG_HOME")
			if dir == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return
				}
				dir = filepath.Join(home, ".config")
			}
			path = filepath.Join(dir, "shoulder-daemon", "env")
		}
		f, err := os.Open(path)
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
