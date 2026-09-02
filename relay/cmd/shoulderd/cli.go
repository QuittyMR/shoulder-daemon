package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/cliapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// clientTimeout outlives the daemon's own digest timeout. Whichever side gives
// up first decides what the user is told, and only the daemon knows whether the
// model is still thinking.
const clientTimeout = 3 * time.Minute

// maxReplyBytes bounds what a reply may be. A digest is paragraphs of prose.
const maxReplyBytes = 4 << 20

// cli is where a subcommand's output goes. Carrying the streams rather than
// writing to os.Stdout directly is what lets the argument handling be tested
// without a process.
type cli struct {
	out io.Writer
	err io.Writer
}

// dispatch runs one subcommand and returns the process exit code: 0 success,
// 1 the daemon could not answer, 2 the command line was wrong.
func (c *cli) dispatch(name string, args []string) int {
	switch name {
	case "doctor":
		return c.doctor(args)
	case "message":
		return c.message(args)
	case "fact":
		return c.fact(args)
	case "digest":
		return c.digest(args)
	case "consolidate":
		return c.consolidate(args)
	case "config":
		return c.config(args)
	case "help", "-h", "-help", "--help":
		// Asking what the commands are is a question, and an answered question
		// leaves by stdout with nothing to report to the shell.
		fmt.Fprint(c.out, usage)
		return 0
	}
	fmt.Fprintf(c.err, "shoulderd: unknown command %q\n\n", name)
	fmt.Fprint(c.err, usage)
	return 2
}

const projectIs = `
A project is the root of the git worktree you are standing in, or that
directory itself when it is not a repository.
`

const usage = `usage:
  shoulderd                                                    run the relay
  shoulderd doctor [--addr=URL] [--json] [--liveness]          check that it is running
  shoulderd message [--local|--global] [--update|--no-update] "text"
  shoulderd fact add    --local|--global [--category=C] [--tag=T]... "content"
  shoulderd fact update --local|--global --id=ID [--category=C] [--tag=T]... "content"
  shoulderd fact list   [--local|--global] [--limit=N]
  shoulderd digest      [--local|--global]
  shoulderd consolidate --local|--global
  shoulderd config [show]                                      what the daemon is doing now
  shoulderd config set [--log-level=L] [--pickiness=P] [--provider=N] [--model=M]

--local is this project alone; --global follows you into every other one.
` + projectIs + `
Omitting both flags means different things on purpose: a write refuses to
choose for you, message and fact list read this project, and digest covers
both. Every subcommand spells out its own default under --help.

Flags come before the text: shoulderd message --no-update "your question"
`

const doctorUsage = `usage: shoulderd doctor [--addr=URL] [--json] [--liveness]

Report whether the relay is running and whether the harness has ever reached it.

  --addr URL   relay base URL (default http://127.0.0.1:8787)
  --json       machine-readable output
  --liveness   only ask whether the relay is serving, not whether hooks have
               fired, which is the question a container healthcheck means
`

const messageUsage = `usage: shoulderd message [--local|--global] [--update|--no-update] [--addr=URL] "text"

Ask what has been remembered, and let the answer record what it establishes.

  --local      answer from this project and from what follows you (default)
  --global     answer from what follows you alone, ignoring every project
  --update     record what the exchange establishes even if it looks unremarkable
  --no-update  answer only; record nothing
  --addr URL   relay base URL (default http://127.0.0.1:8787)

With neither --local nor --global this reads the project you are standing in.
` + projectIs + `
Flags come before the text: shoulderd message --no-update "your question"
`

const factAddUsage = `usage: shoulderd fact add --local|--global [--category=C] [--tag=T]... [--addr=URL] "content"

Store a fact exactly as typed, without asking a model about it.

  --local        this project only          } exactly one is required;
  --global       you, in every project      } there is no default
  --category C   one of: constraint, correction, decision, preference,
                 reference, structure
  --tag T        a tag to attach; repeatable
  --addr URL     relay base URL (default http://127.0.0.1:8787)

The content is required and comes last. A fact filed in the wrong half is not
wrong, it is absent from the project that needed it and present in every
project that did not, so the scope is never guessed for you.
` + projectIs + `
Flags come before the content: shoulderd fact add --global "prefers terse answers"
`

