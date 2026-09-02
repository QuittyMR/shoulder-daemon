package prompts

import (
	"fmt"
	"strconv"
	"strings"
)

// Pickiness is how reluctant the decision model is to write a new fact. It
// exists because that reluctance is a judgement call with no right answer: a
// memory that stores everything fills with noise, and one that stores nothing
// is an expensive way to forget. Which failure is cheaper depends on the person
// and on the store behind them, so it is theirs to set.
//
// Higher is pickier. It changes wording alone — what the model is told counts
// as a rule, and what to do when it is unsure — never what the daemon does with
// the facts that come back.
type Pickiness int

const (
	// Eager stores anything the turn established. Open also takes a rule the
	// user implied. Balanced is the default: it stores the part it is sure of
	// rather than the whole rule or nothing. Careful takes only stated rules
	// and stays silent when unsure, and Strict takes only a rule it could quote.
	Eager Pickiness = iota
	Open
	Balanced
	Careful
	Strict

	// Default is deliberately below Careful, which is where this daemon sat
	// before the knob existed: it declined to store a rule it could see but
	// could not source to a sentence, and the rules people state outright are
	// the minority.
	Default = Balanced
)

var pickinessNames = [...]string{"eager", "open", "balanced", "careful", "strict"}

func (p Pickiness) String() string {
	if !p.Valid() {
		return "invalid"
	}
	return pickinessNames[p]
}

func (p Pickiness) Valid() bool { return p >= Eager && p <= Strict }

// ParsePickiness takes a name or the number behind it. Both are accepted
// because the names say what they mean and the numbers say which way is more:
// somebody who wants "less than it is doing now" should not have to learn
// whether careful is above or below balanced.
func ParsePickiness(s string) (Pickiness, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return Default, fmt.Errorf("no pickiness given: use %s, or 0-%d", strings.Join(pickinessNames[:], ", "), Strict)
	}
	for i, name := range pickinessNames {
		if s == name {
			return Pickiness(i), nil
		}
	}
	if n, err := strconv.Atoi(s); err == nil {
		if p := Pickiness(n); p.Valid() {
			return p, nil
		}
		return Default, fmt.Errorf("pickiness %d is outside 0-%d", n, Strict)
	}
	return Default, fmt.Errorf("unknown pickiness %q: use %s, or 0-%d", s, strings.Join(pickinessNames[:], ", "), Strict)
}

// PickinessNames lists the levels low to high, for the messages that have to
// offer them.
func PickinessNames() []string { return pickinessNames[:] }

// factRules is the one paragraph of the decision prompt that pickiness moves.
// Everything the levels share — that work getting done is history rather than a
// rule, and that a clause the next commit falsifies does not belong — is
// repeated in each rather than factored out, because a prompt is read as a
// whole and stitching one from fragments is how the seams start contradicting
// each other.
var factRules = [...]string{
	Eager: `Store anything this turn established that could still matter next week: a decision,
constraint, preference, correction, or a piece of project structure, whether the user stated it
as a rule or the turn simply settled it. Work getting done is still not a rule - what changed
this turn is history the git log already holds. Keep only the part that outlives the work in
front of you. When in doubt, store it: a fact that turns out to be noise is cheaper than one
that was never written down, and the tidying pass removes the first kind.`,

	Open: `Store a rule the user states, and one they clearly imply without spelling out: a decision,
constraint, preference, correction, or a piece of project structure. Work getting done is not a
rule - what changed this turn is history the git log already holds. Keep only the part that
outlives the work in front of you; drop any clause that the next commit makes false. When in
doubt, store the narrower version you are sure of rather than nothing.`,

	Balanced: `Store a rule that governs later work - a decision, constraint, preference, correction,
or a piece of project structure - whether the user stated it outright or the turn made it plain.
Work getting done is not a rule; what changed this turn is history the git log already holds.
Keep only the part that outlives the work in front of you; drop any clause that the next commit
makes false. When in doubt, store the part you are sure of and drop the rest; store nothing only
when you cannot name what the rule would be.`,

	Careful: `Store a rule the user states that governs later work: a decision, constraint,
preference, correction, or a piece of project structure. Work getting done is not a rule -
what changed this turn is history the git log already holds. Keep only the part that outlives
the work in front of you; drop any clause that the next commit makes false. When in doubt,
store nothing.`,

	Strict: `Store a rule only where the user states it in this turn, in their own words, as a rule
governing later work: a decision, constraint, preference, correction, or a piece of project
structure. Never infer one from what was done, agreed to or implied. Work getting done is not a
rule - what changed this turn is history the git log already holds. Keep only the part that
outlives the work in front of you; drop any clause that the next commit makes false. Unless the
turn contains a sentence you could quote as the rule, store nothing.`,
}

// Decision is the prompt for one turn's decision, written for the pickiness
// asked for. An invalid level is the default rather than an error: this is
// called on the path that must never take a session down, and by the time it
// runs the value has already been through ParsePickiness.
func Decision(p Pickiness) string {
	if !p.Valid() {
		p = Default
	}
	return fmt.Sprintf(decisionTemplate, factRules[p])
}
