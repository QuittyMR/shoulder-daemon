package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

// completion wraps one model answer as the chat completion the provider reads.
func completion(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": content}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// opened records a prompt for the session without a consult, so the advisor
// is asked by PromptAdvice alone.
func opened(s *stack, sid, prompt string) uint64 {
	count, _ := s.pipe.Registry.Observe(session.Event{SessionID: sid, Kind: session.KindUserPrompt, Prompt: prompt, TS: time.Now()})
	return count
}

var questionRule = memory.Record{ID: "mem_q", Content: "When the user asks a question, the answer comes first.", Category: "rule", Scope: scope.Global}

// The prompt-time judgement reads the prompt and the stored rules, and hands
// back the rule the model repeats as advice for this prompt.
func TestPromptAdviceRepeatsTheRuleTheModelPicks(t *testing.T) {
	ts := advisorServer(t, 0, completion(t, `{"inject":"Stored: when the user asks a question, the answer comes first."}`))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{listed: map[scope.Scope][]memory.Record{scope.Global: {questionRule}}}
	s.pipe.Memory = mem
	count := opened(s, "s1", "why does the build fail?")

	a, ok := s.pipe.PromptAdvice(context.Background(), "s1", "why does the build fail?", count)
	if !ok || !strings.HasPrefix(a.Text, "Stored: when the user asks a question") {
		t.Fatalf("got %+v %v", a, ok)
	}
	if a.Level != session.LevelPlan || a.CreatedEvent != count || a.SessionID != "s1" {
		t.Fatalf("the advice is not a plan note for this prompt: %+v", a)
	}
	_, listed := mem.reads()
	if len(listed) == 0 || listed[0].Scope != scope.Global {
		t.Fatalf("the stored rules were not listed: %+v", listed)
	}
}

// An empty answer, a store with nothing that governs, and no model each
// leave the prompt as it was.
func TestPromptAdviceIsSilentWhenThereIsNothingToSay(t *testing.T) {
	ts := advisorServer(t, 0, completion(t, `{"inject":""}`))
	s := newStack(t, ts.URL, 2*time.Second)
	s.pipe.Memory = &fakeMemory{listed: map[scope.Scope][]memory.Record{scope.Global: {questionRule}}}
	count := opened(s, "s2", "move every task to in progress")
	if a, ok := s.pipe.PromptAdvice(context.Background(), "s2", "move every task to in progress", count); ok {
		t.Fatalf("an empty answer became advice: %+v", a)
	}

	s.pipe.Memory = &fakeMemory{listed: map[scope.Scope][]memory.Record{
		scope.Global: {{ID: "mem_f", Content: "the build takes a minute", Category: "finding", Scope: scope.Global}},
	}}
	if a, ok := s.pipe.PromptAdvice(context.Background(), "s2", "why?", count); ok {
		t.Fatalf("a store with no rule was asked about and answered: %+v", a)
	}
}

// An answer that is not the object is asked for once more within the same
// bound; the second answer is read.
func TestPromptAdviceAsksAgainWhenTheAnswerIsNotTheObject(t *testing.T) {
	ts := sequencedAdvisor(t,
		completion(t, "Sure! The rule about questions applies here."),
		completion(t, `{"inject":"Stored: the answer comes first."}`))
	s := newStack(t, ts.URL, 2*time.Second)
	s.pipe.Memory = &fakeMemory{listed: map[scope.Scope][]memory.Record{scope.Global: {questionRule}}}
	count := opened(s, "s3", "why?")

	a, ok := s.pipe.PromptAdvice(context.Background(), "s3", "why?", count)
	if !ok || a.Text != "Stored: the answer comes first." {
		t.Fatalf("got %+v %v", a, ok)
	}
	if got := s.pipe.Metrics.Get("shoulder_prompt_advice_retry_total"); got != 1 {
		t.Fatalf("retries counted %v, want 1", got)
	}
}

// A model that does not answer in time leaves the prompt as it was, and the
// failure is not held against the model: the background consult still runs.
func TestPromptAdviceGivesUpWhenTheModelIsSlow(t *testing.T) {
	ts := advisorServer(t, 500*time.Millisecond, completion(t, `{"inject":"Stored: late."}`))
	s := newStack(t, ts.URL, 5*time.Second)
	s.pipe.Memory = &fakeMemory{listed: map[scope.Scope][]memory.Record{scope.Global: {questionRule}}}
	count := opened(s, "s4", "why?")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if a, ok := s.pipe.PromptAdvice(ctx, "s4", "why?", count); ok {
		t.Fatalf("a late answer became advice: %+v", a)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("PromptAdvice waited %v past its context", took)
	}
	if got := s.pipe.Metrics.Get("shoulder_prompt_advice_error_total"); got != 1 {
		t.Fatalf("errors counted %v, want 1", got)
	}
	if got := s.pipe.Metrics.Get("shoulder_advisor_error_total"); got != 0 {
		t.Fatalf("the slow prompt answer was counted against the advisor: %v", got)
	}
}
