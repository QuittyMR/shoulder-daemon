package llm

import (
	"sort"
	"strings"
	"testing"
)

// clearEnv takes the process environment out of the answer. Every override
// Configure reads is a variable a developer may well have set for real work,
// and a test that passes only on a clean machine is a test that fails in the
// one place it matters.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SHOULDER_LLM", "SHOULDER_LLM_KEY", "SHOULDER_LLM_BASE_URL", "SHOULDER_LLM_MODEL"} {
		t.Setenv(k, "")
	}
	t.Setenv("GEMINI_API_KEY", "test-gemini")
	t.Setenv("OPENAI_API_KEY", "test-openai")
}

// No provider is the daemon observing in silence, which is a supported way to
// run it: the hooks still fire and the store is still read. Returning an error
// here would make a legitimate configuration look broken at every startup.
func TestNoSpecIsNoProviderAndNoComplaint(t *testing.T) {
	clearEnv(t)
	for _, spec := range []string{"", "   ", ","} {
		p, err := Configure(spec, "")
		if err != nil {
			t.Fatalf("Configure(%q, \"\") = %v", spec, err)
		}
		if p != nil {
			t.Fatalf("Configure(%q, \"\") built %v", spec, p)
		}
	}
}

// A model with nothing to send it to is a command that would otherwise report
// success and change nothing anybody could observe.
func TestAModelWithNoProviderIsRefused(t *testing.T) {
	clearEnv(t)
	p, err := Configure("", "gpt-5.2-mini")
	if err == nil {
		t.Fatalf("Configure(\"\", model) = %v, want an error", p)
	}
	if p != nil {
		t.Fatalf("a refused Configure still handed back %v", p)
	}
	// The refusal has to say what to name instead, because the person typing
	// has no list of providers in front of them.
	for _, want := range Presets() {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not offer %q", err, want)
		}
	}
}

// The providers in a chain do not share a model namespace, so one id would be
// right for at most one of them and would 404 on the rest — silently, at the
// moment the leader went down and the fallback was needed.
func TestAModelForAChainIsRefused(t *testing.T) {
	clearEnv(t)
	p, err := Configure("gemini,openai", "gemini-flash-lite-latest")
	if err == nil {
		t.Fatalf("Configure(chain, model) = %v, want an error", p)
	}
	if p != nil {
		t.Fatalf("a refused Configure still handed back %v", p)
	}
	if !strings.Contains(err.Error(), "chain") {
		t.Fatalf("error %q does not say why a chain is different", err)
	}
}

// The typed model wins because it came from somebody at a terminal now, rather
// than from the environment the daemon happened to start in weeks ago.
func TestAnExplicitModelBeatsTheEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("SHOULDER_LLM_MODEL", "from-the-environment")

	p, err := Configure("gemini", "typed-just-now")
	if err != nil {
		t.Fatal(err)
	}
	if got := ModelOf(p); got != "typed-just-now" {
		t.Fatalf("model = %q, want the one that was typed", got)
	}

	// And with nothing typed, the environment is still what it was for.
	p, err = Configure("gemini", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ModelOf(p); got != "from-the-environment" {
		t.Fatalf("model = %q, want the environment's", got)
	}
}

// With no model asked for anywhere, the preset's own default is the answer.
func TestAProviderFallsBackToItsOwnDefaultModel(t *testing.T) {
	clearEnv(t)
	p, err := Configure("gemini", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := ModelOf(p); got != presets["gemini"].DefaultModel {
		t.Fatalf("model = %q, want the preset default %q", got, presets["gemini"].DefaultModel)
	}
	if p.Name() != "gemini" {
		t.Fatalf("name = %q", p.Name())
	}
}

// The single-provider overrides are exactly that. Applied to a chain they would
// point every provider at one base URL and one model, which is the opposite of
// what a chain is for.
func TestAChainIgnoresTheSingleProviderOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("SHOULDER_LLM_MODEL", "from-the-environment")
	t.Setenv("SHOULDER_LLM_BASE_URL", "http://127.0.0.1:9/nowhere")

	p, err := Configure("gemini,openai", "")
	if err != nil {
		t.Fatal(err)
	}
	chain, ok := p.(*Chain)
	if !ok {
		t.Fatalf("Configure of two providers built %T, not a chain", p)
	}
	if len(chain.Providers) != 2 {
		t.Fatalf("chain holds %d providers", len(chain.Providers))
	}
	for _, one := range chain.Providers {
		got := one.(*OpenAICompatible)
		if got.Model == "from-the-environment" {
			t.Fatalf("%s in a chain took the single-provider model override", got.Label)
		}
		if strings.Contains(got.BaseURL, "nowhere") {
			t.Fatalf("%s in a chain took the single-provider base URL override", got.Label)
		}
	}
	// Both models are named, in the order they are tried: reporting only the
	// first would name the model that is not in use exactly when the leader is
	// down and the fallback is answering.
	if got := chain.ModelID(); !strings.Contains(got, presets["gemini"].DefaultModel) ||
		!strings.Contains(got, presets["openai"].DefaultModel) {
		t.Fatalf("the chain reports model %q", got)
	}
}

