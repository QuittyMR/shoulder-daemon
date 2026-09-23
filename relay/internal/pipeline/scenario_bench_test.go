//go:build scenario

// This file is a measurement of the whole loop, not a test of behaviour, and it
// is behind a build tag because it needs a live decision model and takes
// minutes:
//
//	go test -tags scenario ./internal/pipeline/ -run TestScenarioBenchmark -v
//
// The retrieval benchmark in internal/memory answers a narrower question than
// the one that matters. It asks a store a question ("which branch should I
// rebase onto") and reports where the answering record ranked. A session never
// asks a question: it says "committing now", and what has to happen is a chain
// of four things, any one of which can break while the other three work. The
// fact has to be recalled by the turn's own prose, the decision model has to
// choose to say something about it, a later turn that contradicts it has to
// recall it again so the model sees both at once, and the store has to end up
// holding the new fact and not the old one and not both.
//
// So this measures all four, per turn, against every backend the daemon can be
// built with, constructed the way cmd/shoulderd constructs them. It asserts
// nothing: a scenario that fails is printed and counted, because the numbers
// are the output.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/httpapi"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/minilm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

// seed is a fact the store already holds when the session starts. It is written
// through the connector rather than through a turn, so that what a scenario
// measures is the recall and the correction, not whether some earlier turn
// happened to store the right thing.
type seed struct {
	name     string
	content  string
	category string
	sc       scope.Scope
}

// placed is a seed after it has been written: its sentence, for scoring recall,
// and the id the store gave it, for scoring the supersede. The supersede is
// scored against the id rather than the sentence because the question is
// whether the record the session was shown is the one that was replaced, and
// two records can hold the same sentence.
type placed struct{ content, id string }

// turnCase is one turn and everything that must be true of it by the time
// Consult returns. Each expectation is optional: a turn with no recall named
// counts towards nothing, which is what lets a scenario carry setup turns.
type turnCase struct {
	label           string
	user, assistant string

	// recall names the seed whose content has to appear in some search this
	// turn made. It is the first link of the chain and the only one a
	// retrieval benchmark measures.
	recall string

	// inject are substrings the injected advice has to carry, matched
	// case-insensitively. Matching the seed verbatim would measure the model's
	// choice of words rather than whether it said the right thing.
	inject []string

	// silent says nothing at all may be injected. It is a separate field from
	// an empty inject so that "we did not care" and "speaking here is a
	// failure" are different expectations.
	silent bool

	// supersede names the seed that must be unreachable by the end of this
	// turn, replaced rather than merely joined by a second fact.
	supersede string
}

// wanted is one record the store must hold at the end, described rather than
// quoted: the replacement's wording is the model's, and pinning it would make
// this a test of one provider's phrasing.
type wanted struct {
	desc  string
	match func(content string) bool
}

type scenarioCase struct {
	name  string
	seeds []seed
	turns []turnCase
	final []wanted
}

// affirmative reads whether a stored rule was written as what holds, and names
// the word that decided it. The rule in prompts.Decision asks for that form;
// this is how a scenario scores whether it was followed.
//
// The words are the store's own, through memory.Negator, and the check is on
// the whole sentence rather than its opening. An opening check called
// "Secrets must not be committed into this repository." affirmative, which is
// how this column came to report a success the wordings printed underneath it
// contradicted; and a negator anywhere is what the polarity gate reads, so a
// negator anywhere is what stops the next reversal from colliding with this
// record.
func affirmative(content string) (bool, string) {
	w, found := memory.Negator(content)
	return !found, w
}

// holds matches a record that carries every one of these substrings.
func holds(subs ...string) func(string) bool {
	return func(content string) bool {
		low := strings.ToLower(content)
		for _, s := range subs {
			if !strings.Contains(low, strings.ToLower(s)) {
				return false
			}
		}
		return true
	}
}

// holdsAffirmatively is holds plus the rule the decision prompt states: a
// restriction is stored as what holds, not as what must not happen.
func holdsAffirmatively(subs ...string) func(string) bool {
	inner := holds(subs...)
	return func(content string) bool {
		ok, _ := affirmative(content)
		return ok && inner(content)
	}
}

