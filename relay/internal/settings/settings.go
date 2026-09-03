// Package settings holds the handful of knobs that may be turned while the
// daemon is running.
//
// They are here rather than in config because config is a snapshot of the
// environment taken once at startup, and these four are not: a session that has
// gone quiet, or one that is storing the wrong things, is a problem you want to
// fix now rather than after a restart that loses every session's context. What
// is here is deliberately small — the log level, how picky the memory is, and
// which model answers — because everything else in config is read on a path
// where a value changing underneath it would be a race rather than a feature.
//
// Nothing here persists. A restart returns to what the environment says, so the
// env file stays the single description of how this daemon is meant to run and
// a change made at a terminal cannot silently outlive the reason for it.
package settings

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
)

// Live is the current value of each knob. Every reader goes through it, so a
// change reaches the next turn rather than the next process.
type Live struct {
	mu    sync.RWMutex
	pick  prompts.Pickiness
	spec  string
	model string
	prov  llm.Provider

	// level is shared with the logger's handler rather than copied into it.
	// slog reads a Leveler on every record, which is what makes a level change
	// take effect on the next line written instead of the next handler built.
	level *slog.LevelVar
}

// New takes the boot configuration. level is the same LevelVar the logger was
// built with; spec is what SHOULDER_LLM asked for, kept because a model change
// has to rebuild the same providers. model is the explicit override if there
// was one, empty when the presets chose.
func New(level *slog.LevelVar, pick prompts.Pickiness, spec, model string, prov llm.Provider) *Live {
	if level == nil {
		level = new(slog.LevelVar)
	}
	if !pick.Valid() {
		pick = prompts.Default
	}
	return &Live{level: level, pick: pick, spec: strings.TrimSpace(spec), model: model, prov: prov}
}

// Provider is the model to send to now, or nil when none is configured. The
// value is read under the lock and used outside it: a swap that lands mid-call
// leaves the call finishing against the provider it started with, which is what
// you want — the alternative is tearing down a request somebody is waiting on.
func (l *Live) Provider() llm.Provider {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.prov
}

// Pickiness is how reluctant the decision model should be right now. A nil Live
// is the default rather than a panic, so a Pipeline assembled without settings
// behaves as the daemon did before this existed.
func (l *Live) Pickiness() prompts.Pickiness {
	if l == nil {
		return prompts.Default
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.pick
}

// Snapshot is what the daemon is doing now, as a person would want it read
// back. Provider and Model are what is in use, not what was asked for: a preset
// that filled in the model is the answer to "which model", and reporting the
// blank that was typed would hide it.
type Snapshot struct {
	LogLevel  string `json:"log_level"`
	Pickiness string `json:"pickiness"`
	// PickinessLevel is the number behind the name, so a script can compare
	// without knowing the order the names come in.
	PickinessLevel int    `json:"pickiness_level"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
}

func (l *Live) Snapshot() Snapshot {
	if l == nil {
		return Snapshot{
			LogLevel: "info", Pickiness: prompts.Default.String(),
			PickinessLevel: int(prompts.Default), Provider: "none",
		}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.snapshot()
}

func (l *Live) snapshot() Snapshot {
	s := Snapshot{
		LogLevel:       strings.ToLower(l.level.Level().String()),
		Pickiness:      l.pick.String(),
		PickinessLevel: int(l.pick),
		Provider:       "none",
	}
	if l.prov != nil {
		s.Provider = l.prov.Name()
		s.Model = llm.ModelOf(l.prov)
	}
	return s
}

// Change is one request to turn some of the knobs. A nil field is one the
// caller did not mention, which is different from one they cleared: there is no
// way to ask for "no provider", because a daemon told to stop thinking is one
// you stop.
type Change struct {
	LogLevel  *string `json:"log_level,omitempty"`
	Pickiness *string `json:"pickiness,omitempty"`
	Provider  *string `json:"provider,omitempty"`
	Model     *string `json:"model,omitempty"`
}

func (c Change) Empty() bool {
	return c.LogLevel == nil && c.Pickiness == nil && c.Provider == nil && c.Model == nil
}

// ErrBadChange marks a change the caller got wrong, as opposed to one the
// daemon could not carry out. The distinction survives to the exit code of the
// command that asked.
var ErrBadChange = errors.New("bad setting")

// Apply turns every knob the change names, or none of them. All-or-nothing is
// the point: naming a provider and a model that provider does not have would
// otherwise leave the daemon pointed at a model that does not exist, having
// reported an error about something else.
func (l *Live) Apply(c Change) (Snapshot, error) {
	if l == nil {
		return Snapshot{}, fmt.Errorf("%w: this daemon has no live settings", ErrBadChange)
	}
	if c.Empty() {
		return Snapshot{}, fmt.Errorf("%w: nothing to change", ErrBadChange)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Everything is resolved before anything is stored, so a refusal leaves the
	// daemon exactly as it was.
	level := l.level.Level()
	if c.LogLevel != nil {
		v, err := parseLevel(*c.LogLevel)
		if err != nil {
			return l.snapshot(), err
		}
		level = v
	}

	pick := l.pick
	if c.Pickiness != nil {
		v, err := prompts.ParsePickiness(*c.Pickiness)
		if err != nil {
			return l.snapshot(), fmt.Errorf("%w: %w", ErrBadChange, err)
		}
		pick = v
	}

	spec, model, prov := l.spec, l.model, l.prov
	if c.Provider != nil || c.Model != nil {
		if c.Provider != nil {
			spec = strings.TrimSpace(*c.Provider)
			// A new provider drops the old model. The ids do not carry across —
			// asking for openai while still pinned to a Gemini model is a 404
			// nobody would connect to the command they typed — so naming a
			// provider alone means that provider's own default.
			model = ""
		}
		if c.Model != nil {
			model = strings.TrimSpace(*c.Model)
		}
		built, err := llm.Configure(spec, model)
		if err != nil {
			// The variables that error names have to be set where the daemon
			// runs, which is not the shell that typed the command and, for a
			// containerised daemon, not this machine's environment either. Said
			// plainly here because the alternative is exporting the key,
			// retrying, and getting the identical refusal back.
			return l.snapshot(), fmt.Errorf("%w: %w. Any variable named there belongs to the daemon's own environment, not this shell; a containerised daemon reads it from the env file it was started with", ErrBadChange, err)
		}
		// A spec that names nothing — "", "  ", "," — builds cleanly and
		// returns no provider, which at startup is a daemon deliberately
		// running silent. Here it would be a daemon that answers nothing until
		// somebody restarts it, so it is refused: stopping a daemon thinking is
		// what stopping the process is for, not a setting to clear.
		if built == nil {
			return l.snapshot(), fmt.Errorf("%w: --provider=%q names no provider; use one of %s",
				ErrBadChange, spec, strings.Join(llm.Presets(), ", "))
		}
		prov = built
	}

	l.level.Set(level)
	l.pick, l.spec, l.model, l.prov = pick, spec, model, prov
	return l.snapshot(), nil
}

// parseLevel is stricter than the one config uses at startup. A typo there must
// never stop a daemon booting, so it falls back to info; here somebody is
// watching for the answer, and silently giving them info when they asked for
// dbeug is how an hour goes missing.
func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%w: unknown log level %q: use debug, info, warn or error", ErrBadChange, s)
}

// ForProvider is a Live holding nothing but a provider: the info log level and
// the default pickiness, neither of which anything is watching. It is for the
// callers that assemble a pipeline without a daemon around it.
func ForProvider(p llm.Provider) *Live {
	return New(nil, prompts.Default, "", "", p)
}
