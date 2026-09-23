//go:build compare

// This file is a measurement, not a test of behaviour, and it is behind a build
// tag because it needs a live mcp-memory-service and takes minutes:
//
//	SHOULDER_MEMORY_URL=http://127.0.0.1:8101 go test -tags compare \
//	  ./internal/memory/ -run TestCompare -v
//
// It asks one question: where is the store that ships worse than the service it
// replaced, and by how much. The answer decides what the README is allowed to
// claim and which installs should be told to run the service instead.
package memory_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// A case is a query and the one record that answers it, among everything else
// the store holds. The rank of that record is the whole measurement: a store
// that returns it third has failed, because the advisor is handed a shortlist
// and reads the top of it.
type probe struct {
	group string
	query string
	want  string
}

// corpus is what a store holds after a few weeks of use, with the traps that
// separate the two implementations deliberately included: facts that share all
// their vocabulary and differ only in which way round they are, facts whose
// subject is an identifier no English model has ever seen, and facts long
// enough that averaging their words drowns the part that matters.
var corpus = []string{
	// Ordinary knowledge, well separated.
	"the main branch is called master, not main",
	"we ship every build to the staging cluster before production",
	"the release rota is kept in docs/rota.md",
	"prefers terse answers with no preamble",
	"the office cat is called Biscuit",
	"lunch is at one",

	// One subject, many near-identical facts. Every one of these is about a
	// service and a port, in the same words.
	"the billing service listens on port 8081",
	"the catalogue service listens on port 8082",
	"the notifications service listens on port 8083",
	"the search service listens on port 8084",
	"the identity service listens on port 8085",

	// Direction. Same bag of words, opposite meaning.
	"the api gateway calls the billing service, never the other way round",
	"the reporting job reads from the warehouse and writes to the archive",

	// Identifiers and paths, which no word-vector table has ever seen.
	"ledger_entries is partitioned by tenant_id and never by created_at",
	"the k8s namespace for staging is acme-stg-7 and for production acme-prd-2",
	"run migrations with ./bin/mig up, not with the goose binary directly",

	// Long, where the part that answers a question is a fifth of the sentence.
	"the deployment pipeline builds the container, pushes it to the registry, runs the smoke suite against a throwaway namespace, waits for the security scan, and only then promotes the tag, which means a release takes about forty minutes end to end and cannot be rushed by rerunning the job",
	"during the incident in March the team agreed that anything touching the payment path needs a second reviewer, that the runbook has to be updated in the same merge request, and that nobody deploys on a Friday afternoon without telling the on-call engineer first",

	// Negation and its unnegated twin, both stored.
	"the integration tests do not need a live Postgres any more; they use the in-memory fake",

	// Synonymy with no shared vocabulary.
	"the build is considered red until the linter passes as well",
}

var probes = []probe{
	{"plain", "which branch should I rebase onto", "the main branch is called master, not main"},
	{"plain", "where do builds get deployed", "we ship every build to the staging cluster before production"},
	{"plain", "how long should an answer be", "prefers terse answers with no preamble"},

	{"one subject, many facts", "what port does the catalogue service listen on", "the catalogue service listens on port 8082"},
	{"one subject, many facts", "which port is identity on", "the identity service listens on port 8085"},
	{"one subject, many facts", "notifications service port", "the notifications service listens on port 8083"},

	{"direction", "which service does the api gateway call", "the api gateway calls the billing service, never the other way round"},
	{"direction", "where does the reporting job write its output", "the reporting job reads from the warehouse and writes to the archive"},

	{"identifiers", "what column is ledger_entries partitioned by", "ledger_entries is partitioned by tenant_id and never by created_at"},
	{"identifiers", "which namespace is production in", "the k8s namespace for staging is acme-stg-7 and for production acme-prd-2"},
	{"identifiers", "how do I run database migrations here", "run migrations with ./bin/mig up, not with the goose binary directly"},

	{"long fact", "how long does a release take", "the deployment pipeline builds the container, pushes it to the registry, runs the smoke suite against a throwaway namespace, waits for the security scan, and only then promotes the tag, which means a release takes about forty minutes end to end and cannot be rushed by rerunning the job"},
	{"long fact", "who has to review a change to payments", "during the incident in March the team agreed that anything touching the payment path needs a second reviewer, that the runbook has to be updated in the same merge request, and that nobody deploys on a Friday afternoon without telling the on-call engineer first"},

	{"negation", "do the tests need a database running", "the integration tests do not need a live Postgres any more; they use the in-memory fake"},

	{"synonymy", "why is CI failing when the tests pass", "the build is considered red until the linter passes as well"},
}