// lacks matches a record carrying none of these substrings. Every scenario that
// replaces a fact needs it: the seed and its replacement are about the same
// subject, so a matcher written only in what the replacement must say is
// satisfied by the seed sitting there unchanged, and the final column would
// report a supersede that never happened as a store in the right state.
func lacks(subs ...string) func(string) bool {
	return func(content string) bool {
		low := strings.ToLower(content)
		for _, s := range subs {
			if strings.Contains(low, strings.ToLower(s)) {
				return false
			}
		}
		return true
	}
}

func both(a, b func(string) bool) func(string) bool {
	return func(content string) bool { return a(content) && b(content) }
}

// commitSecrets is the owner's scenario, and the seed is deliberately the wrong
// way round: the store holds permission, the user withdraws it, and the only
// way the second turn can supersede the first fact is if the turn's own prose
// recalled it.
const commitSecrets = "secrets can be committed into this repository"

// scenarios are the loops worth measuring. Between them they cover every
// category the decision prompt knows, both scopes, a turn that must produce
// nothing, and a session whose working note must not become knowledge.
func scenarios() []scenarioCase {
	secrets := func(name, contradiction string) scenarioCase {
		return scenarioCase{
			name:  name,
			seeds: []seed{{"permitted", commitSecrets, "constraint", scope.Local}},
			turns: []turnCase{
				{
					label: "commit", user: "committing now", assistant: "Committing the staged changes.",
					recall: "permitted", inject: []string{"secret"},
				},
				{
					label: "withdraw", user: contradiction, assistant: "Understood.",
					recall: "permitted", supersede: "permitted",
				},
			},
			final: []wanted{{
				"one fact about secrets, affirmative, not the seed",
				both(holdsAffirmatively("secret"), lacks("can be committed")),
			}},
		}
	}

	return []scenarioCase{
		// 1. The owner's scenario, worded as a person would withdraw the rule.
		secrets("secrets-plain", "Don't commit secrets here"),

		// 2. The same loop with the contradiction already in each of the three
		// affirmative wordings the rule asks for. If the model only reaches the
		// affirmative form when the user hands it one, that is worth knowing;
		// if a wording the rule itself proposes fails to collide with the seed,
		// that is worth knowing more.
		secrets("secrets-only-public", "only commit data that is public. Never secrets"),
		secrets("secrets-non-secret", "only commit data that is non-secret"),
		secrets("secrets-forbidden", "committing secrets is forbidden"),

		// 3. Branch rename: the classic stale-structure fact.
		{
			name:  "branch-rename",
			seeds: []seed{{"master", "the main branch is master", "structure", scope.Local}},
			turns: []turnCase{
				{
					label: "rebase", user: "rebase onto the main branch", assistant: "Rebasing onto main.",
					recall: "master", inject: []string{"master"},
				},
				{
					label: "renamed", user: "we renamed the main branch to main last week", assistant: "Understood.",
					recall: "master", supersede: "master",
				},
			},
			final: []wanted{{"one fact naming main and not master", both(holds("main"), lacks("master"))}},
		},

		// 4. The false-injection measure. A store with one fact in it and a turn
		// with nothing to do with that fact: speaking here is the failure that
		// makes people turn the daemon off.
		{
			name:  "quiet-unrelated",
			seeds: []seed{{"rota", "the release rota is kept in docs/rota.md", "reference", scope.Local}},
			turns: []turnCase{
				{
					label: "unrelated", user: "rename the variable n to count in parser.go",
					assistant: "Renamed n to count in parser.go.", silent: true,
				},
			},
			final: []wanted{{"the seed, untouched", holds("rota.md")}},
		},

		// 5. Two turns of ordinary work that state no rule. The session keeps a
		// working note across them, and the note is by construction the exact
		// vocabulary of the turns; the measurement is that none of it has become
		// a fact.
		{
			name:  "notes-continuity",
			seeds: []seed{{"bazel", "the build is driven by bazel", "structure", scope.Local}},
			turns: []turnCase{
				{label: "read", user: "what does parser.go do?", assistant: "It tokenises the input and builds the AST.", silent: true},
				{label: "follow-up", user: "and the file after it?", assistant: "analyser.go walks that AST and resolves names.", silent: true},
			},
			final: []wanted{{"the seed alone", holds("bazel")}},
		},

		// 6. Reference, local: a stored procedure that answers the question the
		// turn just asked, then the procedure changing under it.
		{
			name:  "procedure-tests",
			seeds: []seed{{"maketest", "the test suite is run with make test, never go test directly", "reference", scope.Local}},
			turns: []turnCase{
				{
					label: "run", user: "run the tests", assistant: "Running go test ./... now.",
					recall: "maketest", inject: []string{"make test"},
				},
				{
					label: "moved", user: "we replaced make test with make check yesterday", assistant: "Understood.",
					recall: "maketest", supersede: "maketest",
				},
			},
			final: []wanted{{"one fact naming make check", holds("make check")}},
		},

		// 7. Decision, local: a deployment target that moved.
		{
			name:  "decision-region",
			seeds: []seed{{"useast", "deploys go to us-east-1", "decision", scope.Local}},
			turns: []turnCase{
				{
					label: "deploy", user: "deploy this", assistant: "Deploying the current build.",
					recall: "useast", inject: []string{"us-east-1"},
				},
				{
					label: "moved", user: "deploys go to eu-west-2 from today", assistant: "Understood.",
					recall: "useast", supersede: "useast",
				},
			},
			final: []wanted{{"one fact naming eu-west-2", holds("eu-west-2")}},
		},

		// 8. Preference, global: a fact about the person rather than the
		// codebase, which the session has to reach across scopes to recall.
		{
			name:  "preference-commit-mood",
			seeds: []seed{{"imperative", "the user wants commit messages written in the imperative mood", "preference", scope.Global}},
			turns: []turnCase{
				{
					label: "write", user: "write the commit message for this change", assistant: "Wrote: \"Added a retry to the uploader\".",
					recall: "imperative", inject: []string{"imperative"},
				},
				{
					label: "changed", user: "actually I want commit messages in the past tense from now on", assistant: "Understood.",
					recall: "imperative", supersede: "imperative",
				},
			},
			final: []wanted{{"one preference naming the past tense", holds("past tense")}},
		},

		// 9. Constraint, local: a permission withdrawn, which is the same shape
		// as the secrets loop on a subject the model has no prior opinion about.
		{
			name:  "constraint-force-push",
			seeds: []seed{{"allowed", "force pushing to the release branch is allowed", "constraint", scope.Local}},
			turns: []turnCase{
				{
					label: "push", user: "force push this to the release branch", assistant: "Force pushing to release.",
					recall: "allowed", inject: []string{"force"},
				},
				{
					label: "withdraw", user: "never force push to the release branch", assistant: "Understood.",
					recall: "allowed", supersede: "allowed",
				},
			},
			final: []wanted{{"one affirmative fact about force pushing", holdsAffirmatively("force")}},
		},

		// 10. Reference, local: where a thing is kept, and the thing moving.
		{
			name:  "reference-rota",
			seeds: []seed{{"rota", "the on-call rota is kept in docs/rota.md", "reference", scope.Local}},
			turns: []turnCase{
				{
					label: "ask", user: "who is on call this week?", assistant: "Let me find out who is on call.",
					recall: "rota", inject: []string{"rota"},
				},
				{
					label: "moved", user: "the rota moved to docs/oncall.md last sprint", assistant: "Understood.",
					recall: "rota", supersede: "rota",
				},
			},
			final: []wanted{{"one fact naming docs/oncall.md", holds("oncall.md")}},
		},

		// 11. Structure, global: a fact about the person's machines contradicting
		// what the assistant just did in a project. Nothing is superseded; the
		// store must end the scenario exactly as it started.
		{
			name:  "structure-distro",
			seeds: []seed{{"fedora", "the user's machines run Fedora", "structure", scope.Global}},
			turns: []turnCase{
				{
					label: "install", user: "install the build dependencies", assistant: "Running sudo apt-get install build-essential.",
					recall: "fedora", inject: []string{"fedora"},
				},
			},
			final: []wanted{{"the seed, untouched", holds("Fedora")}},
		},
	}
}

