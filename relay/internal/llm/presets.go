package llm

import (
	"fmt"
	"sort"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

// Preset captures the three things that differ between providers, plus the
// per-provider quirks that are silent 401/404 traps if guessed.
type Preset struct {
	BaseURL      string
	DefaultModel string
	KeyEnv       string
	Extra        map[string]any
}

var presets = map[string]Preset{
	"openrouter": {
		BaseURL:      "https://openrouter.ai/api/v1",
		DefaultModel: "google/gemini-2.5-flash-lite",
		KeyEnv:       "OPENROUTER_API_KEY",
	},
	// AI Studio key against the OpenAI compatibility layer. Vertex AI is a
	// different base URL and needs an OAuth token, not this key.
	// gemini-flash-lite-latest is a moving alias: it follows Google's current
	// flash-lite, so behaviour can change without a config change. Pin a
	// dated id if that matters.
	"gemini": {
		BaseURL:      "https://generativelanguage.googleapis.com/v1beta/openai",
		DefaultModel: "gemini-flash-lite-latest",
		KeyEnv:       "GEMINI_API_KEY",
	},
	// Pay-as-you-go key. A Coding Plan key will 401 here; use provider glm-coding.
	"glm": {
		BaseURL:      "https://api.z.ai/api/paas/v4",
		DefaultModel: "glm-4.7-flash",
		KeyEnv:       "GLM_API_KEY",
	},
	// GLM Coding Plan subscription endpoint, scoped to coding scenarios.
	// glm-5.3-flash is the only flash model the plan lists. Reasoning is
	// disabled: it is on by default and wasted on a classification task.
	"glm-coding": {
		BaseURL:      "https://api.z.ai/api/coding/paas/v4",
		DefaultModel: "glm-5.3-flash",
		KeyEnv:       "GLM_API_KEY",
		// Reasoning is on by default and is pure waste for a classification
		// task: measured on glm-5.3-flash, omitting this field spent 54
		// reasoning tokens to answer "OK" in 3. Disabling it is accepted and
		// costs nothing.
		Extra: map[string]any{
			"thinking": map[string]string{"type": "disabled"},
		},
	},
	// Pay-as-you-go Platform billing. A ChatGPT Plus/Pro subscription does
	// not grant API access.
	"openai": {
		BaseURL:      "https://api.openai.com/v1",
		DefaultModel: "gpt-5.2-mini",
		KeyEnv:       "OPENAI_API_KEY",
	},
	// Ollama default. Any OpenAI-compatible server works; override the base URL.
	// OpenCode Go is a hosted gateway for open-weight coding models, sold as a
	// subscription rather than per-token. It is a separate namespace from
	// OpenCode Zen, which carries the frontier models, and it is not the
	// `opencode` CLI: this talks to it directly over its OpenAI-compatible API.
	// The default is a flash-tier model on purpose; deciding whether a turn
	// contradicts a stored fact is classification, not authorship.
	"opencode-go": {
		BaseURL:      "https://opencode.ai/zen/go/v1",
		DefaultModel: "glm-5.3-flash",
		KeyEnv:       "OPENCODE_API_KEY",
	},
	"local": {
		BaseURL:      "http://127.0.0.1:11434/v1",
		DefaultModel: "qwen2.5-coder:7b",
		KeyEnv:       "",
	},
}

// Presets names every provider this daemon knows, sorted. The order is fixed
// because this list is read by people: it appears in the refusal a mistyped
// provider gets, and a set that reshuffles between two identical errors makes
// them look like different errors.
func Presets() []string {
	out := make([]string, 0, len(presets))
	for k := range presets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EnvSpec is what SHOULDER_LLM asks for, unresolved. The daemon keeps it
// because a model swapped at runtime has to be applied to the same chain of
// providers that was configured at boot, and a built Provider no longer says
// which presets it came from.
func EnvSpec() string { return strings.TrimSpace(config.Setting("SHOULDER_LLM")) }

// FromEnv builds the configured provider. SHOULDER_LLM names one preset,
// or several separated by commas, in which case they are tried in order. Base
// URL, model and key overrides apply to a single-provider configuration only —
// with a chain, each provider takes its key from its own preset variable.
func FromEnv() (Provider, error) { return Configure(EnvSpec(), "") }

// Configure builds the provider a spec names, where spec has the shape
// SHOULDER_LLM takes: one preset, or several comma-separated and tried in
// order. An empty spec is no provider and no error, which is the daemon
// observing in silence rather than a misconfiguration.
//
// model overrides the preset default and beats SHOULDER_LLM_MODEL, because it
// comes from somebody typing at a terminal now rather than from the environment
// the daemon happened to start in. It is refused for a chain: the providers in
// one do not share a model namespace, so a single id would be right for at most
// one of them and would silently 404 on the rest.
func Configure(spec, model string) (Provider, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		if model != "" {
			return nil, fmt.Errorf("a model was named but no provider is configured; name one of %s too", strings.Join(Presets(), ", "))
		}
		return nil, nil
	}
	// The empty entries are dropped before anything is counted, so a spec of
	// "gemini," is the one provider it names rather than a chain that happens
	// to be refused a model.
	names := make([]string, 0, strings.Count(spec, ",")+1)
	for _, n := range strings.Split(spec, ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		if model != "" {
			return nil, fmt.Errorf("a model was named but the provider %q names nothing; use one of %s", spec, strings.Join(Presets(), ", "))
		}
		return nil, nil
	}
	single := len(names) == 1
	if !single && model != "" {
		return nil, fmt.Errorf("a model cannot be set for the chain %q: its providers do not share model ids; name one provider instead", spec)
	}

	ps := make([]Provider, 0, len(names))
	for _, n := range names {
		p, err := build(n, single, model)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	if single {
		return ps[0], nil
	}
	return &Chain{Providers: ps}, nil
}

func build(name string, allowOverrides bool, model string) (Provider, error) {
	p, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q; known: %s", name, strings.Join(Presets(), ", "))
	}

	key := ""
	if allowOverrides {
		key = config.Setting("SHOULDER_LLM_KEY")
	}
	if key == "" && p.KeyEnv != "" {
		key = config.Setting(p.KeyEnv)
	}
	if key == "" && p.KeyEnv != "" {
		return nil, fmt.Errorf("provider %q needs %s or SHOULDER_LLM_KEY", name, p.KeyEnv)
	}

	base := p.BaseURL
	if allowOverrides {
		if v := config.Setting("SHOULDER_LLM_BASE_URL"); v != "" {
			base = v
		}
		if model == "" {
			model = config.Setting("SHOULDER_LLM_MODEL")
		}
	}
	if model == "" {
		model = p.DefaultModel
	}

	return &OpenAICompatible{
		Label: name, BaseURL: base, APIKey: key, Model: model,
		MaxTokens: 1200, Temperature: 0.2, Extra: p.Extra,
	}, nil
}