// A provider whose key is not in the daemon's environment cannot answer, and
// finding that out at the first turn rather than at configuration time means
// finding out from a 401 in a log nobody is watching.
func TestAProviderWithNoKeyIsRefusedByName(t *testing.T) {
	clearEnv(t)
	t.Setenv("GEMINI_API_KEY", "")

	if p, err := Configure("gemini", ""); err == nil {
		t.Fatalf("Configure with no key built %v", p)
	} else if !strings.Contains(err.Error(), "GEMINI_API_KEY") {
		t.Fatalf("error %q does not name the variable to set", err)
	}
}

func TestAnUnknownProviderIsRefusedWithTheList(t *testing.T) {
	clearEnv(t)
	p, err := Configure("gemeni", "")
	if err == nil {
		t.Fatalf("Configure(\"gemeni\") built %v", p)
	}
	if !strings.Contains(err.Error(), "gemeni") {
		t.Fatalf("error %q does not repeat what was typed", err)
	}
	for _, want := range Presets() {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not offer %q", err, want)
		}
	}
}

// This list is read by people: it appears in `config set --help` and in the
// refusal a mistyped provider gets. A set that reshuffles between two identical
// errors makes them look like different errors.
func TestPresetsAreSortedAndComplete(t *testing.T) {
	got := Presets()
	if len(got) != len(presets) {
		t.Fatalf("Presets() names %d of %d providers", len(got), len(presets))
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("Presets() is not sorted: %v", got)
	}
	// Twice, because the order used to come out of a map and was stable only
	// by accident.
	if second := Presets(); strings.Join(second, ",") != strings.Join(got, ",") {
		t.Fatalf("two calls disagree: %v then %v", got, second)
	}
	for _, name := range got {
		p := presets[name]
		if p.BaseURL == "" || p.DefaultModel == "" {
			t.Fatalf("preset %q is missing a base URL or a default model: %+v", name, p)
		}
	}
}

// EnvSpec is kept because a model swapped at runtime has to be applied to the
// same providers that were configured at boot, and a built Provider no longer
// says which presets it came from.
func TestEnvSpecIsWhatWasAskedForTrimmed(t *testing.T) {
	clearEnv(t)
	t.Setenv("SHOULDER_LLM", "  gemini,openai \n")
	if got := EnvSpec(); got != "gemini,openai" {
		t.Fatalf("EnvSpec() = %q", got)
	}
	t.Setenv("SHOULDER_LLM", "")
	if got := EnvSpec(); got != "" {
		t.Fatalf("EnvSpec() = %q", got)
	}
}

// ModelOf is how the CLI reports what is in use. Nothing on the decision path
// needs it, so a provider that cannot say must not be a crash.
func TestModelOfAProviderThatCannotSay(t *testing.T) {
	if got := ModelOf(nil); got != "" {
		t.Fatalf("ModelOf(nil) = %q", got)
	}
	if got := ModelOf(stub{label: "says nothing"}); got != "" {
		t.Fatalf("ModelOf of a provider with no model = %q", got)
	}
	if got := ModelOf(&OpenAICompatible{Model: "m"}); got != "m" {
		t.Fatalf("ModelOf = %q", got)
	}
	// A chain reports one gap rather than dropping the provider, so the
	// positions still line up with the order they are tried in.
	chain := &Chain{Providers: []Provider{stub{label: "says nothing"}, &OpenAICompatible{Model: "m"}}}
	if got := chain.ModelID(); got != "\u2192m" {
		t.Fatalf("chain ModelID = %q", got)
	}
}