// recording wraps the connector under test and keeps what it was asked and what
// it did. It is separate from observingMemory in scenario_live_test.go because
// that one narrates for a person reading a log and this one has to be read by
// the code that scores a turn.
type recording struct {
	inner memory.Connector
	log   func(string, ...any)

	// searched is every record content returned since the last turn began. The
	// recall column is read from it, and it is the only place the tool loop's
	// own second search shows up.
	searched []string

	// superOld is every id that has been superseded, cumulatively, because a
	// scenario asks whether a seed is gone by the end of a turn rather than in
	// one particular call.
	superOld []string

	// created is every id this scenario put into the store, so that a shared
	// backend can be left as it was found and a global list can be narrowed to
	// what this run wrote.
	created []string
}

func (r *recording) Name() string { return r.inner.Name() }

func (r *recording) Search(ctx context.Context, q memory.Query) ([]memory.Record, error) {
	res, err := r.inner.Search(ctx, q)
	if err != nil {
		r.log("      search[%s] failed: %v", q.Scope, err)
		return res, err
	}
	r.log("      search[%s](%q) -> %d", q.Scope, textutil.Clip(q.Text, 70), len(res))
	for _, rec := range res {
		r.searched = append(r.searched, rec.Content)
		r.log("        %.3f %q", rec.Score, textutil.Clip(rec.Content, 70))
	}
	return res, nil
}

