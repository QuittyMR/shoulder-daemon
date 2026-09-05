package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
)

// The decision pass is a tool loop, not one question: the pipeline lets the
// model take several round trips and each gap between two of them can hold a
// memory lookup. A default that cannot fit that shape times out precisely the
// pass in which the model used the tools it was given.
func TestAdvisorTimeoutFitsAToolLoop(t *testing.T) {
	const (
		roundTrips   = 4
		perRoundTrip = 10 * time.Second
		lookups      = roundTrips - 1
		perLookup    = 10 * time.Second
	)
	floor := roundTrips*perRoundTrip + lookups*perLookup

	t.Setenv("ADVISOR_TIMEOUT_SECONDS", "")
	if got := Load().AdvisorTimeout; got < floor {
		t.Fatalf("AdvisorTimeout default is %s, too tight for %d round trips with a lookup between them (%s)", got, roundTrips, floor)
	}
}

func TestAdvisorTimeoutStaysOverridable(t *testing.T) {
	t.Setenv("ADVISOR_TIMEOUT_SECONDS", "5")
	if got := Load().AdvisorTimeout; got != 5*time.Second {
		t.Fatalf("AdvisorTimeout is %s, want 5s", got)
	}
}

// The starting pickiness is read by name or by number, and a value that is
// neither is forgiven rather than fatal: the daemon is still useful at the
// default, and a typo in a tuning knob is not worth a machine that comes up
// with no memory at all.
func TestPickinessIsReadFromTheEnvironmentAndForgivesATypo(t *testing.T) {
	cases := []struct {
		set  string
		want prompts.Pickiness
	}{
		{"strict", prompts.Strict},
		{"STRICT", prompts.Strict},
		{"0", prompts.Eager},
		{"", prompts.Default},
		{"   ", prompts.Default},
		{"picky", prompts.Default},
		{"9", prompts.Default},
	}
	for _, c := range cases {
		t.Run(c.set, func(t *testing.T) {
			t.Setenv("SHOULDER_PICKINESS", c.set)
			if got := Load().Pickiness; got != c.want {
				t.Fatalf("SHOULDER_PICKINESS=%q started the daemon at %v, want %v", c.set, got, c.want)
			}
		})
	}
}

func TestLogPathDefaultsToAFileAndStderrMeansNone(t *testing.T) {
	t.Setenv("SHOULDER_ENV_FILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	t.Setenv("SHOULDER_LOG", "")
	if got, want := Load().LogPath, DefaultLogPath(); got != want || !strings.HasSuffix(got, filepath.Join("shoulder-daemon", "shoulderd.log")) {
		t.Errorf("unset: got %q, want %q", got, want)
	}
	t.Setenv("SHOULDER_LOG", "stderr")
	if got := Load().LogPath; got != "" {
		t.Errorf("stderr: got %q, want no file", got)
	}
	t.Setenv("SHOULDER_LOG", "/var/log/shoulderd.log")
	if got := Load().LogPath; got != "/var/log/shoulderd.log" {
		t.Errorf("explicit: got %q", got)
	}
}