const compareProject = "/shoulder-daemon/compare"

// builtIn is the store this measurement is about, named once because two
// things key off it: which store the model has to be settled for, and which
// one needs no marker on what it writes.
const builtIn = "local"

// newRun names this run. It is what keeps a service that already holds an
// earlier run's corpus from being measured on records it deduplicated away:
// that server deduplicates on content alone — the tags carrying the scope and
// the project are not consulted — so a second run's sentences are refused as
// duplicates of the first run's unless something in the text differs.
func newRun(kind string) string {
	return fmt.Sprintf("%s-%d", kind, time.Now().UnixNano())
}

// marked is a sentence as it goes to a store: unchanged for the built-in one
// and carrying the run's name for anything else.
//
// The built-in store is opened on a fresh file for every run and has nothing
// to collide with, and the marker is not free there. The tokeniser splits
// "[compare-1757231402963117000]" into "compare" and a twenty-digit string,
// both of which enter the rarity table and the sentence vector as words; the
// digits differ every run, so the scores this test prints moved by about a
// hundredth between two runs of the same code, which is the size of the
// differences it is being read for. Marking only the store that needs the
// marker is what makes two runs comparable.
func marked(store, content, run string) string {
	if store == builtIn {
		return content
	}
	return content + " [" + run + "]"
}

// embedders are the models a run can measure, keyed by SHOULDER_EMBEDDING so
// that measuring what an install gets is spelling the same word the daemon
// reads. Anything that loads in the background registers itself from its own
// file under a tag of its own, because building it drags in the model runtime
// that the store itself is free of.
var embedders = map[string]func(*testing.T) memory.Embedder{
	"":      func(*testing.T) memory.Embedder { return vectors.Embedder{} },
	"glove": func(*testing.T) memory.Embedder { return vectors.Embedder{} },
}

// floor is the score a record has to reach to be returned at all, overridable
// because it is calibrated per model and not per store: a table whose cosines
// run low answers nothing at a floor measured against another one, and a run
// that reports that as "not found" cannot say whether the ranking is bad or
// only the constant is.
func floor(t *testing.T) float64 {
	t.Helper()
	raw := os.Getenv("SHOULDER_COMPARE_MINSCORE")
	if raw == "" {
		return 0
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("SHOULDER_COMPARE_MINSCORE=%q: %v", raw, err)
	}
	return f
}

// underTest is the model this run measures, ready before the first write. A
// benchmark that stores while the model is still loading measures the words
// the store falls back to and reports them as the model's numbers.
func underTest(t *testing.T) memory.Embedder {
	t.Helper()
	name := strings.ToLower(strings.TrimSpace(os.Getenv("SHOULDER_EMBEDDING")))
	newEmbedder, ok := embedders[name]
	if !ok {
		t.Fatalf("SHOULDER_EMBEDDING=%q: not built into this run; add the tag that registers it", name)
	}
	return newEmbedder(t)
}

// fetchAndLoad bounds a model that is downloaded and converted on first use.
// A later run loads from the cache in under a second.
const fetchAndLoad = 20 * time.Minute