const factUpdateUsage = `usage: shoulderd fact update --local|--global --id=ID [--category=C] [--tag=T]... [--addr=URL] "content"

Replace a stored fact with a corrected one.

  --local        this project only          } exactly one is required;
  --global       you, in every project      } there is no default
  --id ID        the fact this replaces; required
  --category C   one of: constraint, correction, decision, preference,
                 reference, structure
  --tag T        a tag to attach; repeatable
  --addr URL     relay base URL (default http://127.0.0.1:8787)

The content is required and comes last. The named fact must already be in the
scope you pass: an update corrects knowledge, it never moves it between a
project and you.
` + projectIs + `
Flags come before the content: shoulderd fact update --global --id=mem_2 "prefers terse answers"
`

const factListUsage = `usage: shoulderd fact list [--local|--global] [--limit=N] [--addr=URL]

List stored facts, newest first.

  --local      this project only (default)
  --global     what follows you into every project
  --limit N    how many to show (default 50)
  --addr URL   relay base URL (default http://127.0.0.1:8787)

With neither flag this shows this project alone, so a fact added with --global
is missing from it until you ask for --global.
` + projectIs

const factUsage = factAddUsage + "\n" + factUpdateUsage + "\n" + factListUsage

const digestUsage = `usage: shoulderd digest [--local|--global] [--addr=URL]

Describe in prose everything a scope holds.

  --local      this project alone
  --global     what follows you into every project
  --addr URL   relay base URL (default http://127.0.0.1:8787)

With neither flag this covers both at once: this project and everything global.
` + projectIs

const consolidateUsage = `usage: shoulderd consolidate --local|--global [--addr=URL]

Read one scope whole and tidy it: drop facts that have stopped being rules, and
collapse several wordings of one rule into a single record.

  --local      this project alone
  --global     what follows you into every project
  --addr URL   relay base URL (default http://127.0.0.1:8787)

The daemon does this by itself when a session ends and every few turns. Running
it by hand is for watching what it removes.
` + projectIs

const configShowUsage = `usage: shoulderd config [show] [--addr=URL] [--json]

Report what the running daemon is set to: its log level, how picky it is about
storing new facts, and which provider and model answer for it.

  --json       machine-readable output
  --addr URL   relay base URL (default http://127.0.0.1:8787)
`

// configSetUsage is a var rather than a const because it names the providers,
// and that list lives with the presets. A hand-copied list here is one that
// goes stale the first time a provider is added.
var configSetUsage = `usage: shoulderd config set [--log-level=L] [--pickiness=P] [--provider=N] [--model=M] [--addr=URL] [--json]

Change a running daemon without restarting it. Every flag takes effect on the
next turn; nothing in flight is interrupted, and nothing is written down, so a
restart returns to what the environment says.

  --log-level L   debug, info, warn or error
  --pickiness P   how reluctant the memory is to store a new fact:
                  eager, open, balanced, careful, strict, or 0-4.
                  Lower stores more and needs the tidying pass more often;
                  higher keeps the store clean and misses rules you only implied.
  --provider N    one of: ` + strings.Join(llm.Presets(), ", ") + `.
                  Its key must already be in the daemon's environment, and it
                  resets the model to that provider's own default.
  --model M       a model id for the current provider. Not for a failover
                  chain: its providers do not share model ids.
  --json          machine-readable output
  --addr URL      relay base URL (default http://127.0.0.1:8787)

Passing nothing to change is an error rather than a no-op, so a mistyped flag
name cannot look like it worked.
`

var configUsage = configShowUsage + "\n" + configSetUsage

func (c *cli) config(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "set":
			return c.configSet(args[1:])
		case "show":
			return c.configShow(args[1:])
		case "help", "-h", "-help", "--help":
			fmt.Fprint(c.out, configUsage)
			return 0
		}
	}
	// Bare `shoulderd config` reads: the flags below all belong to show, and a
	// change is the thing you should have to name.
	return c.configShow(args)
}

