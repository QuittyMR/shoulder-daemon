package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
)

// TestLiveDecisionAcrossProviders checks the thing that actually breaks with
// small models: whether they return a parseable decision document.
func TestLiveDecisionAcrossProviders(t *testing.T) {
	if os.Getenv("SHOULDER_LIVE") == "" {
		t.Skip("set SHOULDER_LIVE=1")
	}
	cases := []struct{ name, preset string }{
		{"glm-coding", "glm-coding"},
		{"gemini", "gemini"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHOULDER_LLM", tc.preset)
			p, err := FromEnv()
			if err != nil {
				t.Skipf("not configured: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			d, err := Decide(ctx, p, prompts.Default, liveContradictionWindow, liveContradictionRecall)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			t.Logf("inject=%q facts=%d", d.Inject, len(d.Facts))
			if d.Inject == "" {
				t.Error("a stored constraint contradicting the turn should have produced an injection")
			}
		})
	}
}

// TestLiveProcedureIsSurfaced pins the other reason to speak: a stored fact
// that answers how to do what was just asked. A model that only reports
// contradictions leaves the assistant to rediscover what the store holds.
func TestLiveProcedureIsSurfaced(t *testing.T) {
	if os.Getenv("SHOULDER_LIVE") == "" {
		t.Skip("set SHOULDER_LIVE=1")
	}
	for _, preset := range []string{"glm-coding", "gemini"} {
		t.Run(preset, func(t *testing.T) {
			t.Setenv("SHOULDER_LLM", preset)
			p, err := FromEnv()
			if err != nil {
				t.Skipf("not configured: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			d, err := Decide(ctx, p, prompts.Default, liveProcedureWindow, liveProcedureRecall)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			t.Logf("inject=%q facts=%d", d.Inject, len(d.Facts))
			if !strings.Contains(strings.ToLower(d.Inject), "make release") {
				t.Errorf("a stored procedure for the thing asked should have been surfaced, got %q", d.Inject)
			}
		})
	}
}

// TestLiveSilenceIsReachable is the more important half: an unremarkable turn
// must produce no injection. A model that always speaks is useless here.
func TestLiveSilenceIsReachable(t *testing.T) {
	if os.Getenv("SHOULDER_LIVE") == "" {
		t.Skip("set SHOULDER_LIVE=1")
	}
	for _, preset := range []string{"glm-coding", "gemini"} {
		t.Run(preset, func(t *testing.T) {
			t.Setenv("SHOULDER_LLM", preset)
			p, err := FromEnv()
			if err != nil {
				t.Skipf("not configured: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			d, err := Decide(ctx, p, prompts.Default, "<user>what is 2+2</user>\n<assistant>4</assistant>", nil)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			t.Logf("inject=%q facts=%d", d.Inject, len(d.Facts))
			if d.Inject != "" {
				t.Errorf("an unremarkable turn should stay silent, got %q", d.Inject)
			}
		})
	}
}
