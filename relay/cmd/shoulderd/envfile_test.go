package main

import (
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

// The file is read once per process. Each test here wants its own file, so
// the once is reset between them.
func resetEnvFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHOULDER_ENV_FILE", path)
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)
	return path
}

func TestASettingInTheShellWinsOverTheFile(t *testing.T) {
	resetEnvFile(t, `SHOULDER_TOKEN="from-file"`+"\n")
	t.Setenv("SHOULDER_TOKEN", "from-shell")
	if got := setting("SHOULDER_TOKEN"); got != "from-shell" {
		t.Fatalf("setting = %q, want the exported value", got)
	}
}

func TestASettingMissingFromTheShellComesFromTheDaemonsFile(t *testing.T) {
	resetEnvFile(t, "# the daemon's env\nexport SHOULDER_TOKEN='quoted'\nSHOULDER_ADDR = 127.0.0.1:9000\nPATH=/nope\nnot a line\n")
	t.Setenv("SHOULDER_TOKEN", "")
	t.Setenv("SHOULDER_ADDR", "")

	if got := setting("SHOULDER_TOKEN"); got != "quoted" {
		t.Fatalf("export with single quotes read as %q", got)
	}
	if got := setting("SHOULDER_ADDR"); got != "127.0.0.1:9000" {
		t.Fatalf("spaced assignment read as %q", got)
	}
	// Every assignment is read, not only the SHOULDER_ ones: the daemon is
	// configured from this file and a provider key is not called SHOULDER_
	// anything. Nothing is applied to the process — a name is only ever looked
	// up because something asked for it — so a PATH in the file is inert.
	if got := setting("GEMINI_API_KEY"); got != "" {
		t.Fatalf("a key that is not in the file was read as %q", got)
	}
}

func TestAMissingFileIsSimplyNoSettings(t *testing.T) {
	t.Setenv("SHOULDER_ENV_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("SHOULDER_TOKEN", "")
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)
	if got := setting("SHOULDER_TOKEN"); got != "" {
		t.Fatalf("got %q from a file that does not exist", got)
	}
}
