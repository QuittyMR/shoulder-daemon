package config

import (
	"testing"
	"time"
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
