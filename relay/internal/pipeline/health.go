package pipeline

import (
	"context"
	"strings"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
)

// Model states, as ModelHealth reports them.
const (
	// ModelUnknown is a daemon that has not yet heard from its decision model
	// either way: the startup probe is still out, or there is no model.
	ModelUnknown = "unknown"
	ModelOK      = "ok"
	ModelFailing = "failing"
)

// ModelHealth is the last word the decision model gave: whether it answered.
// It exists because a model that refuses every call leaves a daemon that looks
// healthy from every other angle — the relay serves, the store answers, the
// hooks fire — while nothing is learned from any session. Real sessions ran
// for thirteen days in that state with only a log line per failure to show
// for it, and nothing read the log.
type ModelHealth struct {
	State string `json:"state"`
	// Since is when the current state began.
	Since time.Time `json:"since,omitempty"`
	// Failures is how many calls in a row have failed; zero when answering.
	Failures int `json:"failures,omitempty"`
	// Error is the last refusal, verbatim, so doctor can show the cause.
	Error string `json:"error,omitempty"`
}

type modelHealth struct {
	mu   sync.Mutex
	last ModelHealth
}

// ModelHealth reports how the decision model has been answering.
func (p *Pipeline) ModelHealth() ModelHealth {
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	if p.health.last.State == "" {
		return ModelHealth{State: ModelUnknown}
	}
	return p.health.last
}

// modelAnswered records a decision call that came back with a decision. The
// recovery is logged once, with how many calls it took, so the monitor shows
// the model coming back as clearly as it showed it going.
func (p *Pipeline) modelAnswered() {
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	was := p.health.last
	if was.State == ModelOK {
		return
	}
	p.health.last = ModelHealth{State: ModelOK, Since: time.Now()}
	if was.State == ModelFailing {
		p.Log.Info("decision model answers again", "after", was.Failures)
	}
}

// modelFailed records a decision call that did not come back with a decision.
// The first failure after a run of answers is logged as the state change it
// is; every later one is counted, and the per-call warning the caller already
// writes says what went wrong each time.
func (p *Pipeline) modelFailed(err error) {
	p.health.mu.Lock()
	defer p.health.mu.Unlock()
	was := p.health.last
	now := ModelHealth{State: ModelFailing, Since: was.Since, Failures: was.Failures + 1, Error: err.Error()}
	if was.State != ModelFailing {
		now.Since, now.Failures = time.Now(), 1
		p.Log.Warn("decision model stopped answering; nothing is learned until it does", "err", err)
	}
	p.health.last = now
}

// ModelChanged forgets what was known about the model that is no longer in
// use and asks the new one. It is for `config set`, which swaps the provider
// under a running daemon.
func (p *Pipeline) ModelChanged(ctx context.Context) {
	p.health.mu.Lock()
	p.health.last = ModelHealth{}
	p.health.mu.Unlock()
	p.ProbeModel(ctx)
}

// probeTimeout bounds one probe. A model that takes longer than this to say
// one word is not one a decision will get through either.
const probeTimeout = 30 * time.Second

// ProbeModel asks the decision model one trivial question and records whether
// it answered. It runs when the daemon starts and when the model is changed,
// because those are the two moments a dead key would otherwise go unnoticed
// until a session happens to need a decision — and the daemon restarts often
// enough that waiting for a session would leave the health unknown for most
// of the time anybody asks about it. A daemon with no model records nothing.
func (p *Pipeline) ProbeModel(ctx context.Context) {
	prov := p.Settings.Provider()
	if prov == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	msgs := []llm.Message{
		{Role: "system", Content: "Answer with the single word: ok"},
		{Role: "user", Content: "ok?"},
	}
	m, err := prov.Chat(ctx, msgs, nil)
	switch {
	case err != nil:
		p.modelFailed(err)
	case strings.TrimSpace(m.Content) == "" && len(m.ToolCalls) == 0:
		p.modelFailed(errEmptyProbe)
	default:
		p.modelAnswered()
	}
}

type probeError string

func (e probeError) Error() string { return string(e) }

const errEmptyProbe = probeError("the model answered the probe with nothing")
