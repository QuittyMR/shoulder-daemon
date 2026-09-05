// Package config holds runtime configuration, all of it env-driven so the
// relay can run identically as a container or a bare binary.
package config

import (
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
)

type Config struct {
	Addr           string
	Token          string
	AdvisorBaseURL string
	AdvisorModel   string
	AdvisorAPIKey  string
	// AdvisorTimeout bounds the decision pass end to end, not one request to
	// the model. That pass is a tool loop, so the budget has to cover every
	// model round trip the pipeline's step cap allows plus the lookup that
	// happens between each pair of them; sized for a single question, the
	// model that actually uses the tools it was given is exactly the one that
	// gets cut off. Raising the step cap or the recall timeout moves this.
	AdvisorTimeout time.Duration
	SystemPrompt   string

	// MessageTimeout and DigestTimeout bound the two CLI operations. They are
	// separate from AdvisorTimeout because that one is sized for a background
	// call nobody is waiting on and may be cut short with no visible loss,
	// whereas a person is sitting in front of these two.
	MessageTimeout time.Duration
	DigestTimeout  time.Duration

	WindowEvents int
	WindowChars  int

	Budget budget.Gate

	MemoryURL string
	MemoryKey string

	// MemoryPath is the file the built-in store writes to, and is used only
	// when no memory service was named.
	MemoryPath string

	// IdleExit stops the daemon after this long with no session. What normally
	// ends it is the last session ending; this is the backstop for the harness
	// that dies without saying goodbye, which otherwise leaves a daemon sitting
	// on the machine until it is rebooted. An hour is long enough that nobody
	// between two sessions ever notices it and short enough that a crash does
	// not cost the day. Zero turns it off.
	IdleExit time.Duration

	QueueSize int
	LogPath   string
	LogLevel  slog.Level

	// Pickiness is how reluctant the decision model is to write a new fact.
	// Only the starting value lives here: it can be turned while the daemon
	// runs, and from that point on the live value is the one that counts.
	Pickiness prompts.Pickiness
}

func Load() Config {
	c := Config{
		Addr:           Env("SHOULDER_ADDR", "127.0.0.1:8787"),
		Token:          Setting("SHOULDER_TOKEN"),
		AdvisorBaseURL: Env("ADVISOR_BASE_URL", "http://127.0.0.1:9090"),
		AdvisorModel:   Env("ADVISOR_MODEL", "shoulder"),
		AdvisorAPIKey:  Setting("ADVISOR_API_KEY"),
		AdvisorTimeout: time.Duration(envInt("ADVISOR_TIMEOUT_SECONDS", 90)) * time.Second,
		SystemPrompt:   Env("ADVISOR_SYSTEM_PROMPT", prompts.Advisor),
		MessageTimeout: time.Duration(envInt("MESSAGE_TIMEOUT_SECONDS", 60)) * time.Second,
		DigestTimeout:  time.Duration(envInt("DIGEST_TIMEOUT_SECONDS", 120)) * time.Second,
		WindowEvents:   envInt("WINDOW_EVENTS", 40),
		WindowChars:    envInt("WINDOW_CHARS", 12000),
		MemoryURL:      Setting("SHOULDER_MEMORY_URL"),
		MemoryKey:      Setting("SHOULDER_MEMORY_KEY"),
		MemoryPath:     Env("SHOULDER_MEMORY_PATH", memory.DefaultLocalPath()),
		QueueSize:      envInt("QUEUE_SIZE", 1024),
		IdleExit:       time.Duration(envInt("SHOULDER_IDLE_EXIT_MINUTES", 60)) * time.Minute,
		LogPath:        logPath(Setting("SHOULDER_LOG")),
		LogLevel:       logLevel(Env("LOG_LEVEL", "info")),
		Pickiness:      pickiness(Setting("SHOULDER_PICKINESS")),
	}
	g := budget.Default()
	g.MinTurnGap = envInt("BUDGET_MIN_TURN_GAP", g.MinTurnGap)
	g.MaxChars = envInt("BUDGET_MAX_CHARS", g.MaxChars)
	g.SessionMaxChars = envInt("BUDGET_SESSION_MAX_CHARS", g.SessionMaxChars)
	g.DryRun = envBool("SHOULDER_DRY_RUN", false)
	c.Budget = g
	return c
}

// Env reads an environment variable, falling back to d when it is unset or
// empty. Exported so the packages that read their own variables use the same
// empty-means-unset rule as Load.
func Env(k, d string) string {
	if v := Setting(k); v != "" {
		return v
	}
	return d
}

// DefaultLogPath is the file the daemon writes its log to when nobody says
// otherwise. It sits beside the store because it is the same kind of thing: a
// record the daemon keeps of itself, which `shoulderd monitor` reads back.
func DefaultLogPath() string {
	store := memory.DefaultLocalPath()
	if store == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(store), "shoulderd.log")
}

// logPath resolves SHOULDER_LOG. Empty is the default file; "stderr" or "-"
// is no file at all, for a daemon whose output something else is collecting.
func logPath(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DefaultLogPath()
	case "stderr", "-":
		return ""
	}
	return s
}

// logLevel reads the level by name. An unrecognised value is info rather than
// an error: a typo in a log setting must never be the reason a daemon refuses
// to start.
func logLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// pickiness reads the starting level by name or number. Like logLevel it
// forgives a bad value rather than refusing to start: the daemon is still
// useful at the default, and a typo in a tuning knob is not worth a machine
// that comes up with no memory at all.
func pickiness(s string) prompts.Pickiness {
	if strings.TrimSpace(s) == "" {
		return prompts.Default
	}
	p, err := prompts.ParsePickiness(s)
	if err != nil {
		return prompts.Default
	}
	return p
}

func envInt(k string, d int) int {
	if v := Setting(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func envBool(k string, d bool) bool {
	if v := Setting(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return d
}
