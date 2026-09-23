package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
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

// affirmative reports whether content states a restriction as what holds
// rather than as what must not happen, and names the word that decided it.
//
// The words are the store's own, and the check is on the whole sentence rather
// than its opening. Both of those are lessons: "Secrets must not be committed
// into this repository." opens with its subject and is still a prohibition,
// and "only commit data that isn't secret" opens affirmatively and is still
// read as a denial by the polarity gate - which is the gate that decides
// whether a later reversal collides with this record and supersedes it, so a
// negator anywhere in the sentence costs the same thing.
func affirmative(content string) (bool, string) {
	w, found := memory.Negator(content)
	return !found, w
}

// TestLiveProhibitionIsStoredAffirmatively is the rule in prompts.Decision
// measured rather than asserted: a turn that states a prohibition has to come
// back as a fact stating the same restriction as what holds, with its subject
// intact.
//
// It runs against whatever provider this machine is configured with, read the
// way the CLI reads it. TestMain points every other test in this package at an
// env file that does not exist, precisely so that no ordinary test can pick up
// somebody's real key; this one is behind SHOULDER_LIVE and has nothing to
// measure without it.
func TestLiveProhibitionIsStoredAffirmatively(t *testing.T) {
	if os.Getenv("SHOULDER_LIVE") == "" {
		t.Skip("set SHOULDER_LIVE=1")
	}
	t.Setenv("SHOULDER_ENV_FILE", "")
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)

	p, err := FromEnv()
	if err != nil {
		t.Skipf("no provider configured: %v", err)
	}
	if p == nil {
		t.Skip("SHOULDER_LLM names no provider")
	}
	t.Logf("provider %s model %s", p.Name(), ModelOf(p))

	cases := []struct {
		name, window string
		// subject is the words the rewrite has to keep. Dropping them is the
		// other way an affirmative rewrite fails: "only commit what is safe"
		// opens affirmatively and has stopped being about secrets.
		subject []string
	}{
		{"never commit secrets", liveNeverCommitSecretsWindow, []string{"secret"}},
		{"do not use var", liveDoNotUseVarWindow, []string{"var"}},
		{"never force push to main", liveNeverForcePushWindow, []string{"push", "main"}},
		{"no migrations against production", liveNoMigrationsWindow, []string{"migration", "production"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			d, err := Decide(ctx, p, prompts.Default, tc.window, nil)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if len(d.Facts) == 0 {
				t.Fatalf("a turn stating a rule stored nothing (inject=%q)", d.Inject)
			}
			for _, f := range d.Facts {
				t.Logf("stored %q [%s]", f.Content, f.Category)
				if ok, negator := affirmative(f.Content); !ok {
					t.Errorf("fact %q carries the negator %q, so the prohibition was stored as one", f.Content, negator)
				}
				for _, want := range tc.subject {
					if !strings.Contains(strings.ToLower(f.Content), want) {
						t.Errorf("fact %q has lost its subject %q", f.Content, want)
					}
				}
			}
		})
	}
}