// settle waits for an embedder that arrives after the store was opened and
// then brings the store up to it. Without it the composite this daemon ships
// is measured on the fallback vectors its first writes landed with, under the
// model's own queries, which is a number no install ever sees for longer than
// it takes the pass to run.
func settle(t *testing.T, emb memory.Embedder, l *memory.Local) {
	t.Helper()
	s, ok := emb.(memory.Settler)
	if !ok {
		return
	}
	select {
	case <-s.Settled():
	case <-time.After(fetchAndLoad):
		t.Fatalf("the model was still loading after %v", fetchAndLoad)
	}
	// The store starts its own pass on the same signal, so this one usually
	// finds nothing left to do. Both take the same lock, and either order
	// leaves one completed pass behind, which is all this has to guarantee.
	n, err := l.Reembed(context.Background())
	if err != nil {
		t.Fatalf("re-embedding: %v", err)
	}
	t.Logf("the model has landed; %d records still wanted its vectors after the store's own pass", n)
}

func TestCompare(t *testing.T) {
	ctx := context.Background()

	emb := underTest(t)
	local, err := memory.NewLocal(filepath.Join(t.TempDir(), "facts.json"), emb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	stores := []struct {
		name string
		c    memory.Connector
	}{{builtIn, memory.Checked(local)}}

	if url := os.Getenv("SHOULDER_MEMORY_URL"); url != "" {
		stores = append(stores, struct {
			name string
			c    memory.Connector
		}{"mcp-memory-service", memory.Checked(memory.NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 60*time.Second))})
	} else {
		t.Log("SHOULDER_MEMORY_URL is unset: measuring the built-in store alone")
	}

	run := newRun("compare")

	type result struct {
		group, query string
		rank         int
		// first is what the store actually answered with, so a rank that
		// moved between two runs can be read as a better answer or a worse
		// one rather than as a number.
		first string
		// n is how many records came back at all. It is half of what a floor
		// costs: one that keeps every right answer and hands the advisor six
		// wrong ones with each of them has not kept anything.
		n    int
		took time.Duration
	}
	results := map[string][]result{}

	for _, store := range stores {
		for _, content := range corpus {
			_, err := store.c.Store(ctx, memory.Record{
				Content: marked(store.name, content, run),
				Scope:   scope.Local, Project: compareProject,
			})
			if err != nil {
				t.Logf("%s: store %q: %v", store.name, clip(content), err)
			}
		}
		if store.name == builtIn {
			settle(t, emb, local)
		}
		// The service indexes asynchronously in some builds; give it a moment
		// before asking, or the first queries measure an empty index.
		time.Sleep(2 * time.Second)

		for _, p := range probes {
			start := time.Now()
			got, err := store.c.Search(ctx, memory.Query{
				Text: p.query, Limit: 10, MinScore: floor(t),
				Scope: scope.Local, Project: compareProject,
			})
			took := time.Since(start)
			if err != nil {
				t.Fatalf("%s: search %q: %v", store.name, p.query, err)
			}
			rank := 0
			for i, r := range got {
				if strings.HasPrefix(r.Content, p.want) {
					rank = i + 1
					break
				}
			}
			first := "(nothing above the floor)"
			if len(got) > 0 {
				// The score, because it is what the floor is compared
				// against: two models that rank a corpus identically can
				// still need floors half an order apart.
				first = fmt.Sprintf("%.2f %s", got[0].Score, clip(got[0].Content))
			}
			results[store.name] = append(results[store.name], result{p.group, p.query, rank, first, len(got), took})
		}
	}

	for _, store := range stores {
		rs := results[store.name]
		var at1, at3, found int
		var mrr float64
		for _, r := range rs {
			if r.rank == 1 {
				at1++
			}
			if r.rank > 0 && r.rank <= 3 {
				at3++
			}
			if r.rank > 0 {
				found++
				mrr += 1 / float64(r.rank)
			}
		}
		n := float64(len(rs))
		fmt.Printf("\n%s: recall@1 %d/%d  recall@3 %d/%d  found %d/%d  MRR %.3f\n",
			store.name, at1, len(rs), at3, len(rs), found, len(rs), mrr/n)

		byGroup := map[string][]int{}
		var order []string
		for _, r := range rs {
			if _, seen := byGroup[r.group]; !seen {
				order = append(order, r.group)
			}
			byGroup[r.group] = append(byGroup[r.group], r.rank)
		}
		sort.SliceStable(order, func(i, j int) bool { return order[i] < order[j] })
		for _, g := range order {
			fmt.Printf("  %-24s ranks %v\n", g, byGroup[g])
		}

		// Per question, because a group's ranks say how well the store did and
		// not which question it did it on, and because two runs are compared
		// by what came first, not by how far down the wanted record was.
		var total, worst time.Duration
		for _, r := range rs {
			total += r.took
			if r.took > worst {
				worst = r.took
			}
			fmt.Printf("  %-4s n=%-3d %-24s %-46s %s\n", rankOf(r.rank), r.n, r.group, clip(r.query), r.first)
		}
		returned := 0
		for _, r := range rs {
			returned += r.n
		}
		fmt.Printf("  query latency: mean %v, worst %v over %d queries; %d records returned, %.1f per query\n",
			(total / time.Duration(len(rs))).Round(time.Microsecond), worst.Round(time.Microsecond), len(rs),
			returned, float64(returned)/n)
	}

	// Side by side, so the cases where they differ are the output rather than
	// something to be worked out from two tables.
	if len(stores) == 2 {
		fmt.Printf("\n%-4s %-4s  %-24s %s\n", "loc", "mcp", "group", "query")
		for i, p := range probes {
			a, b := results[stores[0].name][i].rank, results[stores[1].name][i].rank
			mark := "  "
			switch {
			case a != 1 && b == 1:
				mark = "<-" // the service answers this and the built-in store does not
			case a == 1 && b != 1:
				mark = "->"
			}
			fmt.Printf("%-4s %-4s %s %-24s %s\n", rankOf(a), rankOf(b), mark, p.group, p.query)
		}
	}
}

