package facts

import (
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

func TestReconcileCollapsesTheProseRestatement(t *testing.T) {
	explicit := []Fact{{Content: "the best number is 1", Category: "preference", Tags: []string{"numbers"}}}
	deduced := []Fact{{Content: "I will record that the best number is 1"}}

	got := Reconcile(explicit, deduced)
	if len(got) != 1 {
		t.Fatalf("expected one fact, got %d: %+v", len(got), got)
	}
	if got[0].Source != Explicit || got[0].Category != "preference" || len(got[0].Tags) != 1 {
		t.Fatalf("the explicit fact and its metadata must survive: %+v", got[0])
	}
}

func TestReconcileKeepsGenuinelyDifferentFacts(t *testing.T) {
	got := Reconcile(
		[]Fact{{Content: "the best number is 1"}},
		[]Fact{{Content: "the deploy target is staging"}},
	)
	if len(got) != 2 {
		t.Fatalf("expected two distinct facts, got %d: %+v", len(got), got)
	}
}

func TestReconcileDeduplicatesWithinASource(t *testing.T) {
	got := Reconcile(nil, []Fact{
		{Content: "the database connection pool size is 20"},
		{Content: "connection pool size for the database is 20"},
	})
	if len(got) != 1 {
		t.Fatalf("reordered restatement should collapse, got %d: %+v", len(got), got)
	}
}

func TestReconcileExplicitReplacesAnAlreadyKeptDeduced(t *testing.T) {
	got := Reconcile(nil, []Fact{{Content: "the best number is 1"}})
	if got[0].Source != Deduced {
		t.Fatalf("expected deduced, got %+v", got[0])
	}
	got = Reconcile(
		[]Fact{{Content: "best number is 1", Category: "preference"}},
		[]Fact{{Content: "noting that the best number is 1"}},
	)
	if len(got) != 1 || got[0].Source != Explicit || got[0].Category != "preference" {
		t.Fatalf("explicit must win: %+v", got)
	}
}

func TestReconcileDropsEmptyAndStopwordOnlyFacts(t *testing.T) {
	got := Reconcile(nil, []Fact{{Content: "  "}, {Content: "I will record that"}})
	if len(got) != 0 {
		t.Fatalf("facts with no substance should be dropped, got %+v", got)
	}
}

func TestReconcileSeparatesNegation(t *testing.T) {
	got := Reconcile(nil, []Fact{
		{Content: "deploys go to staging"},
		{Content: "deploys never go to production"},
	})
	if len(got) != 2 {
		t.Fatalf("different targets must stay separate, got %d: %+v", len(got), got)
	}
}

func TestNormaliseCategory(t *testing.T) {
	for _, c := range []string{"decision", "Constraint", " preference ", ""} {
		if _, ok := NormaliseCategory(c); !ok {
			t.Errorf("%q should be valid", c)
		}
	}
	// A word outside the set means nothing once it is stored, so it must be
	// caught while the model's output can still be inspected.
	for _, c := range []string{"observation", "note", "fact", "misc"} {
		got, ok := NormaliseCategory(c)
		if ok || got != "" {
			t.Errorf("%q should be rejected, got %q ok=%v", c, got, ok)
		}
	}
}

func TestAgainstRecalledSupersedesARestatement(t *testing.T) {
	recalled := []Recalled{{ID: "mem_1", Scope: scope.Global,
		Content: "the output style is set to Terse in the settings file"}}
	got := AgainstRecalled([]Fact{
		{Content: "the settings file sets output style Terse", Scope: scope.Global},
		{Content: "the relay listens on port 8787", Scope: scope.Global},
	}, "", recalled)

	if got[0].Supersedes != "mem_1" {
		t.Errorf("a restatement of a stored fact should supersede it, got %+v", got[0])
	}
	if got[1].Supersedes != "" {
		t.Errorf("an unrelated fact must not supersede anything, got %+v", got[1])
	}
}

func TestAgainstRecalledRespectsAnExplicitSupersedes(t *testing.T) {
	got := AgainstRecalled(
		[]Fact{{Content: "the output style is Terse", Scope: scope.Global, Supersedes: "chosen-by-model"}},
		"",
		[]Recalled{{ID: "mem_1", Content: "the output style is Terse", Scope: scope.Global}},
	)
	if got[0].Supersedes != "chosen-by-model" {
		t.Errorf("the model's own choice must win, got %q", got[0].Supersedes)
	}
}

func TestAgainstRecalledWithNoMatches(t *testing.T) {
	got := AgainstRecalled([]Fact{{Content: "a wholly new fact about deployments", Scope: scope.Global}},
		"",
		[]Recalled{{ID: "m", Content: "the widget calibration constant is 42", Scope: scope.Global}})
	if got[0].Supersedes != "" {
		t.Error("unrelated content must not be linked")
	}
}

// The scope travels with the fact through reconciliation: an explicit fact that
// wins a collapse must impose its own placement, not inherit the deduced one's.
func TestReconcileKeepsTheExplicitFactsScope(t *testing.T) {
	got := Reconcile(
		[]Fact{{Content: "the best number is 1", Scope: scope.Global}},
		[]Fact{{Content: "I will record that the best number is 1", Scope: scope.Local}},
	)
	if len(got) != 1 {
		t.Fatalf("expected one fact, got %+v", got)
	}
	if got[0].Scope != scope.Global {
		t.Fatalf("scope = %q, want global", got[0].Scope)
	}
}

func TestReconcileLeavesAnUndecidedScopeUndecided(t *testing.T) {
	got := Reconcile(nil, []Fact{{Content: "the deploy target is staging"}})
	if got[0].Scope.Valid() {
		t.Fatalf("a fact with no scope must not acquire one here, got %q", got[0].Scope)
	}
}

// The failure this rule exists for: the model files a preference locally while
// the same preference is already held globally and is sitting in recall. A
// supersede would carry the local placement onto the global record, so the
// preference would stop applying anywhere but this one project.
func TestAgainstRecalledNeverSupersedesAcrossScopes(t *testing.T) {
	got := AgainstRecalled(
		[]Fact{{Content: "user prefers terse answers", Scope: scope.Local}},
		"proj-a",
		[]Recalled{{ID: "mem_global", Content: "user prefers terse answers", Scope: scope.Global}},
	)
	if got[0].Supersedes != "" {
		t.Fatalf("a local fact must not supersede a global record, got %q", got[0].Supersedes)
	}
}

func TestAgainstRecalledNeverSupersedesAnotherProject(t *testing.T) {
	got := AgainstRecalled(
		[]Fact{{Content: "the main branch is master", Scope: scope.Local}},
		"proj-a",
		[]Recalled{{ID: "mem_b", Content: "the main branch is master",
			Scope: scope.Local, Project: "proj-b"}},
	)
	if got[0].Supersedes != "" {
		t.Fatalf("a fact must not supersede another project's record, got %q", got[0].Supersedes)
	}
}

func TestAgainstRecalledStillSupersedesWithinOneProject(t *testing.T) {
	got := AgainstRecalled(
		[]Fact{{Content: "the main branch is master", Scope: scope.Local}},
		"proj-a",
		[]Recalled{
			{ID: "mem_b", Content: "the main branch is master", Scope: scope.Local, Project: "proj-b"},
			{ID: "mem_a", Content: "the main branch is master", Scope: scope.Local, Project: "proj-a"},
		},
	)
	if got[0].Supersedes != "mem_a" {
		t.Fatalf("the record in this project should be corrected, got %q", got[0].Supersedes)
	}
}

// A fact whose scope was never decided has nowhere it belongs, so there is no
// record it can be said to replace.
func TestAgainstRecalledIgnoresAnUnscopedFact(t *testing.T) {
	got := AgainstRecalled(
		[]Fact{{Content: "user prefers terse answers"}},
		"proj-a",
		[]Recalled{{ID: "mem_global", Content: "user prefers terse answers", Scope: scope.Global}},
	)
	if got[0].Supersedes != "" {
		t.Fatalf("an unscoped fact must supersede nothing, got %q", got[0].Supersedes)
	}
}

// The decision model may mark a fact explicit itself, so an explicit fact can
// arrive after a deduced restatement of it has already been kept. Letting one
// that named no scope take its place loses the statement outright: the survivor
// is unscoped, and the writer refuses an unscoped fact.
func TestReconcileKeepsTheScopedFactWhenTheExplicitOneHasNoScope(t *testing.T) {
	got := Reconcile(nil, []Fact{
		{Content: "the best number is 1", Scope: scope.Global},
		{Content: "I will record that the best number is 1", Source: Explicit},
	})
	if len(got) != 1 {
		t.Fatalf("expected one fact, got %+v", got)
	}
	if got[0].Scope != scope.Global {
		t.Fatalf("scope = %q, want global: an unscoped explicit fact must not evict a scoped one", got[0].Scope)
	}
}

func TestReconcileExplicitStillWinsWhenItNamedItsOwnScope(t *testing.T) {
	got := Reconcile(nil, []Fact{
		{Content: "the best number is 1", Scope: scope.Local},
		{Content: "best number is 1", Source: Explicit, Category: "preference", Scope: scope.Global},
	})
	if len(got) != 1 {
		t.Fatalf("expected one fact, got %+v", got)
	}
	if got[0].Source != Explicit || got[0].Category != "preference" || got[0].Scope != scope.Global {
		t.Fatalf("the explicit fact and its own placement must win: %+v", got[0])
	}
}
