package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type stub struct {
	label string
	out   string
	err   error
	calls *int
}

func (s stub) Name() string { return s.label }
func (s stub) Complete(context.Context, string, string) (string, error) {
	if s.calls != nil {
		*s.calls++
	}
	return s.out, s.err
}
func (s stub) Chat(context.Context, []Message, []Tool) (Message, error) {
	if s.calls != nil {
		*s.calls++
	}
	return Message{Role: "assistant", Content: s.out}, s.err
}

func TestChainUsesTheFirstProviderThatWorks(t *testing.T) {
	var a, b int
	c := &Chain{Providers: []Provider{
		stub{label: "primary", out: "from primary", calls: &a},
		stub{label: "backup", out: "from backup", calls: &b},
	}}
	got, err := c.Complete(context.Background(), "s", "u")
	if err != nil || got != "from primary" {
		t.Fatalf("got %q err %v", got, err)
	}
	if b != 0 {
		t.Error("the fallback must not be called when the primary succeeds")
	}
}

func TestChainFallsBack(t *testing.T) {
	var fellBack bool
	c := &Chain{
		Providers: []Provider{
			stub{label: "primary", err: errors.New("429 rate limited")},
			stub{label: "backup", out: "from backup"},
		},
		OnFallback: func(from, to string, err error) { fellBack = true },
	}
	got, err := c.Complete(context.Background(), "s", "u")
	if err != nil || got != "from backup" {
		t.Fatalf("got %q err %v", got, err)
	}
	if !fellBack {
		t.Error("fallback should have been reported")
	}
}

func TestChainReportsEveryFailure(t *testing.T) {
	c := &Chain{Providers: []Provider{
		stub{label: "a", err: errors.New("boom a")},
		stub{label: "b", err: errors.New("boom b")},
	}}
	_, err := c.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"boom a", "boom b"} {
		if !errors.Is(err, err) || !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %v", want, err)
		}
	}
}

// A dead parent context fails every provider identically; burning the whole
// chain on it wastes a fallback and the latency budget.
func TestChainStopsOnCancelledContext(t *testing.T) {
	var b int
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Chain{Providers: []Provider{
		stub{label: "a", err: context.Canceled},
		stub{label: "b", out: "unreachable", calls: &b},
	}}
	if _, err := c.Complete(ctx, "s", "u"); err == nil {
		t.Fatal("expected an error")
	}
	if b != 0 {
		t.Error("the fallback must not run under a cancelled context")
	}
}

func TestChainName(t *testing.T) {
	c := &Chain{Providers: []Provider{stub{label: "glm-coding"}, stub{label: "gemini"}}}
	if c.Name() != "glm-coding→gemini" {
		t.Fatalf("got %q", c.Name())
	}
}

func TestChainChatUsesTheFirstProviderThatWorks(t *testing.T) {
	var b int
	c := &Chain{Providers: []Provider{
		stub{label: "primary", out: "from primary"},
		stub{label: "backup", out: "from backup", calls: &b},
	}}
	got, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "u"}}, nil)
	if err != nil || got.Content != "from primary" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if b != 0 {
		t.Error("the fallback must not be called when the primary succeeds")
	}
}

func TestChainChatFallsBack(t *testing.T) {
	var fellBack bool
	c := &Chain{
		Providers: []Provider{
			stub{label: "primary", err: errors.New("429 rate limited")},
			stub{label: "backup", out: "from backup"},
		},
		OnFallback: func(from, to string, err error) { fellBack = true },
	}
	got, err := c.Chat(context.Background(), []Message{{Role: "user", Content: "u"}}, nil)
	if err != nil || got.Content != "from backup" {
		t.Fatalf("got %+v err %v", got, err)
	}
	if !fellBack {
		t.Error("fallback should have been reported")
	}
}

func TestChainChatStopsOnCancelledContext(t *testing.T) {
	var b int
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Chain{Providers: []Provider{
		stub{label: "a", err: context.Canceled},
		stub{label: "b", out: "unreachable", calls: &b},
	}}
	if _, err := c.Chat(ctx, nil, nil); err == nil {
		t.Fatal("expected an error")
	}
	if b != 0 {
		t.Error("the fallback must not run under a cancelled context")
	}
}

func TestChainChatReportsEveryFailure(t *testing.T) {
	c := &Chain{Providers: []Provider{
		stub{label: "a", err: errors.New("boom a")},
		stub{label: "b", err: errors.New("boom b")},
	}}
	_, err := c.Chat(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"boom a", "boom b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %v", want, err)
		}
	}
}
