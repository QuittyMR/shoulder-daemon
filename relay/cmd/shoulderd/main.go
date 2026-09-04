// Command shoulderd is the shoulder-daemon relay: it absorbs hook traffic from a
// coding harness in microseconds and talks to a swappable advisor off the hot
// path. It has no third-party dependencies, so it builds and runs offline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/cliapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/httpapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/pipeline"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

func main() {
	if len(os.Args) > 1 {
		c := &cli{out: os.Stdout, err: os.Stderr}
		os.Exit(c.dispatch(os.Args[1], os.Args[2:]))
	}
	if err := serve(); err != nil {
		fmt.Fprintln(os.Stderr, "shoulderd:", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg := config.Load()
	// The level is a variable the handler reads per record rather than a value
	// baked into it, which is what lets `shoulderd config set --log-level=debug`
	// take effect on the next line written instead of the next process.
	level := new(slog.LevelVar)
	level.Set(cfg.LogLevel)
	log := newLogger(cfg.LogPath, level)

	// A generated token is one the harness has not necessarily been given yet:
	// an editor reads its environment at launch, and the daemon it started may
	// be the run that wrote the value into the editor's settings. Adopting
	// tells the hook surface to let that session through until it sees the
	// token once, rather than turning away every hook until somebody restarts
	// their editor.
	token, adopting := ensureToken(log)
	if token == "" {
		log.Warn("running with no token; any local process can post events and read advice for a live session")
	}

	reg := session.NewRegistry(200)
	box := outbox.New()
	queue := make(chan session.Event, cfg.QueueSize)
	srv := httpapi.New(reg, box, queue, token, cfg.Budget)
	srv.Adopting = adopting
	srv.Log = log
	provider, err := llm.FromEnv()
	if err != nil {
		return err
	}
	if provider == nil {
		log.Warn("no decision model configured; shoulder-daemon will observe and stay silent",
			"hint", "set SHOULDER_LLM to one of: "+strings.Join(llm.Presets(), ", "))
	}

	// A memory service if one was named, and otherwise the store that ships
	// inside this binary. Nothing is the last resort rather than the default,
	// because a daemon that cannot write is a daemon that watched a whole
	// session and kept none of it.
	var mem memory.Connector
	switch {
	case cfg.MemoryURL != "":
		store := memory.NewMCPMemory(cfg.MemoryURL, cfg.MemoryKey, 15*time.Second)
		// What the store discards on the way to an answer is invisible from
		// above it: a recall that returns nothing because the project's own
		// session history outranks its facts looks exactly like a recall from
		// an empty store.
		store.Metrics = srv.Metrics
		mem = store
	default:
		local, lerr := memory.NewLocal(cfg.MemoryPath, vectors.Embedder{})
		if lerr != nil {
			// Refusing to start would take the session's advice down with the
			// store, and the two are not the same loss. This is loud instead:
			// an unreadable file is somebody's facts, and overwriting them is
			// the one outcome that cannot be undone.
			log.Error("the local store could not be opened; nothing will be recalled or stored",
				"path", cfg.MemoryPath, "error", lerr)
			mem = memory.Nop{}
			break
		}
		words, verr := vectors.Words()
		if verr != nil {
			// The table is compiled in, so this is a build that went wrong
			// rather than a machine that is missing something. Recall still
			// works on words in common; it is simply worse than it should be.
			log.Warn("the embedding table did not load; recall will be lexical", "error", verr)
		}
		log.Info("remembering locally", "path", local.Path(), "facts", local.Len(),
			"embedding", vectors.Model, "vocabulary", words,
			"hint", "set SHOULDER_MEMORY_URL to use a memory service instead")
		mem = local
	}
	// Wrapping here is what makes local-or-global a property of the system: no
	// caller above this line can reach a backend with a record that never chose.
	mem = memory.Checked(mem)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	live := settings.New(level, cfg.Pickiness, llm.EnvSpec(), os.Getenv("SHOULDER_LLM_MODEL"), provider)

	pipe := &pipeline.Pipeline{
		Cfg: cfg, Log: log, Metrics: srv.Metrics, Registry: reg,
		IdleExit: cfg.IdleExit, OnIdle: stop,
		Outbox: box, Settings: live, Memory: mem, Queue: queue,
	}
	go pipe.Run(ctx)

	// The CLI routes share the mux, the address and the token with the hooks,
	// and live in another package only because this one may not import the
	// advisor or the store.
	mux := srv.Handler()
	cliapi.New(pipe, token).Mount(mux)

	hs := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()

	log.Info("shoulderd listening",
		"addr", cfg.Addr, "llm", providerName(provider), "memory", mem.Name(),
		"pickiness", cfg.Pickiness, "dry_run", cfg.Budget.DryRun, "auth", token != "")

	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newLogger writes to a file when one is configured and to stderr otherwise.
// It never writes to stdout: on the command-hook fallback path stdout belongs
// to the harness, and polluting it is how the reference project corrupted its
// own hook output.
func newLogger(path string, level slog.Leveler) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil { //nolint:gosec // G304: SHOULDER_LOG is the operator's own setting
			return slog.New(slog.NewJSONHandler(f, opts))
		}
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func (c *cli) doctor(args []string) int {
	fs := c.flags("doctor", doctorUsage)
	base := fs.String("addr", "http://"+config.Load().Addr, "relay base URL")
	asJSON := fs.Bool("json", false, "machine-readable output")
	liveness := fs.Bool("liveness", false, "only check that the relay is up; ignore whether hooks have fired")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}

	client := &http.Client{Timeout: 3 * time.Second}
	out := map[string]any{}
	code := 0

	resp, err := client.Get(*base + "/healthz")
	if err != nil {
		report(*asJSON, map[string]any{"relay": "unreachable", "error": err.Error()},
			"relay unreachable at "+*base+": "+err.Error()+
				"\nHooks fail open, so sessions still work — but nothing is being observed.")
		return 1
	}
	_ = resp.Body.Close()
	out["relay"] = "ok"

	b := currentBuild()
	out["version"] = b.Version
	out["origin"] = b.Origin

	// What a harness actually runs is the copy of the plugin taken at install
	// time, not the checkout somebody is editing. A stale copy posts to the
	// address and with the header it was built against, so the symptom is
	// silence or rejection while the source on disk looks correct.
	if stale, serr := stalePlugins(*base); serr != nil {
		out["plugin"] = "unreadable: " + serr.Error()
	} else if len(stale) > 0 {
		out["plugin_stale"] = stale
		code = 1
	} else {
		out["plugin"] = "ok"
	}

	// Liveness is a strictly weaker question than readiness: "is the process
	// serving?", not "has the harness ever called it?". Container healthchecks
	// must ask the weaker one, or a correctly-running relay reports unhealthy
	// until somebody happens to start a coding session.
	if *liveness {
		if *asJSON {
			report(true, out, "")
		} else {
			fmt.Println("relay:   ok")
		}
		return 0
	}

	// Asked last, because it is the one check that makes the daemon do work,
	// and asked at all because nothing else here can see it: a daemon pointed
	// at a store that never answers passes every other line on this report
	// while remembering nothing.
	if st, merr := memoryStatus(*base); merr != nil {
		out["memory"] = "unknown: " + merr.Error()
	} else {
		out["memory_name"] = st.Name
		switch {
		case !st.Configured:
			out["memory"] = "none"
			code = 1
		case st.OK:
			out["memory"] = "ok"
		default:
			out["memory"] = "unreachable"
			out["memory_error"] = st.Error
			code = 1
		}
	}

	// A newer release is worth one line, not an exit code: a daemon behind by a
	// version is still a working daemon. The proxy being unreachable says
	// nothing about this machine, so that is not reported at all.
	if latest, lerr := latestRelease(context.Background()); lerr == nil {
		out["latest"] = latest
		if newer(latest, b.Version) {
			out["update_available"] = latest
		}
	}

	mresp, err := client.Get(*base + "/metrics")
	if err != nil {
		out["metrics"] = "unreachable"
		code = 1
	} else {
		defer mresp.Body.Close()
		buf := make([]byte, 1<<20)
		n, _ := mresp.Body.Read(buf)
		metrics := string(buf[:n])
		out["metrics"] = "ok"

		missing := []string{}
		for _, e := range httpapi.RoutineEvents() {
			if !strings.Contains(metrics, `event="`+e+`"`) {
				missing = append(missing, e)
			}
		}
		out["events_never_seen"] = missing
		if len(missing) > 0 {
			code = 1
		}

		// A rejected hook is counted after its latency is observed, so it looks
		// exactly like a hook that fired. Without this check doctor reports a
		// healthy relay while every event is being turned away at the door.
		if n := counterValue(metrics, "shoulder_unauthorised_total"); n > 0 {
			out["unauthorised"] = n
			code = 1
		}
	}

	if *asJSON {
		report(true, out, "")
		return code
	}

	fmt.Printf("relay:   %v\n", out["relay"])
	fmt.Printf("version: %s (%s)\n", b.Version, b.Origin)
	if latest, ok := out["update_available"].(string); ok {
		fmt.Printf("update:  %s is out; %s\n", latest, updateHint(b.Origin))
	}
	fmt.Printf("metrics: %v\n", out["metrics"])
	if stale, ok := out["plugin_stale"].([]string); ok {
		fmt.Printf("plugin:  STALE: %v\n", stale)
		fmt.Println("         The harness runs the copy made when the plugin was installed, not")
		fmt.Println("         your checkout. Reinstall it so the copy matches.")
	}
	if n, ok := out["unauthorised"].(int); ok {
		fmt.Printf("auth:    %d REJECTED: the token the harness sends does not match this daemon's\n", n)
		fmt.Println("         SHOULDER_TOKEN must hold the same value here and wherever the harness")
		fmt.Println("         runs. Hooks fail open, so a session looks normal while nothing is observed.")
	}
	switch out["memory"] {
	case "ok":
		fmt.Printf("memory:  ok (%v)\n", out["memory_name"])
	case "none":
		fmt.Println("memory:  NONE: nothing is stored and nothing is recalled")
		fmt.Println("         Start a store and give the daemon SHOULDER_MEMORY_URL; the two-line")
		fmt.Println("         version is in the README, the rest in docs/INSTALL.md.")
	case "unreachable":
		fmt.Printf("memory:  UNREACHABLE (%v): %v\n", out["memory_name"], out["memory_error"])
		fmt.Println("         The daemon holds a store it cannot read. Sessions look normal and")
		fmt.Println("         every fact learned since it broke is gone.")
	default:
		fmt.Printf("memory:  %v\n", out["memory"])
	}
	if missing, ok := out["events_never_seen"].([]string); ok {
		if len(missing) == 0 {
			fmt.Println("hooks:   all expected events have fired at least once")
		} else {
			fmt.Printf("hooks:   NEVER FIRED: %v\n", missing)
			fmt.Println("         Check that the plugin is installed, and that allowedHttpHookUrls")
			fmt.Println("         either is unset or includes http://127.0.0.1:8787/*")
		}
	}
	return code
}

// memoryStatus asks the daemon whether anything is being remembered. Only the
// daemon can answer: the store is named in its environment, not in the shell
// doctor was typed into, and reaching a URL proves nothing about a backend that
// refuses every request behind it.
func memoryStatus(base string) (cliapi.MemoryStatus, error) {
	var st cliapi.MemoryStatus
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(base, "/")+"/v1/cli/memory", nil)
	if err != nil {
		return st, err
	}
	if token := setting("SHOULDER_TOKEN"); token != "" {
		req.Header.Set("X-Shoulder-Token", token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A daemon older than this CLI has never heard of the route, and a
		// token mismatch is already reported on its own line. Neither is a
		// verdict on the store, so neither becomes one.
		return st, fmt.Errorf("the daemon answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(&st); err != nil {
		return st, err
	}
	return st, nil
}

// counterValue reads one Prometheus counter out of a scrape. It returns 0 when
// the counter has never been incremented, which is indistinguishable from
// absent and means the same thing here.
func counterValue(scrape, name string) int {
	for _, line := range strings.Split(scrape, "\n") {
		rest, ok := strings.CutPrefix(line, name+" ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// stalePlugins returns the installed Claude Code plugins whose hook URLs do not
// point at this relay. It reads the harness's own installed-plugin registry
// rather than any checkout, because that registry is what the harness loads.
func stalePlugins(base string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json")) //nolint:gosec // G304: built from the home directory, not from input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var reg struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, err
	}

	want := strings.TrimPrefix(strings.TrimPrefix(base, "http://"), "https://")
	var stale []string
	for name, installs := range reg.Plugins {
		for _, in := range installs {
			hooks, err := os.ReadFile(filepath.Join(in.InstallPath, "hooks", "hooks.json"))
			if err != nil {
				continue
			}
			body := string(hooks)
			// Only plugins that speak this protocol are ours to judge.
			if !strings.Contains(body, "/v1/hooks/claude-code/") {
				continue
			}
			if !strings.Contains(body, want) || !strings.Contains(body, "X-Shoulder-Token") {
				stale = append(stale, name+" at "+in.InstallPath)
			}
		}
	}
	sort.Strings(stale)
	return stale, nil
}

func report(asJSON bool, v map[string]any, text string) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return
	}
	fmt.Println(text)
}

func providerName(p llm.Provider) string {
	if p == nil {
		return "none"
	}
	return p.Name()
}