func (r *recording) List(ctx context.Context, q memory.Query) ([]memory.Record, error) {
	return r.inner.List(ctx, q)
}

func (r *recording) Store(ctx context.Context, rec memory.Record) (string, error) {
	id, err := r.inner.Store(ctx, rec)
	if err != nil {
		r.log("      STORE refused %q: %v", textutil.Clip(rec.Content, 70), err)
		return id, err
	}
	r.created = append(r.created, id)
	r.log("      STORE %q [%s/%s]", textutil.Clip(rec.Content, 70), rec.Scope, rec.Category)
	return id, nil
}

func (r *recording) Supersede(ctx context.Context, old string, rec memory.Record) (string, error) {
	id, err := r.inner.Supersede(ctx, old, rec)
	if err != nil {
		r.log("      SUPERSEDE %s failed: %v", clipID(old), err)
		return id, err
	}
	r.superOld = append(r.superOld, old)
	r.created = append(r.created, id)
	r.log("      SUPERSEDE %s -> %q", clipID(old), textutil.Clip(rec.Content, 70))
	return id, nil
}

func (r *recording) Forget(ctx context.Context, id string, q memory.Query) error {
	return r.inner.Forget(ctx, id, q)
}

func (r *recording) sawSuperseded(id string) bool {
	for _, s := range r.superOld {
		if s == id {
			return true
		}
	}
	return false
}

func (r *recording) sawRecalled(content string) bool {
	for _, s := range r.searched {
		if strings.Contains(s, content) {
			return true
		}
	}
	return false
}

func clipID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// spy records what the decision model actually replied. Consult keeps the
// decision document to itself - it parses it and logs only the parts that had
// an effect - so without this the only evidence of a turn where the model chose
// to say nothing is that nothing was said. The document is the evidence.
type spy struct {
	llm.Provider
	replies []string
}

func (s *spy) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (llm.Message, error) {
	m, err := s.Provider.Chat(ctx, msgs, tools)
	if err == nil {
		if text := strings.TrimSpace(m.Content); text != "" {
			s.replies = append(s.replies, text)
		}
		for _, c := range m.ToolCalls {
			s.replies = append(s.replies, "tool_call "+c.Name+" "+string(c.Args))
		}
	}
	return m, err
}

// backend is one of the stores a person can be running, built the way
// cmd/shoulderd builds it. build returns the connector and a function that
// leaves a shared backend as it was found; it is handed the recording so it can
// undo what the scenario wrote outside its own project.
type backend struct {
	name  string
	build func(t *testing.T, dir, project string) (memory.Connector, func(*recording), error)
}

// modelWait bounds a first run that has to fetch and convert the transformer. A
// later run loads it from the cache in about a second.
const modelWait = 20 * time.Minute

var (
	minilmOnce sync.Once
	minilmEmb  memory.Embedder
	minilmErr  error
)

// settledMiniLM builds the composite an install with SHOULDER_EMBEDDING=minilm
// gets, and waits for it. It is built once for the whole run rather than once
// per scenario: the store is fresh every time, the model is the same model, and
// reloading ninety megabytes of weights thirteen times measures the loader.
func settledMiniLM() (memory.Embedder, error) {
	minilmOnce.Do(func() {
		dir := config.Setting("SHOULDER_MODEL_DIR")
		if dir == "" {
			dir = memory.DefaultModelDir()
		}
		m := minilm.New(dir, nil)
		select {
		case <-m.Settled():
		case <-time.After(modelWait):
			minilmErr = fmt.Errorf("the model was still loading after %v", modelWait)
			return
		}
		if err := m.Err(); err != nil {
			minilmErr = err
			return
		}
		minilmEmb = memory.NewFallback(m, vectors.Embedder{})
	})
	return minilmEmb, minilmErr
}