func rankOf(r int) string {
	if r == 0 {
		return "-"
	}
	return fmt.Sprint(r)
}

func clip(s string) string {
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}

// TestCompareIdentifierFamily is the scenario the two implementations do not
// merely differ on: they disagree about what a fact is.
//
// A project's knowledge is full of families — one sentence, one identifier
// changed. Ports, namespaces, table names, feature flags. To a mean of word
// vectors those sentences are the same sentence: the identifier is a token the
// table has never seen and contributes nothing, and everything else is shared.
// The built-in store therefore reads the second one as a restatement of the
// first and refuses it, which is not a wasted write — the caller's answer to a
// refusal is to supersede the record it collided with, so the family collapses
// to whichever member was written last, and every question about the others is
// answered confidently and wrongly.
func TestCompareIdentifierFamily(t *testing.T) {
	ctx := context.Background()
	family := []string{
		"the billing service listens on port 8081",
		"the catalogue service listens on port 8082",
		"the notifications service listens on port 8083",
		"the search service listens on port 8084",
		"the identity service listens on port 8085",
		"the audit service listens on port 8086",
		"the scheduler service listens on port 8087",
		"the webhooks service listens on port 8088",
	}
	asked := []struct{ query, want string }{
		{"what port does billing listen on", family[0]},
		{"what port does catalogue listen on", family[1]},
		{"what port does notifications listen on", family[2]},
		{"what port does search listen on", family[3]},
		{"what port does identity listen on", family[4]},
		{"what port does audit listen on", family[5]},
		{"what port does the scheduler listen on", family[6]},
		{"what port does webhooks listen on", family[7]},
	}

	emb := underTest(t)
	local, err := memory.NewLocal(filepath.Join(t.TempDir(), "facts.json"), emb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	stores := []struct {
		name string
		c    memory.Connector
	}{{builtIn, memory.Checked(local)}}
	if url := os.Getenv("SHOULDER_MEMORY_URL"); url != "" {
		stores = append(stores, struct {
			name string
			c    memory.Connector
		}{"mcp-memory-service", memory.Checked(memory.NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 60*time.Second))})
	}

	run := newRun("family")
	project := "/shoulder-daemon/family"

	for _, store := range stores {
		var refused int
		for _, content := range family {
			if _, err := store.c.Store(ctx, memory.Record{
				Content: marked(store.name, content, run),
				Scope:   scope.Local, Project: project,
			}); err != nil {
				refused++
				t.Logf("%s refused %q: %v", store.name, content, err)
			}
		}
		if store.name == builtIn {
			settle(t, emb, local)
		}
		time.Sleep(2 * time.Second)

		held, err := store.c.List(ctx, memory.Query{Scope: scope.Local, Project: project, Limit: 100})
		if err != nil {
			t.Fatalf("%s: list: %v", store.name, err)
		}
		// The built-in store is opened on a file of this run's own, so
		// everything the scope holds is this run's; the service is not, and
		// only the marked records are.
		mine := len(held)
		if store.name != builtIn {
			mine = 0
			for _, r := range held {
				if strings.Contains(r.Content, run) {
					mine++
				}
			}
		}

		right := 0
		for _, a := range asked {
			got, err := store.c.Search(ctx, memory.Query{
				Text: a.query, Limit: 5, Scope: scope.Local, Project: project,
			})
			if err != nil {
				t.Fatalf("%s: search: %v", store.name, err)
			}
			if len(got) > 0 && strings.HasPrefix(got[0].Content, a.want) {
				right++
			}
		}
		fmt.Printf("%-20s wrote %d/%d, refused %d, holds %d, answered %d/%d correctly\n",
			store.name, len(family)-refused, len(family), refused, mine, right, len(asked))
	}
}