func (c *cli) configShow(args []string) int {
	fs := c.flags("config show", configShowUsage)
	addr := bindAddr(fs)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("config show takes no arguments, got %q; to change a setting use `config set`", fs.Arg(0)))
	}
	var reply cliapi.ConfigResponse
	if code := c.call(*addr, http.MethodGet, "/v1/cli/config", nil, &reply); code != 0 {
		return code
	}
	return c.printConfig(reply.Snapshot, *asJSON)
}

func (c *cli) configSet(args []string) int {
	fs := c.flags("config set", configSetUsage)
	addr := bindAddr(fs)
	asJSON := fs.Bool("json", false, "machine-readable output")
	level := fs.String("log-level", "", "debug, info, warn or error")
	pick := fs.String("pickiness", "", strings.Join(prompts.PickinessNames(), ", ")+", or 0-4")
	provider := fs.String("provider", "", "one of: "+strings.Join(llm.Presets(), ", "))
	model := fs.String("model", "", "model id for the current provider")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("config set takes no arguments, got %q; every setting is a flag", fs.Arg(0)))
	}

	// Only the flags actually typed are sent. The zero value of a string flag
	// and an empty one deliberately asked for are the same thing to the flag
	// package, so what was set is read back from the FlagSet rather than from
	// the values.
	var change settings.Change
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "log-level":
			change.LogLevel = level
		case "pickiness":
			change.Pickiness = pick
		case "provider":
			change.Provider = provider
		case "model":
			change.Model = model
		}
	})
	if change.Empty() {
		return c.reject(errors.New("nothing to change: pass --log-level, --pickiness, --provider or --model"))
	}

	var reply cliapi.ConfigResponse
	if code := c.call(*addr, http.MethodPatch, "/v1/cli/config", change, &reply); code != 0 {
		return code
	}
	return c.printConfig(reply.Snapshot, *asJSON)
}