func backends(t *testing.T) []backend {
	t.Helper()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	local := func(emb func() (memory.Embedder, error)) func(*testing.T, string, string) (memory.Connector, func(*recording), error) {
		return func(t *testing.T, _, _ string) (memory.Connector, func(*recording), error) {
			e, err := emb()
			if err != nil {
				return nil, nil, err
			}
			l, err := memory.NewLocal(filepath.Join(t.TempDir(), "facts.json"), e)
			if err != nil {
				return nil, nil, err
			}
			t.Cleanup(func() { _ = l.Close() })
			l.SetLog(quiet)
			// The re-embed pass the store runs for itself on a settled model
			// finds nothing, because the model was ready before the store was
			// opened. Running it anyway is what makes that a measured fact
			// rather than an assumption.
			if _, err := l.Reembed(context.Background()); err != nil {
				return nil, nil, err
			}
			return memory.Checked(l), nil, nil
		}
	}

	out := []backend{
		{"local+glove", local(func() (memory.Embedder, error) { return vectors.Embedder{}, nil })},
		{"local+minilm", local(settledMiniLM)},
		{"docs+glove", func(t *testing.T, _, _ string) (memory.Connector, func(*recording), error) {
			base := t.TempDir()
			d, err := memory.NewDocs(memory.DocsOptions{
				GlobalDir:   filepath.Join(base, "global"),
				DirName:     "docs",
				SessionPath: filepath.Join(base, "session.json"),
				CacheDir:    filepath.Join(base, "vectors"),
				Embedder:    vectors.Embedder{},
				Log:         quiet,
			})
			if err != nil {
				return nil, nil, err
			}
			t.Cleanup(func() { _ = d.Close() })
			return memory.Checked(d), nil, nil
		}},
	}

	if url := config.Setting("SHOULDER_MEMORY_URL"); url != "" {
		out = append(out, backend{"mcp-memory-service", func(t *testing.T, dir, project string) (memory.Connector, func(*recording), error) {
			store := memory.NewMCPMemory(url, config.Setting("SHOULDER_MEMORY_KEY"), 60*time.Second)
			c := memory.Checked(store)
			// The service is shared and outlives the run, and it deduplicates
			// on content alone, so a scenario that left its seeds behind would
			// have the next run's identical seeds refused as duplicates. The
			// project is unique per scenario; everything under it, and every id
			// this scenario created anywhere, goes at the end.
			return c, func(mem *recording) { clearRun(t, c, store, dir, project, mem) }, nil
		}})
	} else {
		t.Log("SHOULDER_MEMORY_URL is unset: measuring the built-in stores alone")
	}
	return out
}

// clearRun deletes everything a scenario put in a shared backend. This
// scenario's own project is emptied whole, because the project identity is a
// temporary directory nothing else will ever use again. The global scope is
// only ever cleared by id: a global list from a shared service is the person's
// real knowledge, and a benchmark that emptied it would be the worst bug in
// this file.
func clearRun(t *testing.T, c, raw memory.Connector, dir, project string, mem *recording) {
	t.Helper()
	ctx := context.Background()
	for _, kind := range []memory.Kind{memory.KindFact, memory.KindSession} {
		q := memory.Query{Scope: scope.Local, Project: project, Dir: dir, Limit: DigestLimit, Kind: kind}
		recs, err := c.List(ctx, q)
		if err != nil {
			t.Logf("  cleanup: list %s: %v", kind, err)
			continue
		}
		for _, r := range recs {
			if err := c.Forget(ctx, r.ID, q); err != nil {
				t.Logf("  cleanup: forget %s: %v", clipID(r.ID), err)
			}
		}
	}
	// Every id this run created anywhere, deleted through the raw connector
	// rather than the checked one. Two reasons, and both are properties of the
	// service rather than of this benchmark. A global record is not reachable
	// by the checked Forget's scope proof once it has been superseded, and the
	// superseded id has to go: this service keeps a versioned update as a
	// version, so the sentence a supersede replaced stays in its duplicate
	// index and every later write of that sentence - the next run's seed - is
	// refused verbatim. Deleting the old id is what frees it.
	for _, id := range mem.created {
		if err := raw.Forget(ctx, id, memory.Query{Scope: scope.Global}); err != nil {
			t.Logf("  cleanup: forget %s: %v", clipID(id), err)
		}
	}
}

