// Command shoulderd is the shoulder-daemon relay: it absorbs hook traffic from a
// coding harness in microseconds and talks to a swappable advisor off the hot
// path. It is one static binary that builds and runs offline; the only thing it
// ever fetches for itself is the optional embedding model, and only when asked.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/minilm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/pipeline"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
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

// logRenamed names the replacement of every setting read under an old name.
func logRenamed(log *slog.Logger, renamed []config.Renamed) {
	for _, r := range renamed {
		log.Info("setting read under its old name; rename it in the env file", "key", r.Old, "replacement", r.New)
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
	logRenamed(log, cfg.Renamed)

	// A generated token is one the harness has not necessarily been given yet:
	// an editor reads its environment at launch, and the daemon it started may
	// be the run that wrote the value into the editor's settings. Adopting
	// tells the hook surface to let that session through until it sees the
	// token once, rather than refusing every hook until somebody restarts
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
	jev, err := llm.JevFromEnv()
	if err != nil {
		return err
	}
	warnMissingModel(log, provider, jev)

	// A memory service if one was named, and otherwise one of the two stores
	// that ship inside this binary. Nothing is the last resort rather than the
	// default, because a daemon that cannot write is a daemon that watched a
	// whole session and kept none of it.
	var mem memory.Connector
	var memErr string
	switch {
	case cfg.MemoryURL != "":
		store := memory.NewMCPMemory(cfg.MemoryURL, cfg.MemoryKey, 15*time.Second)
		// What the store discards on the way to an answer is invisible from
		// above it: a recall that returns nothing because the project's own
		// session history outranks its facts looks exactly like a recall from
		// an empty store.
		store.Metrics = srv.Metrics
		mem = store
		if cfg.Memory != config.MemoryLocal {
			log.Warn("SHOULDER_MEMORY is ignored while SHOULDER_MEMORY_URL is set", "memory", cfg.Memory, "url", cfg.MemoryURL)
		}
	default:
		// The vectors compiled into the binary always answer. The transformer
		// is opt-in and arrives when it arrives: the store writes through
		// whichever is ready and brings the rest up to it afterwards.
		var emb memory.Embedder = vectors.Embedder{}
		if cfg.Embedding == config.EmbeddingMiniLM {
			emb = memory.NewFallback(minilm.New(cfg.ModelDir, log), vectors.Embedder{})
		}
		words, verr := vectors.Words()
		if verr != nil {
			// The table is compiled in, so this is a build that went wrong
			// rather than a machine that is missing something. Recall still
			// works on words in common; it is simply worse than it should be.
			log.Warn("the embedding table did not load; recall will be lexical", "error", verr)
		}
		// Either store may fail to open, and refusing to start would take the
		// session's advice down with it; the two are not the same loss. This
		// is loud instead: an unreadable file is somebody's facts, and
		// overwriting them is the one outcome that cannot be undone.
		if cfg.Memory == config.MemoryDocs {
			docs, derr := memory.NewDocs(memory.DocsOptions{
				GlobalDir: cfg.GlobalDocs, DirName: cfg.DocsDir, Embedder: emb, Log: log,
			})
			if derr != nil {
				log.Error("the docs store could not be opened; nothing will be recalled or stored",
					"global", cfg.GlobalDocs, "error", derr)
				mem, memErr = memory.Nop{}, "the docs store: "+derr.Error()
				break
			}
			// Closed on the way out, once serveUntil has let the requests and
			// the pipeline finish or run out of time, so exit does not cut the
			// re-embedding pass off mid-save and leave its temp file beside the
			// notes.
			defer func() { _ = docs.Close() }()
			// Only the global files can be counted here: the local ones are
			// one directory per checkout, found as sessions arrive.
			global, gerr := docs.List(context.Background(), memory.Query{Scope: scope.Global})
			if gerr != nil {
				log.Warn("the global docs files could not be read", "dir", cfg.GlobalDocs, "error", gerr)
			}
			log.Info("remembering in docs files", "global", docs.GlobalDir(), "global_facts", len(global),
				"docs_dir", cfg.DocsDir, "session_notes", docs.SessionPath(),
				"embedding", cfg.Embedding, "vocabulary", words, "model_dir", cfg.ModelDir,
				"hint", "local facts go in <worktree>/"+cfg.DocsDir+"/*.shoulder.md and are committed with the code")
			mem = docs
			break
		}
		local, lerr := memory.NewLocal(cfg.MemoryPath, emb)
		if lerr != nil {
			log.Error("the local store could not be opened; nothing will be recalled or stored",
				"path", cfg.MemoryPath, "error", lerr)
			mem, memErr = memory.Nop{}, "the local store at "+cfg.MemoryPath+": "+lerr.Error()
			break
		}
		defer func() { _ = local.Close() }()
		local.SetLog(log)
		log.Info("remembering locally", "path", local.Path(), "facts", local.Len(),
			"embedding", cfg.Embedding, "vocabulary", words, "model_dir", cfg.ModelDir,
			"hint", "set SHOULDER_MEMORY=docs to keep facts with each checkout, or SHOULDER_MEMORY_URL to use a memory service")
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
	// Assigned only when there is one: a nil *Jev in the interface is a
	// triage that is set and panics on first use.
	if jev != nil {
		pipe.Triage = jev
	}

	// The CLI routes share the mux, the address and the token with the hooks,
	// and live in another package only because this one may not import the
	// advisor or the store.
	mux := srv.Handler()
	api := cliapi.New(pipe, token)
	api.MemoryOpenError = memErr
	api.Mount(mux)

	hs := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Bound before the pipeline starts, so a port that is taken fails the
	// start before there is a sweep or a request to wait for.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		pipe.Run(ctx)
	}()

	log.Info("shoulderd listening",
		"addr", cfg.Addr, "llm", providerName(provider), "triage", triageName(jev), "memory", mem.Name(),
		"pickiness", cfg.Pickiness, "dry_run", cfg.Budget.DryRun, "auth", token != "",
		"log", cfg.LogPath)

	return serveUntil(ctx, stop, hs, ln, ran, httpGrace, runGrace, log)
}

// httpGrace is how long requests in flight get once the daemon is asked to
// stop, and is longer than the two seconds a cancelled request keeps for
// writing down what it had already decided. runGrace is how long the pipeline
// gets: the budget Run keeps to once it is told to stop, and a second more.
const (
	httpGrace = 3 * time.Second
	runGrace  = pipeline.ShutdownBudget + time.Second
)

// serveUntil serves until ctx ends or the listener fails, then cancels the
// requests in flight and waits for them and for the pipeline to return. Both
// write to the store, and the store is closed as soon as this returns: a save
// cut off by exit leaves its temp file behind, and a sweep cut off leaves
// session notes nobody will forget. A request is cancelled rather than waited
// out because a learn or a tidying pass can run for minutes; cancelled, it
// stops asking the model and keeps only the grace to write down what it had
// already been told. Neither wait is open-ended, because a daemon that will
// not stop is killed anyway, only later and with less said about why.
func serveUntil(ctx context.Context, stop context.CancelFunc, hs *http.Server, ln net.Listener,
	ran <-chan struct{}, reqWait, runWait time.Duration, log *slog.Logger,
) error {
	hs.BaseContext = func(net.Listener) context.Context { return ctx }
	shut := make(chan struct{})
	go func() {
		defer close(shut)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reqWait)
		defer cancel()
		if err := hs.Shutdown(sctx); err != nil {
			log.Warn("requests still running at exit were abandoned", "grace", reqWait, "error", err)
			_ = hs.Close()
		}
	}()
	// Serve returns the moment Shutdown begins, not when it ends, which is why
	// the waits below exist at all.
	err := hs.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	// A listener that failed on its own leaves ctx live, and both the pipeline
	// and the shutdown above are waiting on it.
	stop()
	<-shut
	select {
	case <-ran:
	case <-time.After(runWait):
		log.Warn("the pipeline did not finish in time; closing the store under it", "grace", runWait)
	}
	return err
}