// printConfig renders the same four values whether they were just read or just
// changed, so `config set` answers the question `config show` would have.
func (c *cli) printConfig(s settings.Snapshot, asJSON bool) int {
	if asJSON {
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(s); err != nil {
			fmt.Fprintln(c.err, "shoulderd:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(c.out, "log level:  %s\n", s.LogLevel)
	fmt.Fprintf(c.out, "pickiness:  %s (%d)\n", s.Pickiness, s.PickinessLevel)
	fmt.Fprintf(c.out, "provider:   %s\n", s.Provider)
	if s.Model != "" {
		fmt.Fprintf(c.out, "model:      %s\n", s.Model)
	}
	return 0
}

func (c *cli) message(args []string) int {
	fs := c.flags("message", messageUsage)
	addr := bindAddr(fs)
	var sf scopeFlags
	sf.bind(fs)
	update := fs.Bool("update", false, "record what the exchange establishes even if it looks unremarkable")
	noUpdate := fs.Bool("no-update", false, "answer only; record nothing")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if err := trailingFlag(fs.Args()); err != nil {
		return c.reject(err)
	}

	text := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if text == "" {
		return c.reject(errors.New(`nothing to ask: shoulderd message "your question"`))
	}
	if *update && *noUpdate {
		return c.reject(errors.New("--update and --no-update are mutually exclusive"))
	}
	sc, project, err := sf.forReading()
	if err != nil {
		return c.reject(err)
	}
	mode := "auto"
	switch {
	case *update:
		mode = "force"
	case *noUpdate:
		mode = "never"
	}

	var reply cliapi.MessageResponse
	if code := c.call(*addr, http.MethodPost, "/v1/cli/message", cliapi.MessageRequest{
		Text: text, Scope: string(sc), Project: project, Update: mode,
	}, &reply); code != 0 {
		return code
	}

	fmt.Fprintln(c.out, reply.Reply)
	// What was written goes to stderr so the answer alone is what a pipe sees,
	// while the person still finds out that their question changed the store.
	for _, f := range reply.Facts {
		fmt.Fprintf(c.err, "recorded (%s): %s\n", f.Scope, f.Content)
	}
	return 0
}

func (c *cli) fact(args []string) int {
	if len(args) == 0 {
		return c.reject(errors.New("fact needs a verb: add, update or list"))
	}
	switch args[0] {
	case "add":
		return c.factWrite("add", http.MethodPost, args[1:])
	case "update":
		return c.factWrite("update", http.MethodPatch, args[1:])
	case "list":
		return c.factList(args[1:])
	case "help", "-h", "-help", "--help":
		fmt.Fprint(c.out, factUsage)
		return 0
	}
	return c.reject(fmt.Errorf("unknown fact verb %q: use add, update or list", args[0]))
}

func (c *cli) factWrite(verb, method string, args []string) int {
	help := factAddUsage
	if method == http.MethodPatch {
		help = factUpdateUsage
	}
	fs := c.flags("fact "+verb, help)
	addr := bindAddr(fs)
	var sf scopeFlags
	sf.bind(fs)
	category := fs.String("category", "", "one of: decision, constraint, preference, correction, structure, reference")
	var tags stringList
	fs.Var(&tags, "tag", "tag to attach; repeatable")
	id := ""
	if method == http.MethodPatch {
		fs.StringVar(&id, "id", "", "id of the fact this replaces")
	}
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if err := trailingFlag(fs.Args()); err != nil {
		return c.reject(err)
	}

	content := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if content == "" {
		return c.reject(fmt.Errorf(`fact %s needs the fact: shoulderd fact %s ... "content"`, verb, verb))
	}
	if method == http.MethodPatch && id == "" {
		return c.reject(errors.New("fact update needs --id=ID: the fact it replaces"))
	}
	sc, project, err := sf.forWriting()
	if err != nil {
		return c.reject(err)
	}

	var reply cliapi.FactResponse
	if code := c.call(*addr, method, "/v1/cli/facts", cliapi.FactRequest{
		ID: id, Content: content, Category: *category, Tags: tags,
		Scope: string(sc), Project: project,
	}, &reply); code != 0 {
		return code
	}
	switch {
	case reply.AlreadyKnown:
		// The store already holds the state the command asked for, so the
		// command got what it wanted and a script running twice is not broken.
		fmt.Fprintln(c.out, "already known")
	case reply.ID == "":
		// Not every backend names what it wrote.
		fmt.Fprintln(c.out, "ok")
	default:
		fmt.Fprintln(c.out, reply.ID)
	}
	return 0
}

func (c *cli) factList(args []string) int {
	fs := c.flags("fact list", factListUsage)
	addr := bindAddr(fs)
	var sf scopeFlags
	sf.bind(fs)
	limit := fs.Int("limit", cliapi.DefaultListLimit, "how many facts to show")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("fact list takes no arguments, got %q", fs.Arg(0)))
	}
	sc, project, err := sf.forReading()
	if err != nil {
		return c.reject(err)
	}
	// Half of "nothing stored" is that the other half was not asked about. A
	// user who has just filed a global preference would otherwise read an empty
	// project list as the fact having vanished.
	if !sf.local && !sf.global {
		fmt.Fprintf(c.err, "this project only (%s); pass --global for what follows you into every project\n", project)
	}

	q := url.Values{"scope": {string(sc)}, "limit": {strconv.Itoa(*limit)}}
	if project != "" {
		q.Set("project", project)
	}
	var reply cliapi.FactsResponse
	if code := c.call(*addr, http.MethodGet, "/v1/cli/facts?"+q.Encode(), nil, &reply); code != 0 {
		return code
	}
	if len(reply.Facts) == 0 {
		fmt.Fprintln(c.out, "nothing stored")
		return 0
	}
	for _, f := range reply.Facts {
		line := f.Content
		if f.Category != "" {
			line = "(" + f.Category + ") " + line
		}
		if f.ID != "" {
			line = f.ID + "  " + line
		}
		fmt.Fprintln(c.out, line)
	}
	return 0
}

func (c *cli) digest(args []string) int {
	fs := c.flags("digest", digestUsage)
	addr := bindAddr(fs)
	var sf scopeFlags
	sf.bind(fs)
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("digest takes no arguments, got %q", fs.Arg(0)))
	}
	sc, err := sf.choose()
	if err != nil {
		return c.reject(err)
	}

	project := ""
	switch sc {
	case scope.Local:
		if _, project, err = withProject(scope.Local); err != nil {
			return c.reject(err)
		}
	case scope.Any:
		// Neither flag means both scopes, so the global half is still worth
		// describing from a directory that has gone away underneath the shell.
		project, _ = scope.Project(".")
	}

	var reply cliapi.DigestResponse
	if code := c.call(*addr, http.MethodPost, "/v1/cli/digest", cliapi.DigestRequest{
		Scope: string(sc), Project: project,
	}, &reply); code != 0 {
		return code
	}
	fmt.Fprintln(c.out, reply.Digest)
	return 0
}