// turnResult is one row of the table.
type turnResult struct {
	label     string
	recall    string
	inject    string
	supersede string
	took      time.Duration
	// ok is false when any expectation this turn carried was not met, which is
	// what the totals count and what decides whether the turn's detail is
	// worth reading.
	ok bool
}

type scenarioResult struct {
	name    string
	turns   []turnResult
	final   string
	finalOK bool
	held    []string
}

type tally struct{ want, got int }

func (t *tally) add(wanted, ok bool) {
	if !wanted {
		return
	}
	t.want++
	if ok {
		t.got++
	}
}

func (t tally) String() string {
	if t.want == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", t.got, t.want, 100*float64(t.got)/float64(t.want))
}

type backendResult struct {
	name      string
	scenarios []scenarioResult
	recall    tally
	inject    tally
	supersede tally
	final     tally
	latency   []time.Duration
	failed    string
}

// TestScenarioBenchmark runs every scenario against every backend and prints
// what happened. Nothing here fails the run: a broken chain is the measurement.
func TestScenarioBenchmark(t *testing.T) {
	t.Setenv("SHOULDER_ENV_FILE", "")
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)

	if llm.EnvSpec() == "" {
		t.Skip("SHOULDER_LLM names no provider: set it and its key in the environment or in the daemon's env file, or this benchmark has nothing to measure")
	}
	provider, err := llm.FromEnv()
	if err != nil {
		t.Skipf("SHOULDER_LLM is set but the provider could not be built (its key is probably missing): %v", err)
	}
	if provider == nil {
		t.Skip("SHOULDER_LLM names no provider")
	}
	t.Logf("provider %s model %s", provider.Name(), llm.ModelOf(provider))

	cases := scenarios()
	var results []backendResult
	for _, b := range backends(t) {
		res := backendResult{name: b.name}
		t.Run(b.name, func(t *testing.T) {
			for _, sc := range cases {
				t.Run(sc.name, func(t *testing.T) {
					sr, lat, err := runScenario(t, provider, b, sc)
					if err != nil {
						res.failed = err.Error()
						t.Logf("backend unavailable: %v", err)
						t.SkipNow()
					}
					res.scenarios = append(res.scenarios, sr)
					res.latency = append(res.latency, lat...)
					for _, tr := range sr.turns {
						res.recall.add(tr.recall != "-", tr.recall == "hit")
						res.inject.add(tr.inject != "-", tr.inject == "ok")
						res.supersede.add(tr.supersede != "-", tr.supersede == "ok")
					}
					res.final.add(true, sr.finalOK)
				})
				if res.failed != "" {
					break
				}
			}
		})
		results = append(results, res)
	}
	report(results)
}

