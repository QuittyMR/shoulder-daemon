package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
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
	envOnce = sync.Once{}
	envVars = nil
	t.Cleanup(func() { envOnce = sync.Once{}; envVars = nil })
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
	if got := envFile()["PATH"]; got != "" {
		t.Fatalf("only SHOULDER_ variables belong to the daemon, but PATH was read as %q", got)
	}
}

func TestAMissingFileIsSimplyNoSettings(t *testing.T) {
	t.Setenv("SHOULDER_ENV_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("SHOULDER_TOKEN", "")
	envOnce = sync.Once{}
	envVars = nil
	t.Cleanup(func() { envOnce = sync.Once{}; envVars = nil })
	if got := setting("SHOULDER_TOKEN"); got != "" {
		t.Fatalf("got %q from a file that does not exist", got)
	}
}
