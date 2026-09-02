package pipeline

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/httpapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

// observingMemory wraps a real connector and records every call so a scenario
// can assert on what was searched, stored and superseded.
type observingMemory struct {
	inner     memory.Connector
	t         *testing.T
	searches  []int // result count per search
	stored    []string
	superOld  []string
	forgotten []string
}

func (o *observingMemory) Name() string { return o.inner.Name() }

func (o *observingMemory) Search(ctx context.Context, q memory.Query) ([]memory.Record, error) {
	res, err := o.inner.Search(ctx, q)
	o.searches = append(o.searches, len(res))
	o.t.Logf("    search[%s](%q) -> %d result(s)", q.Scope, textutil.Clip(q.Text, 60), len(res))
	for _, r := range res {
		o.t.Logf("      recalled: %s %.3f %q", r.ID[:8], r.Score, textutil.Clip(r.Content, 60))
	}
	return res, err
}

func (o *observingMemory) List(ctx context.Context, q memory.Query) ([]memory.Record, error) {
	res, err := o.inner.List(ctx, q)
	o.t.Logf("    list[%s] -> %d result(s)", q.Scope, len(res))
	return res, err
}

func (o *observingMemory) Store(ctx context.Context, r memory.Record) (string, error) {
	id, err := o.inner.Store(ctx, r)
	if err == nil {
		o.stored = append(o.stored, r.Content)
		o.t.Logf("    STORE ok %s %q", id[:8], textutil.Clip(r.Content, 60))
	} else {
		o.t.Logf("    STORE refused: %v", err)
	}
	return id, err
}

func (o *observingMemory) Supersede(ctx context.Context, old string, r memory.Record) (string, error) {
	id, err := o.inner.Supersede(ctx, old, r)
	if err == nil {
		o.superOld = append(o.superOld, old)
		o.stored = append(o.stored, r.Content)
		o.t.Logf("    SUPERSEDE %s -> %s %q", old[:8], id[:8], textutil.Clip(r.Content, 60))
	} else {
		o.t.Logf("    SUPERSEDE failed: %v", err)
	}
	return id, err
}

func (o *observingMemory) Forget(ctx context.Context, id string, q memory.Query) error {
	err := o.inner.Forget(ctx, id, q)
	o.forgotten = append(o.forgotten, id)
	o.t.Logf("    FORGET %s -> %v", id, err)
	return err
}

// TestLiveBranchScenario walks the exact sequence that matters: a new fact, a
// recall, an unrelated turn, and a contradiction that must both store and
// supersede.
func TestLiveBranchScenario(t *testing.T) {
	url := os.Getenv("SHOULDER_MEMORY_URL")
	if url == "" || os.Getenv("SHOULDER_LLM") == "" {
		t.Skip("set SHOULDER_MEMORY_URL and SHOULDER_LLM")
	}
	provider, err := llm.FromEnv()
	if err != nil {
		t.Fatalf("provider: %v", err)
	}

	reg := session.NewRegistry(100)
	box := outbox.New()
	q := make(chan session.Event, 128)
	srv := httpapi.New(reg, box, q, "", budget.Default())

	cfg := config.Load()
	cfg.WindowEvents, cfg.WindowChars = 40, 12000
	cfg.AdvisorTimeout = 90 * time.Second
	g := budget.Default()
	g.MinTurnGap = 0
	cfg.Budget = g

	mem := &observingMemory{
		inner: memory.NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 30*time.Second),
		t:     t,
	}
	p := &Pipeline{
		Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: srv.Metrics, Registry: reg, Outbox: box,
		Settings: settings.ForProvider(provider), Memory: mem, Queue: q,
	}

	sid := "scenario"
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	turn := func(label, userText, assistantText string) {
		t.Logf("  TURN: %s", label)
		reg.Observe(session.Event{
			Protocol: 1, Harness: "test", SessionID: sid, TS: time.Now(),
			Kind: session.KindUserPrompt, CWD: cwd, Prompt: userText,
		})
		reg.Observe(session.Event{
			Protocol: 1, Harness: "test", SessionID: sid, TS: time.Now(),
			Kind: session.KindTurnEnd, CWD: cwd, Assistant: assistantText,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		p.Consult(ctx, sid)
		if a, ok := box.Take(sid, reg.Turn(sid), session.KindUserPrompt); ok {
			t.Logf("    INJECT: %q", textutil.Clip(a.Text, 100))
		} else {
			t.Logf("    INJECT: (none)")
		}
	}

	turn("1 — new fact", "the main branch is main", "Understood, the main branch is main.")
	turn("2 — should recall", "I should commit this", "I'll commit your changes.")
	turn("3 — unrelated", "I'm on the feature-a branch", "Noted, you're on feature-a.")
	turn("4 — contradiction", "the main branch is now master", "Understood, the main branch is now master.")

	t.Logf("SUMMARY searches=%v stored=%d superseded=%d", mem.searches, len(mem.stored), len(mem.superOld))
	for _, s := range mem.stored {
		t.Logf("  stored: %q", textutil.Clip(s, 70))
	}

	project, err := scope.Project(cwd)
	if err != nil {
		t.Fatal(err)
	}
	final, err := mem.inner.Search(context.Background(), memory.Query{
		Text: "what is the main branch called", Limit: 10, Scope: scope.Local, Project: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("FINAL recall for 'what is the main branch called': %d result(s)", len(final))
	for _, r := range final {
		t.Logf("  %s %.3f %q", r.ID[:8], r.Score, textutil.Clip(r.Content, 70))
	}
}
