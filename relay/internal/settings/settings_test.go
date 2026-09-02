package settings

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
)

// keys puts the provider keys where Configure looks for them and clears the
// single-provider overrides. No network is involved: Configure builds a struct
// and never calls anything. Clearing the overrides matters because they are
// read from the process environment, so a developer who has SHOULDER_LLM_MODEL
// set would otherwise be running different tests from CI.
func keys(t *testing.T) {
	t.Helper()
	t.Setenv("GEMINI_API_KEY", "test-gemini")
	t.Setenv("OPENAI_API_KEY", "test-openai")
	t.Setenv("SHOULDER_LLM_KEY", "")
	t.Setenv("SHOULDER_LLM_BASE_URL", "")
	t.Setenv("SHOULDER_LLM_MODEL", "")
}

func ptr(s string) *string { return &s }

// provider builds one the way the daemon does, so the tests below hold a real
// provider rather than a stub that could not fail the way a real one does.
func provider(t *testing.T, spec, model string) llm.Provider {
	t.Helper()
	p, err := llm.Configure(spec, model)
	if err != nil {
		t.Fatalf("configure %q/%q: %v", spec, model, err)
	}
	return p
}

func TestSettingsReportWhatTheDaemonStartedWith(t *testing.T) {
	keys(t)
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn)
	live := New(level, prompts.Careful, "gemini", "", provider(t, "gemini", ""))

	got := live.Snapshot()
	if got.LogLevel != "warn" {
		t.Fatalf("log level = %q", got.LogLevel)
	}
	if got.Pickiness != "careful" || got.PickinessLevel != int(prompts.Careful) {
		t.Fatalf("pickiness = %q (%d)", got.Pickiness, got.PickinessLevel)
	}
	if got.Provider != "gemini" {
		t.Fatalf("provider = %q", got.Provider)
	}
	// The model was not asked for, so the preset filled it in. Reporting the
	// blank that was typed would hide which model is actually being paid for.
	if got.Model == "" {
		t.Fatal("the snapshot names no model, so it reports what was asked for rather than what is in use")
	}
	if live.Pickiness() != prompts.Careful {
		t.Fatalf("Pickiness() = %v", live.Pickiness())
	}
}

// A pickiness that could never have come from ParsePickiness still must not
// reach the prompt table, because New is also called from main with a value
// that came out of the environment.
func TestAnImpossibleStartingPickinessBecomesTheDefault(t *testing.T) {
	live := New(nil, prompts.Pickiness(99), "", "", nil)
	if live.Pickiness() != prompts.Default {
		t.Fatalf("Pickiness() = %v, want the default", live.Pickiness())
	}
}

// A pipeline assembled without settings — every test and every caller that
// predates this package — must behave as the daemon did before it existed.
func TestNilSettingsAnswerRatherThanPanic(t *testing.T) {
	var live *Live

	if got := live.Pickiness(); got != prompts.Default {
		t.Fatalf("Pickiness() = %v, want the default", got)
	}
	if got := live.Provider(); got != nil {
		t.Fatalf("Provider() = %v, want nil", got)
	}
	got := live.Snapshot()
	if got.LogLevel != "info" || got.Pickiness != prompts.Default.String() || got.Provider != "none" {
		t.Fatalf("snapshot = %+v", got)
	}
	if _, err := live.Apply(Change{Pickiness: ptr("strict")}); !errors.Is(err, ErrBadChange) {
		t.Fatalf("Apply on a daemon with no settings = %v, want ErrBadChange", err)
	}
}

func TestForProviderIsTheDefaultsAndNothingElse(t *testing.T) {
	keys(t)
	live := ForProvider(provider(t, "gemini", ""))
	got := live.Snapshot()
	if got.LogLevel != "info" || got.Pickiness != prompts.Default.String() {
		t.Fatalf("snapshot = %+v", got)
	}
	if got.Provider != "gemini" {
		t.Fatalf("provider = %q", got.Provider)
	}
}

// A change names the knobs it wants moved. Everything it does not name is a
// knob somebody else set for a reason.
func TestApplyMovesOnlyWhatTheChangeNames(t *testing.T) {
	keys(t)
	level := new(slog.LevelVar)
	before := New(level, prompts.Eager, "gemini", "", provider(t, "gemini", "")).Snapshot()

	live := New(level, prompts.Eager, "gemini", "", provider(t, "gemini", ""))
	after, err := live.Apply(Change{Pickiness: ptr("strict")})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if after.Pickiness != "strict" || after.PickinessLevel != int(prompts.Strict) {
		t.Fatalf("pickiness = %q (%d)", after.Pickiness, after.PickinessLevel)
	}
	if after.LogLevel != before.LogLevel {
		t.Fatalf("the log level moved to %q on a change that never mentioned it", after.LogLevel)
	}
	if after.Provider != before.Provider || after.Model != before.Model {
		t.Fatalf("the model moved to %s/%s on a change that never mentioned it", after.Provider, after.Model)
	}
	// The reply and the next read must agree, or a script that trusted the
	// reply is acting on a state the daemon is not in.
	if live.Snapshot() != after {
		t.Fatalf("the reply %+v is not what the daemon then reports, %+v", after, live.Snapshot())
	}
}

