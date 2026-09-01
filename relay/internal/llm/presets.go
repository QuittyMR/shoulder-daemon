package llm

import (
	"fmt"
	"os"
	"strings"
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

func Presets() []string {
	out := make([]string, 0, len(presets))
	for k := range presets {
		out = append(out, k)
	}
	return out
}

// FromEnv builds the configured provider. SHOULDER_LLM names one preset,
// or several separated by commas, in which case they are tried in order. Base
// URL, model and key overrides apply to a single-provider configuration only —
// with a chain, each provider takes its key from its own preset variable.
func FromEnv() (Provider, error) {
	spec := strings.TrimSpace(os.Getenv("SHOULDER_LLM"))
	if spec == "" {
		return nil, nil
	}
	names := strings.Split(spec, ",")
	single := len(names) == 1

	ps := make([]Provider, 0, len(names))
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		p, err := build(n, single)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	if len(ps) == 0 {
		return nil, nil
	}
	if single {
		return ps[0], nil
	}
	return &Chain{Providers: ps}, nil
}

func build(name string, allowOverrides bool) (Provider, error) {
	p, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q; known: %s", name, strings.Join(Presets(), ", "))
	}

	key := ""
	if allowOverrides {
		key = os.Getenv("SHOULDER_LLM_KEY")
	}
	if key == "" && p.KeyEnv != "" {
		key = os.Getenv(p.KeyEnv)
	}
	if key == "" && p.KeyEnv != "" {
		return nil, fmt.Errorf("provider %q needs %s or SHOULDER_LLM_KEY", name, p.KeyEnv)
	}

	base, model := p.BaseURL, p.DefaultModel
	if allowOverrides {
		if v := os.Getenv("SHOULDER_LLM_BASE_URL"); v != "" {
			base = v
		}
		if v := os.Getenv("SHOULDER_LLM_MODEL"); v != "" {
			model = v
		}
	}

	return &OpenAICompatible{
		Label: name, BaseURL: base, APIKey: key, Model: model,
		MaxTokens: 1200, Temperature: 0.2, Extra: p.Extra,
	}, nil
}