// runScenario is one fresh store, one fresh session, and the whole sequence.
func runScenario(t *testing.T, provider llm.Provider, b backend, sc scenarioCase) (scenarioResult, []time.Duration, error) {
	t.Helper()
	ctx := context.Background()

	// The project is a directory of its own, outside any repository, so that
	// scope.Project gives this scenario an identity nothing else shares and the
	// docs store writes its files there instead of into a checkout.
	dir := t.TempDir()
	project, err := scope.Project(dir)
	if err != nil {
		return scenarioResult{}, nil, err
	}

	conn, cleanup, err := b.build(t, dir, project)
	if err != nil {
		return scenarioResult{}, nil, err
	}
	mem := &recording{inner: conn, log: t.Logf}
	if cleanup != nil {
		defer cleanup(mem)
	}

	reg := session.NewRegistry(100)
	box := outbox.New()
	queue := make(chan session.Event, 256)
	srv := httpapi.New(reg, box, queue, "", budget.Default())

	cfg := config.Load()
	cfg.WindowEvents, cfg.WindowChars = 40, 12000
	cfg.AdvisorTimeout = 90 * time.Second
	g := budget.Default()
	// The gate lives in httpapi, after the outbox this benchmark reads, so the
	// only part of it that reaches here is MaxChars. Zeroing the gap keeps that
	// explicit rather than accidental.
	g.MinTurnGap = 0
	cfg.Budget = g

	watched := &spy{Provider: provider}
	p := &Pipeline{
		Cfg: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: srv.Metrics, Registry: reg, Outbox: box,
		Settings: settings.ForProvider(watched), Memory: mem, Queue: queue,
	}

	seeded := map[string]placed{}
	for _, s := range sc.seeds {
		rec := memory.Record{Content: s.content, Category: s.category, Scope: s.sc}
		if s.sc == scope.Local {
			rec.Project, rec.Dir = project, dir
		}
		id, serr := mem.Store(ctx, rec)
		if serr != nil {
			// A shared service that already holds this sentence refuses it, and
			// every expectation below then measures a store that never had the
			// seed. Reported rather than skipped: it is a property of that
			// backend.
			return scenarioResult{name: sc.name, final: "seed refused: " + serr.Error()}, nil, nil
		}
		seeded[s.name] = placed{content: s.content, id: id}
	}

	sid := "bench-" + sc.name
	res := scenarioResult{name: sc.name}
	var lat []time.Duration
	for i, tc := range sc.turns {
		mem.searched = mem.searched[:0]
		watched.replies = watched.replies[:0]
		t.Logf("  turn %d (%s): %q", i+1, tc.label, tc.user)

		now := time.Now()
		reg.Observe(session.Event{
			Protocol: 1, Harness: "bench", SessionID: sid, TS: now,
			Kind: session.KindUserPrompt, CWD: dir, Prompt: tc.user,
		})
		reg.Observe(session.Event{
			Protocol: 1, Harness: "bench", SessionID: sid, TS: now,
			Kind: session.KindTurnEnd, CWD: dir, Assistant: tc.assistant,
		})

		cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
		start := time.Now()
		p.Consult(cctx, sid)
		took := time.Since(start)
		cancel()
		lat = append(lat, took)

		for _, reply := range watched.replies {
			t.Logf("    decision: %s", reply)
		}
		advice := drain(box, sid, reg.Turn(sid))
		res.turns = append(res.turns, score(t, mem, seeded, tc, advice, took))
	}

	res.held = finalFacts(t, mem, dir, project)
	res.final, res.finalOK = matchFinal(sc.final, res.held)
	t.Logf("  final store: %v -> %s", res.held, res.final)
	return res, lat, nil
}

// drain empties the outbox for this turn at both delivery points. Reading only
// the prompt point would report every note the model marked "action" - which is
// what it is told to do for a push, a delete or a deploy, and so exactly the
// turns this benchmark is about - as no injection at all.
func drain(box *outbox.Box, sid string, turn uint64) []session.Advice {
	var out []session.Advice
	for _, kind := range []session.Kind{session.KindUserPrompt, session.KindToolCall} {
		for {
			a, ok := box.Take(sid, turn, kind)
			if !ok {
				break
			}
			out = append(out, a)
		}
	}
	return out
}

// score reads one turn's four columns. A column a turn made no claim about is
// "-" and counts towards nothing.
func score(t *testing.T, mem *recording, seeded map[string]placed, tc turnCase, advice []session.Advice, took time.Duration) turnResult {
	t.Helper()
	tr := turnResult{label: tc.label, recall: "-", inject: "-", supersede: "-", took: took, ok: true}

	if tc.recall != "" {
		tr.recall = "miss"
		if mem.sawRecalled(seeded[tc.recall].content) {
			tr.recall = "hit"
		} else {
			tr.ok = false
		}
	}
	var texts []string
	for _, a := range advice {
		texts = append(texts, fmt.Sprintf("[%s] %s", a.Level, a.Text))
	}
	switch {
	case tc.silent:
		tr.inject = "ok"
		if len(advice) > 0 {
			tr.inject = "spoke"
			tr.ok = false
		}
	case len(tc.inject) > 0:
		tr.inject = "silent"
		joined := strings.ToLower(strings.Join(texts, " "))
		if len(advice) > 0 {
			tr.inject = "ok"
			for _, want := range tc.inject {
				if !strings.Contains(joined, strings.ToLower(want)) {
					tr.inject = "off-target"
					break
				}
			}
		}
		if tr.inject != "ok" {
			tr.ok = false
		}
	}
	if tc.supersede != "" {
		tr.supersede = "no"
		if mem.sawSuperseded(seeded[tc.supersede].id) {
			tr.supersede = "ok"
		} else {
			tr.ok = false
		}
	}
	if len(texts) == 0 {
		t.Logf("    inject: (none)")
	}
	for _, s := range texts {
		t.Logf("    inject: %s", textutil.Clip(s, 160))
	}
	t.Logf("    recall=%s inject=%s supersede=%s advisor=%s", tr.recall, tr.inject, tr.supersede, took.Round(time.Millisecond))
	return tr
}