// The all-or-nothing property, which is the reason this package resolves
// everything before it stores anything. Each case pairs a good value with a bad
// one: if the good half landed before the bad half was noticed, the daemon
// would be left in a state nobody asked for, having reported an error about
// something else entirely.
func TestARefusedChangeTurnsNoKnobAtAll(t *testing.T) {
	cases := []struct {
		name   string
		change Change
		want   string
	}{
		{
			name:   "a log level that does not exist",
			change: Change{LogLevel: ptr("dbeug"), Pickiness: ptr("strict")},
			want:   "dbeug",
		},
		{
			name:   "a pickiness that does not exist",
			change: Change{LogLevel: ptr("debug"), Pickiness: ptr("picky")},
			want:   "picky",
		},
		{
			name:   "a provider nobody has heard of",
			change: Change{LogLevel: ptr("debug"), Pickiness: ptr("strict"), Provider: ptr("nope")},
			want:   "nope",
		},
		{
			name:   "a model for a provider with no key in this environment",
			change: Change{LogLevel: ptr("debug"), Provider: ptr("openrouter")},
			want:   "OPENROUTER_API_KEY",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keys(t)
			t.Setenv("OPENROUTER_API_KEY", "")
			level := new(slog.LevelVar)
			live := New(level, prompts.Careful, "gemini", "", provider(t, "gemini", ""))
			before := live.Snapshot()

			got, err := live.Apply(c.change)
			if err == nil {
				t.Fatalf("Apply(%+v) succeeded", c.change)
			}
			if !errors.Is(err, ErrBadChange) {
				t.Fatalf("error %v is not an ErrBadChange, so the CLI cannot tell it from a daemon fault", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name the value that was wrong (%q)", err, c.want)
			}
			// Every knob, read three ways: the snapshot the refusal handed
			// back, the snapshot the next reader gets, and the live values the
			// decision path actually uses.
			if got != before {
				t.Fatalf("the refusal reported %+v, was %+v", got, before)
			}
			if now := live.Snapshot(); now != before {
				t.Fatalf("the daemon is now %+v, was %+v", now, before)
			}
			if live.Pickiness() != prompts.Careful {
				t.Fatalf("pickiness moved to %v", live.Pickiness())
			}
			if level.Level() != slog.LevelInfo {
				t.Fatalf("the log level moved to %v", level.Level())
			}
			if live.Provider() == nil || live.Provider().Name() != "gemini" {
				t.Fatalf("the provider moved to %v", live.Provider())
			}
		})
	}
}