// A pair is one restriction written both ways: as the prohibition somebody
// says out loud, and as the affirmative statement of the same restriction the
// write path is now asked to store instead. The three rewrites the prompts
// show are the first three, and the rest follow their pattern - the subject
// and the restriction are kept, the sentence states what is allowed or what is
// forbidden, and nothing the conversation did not say is added.
//
// No affirmative here carries a negator anywhere in it, which is what the
// prompts now ask for. The earlier wordings did: "only commit data that isn't
// secret" and "deploy only on days that aren't Friday" put the negator on a
// subordinate clause, where the polarity gate reads it exactly as it reads one
// at the front, and a rule the gate holds to be a denial does not collide with
// its own reversal. That is the property this measurement prices.
type affirmativePair struct {
	subject     string
	prohibition string
	affirmative string
	// asked is what an agent would type before doing the thing the rule
	// governs. A rule that is only recalled by a question phrased like itself
	// is not recalled.
	asked []string
	// permit reverses the rule outright, and permitNegated says the same
	// reversal carrying a negator. Both are written because polarity is what
	// the store judges a collision by, and the two forms of a rule do not sit
	// on the same side of that gate: a refusal is the only way the pipeline is
	// told which record to supersede, so a reversal that is quietly kept
	// leaves the store holding a rule and its opposite at once.
	permit        string
	permitNegated string
	// distinct is a different rule about the same subject. Refusing it is the
	// expensive mistake: the caller's answer to a refusal is to supersede what
	// it collided with, so a false collision deletes a fact.
	distinct string
}

var affirmativePairs = []affirmativePair{
	{
		subject:     "secrets",
		prohibition: "never commit secrets",
		affirmative: "only commit data that is non-secret",
		asked: []string{
			"can I commit this .env file",
			"is it ok to put the api key in the repo",
			"what goes in the repository",
		},
		permit:        "secrets may be committed",
		permitNegated: "committing secrets is not forbidden",
		distinct:      "commit messages are in the imperative",
	},
	{
		subject:     "var",
		prohibition: "do not use var",
		affirmative: "use of var is forbidden",
		asked: []string{
			"is var allowed in this codebase",
			"how should I declare a local variable here",
			"can I write var x = 1",
		},
		permit:        "var is allowed",
		permitNegated: "use of var is not forbidden",
		distinct:      "var declarations go at the top of the function",
	},
	{
		subject:     "force push",
		prohibition: "never force push to main",
		affirmative: "force pushing to main is forbidden",
		asked: []string{
			"can I force push this fix to main",
			"is git push --force ok here",
			"how do I get my rebased branch onto main",
		},
		permit:        "force pushing to main is allowed",
		permitNegated: "force pushing to main is not forbidden",
		distinct:      "pushes to main go through a merge request",
	},
	{
		subject:     "friday deploys",
		prohibition: "do not deploy on Fridays",
		affirmative: "deploying on a Friday is forbidden",
		asked: []string{
			"can I ship this today, it is Friday",
			"which days can I deploy on",
			"is it fine to release before the weekend",
		},
		permit:        "deploying on a Friday is allowed",
		permitNegated: "deploying on a Friday is not forbidden",
		distinct:      "deploys are announced in the release channel",
	},
	{
		subject:     "passwords in logs",
		prohibition: "never log passwords",
		affirmative: "logging a password is forbidden",
		asked: []string{
			"can I log the request body",
			"is it ok to print the user's password while debugging",
			"what may go in a log line",
		},
		permit:        "passwords may be logged",
		permitNegated: "logging passwords is not forbidden",
		distinct:      "log lines are one JSON object each",
	},
	{
		subject:     "generated files",
		prohibition: "do not edit the generated files",
		affirmative: "editing a generated file is forbidden",
		asked: []string{
			"can I fix this typo in api.pb.go",
			"which files am I allowed to edit",
			"is it ok to change the generated client",
		},
		permit:        "editing a generated file is allowed",
		permitNegated: "editing a generated file is not forbidden",
		distinct:      "generated files are rebuilt with make gen",
	},
}