// logRotateBytes is the size past which the log is moved aside at startup.
// The file is append-only and the daemon is restarted every editor session,
// so one rename at boot is all the rotation it needs.
const logRotateBytes = 8 << 20

// newLogger writes to stderr and, when a path is given, to that file as well.
// Both, always: the file is what `shoulderd monitor` reads, and the adapters
// start the daemon with stderr closed, so a daemon that chose one or the other
// would be silent for whichever reader it did not pick. It never writes to
// stdout: on the command-hook fallback path stdout belongs to the harness, and
// polluting it is how the reference project corrupted its own hook output.
func newLogger(path string, level slog.Leveler) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var out io.Writer = os.Stderr
	if path != "" {
		if f, err := openLog(path); err == nil {
			out = io.MultiWriter(os.Stderr, f)
		} else {
			fmt.Fprintf(os.Stderr, "shoulderd: log file %s: %v; logging to stderr only\n", path, err)
		}
	}
	return slog.New(slog.NewJSONHandler(out, opts))
}

// openLog opens the log for appending, creating its directory and moving a
// file that has grown past logRotateBytes to path.1 first.
func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > logRotateBytes {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: SHOULDER_LOG is the operator's own setting
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
		v := map[string]any{"relay": "unreachable", "error": err.Error()}
		text := "relay unreachable at " + *base + ": " + err.Error() +
			"\nHooks fail open, so sessions still work — but nothing is being observed."
		// The plugin starts the relay with SHOULDER_START_CMD at session start,
		// where nobody reads its output, and keeps that output here.
		if path, tail := startLog(); tail != "" && !*liveness {
			v["start_log"] = tail
			text += "\nThe last start command said, in " + path + ":\n" + tail
		}
		report(*asJSON, v, text)
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
		case st.OpenError != "":
			out["memory"] = "failed"
			out["memory_error"] = st.OpenError
			code = 1
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
		if st.Overridden != "" {
			out["memory_overridden"] = st.Overridden
		}
		// A store that failed to open is not the wrong store, and saying it
		// is would send somebody to edit a file that is already right.
		if st.OpenError == "" {
			out["memory_source"] = st.Source
			if msg, stale := compare("store", st.Name, st.Source, st.EnvFile, wantMemory()); stale {
				out["memory_mismatch"] = msg
				code = 1
			} else if msg != "" {
				out["memory_note"] = msg
			}
		}
		// The docs store keeps local facts with the checkout, so whether this
		// directory has any is a question doctor can answer from where it was
		// typed, and the daemon cannot.
		if st.GlobalDocs != "" {
			out["memory_global_docs"] = st.GlobalDocs
			if dir, files, ok := docsHere(st.DocsDir); ok {
				out["memory_docs_here"] = dir
				out["memory_docs_files"] = files
			}
		}
	}

	// The daemon reads its environment once, when it starts, and a container
	// keeps the environment it was created with. Both mean the file a person
	// edits and the daemon that is running can disagree for weeks without a
	// single error, so what the file asks for is compared with what runs.
	var running cliapi.ConfigResponse
	if lerr := cliGet(*base, "/v1/cli/config", &running); lerr != nil {
		out["llm"] = "unknown: " + lerr.Error()
	} else {
		out["llm"] = running.Provider
		out["llm_source"] = running.ProviderSource
		if running.Model != "" {
			out["llm_model"] = running.Model
			out["llm_model_source"] = running.ModelSource
		}
		// A triage with no decision model is a supported way to run: stored
		// facts are still repeated, only none is deduced from the session.
		switch {
		case running.Provider != providerName(nil):
		case running.Triage != "" && running.Triage != "none":
			out["llm_triage"] = running.Triage
		default:
			code = 1
		}
		names := llm.SpecNames(config.FileSetting("SHOULDER_LLM"))
		want := providerName(nil)
		if len(names) > 0 {
			want = strings.Join(names, "→")
		}
		if msg, stale := compare("model provider", running.Provider, running.ProviderSource, running.EnvFile, want); stale {
			out["llm_mismatch"] = msg
			code = 1
		} else if msg != "" {
			out["llm_note"] = msg
		}
		// The model is compared only where the file names one: otherwise it is
		// the preset's default, which this command has no business guessing.
		if model := config.FileSetting("SHOULDER_LLM_MODEL"); model != "" && len(names) == 1 && running.Provider == want {
			if msg, stale := compare("model", running.Model, running.ModelSource, running.EnvFile, model); stale {
				out["llm_model_mismatch"] = msg
				code = 1
			} else if msg != "" {
				out["llm_model_note"] = msg
			}
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
		// healthy relay while every event is being refused at the door.
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
		if dir, ok := out["memory_global_docs"].(string); ok {
			fmt.Printf("         global facts: %s\n", dir)
			switch here, files := out["memory_docs_here"], out["memory_docs_files"]; {
			case here == nil:
				fmt.Println("         this checkout: not resolvable from this directory")
			case files == 0:
				fmt.Printf("         this checkout: %s has no shoulder files yet; the first local fact creates one\n", here)
			default:
				fmt.Printf("         this checkout: %s holds %d shoulder file(s)\n", here, files)
			}
		}
	case "none":
		fmt.Println("memory:  NONE: nothing is stored and nothing is recalled")
		fmt.Println("         Start a store and give the daemon SHOULDER_MEMORY_URL; the two-line")
		fmt.Println("         version is in the README, the rest in docs/INSTALL.md.")
	case "failed":
		fmt.Printf("memory:  FAILED TO OPEN: %v\n", out["memory_error"])
		fmt.Println("         The daemon started on no store at all; nothing is stored or recalled")
		fmt.Println("         until the cause is fixed and it is restarted.")
	case "unreachable":
		fmt.Printf("memory:  UNREACHABLE (%v): %v\n", out["memory_name"], out["memory_error"])
		fmt.Println("         The daemon holds a store it cannot read. Sessions look normal and")
		fmt.Println("         every fact learned since it broke is gone.")
	default:
		fmt.Printf("memory:  %v\n", out["memory"])
	}
	if why, ok := out["memory_overridden"].(string); ok {
		fmt.Printf("         %s\n", why)
	}
	printFinding(out, "memory_mismatch", "memory_note")
	switch name := out["llm"]; {
	case name == providerName(nil) && out["llm_triage"] != nil:
		fmt.Printf("llm:     none (triage only: %v); stored facts are repeated, none is deduced from the session\n", out["llm_triage"])
	case name == providerName(nil):
		fmt.Println("llm:     NONE: no decision model; the daemon observes and stays silent")
		fmt.Printf("         Set SHOULDER_LLM in %s to one of: %s\n", envFilePath(), strings.Join(llm.Presets(), ", "))
	case out["llm_model"] != nil:
		fmt.Printf("llm:     %v (%v)\n", name, out["llm_model"])
	default:
		fmt.Printf("llm:     %v\n", name)
	}
	printFinding(out, "llm_mismatch", "llm_note")
	printFinding(out, "llm_model_mismatch", "llm_model_note")
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

// docsHere is where the docs store would keep this directory's facts and how
// many of its files are already there. It is answered from the directory the
// command ran in, which is the one thing the daemon does not know.
func docsHere(name string) (dir string, files int, ok bool) {
	wd, err := os.Getwd()
	if err != nil {
		return "", 0, false
	}
	dir, _ = memory.DocsDirFor(wd, name)
	found, _ := filepath.Glob(filepath.Join(dir, "*.shoulder.md"))
	return dir, len(found), true
}

// compare weighs a value the daemon runs against the one the env file asks for
// now. Only a value the daemon took from the file, or from nothing, is stale
// when the two differ: the file was edited after it started, or it started
// from another file. One from its own environment or from `config set` beat the
// file on purpose, so it is reported and not failed.
func compare(what, running, source, daemonFile, want string) (msg string, stale bool) {
	if running == want {
		return "", false
	}
	file := envFilePath()
	switch source {
	case cliapi.SourceConfigSet:
		return fmt.Sprintf("note: the %s %s was set with `config set`; %s asks for %s, which a restart returns to",
			what, running, file, want), false
	case config.SourceEnvironment:
		return fmt.Sprintf("note: the %s %s comes from the daemon's process environment, which wins over %s asking for %s",
			what, running, file, want), false
	}
	from := "with nothing set"
	switch {
	case source == config.SourceFile && daemonFile != file:
		from = "from " + daemonFile + ", not this file"
	case source == config.SourceFile:
		from = "from this file, since edited"
	case source == "":
		from = "from a source it does not report"
	}
	return fmt.Sprintf("MISMATCH: %s asks for the %s %s; the daemon started %s and runs %s. "+
		"It reads its settings only at start: restart it, or `make up` for the container.",
		file, what, want, from, running), true
}

// startLog is the file ensure-daemon.sh keeps the start command's output in,
// and its last few lines.
func startLog() (path, tail string) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	path = filepath.Join(dir, "shoulder-daemon", "up.log")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: built from the state directory
	if err != nil {
		return path, ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	return path, strings.Join(lines[max(0, len(lines)-5):], "\n")
}

// printFinding prints whichever of a mismatch and a note doctor recorded.
func printFinding(out map[string]any, mismatch, note string) {
	for _, k := range []string{mismatch, note} {
		if msg, ok := out[k].(string); ok {
			fmt.Printf("         %s\n", msg)
		}
	}
}

// wantMemory is the store the env file asks for, by the precedence the daemon
// applies: a memory service, when named, beats the rest.
func wantMemory() string {
	if config.FileSetting("SHOULDER_MEMORY_URL") != "" {
		return (&memory.MCPMemory{}).Name()
	}
	return config.MemoryBackend(config.FileSetting("SHOULDER_MEMORY"))
}

// memoryStatus asks the daemon whether anything is being remembered. Only the
// daemon can answer: the store is named in its environment, not in the shell
// doctor was typed into, and reaching a URL proves nothing about a backend that
// refuses every request behind it.
func memoryStatus(base string) (cliapi.MemoryStatus, error) {
	var st cliapi.MemoryStatus
	err := cliGet(base, "/v1/cli/memory", &st)
	return st, err
}

// cliGet reads one CLI route into out, with the token the daemon expects.
func cliGet(base, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return err
	}
	if token := setting("SHOULDER_TOKEN"); token != "" {
		req.Header.Set("X-Shoulder-Token", token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A daemon older than this CLI has never heard of the route, and a
		// token mismatch is already reported on its own line. Neither is a
		// verdict on what the route reports, so neither becomes one.
		return fmt.Errorf("the daemon answered %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxReplyBytes)).Decode(out)
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

// warnMissingModel says what a daemon without a decision model can still do.
// With a triage it is more than nothing: facts the agent records are written
// and stored ones repeated, and only deducing one from the session is lost.
func warnMissingModel(log *slog.Logger, provider llm.Provider, jev *llm.Jev) {
	if provider != nil {
		return
	}
	hint := "set SHOULDER_LLM to one of: " + strings.Join(llm.Presets(), ", ")
	if jev == nil {
		log.Warn("no decision model configured; shoulder-daemon will observe and stay silent", "hint", hint)
		return
	}
	log.Warn("triage without a decision model; stored facts can be repeated but no fact is deduced from the session", "hint", hint)
}

func triageName(j *llm.Jev) string {
	if j == nil {
		return "none"
	}
	return j.Name() + " (" + j.Model + ")"
}

func providerName(p llm.Provider) string {
	if p == nil {
		return "none"
	}
	return p.Name()
}
