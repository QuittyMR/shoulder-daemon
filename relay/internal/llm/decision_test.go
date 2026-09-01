package llm

import (
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

func TestParseDecisionTolerates(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantInject string
		wantFacts  int
		wantErr    bool
	}{
		{"clean json", `{"inject":"careful","facts":[{"content":"x","category":"decision"}]}`, "careful", 1, false},
		{"fenced json", "```json\n{\"inject\":\"\",\"facts\":[]}\n```", "", 0, false},
		{"fenced no lang", "```\n{\"inject\":\"hi\",\"facts\":[]}\n```", "hi", 0, false},
		{"leading prose", "Sure! Here you go:\n{\"inject\":\"hi\",\"facts\":[]}", "hi", 0, false},
		{"trailing prose", `{"inject":"hi","facts":[]} Hope that helps!`, "hi", 0, false},
		{"empty", "", "", 0, false},
		{"noop sentinel", "NOOP", "", 0, false},
		{"inject says none", `{"inject":"none","facts":[]}`, "", 0, false},
		{"blank facts dropped", `{"inject":"","facts":[{"content":"  "},{"content":"real"}]}`, "", 1, false},
		{"not json at all", "I think you should be careful.", "", 0, true},
		{"malformed json", `{"inject": }`, "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDecision(tc.raw)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if d.Inject != tc.wantInject {
				t.Errorf("inject = %q want %q", d.Inject, tc.wantInject)
			}
			if len(d.Facts) != tc.wantFacts {
				t.Errorf("facts = %d want %d", len(d.Facts), tc.wantFacts)
			}
		})
	}
}

// A model that exhausts its output budget mid-object leaves a valid injection
// followed by a truncated tail. Discarding the whole decision throws away work
// already done; storing a half-written fact would be worse.
func TestParseDecisionSalvagesTruncatedOutput(t *testing.T) {
	truncated := `{"inject":"Stored: the main branch is master, not main.","facts":[{"content":"the main bra`
	d, err := ParseDecision(truncated)
	if err != nil {
		t.Fatalf("expected salvage, got %v", err)
	}
	if d.Inject != "Stored: the main branch is master, not main." {
		t.Fatalf("inject not recovered: %q", d.Inject)
	}
	if len(d.Facts) != 0 {
		t.Fatalf("a truncated fact must never be stored, got %+v", d.Facts)
	}
}

func TestParseDecisionStillFailsOnUnsalvageableOutput(t *testing.T) {
	if _, err := ParseDecision(`{"facts":[{"content":"half`); err == nil {
		t.Fatal("output with no recoverable injection must still be an error")
	}
}

// The scope is part of the contract, so it must survive the parse. Case is
// repaired because a model that wrote "Local" decided; an absent or unknown
// scope is left invalid for the writer to reject and count.
func TestParseDecisionReadsTheScope(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want scope.Scope
	}{
		{"local", `{"inject":"","facts":[{"content":"x","scope":"local"}]}`, scope.Local},
		{"global", `{"inject":"","facts":[{"content":"x","scope":"global"}]}`, scope.Global},
		{"shouty", `{"inject":"","facts":[{"content":"x","scope":" GLOBAL "}]}`, scope.Global},
		{"absent", `{"inject":"","facts":[{"content":"x"}]}`, scope.Any},
		{"nonsense", `{"inject":"","facts":[{"content":"x","scope":"project"}]}`, scope.Scope("project")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDecision(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.Facts) != 1 {
				t.Fatalf("expected one fact, got %+v", d.Facts)
			}
			if d.Facts[0].Scope != tc.want {
				t.Fatalf("scope = %q, want %q", d.Facts[0].Scope, tc.want)
			}
		})
	}
}

// The prompt has to ask for what the parser reads, or a small model never sends
// it and every fact is dropped downstream.
func TestDecisionPromptDemandsAScope(t *testing.T) {
	for _, want := range []string{`"scope"`, "local", "global"} {
		if !strings.Contains(prompts.Decision, want) {
			t.Errorf("the decision prompt never mentions %s", want)
		}
	}
}

// The keywords are what a later "do it" is resolved against, so they have to
// survive the parse; the cap on them is applied by the caller, not here.
func TestParseDecisionReadsKeywords(t *testing.T) {
	d, err := ParseDecision(`{"inject":"","facts":[],"keywords":["auth","  retry  ","","rate limit"]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"auth", "retry", "rate limit"}
	if len(d.Keywords) != len(want) {
		t.Fatalf("keywords = %q", d.Keywords)
	}
	for i := range want {
		if d.Keywords[i] != want[i] {
			t.Errorf("keyword %d = %q want %q", i, d.Keywords[i], want[i])
		}
	}
}

func TestParseDecisionWithoutKeywords(t *testing.T) {
	d, err := ParseDecision(`{"inject":"hi","facts":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Keywords) != 0 {
		t.Fatalf("keywords = %q", d.Keywords)
	}
}
