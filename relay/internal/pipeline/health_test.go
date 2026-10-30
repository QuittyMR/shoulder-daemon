package pipeline

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/metrics"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// The incident: a key that died was refused on every call for thirteen days,
// and nothing but a log line per call said so. The health is what doctor and
// the session-start hook now read, so a refusal has to land in it.
func TestARefusedDecisionIsRecordedAsTheModelFailing(t *testing.T) {
	s := newStack(t, "http://127.0.0.1:1", 200*time.Millisecond)
	if got := s.pipe.ModelHealth().State; got != ModelUnknown {
		t.Fatalf("before any call the model is %q, want %q", got, ModelUnknown)
	}

	s.promptAndAnswer(t, prompt("s1", "hi"), stop("s1", "done"))

	// The prompt and the answer are two consults, and both are refused.
	h := s.pipe.ModelHealth()
	if h.State != ModelFailing || h.Failures < 1 || h.Error == "" || h.Since.IsZero() {
		t.Fatalf("a refused call must read as failing, with the refusal: %+v", h)
	}
}

func TestAnAnsweredDecisionClearsTheFailure(t *testing.T) {
	p := bare(t, advisor(advisorServer(t, 0, decisionBody(t, ""))))
	p.modelFailed(probeError("gemini status 400: bad key"))
	p.modelFailed(probeError("gemini status 400: bad key"))
	if h := p.ModelHealth(); h.Failures != 2 {
		t.Fatalf("two refusals count as two: %+v", h)
	}

	p.ProbeModel(context.Background())

	h := p.ModelHealth()
	if h.State != ModelOK || h.Failures != 0 || h.Error != "" {
		t.Fatalf("an answer must clear the failure: %+v", h)
	}
}

func TestTheProbeRecordsAModelThatRefuses(t *testing.T) {
	p := bare(t, &llm.OpenAICompatible{Label: "dead", BaseURL: "http://127.0.0.1:1", Model: "m"})

	p.ProbeModel(context.Background())

	h := p.ModelHealth()
	if h.State != ModelFailing || !strings.Contains(h.Error, "connect") {
		t.Fatalf("a probe nobody answers must read as failing with the cause: %+v", h)
	}
}

func TestTheProbeAsksNothingWithoutAModel(t *testing.T) {
	p := bare(t, nil)
	p.ProbeModel(context.Background())
	if got := p.ModelHealth().State; got != ModelUnknown {
		t.Fatalf("a daemon with no model has nothing to report, got %q", got)
	}
}

// ModelChanged is `config set` swapping the provider: what was known about
// the old one is forgotten, and the new one is asked at once.
func TestAChangedModelIsAskedAfresh(t *testing.T) {
	p := bare(t, advisor(advisorServer(t, 0, decisionBody(t, ""))))
	p.modelFailed(probeError("the old key"))

	p.ModelChanged(context.Background())

	if h := p.ModelHealth(); h.State != ModelOK || h.Error != "" {
		t.Fatalf("the new model answered and the old refusal must be gone: %+v", h)
	}
}

// bare is a pipeline with nothing running: enough to probe and to record.
func bare(t *testing.T, prov llm.Provider) *Pipeline {
	t.Helper()
	return &Pipeline{
		Cfg: config.Load(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New(), Settings: settings.ForProvider(prov),
	}
}