// The level is shared with the handler rather than copied into it, which is
// what makes a change take effect on the next line written instead of the next
// handler built. A logger constructed before the change is the only thing that
// proves it.
func TestALogLevelChangeReachesALoggerAlreadyBuilt(t *testing.T) {
	var out bytes.Buffer
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: level}))
	live := New(level, prompts.Default, "", "", nil)

	log.Debug("before the change")
	if strings.Contains(out.String(), "before the change") {
		t.Fatalf("debug was already on: %q", out.String())
	}

	if _, err := live.Apply(Change{LogLevel: ptr("debug")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	log.Debug("after the change")
	if !strings.Contains(out.String(), "after the change") {
		t.Fatalf("the logger built before the change never saw it: %q", out.String())
	}
	if got := live.Snapshot().LogLevel; got != "debug" {
		t.Fatalf("the snapshot says %q", got)
	}

	// And back down again, so the knob is a knob rather than a one-way door.
	if _, err := live.Apply(Change{LogLevel: ptr("WARN")}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	out.Reset()
	log.Info("info is now below the bar")
	if out.Len() != 0 {
		t.Fatalf("raising the level let an info line through: %q", out.String())
	}
}

// Model ids do not carry across providers, so a switch that kept the old one
// would point the daemon at a model that does not exist and report success.
func TestNamingANewProviderResetsTheModel(t *testing.T) {
	keys(t)
	live := New(new(slog.LevelVar), prompts.Default, "gemini", "pinned-model-id",
		provider(t, "gemini", "pinned-model-id"))
	if got := live.Snapshot().Model; got != "pinned-model-id" {
		t.Fatalf("model = %q before the change", got)
	}

	got, err := live.Apply(Change{Provider: ptr("openai")})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.Provider != "openai" {
		t.Fatalf("provider = %q", got.Provider)
	}
	if got.Model == "pinned-model-id" {
		t.Fatal("the new provider is still pinned to the old provider's model")
	}
	if got.Model == "" {
		t.Fatal("the new provider names no model at all")
	}
	// And the provider's own default is what it landed on, rather than
	// whatever the environment happened to say.
	want := llm.ModelOf(provider(t, "openai", ""))
	if got.Model != want {
		t.Fatalf("model = %q, want the provider's own default %q", got.Model, want)
	}
}

// Naming a model alone is the common case: same provider, different model.
func TestAModelAloneStaysOnTheProviderItWasGiven(t *testing.T) {
	keys(t)
	live := New(new(slog.LevelVar), prompts.Default, "gemini", "", provider(t, "gemini", ""))

	got, err := live.Apply(Change{Model: ptr("gemini-2.5-pro")})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.Provider != "gemini" || got.Model != "gemini-2.5-pro" {
		t.Fatalf("snapshot = %+v", got)
	}
	if llm.ModelOf(live.Provider()) != "gemini-2.5-pro" {
		t.Fatalf("the provider in use still sends to %q", llm.ModelOf(live.Provider()))
	}
}

// A provider and a model in the same breath keeps the model: the reset exists
// so a lone provider gets its own default, not to overrule what was typed.
func TestAProviderAndAModelTogetherKeepTheModel(t *testing.T) {
	keys(t)
	live := New(new(slog.LevelVar), prompts.Default, "gemini", "", provider(t, "gemini", ""))

	got, err := live.Apply(Change{Provider: ptr("openai"), Model: ptr("gpt-4.1-mini")})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.Provider != "openai" || got.Model != "gpt-4.1-mini" {
		t.Fatalf("snapshot = %+v", got)
	}
}

// A daemon with no provider cannot be given a model, because there is nothing
// to send it to. Reporting that is better than storing a model that is never
// used and answering "which model" with it.
func TestAModelWithNoProviderIsRefused(t *testing.T) {
	keys(t)
	live := New(new(slog.LevelVar), prompts.Default, "", "", nil)
	before := live.Snapshot()

	got, err := live.Apply(Change{Model: ptr("gpt-4.1-mini")})
	if !errors.Is(err, ErrBadChange) {
		t.Fatalf("Apply = %v, want ErrBadChange", err)
	}
	if got != before || live.Snapshot() != before {
		t.Fatalf("the daemon changed to %+v", live.Snapshot())
	}
}

// Blanking the provider used to build cleanly and leave a daemon that observed
// every session and answered nothing, recoverable only by restarting it. There
// is no setting for "stop thinking"; that is what stopping the process is for.
func TestBlankingTheProviderIsRefused(t *testing.T) {
	keys(t)
	live := New(new(slog.LevelVar), prompts.Careful, "gemini", "", provider(t, "gemini", ""))
	before := live.Snapshot()

	for _, blank := range []string{"", "   ", ","} {
		got, err := live.Apply(Change{Provider: ptr(blank)})
		if !errors.Is(err, ErrBadChange) {
			t.Fatalf("Apply(--provider=%q) = %v, want ErrBadChange", blank, err)
		}
		if got != before || live.Snapshot() != before {
			t.Fatalf("--provider=%q left the daemon at %+v, was %+v", blank, live.Snapshot(), before)
		}
		if live.Provider() == nil {
			t.Fatalf("--provider=%q blinded the daemon: every question is now refused until it restarts", blank)
		}
	}
}

// A change that names nothing is a mistyped flag that would otherwise look
// like it worked.
func TestAnEmptyChangeIsRefused(t *testing.T) {
	live := New(new(slog.LevelVar), prompts.Default, "", "", nil)
	if !(Change{}).Empty() {
		t.Fatal("an empty Change does not report itself empty")
	}
	_, err := live.Apply(Change{})
	if !errors.Is(err, ErrBadChange) {
		t.Fatalf("Apply(Change{}) = %v, want ErrBadChange", err)
	}
	if !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("error %q does not say what was wrong", err)
	}
}

// Every spelling the --help text offers has to be one Apply accepts, or the
// documentation is a list of ways to get an error.
func TestEveryOfferedSpellingIsAccepted(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "warning", "error", "ERROR", " info "} {
		live := New(new(slog.LevelVar), prompts.Default, "", "", nil)
		if _, err := live.Apply(Change{LogLevel: ptr(level)}); err != nil {
			t.Fatalf("log level %q: %v", level, err)
		}
	}
	for _, name := range prompts.PickinessNames() {
		live := New(new(slog.LevelVar), prompts.Default, "", "", nil)
		got, err := live.Apply(Change{Pickiness: ptr(name)})
		if err != nil {
			t.Fatalf("pickiness %q: %v", name, err)
		}
		if got.Pickiness != name {
			t.Fatalf("asked for %q, got %q", name, got.Pickiness)
		}
	}
}
