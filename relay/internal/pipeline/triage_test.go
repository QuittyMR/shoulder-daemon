package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// fakeTriage answers every turn with the same verdict and keeps what it was
// shown.
type fakeTriage struct {
	verdict llm.Verdict
	err     error
	min     float64
	// stall holds every call until its context ends, as a triage service
	// that has stopped answering would.
	stall bool

	mu       sync.Mutex
	windows  []string
	recalled [][]memory.Record
}

func (f *fakeTriage) Name() string           { return "fake" }
func (f *fakeTriage) MinConfidence() float64 { return f.min }

func (f *fakeTriage) Triage(ctx context.Context, window string, recalled []memory.Record) (llm.Verdict, error) {
	f.mu.Lock()
	f.windows = append(f.windows, window)
	f.recalled = append(f.recalled, recalled)
	f.mu.Unlock()
	if f.stall {
		<-ctx.Done()
		return llm.Verdict{}, ctx.Err()
	}
	return f.verdict, f.err
}

func (f *fakeTriage) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.windows)
}

const reminder = "Use pnpm, never npm."

// triageStack is a pipeline with a triage in front of a decision model that
// would inject its own marker, so a test can see which of the two answered.
func triageStack(t *testing.T, v llm.Verdict, err error) (*stack, *fakeTriage, *recordingAdvisor, *fakeMemory) {
	t.Helper()
	adv, ts := newRecordingAdvisor(t, decisionBody(t, "the decision model's advice"))
	s := newStack(t, ts.URL, 2*time.Second)
	tr := &fakeTriage{verdict: v, err: err, min: 0.6}
	mem := &fakeMemory{recalled: map[scope.Scope][]memory.Record{
		scope.Global: {{ID: "f1", Scope: scope.Global, Category: "preference", Content: reminder}},
	}}
	s.pipe.Triage = tr
	s.pipe.Memory = mem
	return s, tr, adv, mem
}

// endTurn ends one turn in which the agent recorded a fact explicitly, and
// waits for the one consult it causes. A prompt would cause a second, racing
// the first for the same counters.
func endTurn(t *testing.T, s *stack) {
	t.Helper()
	s.pipe.Registry.AddFact("s1", facts.Fact{Content: "Deploys happen on Fridays.", Category: "convention", Scope: scope.Global})
	s.post(t, "Stop", stop("s1", "Running npm install."))
	select {
	case <-s.consults:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn was never consulted on")
	}
}

// delivered is what the session is handed on its next prompt.
func delivered(t *testing.T, s *stack) string {
	t.Helper()
	return s.post(t, "UserPromptSubmit", prompt("s1", "next"))
}

// atToolCall is what the session is handed at its next tool call, which is
// where a warning about an action has to land.
func atToolCall(t *testing.T, s *stack) string {
	t.Helper()
	return s.post(t, "PreToolUse", `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"npm install"}}`)
}

// withPickiness keeps the stack's decision model and sets how picky it is.
func withPickiness(s *stack, pick prompts.Pickiness) {
	s.pipe.Settings = settings.New(nil, pick, "", "", s.pipe.Settings.Provider())
}

func TestASureNothingSkipsTheDecisionModel(t *testing.T) {
	s, tr, adv, mem := triageStack(t, llm.Verdict{Action: llm.Nothing, Confidence: 0.9}, nil)

	endTurn(t, s)
	if n := len(adv.requests()); n != 0 {
		t.Fatalf("the decision model was asked %d times after a sure nothing", n)
	}
	if s.srv.Metrics.Get("shoulder_triage_nothing_total") != 1 {
		t.Fatal("the verdict was not counted")
	}
	// Triage sees what the decision model would have: the turn and what it
	// recalled.
	tr.mu.Lock()
	windows, recalled := tr.windows, tr.recalled
	tr.mu.Unlock()
	if len(windows) != 1 || !strings.Contains(windows[0], "npm install") ||
		len(recalled[0]) != 1 || recalled[0][0].ID != "f1" {
		t.Fatalf("triage was shown %q with %+v", windows, recalled)
	}
	// The decision model is what writes the facts the agent recorded
	// explicitly, so a turn settled without it must still write them.
	if stored, _, _ := mem.snapshot(); len(stored) != 1 || stored[0].Content != "Deploys happen on Fridays." {
		t.Fatalf("stored = %+v", stored)
	}
	if got := delivered(t, s); strings.TrimSpace(got) != "{}" {
		t.Fatalf("nothing must mean silence, got %q", got)
	}
}