// call makes one request to a daemon somebody else started. A missing daemon
// and a broken one are both exit 1; a request the daemon rejected is the
// command line's fault, so it is exit 2.
func (c *cli) call(base, method, path string, body, out any) int {
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			fmt.Fprintln(c.err, "shoulderd:", err)
			return 2
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(base, "/")+path, payload)
	if err != nil {
		return c.reject(fmt.Errorf("bad --addr %q: %w", base, err))
	}
	req.Header.Set("Content-Type", "application/json")
	if token := setting("SHOULDER_TOKEN"); token != "" {
		req.Header.Set("X-Shoulder-Token", token)
	}

	resp, err := (&http.Client{Timeout: clientTimeout}).Do(req)
	if err != nil {
		fmt.Fprintf(c.err, "shoulderd: no daemon answering at %s (%v); start one with `shoulderd`\n", base, err)
		return 1
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		fmt.Fprintf(c.err, "shoulderd: the daemon at %s stopped mid-reply: %v\n", base, err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		if msg := serverError(raw); msg != "" {
			fmt.Fprintln(c.err, "shoulderd:", msg)
		} else {
			// Nothing that answers these routes replies without a reason, so a
			// bare status is a daemon that has never heard of the route — which
			// is what an old process still listening looks like.
			route, _, _ := strings.Cut(path, "?")
			fmt.Fprintf(c.err, "shoulderd: the daemon at %s answered %s for %s; it is older than this CLI, so restart it from this build\n",
				base, resp.Status, route)
		}
		if resp.StatusCode == http.StatusBadRequest {
			return 2
		}
		return 1
	}
	if err := json.Unmarshal(raw, out); err != nil {
		fmt.Fprintf(c.err, "shoulderd: unreadable reply from %s: %v\n", base, err)
		return 1
	}
	return 0
}

// serverError is the daemon's own sentence, which names the flag the user
// should have passed. It is empty when the reply carries none.
func serverError(raw []byte) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return body.Error
}

// flagSet buffers what the flag package prints so the same text can leave by
// the right door: a request for help is answered on stdout, a command line the
// user has to fix is reported on stderr.
type flagSet struct {
	*flag.FlagSet
	printed bytes.Buffer
}

// flags builds a subcommand's flags with the usage text it should teach.
// PrintDefaults is not used: it renders --local and --global as two unrelated
// optional booleans, which is the opposite of the one rule this daemon has.
func (c *cli) flags(name, usage string) *flagSet {
	fs := &flagSet{FlagSet: flag.NewFlagSet(name, flag.ContinueOnError)}
	fs.SetOutput(&fs.printed)
	fs.Usage = func() { fmt.Fprint(&fs.printed, usage) }
	return fs
}

// parse reports the exit code the subcommand must return, or -1 to carry on.
func (c *cli) parse(fs *flagSet, args []string) int {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return -1
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(c.out, fs.printed.String())
		return 0
	}
	fmt.Fprint(c.err, fs.printed.String())
	return 2
}

