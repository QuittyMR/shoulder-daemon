package main

import (
	"os"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

// setting reads one variable the way the daemon does — the environment, then
// the daemon's env file — and then, for the token alone, the file the daemon
// generated it into.
//
// Without this the CLI is the one piece that insists on an export, which means
// a terminal that has not sourced anything cannot talk to a daemon running
// perfectly well on the same machine, and the error it gives says the token is
// missing rather than that it is sitting in a file nobody read.
func setting(name string) string {
	if v := config.Setting(name); v != "" {
		return v
	}
	// The token is the one setting nobody writes down: the daemon generates it
	// on first start and keeps it beside its own state.
	if name == "SHOULDER_TOKEN" {
		if raw, err := os.ReadFile(tokenPath()); err == nil { //nolint:gosec // G304: our own state directory
			return strings.TrimSpace(string(raw))
		}
	}
	return ""
}

// envFilePath is where that file lives.
func envFilePath() string { return config.EnvFilePath() }
