package config

import (
	"log/slog"
	"testing"
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
