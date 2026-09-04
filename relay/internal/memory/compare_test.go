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
package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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

func TestCompare(t *testing.T) {
	ctx := context.Background()

	local, err := NewLocal(filepath.Join(t.TempDir(), "facts.json"), vectors.Embedder{})
	if err != nil {
		t.Fatal(err)
	}
	stores := []struct {
		name string
		c    Connector
	}{{"local", Checked(local)}}

	if url := os.Getenv("SHOULDER_MEMORY_URL"); url != "" {
		stores = append(stores, struct {
			name string
			c    Connector
		}{"mcp-memory-service", Checked(NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 60*time.Second))})
	} else {
		t.Log("SHOULDER_MEMORY_URL is unset: measuring the built-in store alone")
	}

	// A tag of this run's own, so a service that already holds a previous run's
	// corpus is not measured on records it deduplicated away.
	run := fmt.Sprintf("compare-%d", time.Now().UnixNano())

	type result struct {
		group, query string
		rank         int
	}
	results := map[string][]result{}

	for _, store := range stores {
		for _, content := range corpus {
			// The marker makes each run's copy of a sentence a different
			// record, and is stripped before anything is compared.
			_, err := store.c.Store(ctx, Record{
				Content: content + " [" + run + "]",
				Scope:   scope.Local, Project: compareProject,
			})
			if err != nil {
				t.Logf("%s: store %q: %v", store.name, clip(content), err)
			}
		}
		// The service indexes asynchronously in some builds; give it a moment
		// before asking, or the first queries measure an empty index.
		time.Sleep(2 * time.Second)

		for _, p := range probes {
			got, err := store.c.Search(ctx, Query{
				Text: p.query, Limit: 10, Scope: scope.Local, Project: compareProject,
			})
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
			results[store.name] = append(results[store.name], result{p.group, p.query, rank})
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

	local, err := NewLocal(filepath.Join(t.TempDir(), "facts.json"), vectors.Embedder{})
	if err != nil {
		t.Fatal(err)
	}
	stores := []struct {
		name string
		c    Connector
	}{{"local", Checked(local)}}
	if url := os.Getenv("SHOULDER_MEMORY_URL"); url != "" {
		stores = append(stores, struct {
			name string
			c    Connector
		}{"mcp-memory-service", Checked(NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 60*time.Second))})
	}

	run := fmt.Sprintf("family-%d", time.Now().UnixNano())
	project := "/shoulder-daemon/family"

	for _, store := range stores {
		var refused int
		for _, content := range family {
			if _, err := store.c.Store(ctx, Record{
				Content: content + " [" + run + "]",
				Scope:   scope.Local, Project: project,
			}); err != nil {
				refused++
				t.Logf("%s refused %q: %v", store.name, content, err)
			}
		}
		time.Sleep(2 * time.Second)

		held, err := store.c.List(ctx, Query{Scope: scope.Local, Project: project, Limit: 100})
		if err != nil {
			t.Fatalf("%s: list: %v", store.name, err)
		}
		mine := 0
		for _, r := range held {
			if strings.Contains(r.Content, run) {
				mine++
			}
		}

		right := 0
		for _, a := range asked {
			got, err := store.c.Search(ctx, Query{
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
