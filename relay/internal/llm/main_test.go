package llm

import (
	"os"
	"path/filepath"
	"testing"
)

// The presets read the daemon's env file when the environment is silent, which
// is what lets an editor started from a desktop launcher find a key nobody
// exported. It also means a test that clears the environment reads whatever
// the person running it has configured for their own daemon — their real
// provider key, in the output of a failing assertion. Every test here gets a
// file that does not exist instead.
func TestMain(m *testing.M) {
	if err := os.Setenv("SHOULDER_ENV_FILE", filepath.Join(os.TempDir(), "shoulder-daemon-no-such-env-file")); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