// loadedStore opens a store of this run's own and writes contents into it,
// saying which of them it accepted. A refusal is not a failure here: which
// sentences a form cannot get into the store at all is half of what this
// measurement is about.
func loadedStore(ctx context.Context, t *testing.T, emb memory.Embedder, project string, contents []string) (memory.Connector, map[string]bool) {
	t.Helper()
	l, err := memory.NewLocal(filepath.Join(t.TempDir(), "facts.json"), emb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	c := memory.Checked(l)
	stored := make(map[string]bool, len(contents))
	for _, content := range contents {
		_, err := c.Store(ctx, memory.Record{Content: content, Scope: scope.Local, Project: project})
		stored[content] = err == nil
		if err != nil {
			t.Logf("%s: refused %q: %v", project, clip(content), err)
		}
	}
	settle(t, emb, l)
	return c, stored
}

// verdict is a yes/no that can also be neither, for a probe whose rule never
// made it into the store. Printing "no" there would read as the store keeping
// a contradiction it was never shown.
type verdict string

const (
	yes verdict = "yes"
	no  verdict = "no"
	na  verdict = "-"
)

func tick(b bool) verdict {
	if b {
		return yes
	}
	return no
}

// TestCompareAffirmativeForm prices the write-time rewrite the decision prompt
// now asks for. A prohibition and its affirmative restatement bind the same
// work, so the only thing that can separate them is what the store does with
// them, on three counts: whether the rule is recalled by the question that
// should reach it, whether its reversal is refused - which is the only way the
// pipeline is ever told which record to supersede - and whether an unrelated
// rule about the same subject survives being written next to it.
//
// Each form gets a store of its own holding the same background corpus, so a
// rank is a rank among the same distractors, and each write probe gets a store
// holding the corpus and that one rule, so no probe is measured against what
// another probe left behind.
func TestCompareAffirmativeForm(t *testing.T) {
	ctx := context.Background()
	emb := underTest(t)
	model := emb.ID()
	if model == "" {
		model = "(no embedding: words alone)"
	}

	forms := []struct {
		name string
		text func(affirmativePair) string
	}{
		{"prohibition", func(p affirmativePair) string { return p.prohibition }},
		{"affirmative", func(p affirmativePair) string { return p.affirmative }},
	}

	type row struct {
		pair, form string
		wrote      bool
		ranks      []int
		scores     []float64
		permit     verdict
		permitNeg  verdict
		distinct   verdict
	}
	var rows []row

	// refusedAs writes probe into a store holding the corpus and this one
	// rule, and reports whether the store turned it away as a restatement.
	refusedAs := func(project, rule, probe string) verdict {
		c, stored := loadedStore(ctx, t, emb, project, append(append([]string{}, corpus...), rule))
		if !stored[rule] {
			return na
		}
		_, err := c.Store(ctx, memory.Record{Content: probe, Scope: scope.Local, Project: project})
		var dup *memory.ErrDuplicateSemantic
		switch {
		case err == nil:
			return no
		case errors.As(err, &dup):
			return yes
		default:
			t.Fatalf("storing %q: %v", probe, err)
			return na
		}
	}

	for _, form := range forms {
		project := "/shoulder-daemon/affirmative/" + form.name
		contents := append([]string{}, corpus...)
		for _, p := range affirmativePairs {
			contents = append(contents, form.text(p))
		}
		recall, stored := loadedStore(ctx, t, emb, project, contents)

		for _, p := range affirmativePairs {
			rule := form.text(p)
			r := row{pair: p.subject, form: form.name, wrote: stored[rule]}
			for _, q := range p.asked {
				got, err := recall.Search(ctx, memory.Query{
					Text: q, Limit: 10, MinScore: floor(t),
					Scope: scope.Local, Project: project,
				})
				if err != nil {
					t.Fatalf("search %q: %v", q, err)
				}
				rank, score := 0, 0.0
				for i, rec := range got {
					if rec.Content == rule {
						rank, score = i+1, rec.Score
						break
					}
				}
				r.ranks = append(r.ranks, rank)
				r.scores = append(r.scores, score)
			}
			base := "/shoulder-daemon/affirmative/" + form.name + "/" + p.subject
			r.permit = refusedAs(base+"/permit", rule, p.permit)
			r.permitNeg = refusedAs(base+"/permit-negated", rule, p.permitNegated)
			// Inverted on purpose: this column says whether the distinct fact
			// was kept, which is the right answer, so every column in the
			// table reads yes for good.
			r.distinct = na
			if v := refusedAs(base+"/distinct", rule, p.distinct); v != na {
				r.distinct = tick(v == no)
			}
			rows = append(rows, r)
		}
	}

	fmt.Printf("\naffirmative form, %s\n", model)
	fmt.Printf("%-18s %-12s %-6s %-10s %-20s %-10s %-12s %s\n",
		"pair", "form", "wrote", "ranks", "scores", "reversal", "neg reversal", "distinct kept")
	for _, r := range rows {
		ranks := make([]string, len(r.ranks))
		scores := make([]string, len(r.scores))
		for i := range r.ranks {
			ranks[i] = rankOf(r.ranks[i])
			scores[i] = fmt.Sprintf("%.2f", r.scores[i])
		}
		fmt.Printf("%-18s %-12s %-6s %-10s %-20s %-10s %-12s %s\n",
			r.pair, r.form, tick(r.wrote), strings.Join(ranks, "/"), strings.Join(scores, " "),
			r.permit, r.permitNeg, r.distinct)
	}

	// The counts the docs are allowed to quote, per form: pairs whose rule the
	// store took at all, questions that put it first, reversals refused (the
	// two wordings counted apart), and distinct facts kept.
	fmt.Printf("\n%-12s %-8s %-10s %-10s %-12s %-12s %s\n",
		"form", "wrote", "recall@1", "recall@3", "reversal", "neg reversal", "distinct kept")
	for _, form := range forms {
		var wrote, at1, at3, questions, permit, permitNeg, distinct, pairs int
		for _, r := range rows {
			if r.form != form.name {
				continue
			}
			pairs++
			if r.wrote {
				wrote++
			}
			for _, rank := range r.ranks {
				questions++
				if rank == 1 {
					at1++
				}
				if rank > 0 && rank <= 3 {
					at3++
				}
			}
			if r.permit == yes {
				permit++
			}
			if r.permitNeg == yes {
				permitNeg++
			}
			if r.distinct == yes {
				distinct++
			}
		}
		fmt.Printf("%-12s %-8s %-10s %-10s %-12s %-12s %s\n",
			form.name,
			fmt.Sprintf("%d/%d", wrote, pairs),
			fmt.Sprintf("%d/%d", at1, questions),
			fmt.Sprintf("%d/%d", at3, questions),
			fmt.Sprintf("%d/%d", permit, pairs),
			fmt.Sprintf("%d/%d", permitNeg, pairs),
			fmt.Sprintf("%d/%d", distinct, pairs))
	}
}
