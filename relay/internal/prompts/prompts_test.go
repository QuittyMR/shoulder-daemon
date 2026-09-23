package prompts

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The three rewrites the rule is taught by. They are spelled out here rather
// than matched loosely because they are the whole of the instruction: a model
// shown "state it affirmatively" and no example writes the opposite of the
// rule about half the time, and these three are the shapes that keep the
// subject and the restriction without inventing the alternative.
//
// Each of them is a wording the loop produced and the store's polarity gate
// then read as an affirmative claim. The earlier "only commit data that isn't
// secret" is gone from the list on purpose: it carries a negator on its
// subordinate clause, the gate counted it as a denial, and an example that
// fails the rule above it teaches the failure.
var affirmativeRewrites = []string{
	`"never commit secrets" is stored as`,
	`"only commit data that is non-secret"`,
	`"do not use var" as "use of var is forbidden"`,
	`"never force push to main" as "force pushes to main are forbidden"`,
}

// Both prompts that write facts have to carry the rule, and the decision one
// has to carry it at every pickiness: the level rewrites the paragraph the
// rule sits under, and a rendering that swallowed it would store prohibitions
// on that level alone.
func TestEveryFactWritingPromptAsksForTheAffirmativeForm(t *testing.T) {
	prompts := map[string]string{"learn": Learn}
	for i := range PickinessNames() {
		p := Pickiness(i)
		prompts["decision/"+p.String()] = Decision(p)
	}
	for name, out := range prompts {
		t.Run(name, func(t *testing.T) {
			// Matched against the prompt with its line breaks collapsed. The
			// rule is a paragraph the wrapping moves every time a word is
			// added to it, and a pin that fails on a rewrap says nothing
			// about whether the instruction is still there.
			flat := strings.Join(strings.Fields(out), " ")
			if !strings.Contains(flat, "State a rule as what is allowed or what is forbidden") {
				t.Fatal("the prompt does not ask for a rule to be stated as what is allowed or forbidden")
			}
			// The words the store's own polarity gate counts. Naming them is
			// what turned "state it affirmatively" from a taste into a
			// checkable property of the sentence that comes back.
			for _, banned := range []string{`"not"`, `"never"`, `"don't"`, `"must not"`, `"no longer"`} {
				if !strings.Contains(flat, banned) {
					t.Fatalf("the prompt does not forbid %s in a stored rule", banned)
				}
			}
			for _, want := range affirmativeRewrites {
				if !strings.Contains(flat, want) {
					t.Fatalf("the prompt does not show the rewrite %s", want)
				}
			}
			// The rule is only safe because it forbids inventing the
			// alternative: "do not use var" must not become "use const".
			if !strings.Contains(flat, "Keep the subject and the restriction, and add nothing") {
				t.Fatal("the rule does not say the subject is kept and nothing is added")
			}
		})
	}
}

var workedContent = regexp.MustCompile(`"content":"([^"]*)"`)

// A rule is taught by the worked examples as much as by the sentence stating
// it. One example that answers a prohibition with a prohibition teaches the
// opposite of the rule above it, and the model follows the example.
//
// The words checked for are the ones the store's own polarity gate counts, so
// a worked fact that passes here is one the gate reads as an affirmative
// claim; that is the property the rule exists to produce.
func TestNoWorkedFactIsWrittenAsAProhibition(t *testing.T) {
	negators := []string{"not", "never", "no", "without", "cannot", "anymore"}
	prompts := map[string]string{"learn": Learn, "consolidate": Consolidate}
	for i := range PickinessNames() {
		p := Pickiness(i)
		prompts["decision/"+p.String()] = Decision(p)
	}
	for name, out := range prompts {
		t.Run(name, func(t *testing.T) {
			ms := workedContent.FindAllStringSubmatch(out, -1)
			if len(ms) == 0 {
				t.Fatal("the prompt shows no fact content at all, so it teaches no shape")
			}
			for _, m := range ms {
				content := m[1]
				if content == "" {
					continue // the empty output contract
				}
				for _, w := range words(content) {
					if strings.HasSuffix(w, "n't") {
						t.Errorf("worked fact %q is written as a prohibition (%q)", content, w)
					}
					for _, n := range negators {
						if w == n {
							t.Errorf("worked fact %q is written as a prohibition (%q)", content, n)
						}
					}
				}
			}
		})
	}
}

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	})
}