func TestASureInjectRepeatsTheStoredFact(t *testing.T) {
	s, _, adv, mem := triageStack(t, llm.Verdict{Action: llm.Inject, Confidence: 0.8, FactID: "f1"}, nil)

	endTurn(t, s)
	if n := len(adv.requests()); n != 0 {
		t.Fatalf("the decision model was asked %d times after a sure inject", n)
	}
	if s.srv.Metrics.Get("shoulder_triage_inject_total") != 1 || s.srv.Metrics.Get("shoulder_advice_queued_total") != 1 {
		t.Fatal("the injection was not counted")
	}
	if stored, _, _ := mem.snapshot(); len(stored) != 1 {
		t.Fatalf("the explicit fact was not stored: %+v", stored)
	}
	if got := delivered(t, s); strings.Contains(got, reminder) {
		t.Fatalf("the stored fact was spent on the prompt, after the action it is about: %s", got)
	}
	if got := atToolCall(t, s); !strings.Contains(got, reminder) {
		t.Fatalf("the stored fact was not delivered at the tool call, got %s", got)
	}
}

// Everything triage cannot settle goes to the decision model exactly as it
// would have without it.
func TestTriageHandsTheRestToTheDecisionModel(t *testing.T) {
	cases := []struct {
		name    string
		verdict llm.Verdict
		err     error
		metric  string
	}{
		{"create", llm.Verdict{Action: llm.Create, Confidence: 0.9}, nil, "shoulder_triage_create_total"},
		{"update", llm.Verdict{Action: llm.Update, Confidence: 0.9, FactID: "f1"}, nil, "shoulder_triage_update_total"},
		{"unsure nothing", llm.Verdict{Action: llm.Nothing, Confidence: 0.4}, nil, "shoulder_triage_unsure_total"},
		{"unsure inject", llm.Verdict{Action: llm.Inject, Confidence: 0.59, FactID: "f1"}, nil, "shoulder_triage_unsure_total"},
		{"error", llm.Verdict{}, errors.New("jev status 503"), "shoulder_triage_error_total"},
		{"fact not recalled", llm.Verdict{Action: llm.Inject, Confidence: 0.9, FactID: "f9"}, nil, "shoulder_triage_error_total"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, adv, _ := triageStack(t, c.verdict, c.err)

			endTurn(t, s)
			if len(adv.requests()) != 1 {
				t.Fatalf("the decision model was asked %d times, want once", len(adv.requests()))
			}
			if s.srv.Metrics.Get(c.metric) != 1 {
				t.Fatalf("%s was not counted", c.metric)
			}
			if s.srv.Metrics.Get("shoulder_triage_unhandled_total") != 0 {
				t.Fatal("a turn the decision model took was counted as unhandled")
			}
			got := delivered(t, s)
			if !strings.Contains(got, "the decision model's advice") {
				t.Fatalf("the decision model's answer was not delivered, got %s", got)
			}
			if strings.Contains(got, reminder) {
				t.Fatal("triage injected on a verdict it does not settle")
			}
		})
	}
}