// trailingFlag catches `shoulderd message "text" --no-update`. Go's flag
// package stops at the first argument that is not a flag, so what follows would
// be silently swallowed into the text and the option asked for would be off:
// the exact opposite of what was typed, with nothing said about it.
func trailingFlag(args []string) error {
	for _, a := range args {
		if len(a) > 1 && strings.HasPrefix(a, "-") {
			return fmt.Errorf("%s comes after the text, where it is read as part of it; flags go first", a)
		}
	}
	return nil
}

// reject reports a command line the user has to fix.
func (c *cli) reject(err error) int {
	fmt.Fprintln(c.err, "shoulderd:", err)
	return 2
}

func bindAddr(fs *flagSet) *string {
	addr := setting("SHOULDER_ADDR")
	if addr == "" {
		addr = config.Load().Addr
	}
	return fs.String("addr", "http://"+addr, "relay base URL")
}

// scopeFlags is --local/--global, the choice the whole daemon is built around.
type scopeFlags struct {
	local  bool
	global bool
}

func (s *scopeFlags) bind(fs *flagSet) {
	fs.BoolVar(&s.local, "local", false, "this project only")
	fs.BoolVar(&s.global, "global", false, "you, in every project")
}

// choose reports the scope the flags name, and Any when they name none.
func (s *scopeFlags) choose() (scope.Scope, error) {
	switch {
	case s.local && s.global:
		return scope.Any, errors.New("--local and --global are mutually exclusive")
	case s.local:
		return scope.Local, nil
	case s.global:
		return scope.Global, nil
	}
	return scope.Any, nil
}

// forWriting refuses to pick a scope the user did not. Nothing later can catch
// the mistake: a fact filed in the wrong half is not wrong, it is merely absent
// from the project that needed it and present in every project that did not.
func (s *scopeFlags) forWriting() (scope.Scope, string, error) {
	sc, err := s.choose()
	if err != nil {
		return scope.Any, "", err
	}
	if sc == scope.Any {
		return scope.Any, "", errors.New("say where this belongs: pass --local or --global")
	}
	return withProject(sc)
}

// forReading defaults to the project the terminal is standing in. The asymmetry
// with forWriting is deliberate: reading the obvious place is what the user
// meant, and reading the wrong one costs them a sentence rather than a memory
// that resurfaces somewhere it does not belong.
func (s *scopeFlags) forReading() (scope.Scope, string, error) {
	sc, err := s.choose()
	if err != nil {
		return scope.Any, "", err
	}
	if sc == scope.Any {
		sc = scope.Local
	}
	return withProject(sc)
}

// withProject attaches the project a local scope is meaningless without. The
// daemon may be running in another directory, so the resolution happens here,
// where the user's shell is. A directory outside a repository is a project in
// its own right: knowledge is tied to the directory, not to git.
func withProject(sc scope.Scope) (scope.Scope, string, error) {
	if sc != scope.Local {
		return sc, "", nil
	}
	project, err := scope.Project(".")
	if err != nil {
		return scope.Any, "", err
	}
	return scope.Local, project, nil
}

// stringList collects a repeatable flag: --tag=build --tag=ci.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty tag")
	}
	*l = append(*l, v)
	return nil
}

// consolidate runs one tidying pass by hand. The daemon does this on its own at
// the end of a session and every few turns; this is for looking at the result,
// and for a store that has been collecting clutter since before it did.
func (c *cli) consolidate(args []string) int {
	fs := c.flags("consolidate", consolidateUsage)
	addr := bindAddr(fs)
	var sf scopeFlags
	sf.bind(fs)
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("consolidate takes no arguments, got %q", fs.Arg(0)))
	}
	sc, project, err := sf.forWriting()
	if err != nil {
		return c.reject(err)
	}

	var reply cliapi.ConsolidateResponse
	if code := c.call(*addr, http.MethodPost, "/v1/cli/consolidate", cliapi.ConsolidateRequest{
		Scope: string(sc), Project: project,
	}, &reply); code != 0 {
		return code
	}
	fmt.Fprintf(c.out, "%d dropped, %d merged\n", reply.Dropped, reply.Merged)
	return 0
}
