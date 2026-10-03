package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
)

func TestEnvTreatsEmptyAsUnset(t *testing.T) {
	t.Setenv("SHOULDER_TEST_ENV", "")
	if got := Env("SHOULDER_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("empty variable read as %q, want the default", got)
	}
	t.Setenv("SHOULDER_TEST_ENV", "set")
	if got := Env("SHOULDER_TEST_ENV", "fallback"); got != "set" {
		t.Fatalf("got %q", got)
	}
}

// A typo in a log setting must never be the reason a daemon refuses to start.
func TestLogLevelForgivesCaseSpaceAndNonsense(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{" Debug ", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"WARNING", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
	}
	for _, tc := range cases {
		if got := logLevel(tc.in); got != tc.want {
			t.Errorf("logLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNumericAndBooleanSettingsKeepTheirDefaultOnGarbage(t *testing.T) {
	t.Setenv("SHOULDER_TEST_INT", "twelve")
	if got := envInt("SHOULDER_TEST_INT", 4); got != 4 {
		t.Fatalf("envInt on garbage = %d, want the default", got)
	}
	t.Setenv("SHOULDER_TEST_INT", "12")
	if got := envInt("SHOULDER_TEST_INT", 4); got != 12 {
		t.Fatalf("envInt = %d", got)
	}
	t.Setenv("SHOULDER_TEST_BOOL", "yes please")
	if got := envBool("SHOULDER_TEST_BOOL", true); !got {
		t.Fatal("envBool on garbage must keep the default")
	}
	t.Setenv("SHOULDER_TEST_BOOL", "0")
	if got := envBool("SHOULDER_TEST_BOOL", true); got {
		t.Fatal("envBool(\"0\") must be false")
	}
	t.Setenv("SHOULDER_TEST_BOOL", "")
	if got := envBool("SHOULDER_TEST_BOOL", false); got {
		t.Fatal("an empty variable is unset")
	}
}

func TestDryRunIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("SHOULDER_DRY_RUN", "true")
	if !Load().Budget.DryRun {
		t.Fatal("SHOULDER_DRY_RUN=true did not set the gate to dry run")
	}
	t.Setenv("SHOULDER_DRY_RUN", "")
	if Load().Budget.DryRun {
		t.Fatal("dry run must be off by default")
	}
}

// The process environment wins over the file, except that a container runtime
// that loaded the file put the file's values there, and says which file.
func TestSourceSaysWhereASettingCameFrom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("SHOULDER_LLM=gemini\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHOULDER_ENV_FILE", path)
	t.Setenv(EnvFileLoaded, "")
	t.Setenv("SHOULDER_LLM", "")
	t.Setenv("SHOULDER_MEMORY", "")
	ResetEnvFile()
	t.Cleanup(ResetEnvFile)

	if got := Source("SHOULDER_LLM"); got != SourceFile {
		t.Errorf("a value only the file sets: %q", got)
	}
	if got := Source("SHOULDER_MEMORY"); got != SourceDefault {
		t.Errorf("a value nothing sets: %q", got)
	}
	t.Setenv("SHOULDER_LLM", "openrouter")
	if got, file := Source("SHOULDER_LLM"), FileSetting("SHOULDER_LLM"); got != SourceEnvironment || file != "gemini" {
		t.Errorf("an exported value: %q; the file still says %q", got, file)
	}
	if LoadedFile() != path {
		t.Errorf("LoadedFile = %q, want %q", LoadedFile(), path)
	}
	t.Setenv(EnvFileLoaded, "/host/env")
	if got := Source("SHOULDER_LLM"); got != SourceFile || LoadedFile() != "/host/env" {
		t.Errorf("under a runtime that loaded the file: %q from %q", got, LoadedFile())
	}
}

// An env file written before the key was renamed keeps its gap, the new name
// wins once it is set, and the old name is reported only when it was the one
// read.
func TestTheMinimumGapIsStillReadUnderItsOldName(t *testing.T) {
	t.Setenv("BUDGET_MIN_EVENT_GAP", "")
	t.Setenv("BUDGET_MIN_TURN_GAP", "")
	c := Load()
	if c.Budget.MinEventGap != budget.Default().MinEventGap || len(c.Renamed) != 0 {
		t.Fatalf("with neither key set: gap %d, renamed %v", c.Budget.MinEventGap, c.Renamed)
	}

	t.Setenv("BUDGET_MIN_TURN_GAP", "9")
	c = Load()
	if c.Budget.MinEventGap != 9 {
		t.Fatalf("gap = %d, want the 9 set under the old name", c.Budget.MinEventGap)
	}
	if len(c.Renamed) != 1 || c.Renamed[0] != (Renamed{Old: "BUDGET_MIN_TURN_GAP", New: "BUDGET_MIN_EVENT_GAP"}) {
		t.Fatalf("renamed = %v, want the old key and its replacement", c.Renamed)
	}

	t.Setenv("BUDGET_MIN_EVENT_GAP", "2")
	c = Load()
	if c.Budget.MinEventGap != 2 || len(c.Renamed) != 0 {
		t.Fatalf("with both set: gap %d, renamed %v; the new name must win and nothing be reported", c.Budget.MinEventGap, c.Renamed)
	}
}