// Triage alone is a supported configuration: it can still say nothing or
// repeat a stored fact, and what it cannot act on is counted.
func TestTriageWithoutADecisionModel(t *testing.T) {
	cases := []struct {
		name      string
		verdict   llm.Verdict
		err       error
		injected  bool
		unhandled uint64
	}{
		{"nothing", llm.Verdict{Action: llm.Nothing, Confidence: 0.9}, nil, false, 0},
		{"inject", llm.Verdict{Action: llm.Inject, Confidence: 0.9, FactID: "f1"}, nil, true, 0},
		{"create", llm.Verdict{Action: llm.Create, Confidence: 0.9}, nil, false, 1},
		{"update", llm.Verdict{Action: llm.Update, Confidence: 0.9, FactID: "f1"}, nil, false, 1},
		{"unsure create", llm.Verdict{Action: llm.Create, Confidence: 0.3}, nil, false, 0},
		{"error", llm.Verdict{}, errors.New("jev status 503"), false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, tr, adv, mem := triageStack(t, c.verdict, c.err)
			s.pipe.Settings = settings.ForProvider(nil)

			endTurn(t, s)
			if tr.calls() != 1 {
				t.Fatalf("triage was asked %d times", tr.calls())
			}
			if len(adv.requests()) != 0 {
				t.Fatal("a decision model that is not configured was asked")
			}
			if n := s.srv.Metrics.Get("shoulder_triage_unhandled_total"); n != c.unhandled {
				t.Fatalf("unhandled = %d, want %d", n, c.unhandled)
			}
			if stored, _, _ := mem.snapshot(); len(stored) != 1 {
				t.Fatalf("the explicit fact was not stored: %+v", stored)
			}
			if got := atToolCall(t, s); strings.Contains(got, reminder) != c.injected {
				t.Fatalf("injected = %v, want %v: %s", !c.injected, c.injected, got)
			}
		})
	}
}

// With neither a decision model nor a triage the daemon observes in silence,
// as it did before triage existed: nothing is asked and nothing is written.
func TestNoTriageAndNoDecisionModelDoesNothing(t *testing.T) {
	s, tr, _, mem := triageStack(t, llm.Verdict{Action: llm.Nothing, Confidence: 0.9}, nil)
	s.pipe.Triage = nil
	s.pipe.Settings = settings.ForProvider(nil)

	endTurn(t, s)
	if tr.calls() != 0 {
		t.Fatal("a triage that was taken away was asked")
	}
	if stored, _, _ := mem.snapshot(); len(stored) != 0 {
		t.Fatalf("stored = %+v", stored)
	}
}

// A sure "nothing" settles the turn only where the decision model would also
// have been told to leave an implied rule alone. Below Balanced it is told to
// store those, so the turn is still put to it.
func TestNothingSettlesOnlyAtBalancedOrStricter(t *testing.T) {
	for _, c := range []struct {
		pick  prompts.Pickiness
		asked bool
	}{
		{prompts.Eager, true},
		{prompts.Open, true},
		{prompts.Balanced, false},
		{prompts.Careful, false},
		{prompts.Strict, false},
	} {
		t.Run(c.pick.String(), func(t *testing.T) {
			s, _, adv, _ := triageStack(t, llm.Verdict{Action: llm.Nothing, Confidence: 0.95}, nil)
			withPickiness(s, c.pick)

			endTurn(t, s)
			if asked := len(adv.requests()) > 0; asked != c.asked {
				t.Fatalf("decision model asked = %v, want %v", asked, c.asked)
			}
			if s.srv.Metrics.Get("shoulder_triage_nothing_total") != 1 {
				t.Fatal("the verdict was not counted")
			}
		})
	}
}

// A triage that stops answering is cut off on its own budget, and the decision
// model still gets the whole of its own.
func TestAStalledTriageLeavesTheDecisionModelItsTime(t *testing.T) {
	s, tr, adv, _ := triageStack(t, llm.Verdict{}, nil)
	tr.stall = true

	start := time.Now()
	endTurn(t, s)
	if took := time.Since(start); took > s.pipe.Cfg.AdvisorTimeout {
		t.Fatalf("the turn took %v, past the advisor timeout", took)
	}
	if len(adv.requests()) != 1 {
		t.Fatalf("the decision model was asked %d times, want once", len(adv.requests()))
	}
	if s.srv.Metrics.Get("shoulder_triage_error_total") != 1 || s.srv.Metrics.Get("shoulder_advisor_error_total") != 0 {
		t.Fatal("the stall should be a triage error and nothing else")
	}
	if got := delivered(t, s); !strings.Contains(got, "the decision model's advice") {
		t.Fatalf("the decision model's answer was not delivered, got %s", got)
	}
	if !strings.Contains(s.srv.Metrics.Render(), `shoulder_hook_latency_seconds_count{event="triage"} 1`) {
		t.Fatal("the triage call's latency was not observed")
	}
}
