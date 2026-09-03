package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// recorder is a provider that keeps the prompt it was given, which is what a
// test of prompt assembly needs and nothing more.
type recorder struct {
	system, user string
	reply        string
	err          error
}

func (r *recorder) Name() string { return "recorder" }

func (r *recorder) Complete(_ context.Context, system, user string) (string, error) {
	r.system, r.user = system, user
	return r.reply, r.err
}

func (r *recorder) Chat(_ context.Context, _ []Message, _ []Tool) (Message, error) {
	return Message{}, errors.New("not a chat")
}

func TestDecidePutsTheWindowAndEveryRecalledFactInFrontOfTheModel(t *testing.T) {
	p := &recorder{reply: `{"inject":"branch is master","facts":[]}`}
	recalled := []memory.Record{
		{ID: "r1", Scope: scope.Global, Category: "preference", Content: "terse answers"},
		{ID: "r2", Scope: scope.Local, Category: "structure", Content: "main branch is master"},
	}
	d, err := Decide(context.Background(), p, prompts.Strict, "<user>rebase onto main</user>", recalled)
	if err != nil {
		t.Fatal(err)
	}
	if d.Inject != "branch is master" {
		t.Fatalf("decision = %+v", d)
	}
	if p.system != prompts.Decision(prompts.Strict) {
		t.Fatal("the pickiness asked for was not the one sent")
	}
	for _, want := range []string{
		"<recent-turn>\n<user>rebase onto main</user>\n</recent-turn>",
		"id=r1 scope=global category=preference: terse answers",
		"id=r2 scope=local category=structure: main branch is master",
	} {
		if !strings.Contains(p.user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p.user)
		}
	}
	if strings.Contains(p.user, "(none matched)") {
		t.Fatal("said nothing matched while handing over two facts")
	}
}

func TestDecideSaysSoWhenNothingWasRecalled(t *testing.T) {
	p := &recorder{reply: `{"inject":"","facts":[]}`}
	if _, err := Decide(context.Background(), p, prompts.Default, "w", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.user, "<stored-facts>\n(none matched)") {
		t.Fatalf("an empty recall must be stated, not left as a blank block:\n%s", p.user)
	}
}

func TestDecideReturnsTheProvidersErrorUntouched(t *testing.T) {
	boom := errors.New("rate limited")
	p := &recorder{err: boom}
	if _, err := Decide(context.Background(), p, prompts.Default, "w", nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the provider's", err)
	}
	p = &recorder{reply: "not json at all"}
	if _, err := Decide(context.Background(), p, prompts.Default, "w", nil); err == nil {
		t.Fatal("an unparseable answer must be an error, not a silent empty decision")
	}
}

func TestNoProviderConfiguredIsNotAnError(t *testing.T) {
	t.Setenv("SHOULDER_LLM", "")
	p, err := FromEnv()
	if err != nil || p != nil {
		t.Fatalf("FromEnv with nothing set = %v, %v; the daemon runs without a model", p, err)
	}
}

func TestAModelWithoutAProviderIsRefusedWithTheChoices(t *testing.T) {
	_, err := Configure("", "gpt-5.2-mini")
	if err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("err = %v; it must list the providers to pick from", err)
	}
}
