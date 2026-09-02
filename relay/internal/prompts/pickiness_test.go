package prompts

import (
	"strconv"
	"strings"
	"testing"
)

// Both spellings reach every level. The number matters as much as the name:
// somebody who wants "one less than it is doing now" should not have to learn
// whether careful is above or below balanced, and the only way that stays true
// is if the number and the name land on the same place.
func TestEveryLevelAnswersToItsNameAndItsNumber(t *testing.T) {
	names := PickinessNames()
	if len(names) == 0 {
		t.Fatal("no levels are offered at all")
	}
	for i, name := range names {
		want := Pickiness(i)
		t.Run(name, func(t *testing.T) {
			got, err := ParsePickiness(name)
			if err != nil || got != want {
				t.Fatalf("ParsePickiness(%q) = %v, %v; want %v", name, got, err, want)
			}
			got, err = ParsePickiness(strconv.Itoa(i))
			if err != nil || got != want {
				t.Fatalf("ParsePickiness(%q) = %v, %v; want %v", strconv.Itoa(i), got, err, want)
			}
			// A level read out of a daemon must be typeable back into one.
			// Snapshot prints String(), and `config set` parses what was
			// printed, so these two are one contract rather than two.
			if want.String() != name {
				t.Fatalf("%d prints as %q but is named %q", i, want.String(), name)
			}
			back, err := ParsePickiness(want.String())
			if err != nil || back != want {
				t.Fatalf("%v printed as %q and came back as %v, %v", want, want.String(), back, err)
			}
		})
	}
	// The list is what the refusals and the --help text offer, so the order
	// has to be the order the numbers mean.
	if names[0] != Eager.String() || names[len(names)-1] != Strict.String() {
		t.Fatalf("the levels are not listed low to high: %v", names)
	}
	if Default.String() != "balanced" {
		t.Fatalf("the default prints as %q", Default.String())
	}
}

// The names arrive from a shell, where a stray space or a capital is a typo
// nobody should have to see an error about.
func TestALevelIsRecognisedHoweverItWasTyped(t *testing.T) {
	for _, in := range []string{"STRICT", "  strict", "Strict\n", " 4 "} {
		got, err := ParsePickiness(in)
		if err != nil || got != Strict {
			t.Fatalf("ParsePickiness(%q) = %v, %v; want strict", in, got, err)
		}
	}
}

// A refusal has to say what would have worked. The person typing has no list
// in front of them, and "unknown pickiness" alone sends them to the source.
func TestParsePickinessRefusesWhatItCannotPlace(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// want is what the message has to carry. A number that is merely out
		// of range is answered with the range alone: somebody who typed 5
		// knows the names exist and is asking how far up they go.
		want []string
	}{
		{"a word that is not a level", "picky", []string{"picky", "balanced", "0-4"}},
		{"a number above the top level", "5", []string{"0-4"}},
		{"a number below the bottom one", "-1", []string{"0-4"}},
		{"nothing at all", "   ", []string{"balanced", "0-4"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParsePickiness(c.in)
			if err == nil {
				t.Fatalf("ParsePickiness(%q) = %v with no error", c.in, got)
			}
			// A caller that ignores the error must still be left holding the
			// documented default rather than eager, which is the level that
			// stores everything.
			if got != Default {
				t.Fatalf("a refusal handed back %v rather than the default %v", got, Default)
			}
			msg := err.Error()
			for _, want := range c.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q does not mention %q, so it does not say what would have worked", msg, want)
				}
			}
		})
	}
}

// A level outside the range prints as something, and that something must not
// be an index into the names array.
func TestAnImpossibleLevelHasAName(t *testing.T) {
	for _, p := range []Pickiness{-1, 5, 99} {
		if got := p.String(); got != "invalid" {
			t.Fatalf("Pickiness(%d).String() = %q", p, got)
		}
		if p.Valid() {
			t.Fatalf("Pickiness(%d) reports itself valid", p)
		}
	}
}

// The whole point of the knob is that the model is told something different.
// Two levels that render the same prompt are a knob that does nothing.
func TestEachLevelRendersADifferentPrompt(t *testing.T) {
	seen := map[string]Pickiness{}
	for i := range PickinessNames() {
		p := Pickiness(i)
		out := Decision(p)
		if prev, dup := seen[out]; dup {
			t.Fatalf("%v renders exactly what %v renders", p, prev)
		}
		seen[out] = p
	}
}

// Pickiness rewrites one paragraph of the prompt. Everything the decision
// parser depends on sits outside that paragraph, and a rewrite that dropped it
// would leave a level that quietly produces unusable output.
func TestEveryRenderingKeepsWhatTheParserNeeds(t *testing.T) {
	// The output contract, the field that lets a fact replace another, the
	// categories the writer normalises against, and the scope that has no
	// default. Each of these is read back out of the model's reply.
	wants := []string{
		`{"inject":"","level":"","facts":[{"content":"","category":"","scope":"local","tags":[],"supersedes":""}],"keywords":[]}`,
		"JSON only, no prose, no fence",
		`"supersedes"`,
		`"scope"`,
		"decision | constraint | preference | correction | structure | reference",
		"local for this codebase, global for the person",
	}
	for i := range PickinessNames() {
		p := Pickiness(i)
		t.Run(p.String(), func(t *testing.T) {
			out := Decision(p)
			for _, want := range wants {
				if !strings.Contains(out, want) {
					t.Fatalf("the %v prompt no longer contains %q", p, want)
				}
			}
		})
	}
}

// This runs on the decision path, which must never take a session down, and by
// the time it runs the value has already been through ParsePickiness. A level
// that got here invalid anyway is a bug above, not a reason to stop watching.
func TestAnInvalidLevelRendersTheDefault(t *testing.T) {
	want := Decision(Default)
	for _, p := range []Pickiness{-1, 5, 99} {
		if got := Decision(p); got != want {
			t.Fatalf("Decision(%d) is not the default prompt", p)
		}
	}
}

// A missing or spare argument to Sprintf does not panic, it leaves %!s(MISSING)
// in the string and ships it to the model. The prompt would still look right at
// a glance and the daemon would be quietly asking for something else.
func TestNoRenderingCarriesAFormattingArtefact(t *testing.T) {
	levels := []Pickiness{-1, 5}
	for i := range PickinessNames() {
		levels = append(levels, Pickiness(i))
	}
	for _, p := range levels {
		out := Decision(p)
		if strings.Contains(out, "%!") {
			t.Fatalf("the %v prompt contains a formatting artefact:\n%s", p, out)
		}
		// The hole was filled with something, rather than with the empty
		// string a mis-indexed table would supply.
		if !strings.Contains(out, "Store a rule") && !strings.Contains(out, "Store anything") {
			t.Fatalf("the %v prompt has no fact rules in it:\n%s", p, out)
		}
	}
}

// The paragraph a level asks for is the paragraph it gets. Without this the
// table could be rotated by one and every other test here would still pass.
func TestALevelRendersItsOwnParagraph(t *testing.T) {
	cases := []struct {
		level Pickiness
		want  string
	}{
		{Eager, "When in doubt, store it"},
		{Open, "one they clearly imply"},
		{Balanced, "store the part you are sure of"},
		{Careful, "When in doubt,\nstore nothing"},
		{Strict, "Never infer one from what was done"},
	}
	for _, c := range cases {
		t.Run(c.level.String(), func(t *testing.T) {
			if !strings.Contains(Decision(c.level), c.want) {
				t.Fatalf("the %v prompt does not contain %q", c.level, c.want)
			}
		})
	}
}