// held is every fact the store holds for this scenario at the end: the whole of
// this scenario's project, plus the global records this run created. A global
// list from a shared service holds knowledge that was there before the run, and
// counting it would make the final column a property of the machine.
func finalFacts(t *testing.T, mem *recording, dir, project string) []string {
	t.Helper()
	ctx := context.Background()
	var out []string

	locals, err := mem.List(ctx, memory.Query{Scope: scope.Local, Project: project, Dir: dir, Limit: DigestLimit})
	if err != nil {
		t.Logf("  final list (local) failed: %v", err)
	}
	for _, r := range locals {
		out = append(out, r.Content)
	}

	mine := map[string]bool{}
	for _, id := range mem.created {
		mine[id] = true
	}
	globals, err := mem.List(ctx, memory.Query{Scope: scope.Global, Limit: DigestLimit})
	if err != nil {
		t.Logf("  final list (global) failed: %v", err)
	}
	for _, r := range globals {
		if mine[r.ID] {
			out = append(out, r.Content)
		}
	}
	sort.Strings(out)
	return out
}

// matchFinal pairs each wanted record with one record the store holds. Both
// directions matter: a missing record is a correction that was lost, and a
// leftover is the stale fact still sitting beside its replacement, which is the
// failure that makes every later turn recall the wrong thing.
func matchFinal(want []wanted, got []string) (string, bool) {
	used := make([]bool, len(got))
	var missing []string
	for _, w := range want {
		found := false
		for i, g := range got {
			if !used[i] && w.match(g) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			missing = append(missing, w.desc)
		}
	}
	var extra []string
	for i, g := range got {
		if !used[i] {
			extra = append(extra, fmt.Sprintf("%q", textutil.Clip(g, 60)))
		}
	}
	switch {
	case len(missing) == 0 && len(extra) == 0:
		return "ok", true
	case len(missing) > 0 && len(extra) > 0:
		return fmt.Sprintf("missing [%s], extra [%s]", strings.Join(missing, "; "), strings.Join(extra, "; ")), false
	case len(missing) > 0:
		return "missing [" + strings.Join(missing, "; ") + "]", false
	default:
		return "extra [" + strings.Join(extra, "; ") + "]", false
	}
}

func report(results []backendResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(w, "\n=== %s\n", r.name)
		if r.failed != "" {
			fmt.Fprintf(w, "could not be built: %s\n", r.failed)
			continue
		}
		fmt.Fprintln(w, "scenario\tturn\trecall\tinject\tsupersede\tadvisor")
		for _, sc := range r.scenarios {
			for _, tr := range sc.turns {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", sc.name, tr.label,
					tr.recall, tr.inject, tr.supersede, tr.took.Round(10*time.Millisecond))
			}
			state := sc.final
			if !sc.finalOK {
				state = "FAIL " + state
			}
			fmt.Fprintf(w, "%s\t= final\t\t\t\t%s\n", sc.name, state)
			// Verbatim, and with the affirmative verdict beside it. The
			// percentages above say how often the loop closed; only the
			// sentence and the verdict say what the store was left holding,
			// and the wording is the half of this measurement no number
			// carries.
			for _, h := range sc.held {
				verdict := "affirmative"
				if ok, n := affirmative(h); !ok {
					verdict = "negator " + strconv.Quote(n)
				}
				fmt.Fprintf(w, "%s\t  held\t%s\t\t\t%q\n", sc.name, verdict, h)
			}
		}
		mean, worst := latency(r.latency)
		fmt.Fprintf(w, "TOTAL %s\trecall %s\tinject %s\tsupersede %s\tfinal %s\tadvisor mean %s worst %s\n",
			r.name, r.recall, r.inject, r.supersede, r.final,
			mean.Round(time.Millisecond), worst.Round(time.Millisecond))
	}
	w.Flush()
}

func latency(all []time.Duration) (mean, worst time.Duration) {
	if len(all) == 0 {
		return 0, 0
	}
	var sum time.Duration
	for _, d := range all {
		sum += d
		if d > worst {
			worst = d
		}
	}
	return sum / time.Duration(len(all)), worst
}
