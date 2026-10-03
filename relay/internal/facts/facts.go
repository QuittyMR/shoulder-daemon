// Package facts is the vocabulary of what the daemon stores, and the merge
// that keeps one statement from being written twice.
//
// A turn that states "the best number is 1" and later "I will record that the
// best number is 1" yields two facts in slightly different words, and a person
// who types one on the command line that the model also reads out of the
// exchange yields two more. Without reconciliation the memory backend gets
// the same fact several times over, which is worse than getting it once.
package facts

import (
	"sort"
	"strings"
	"unicode"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

type Source string

const (
	// Explicit is what the person typed, through the CLI. It is authoritative:
	// they chose those words and that scope deliberately.
	Explicit Source = "explicit"
	// Deduced was inferred from the turn's prose by the decision model.
	Deduced Source = "deduced"
)

type Fact struct {
	Content  string   `json:"content"`
	Category string   `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`

	// Scope decides which memory this fact joins, and has no default. A fact
	// that reaches the writer without one is dropped: filing a project detail
	// among the user's cross-project preferences, or the reverse, is worse
	// than losing it.
	Scope scope.Scope `json:"scope"`

	// Private marks a fact about this person's machine, accounts, paths or
	// habits rather than about the code, so a backend that files records
	// beside a checkout keeps it out of what the team commits. It is an axis
	// of its own because scope cannot carry it: "Postgres listens on 5433
	// here" is local to this project and still must not reach a teammate who
	// clones it.
	Private bool `json:"private,omitempty"`

	Source     Source `json:"source,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
}

// SimilarityThreshold is the Jaccard overlap above which two facts are treated
// as the same statement. 0.6 keeps "the best number is 1" and "I will record
// that the best number is 1" together (both reduce to the same three tokens)
// while separating facts that merely share a subject.
const SimilarityThreshold = 0.6

// Reconcile merges explicit and deduced facts, dropping deduced restatements of
// something already recorded explicitly. Explicit facts always survive intact,
// including their tags and category.
//
// Privacy is the one thing a dropped duplicate leaves behind. Two wordings of
// one rule reach the writer as a single record, so a restatement that judged
// the rule private and lost is a record committed to a repository against the
// only judgement anybody made about it. An explicit fact still overrides that:
// the agent was told the rule in those words and chose the flag deliberately.
func Reconcile(explicit, deduced []Fact) []Fact {
	out := make([]Fact, 0, len(explicit)+len(deduced))
	kept := make([][]string, 0, len(explicit)+len(deduced))

	add := func(f Fact) {
		t := tokens(f.Content)
		if len(t) == 0 {
			return
		}
		for i, prev := range kept {
			if similarity(t, prev) >= SimilarityThreshold {
				// Explicit wins over an already-kept deduced duplicate, but
				// only when it is at least as placeable: an explicit fact whose
				// scope was never decided is dropped by the writer, so letting
				// it evict a scoped one loses the statement entirely.
				if f.Source == Explicit && out[i].Source != Explicit &&
					(f.Scope.Valid() || !out[i].Scope.Valid()) {
					out[i] = f
					kept[i] = t
					return
				}
				if f.Private && !out[i].Private && out[i].Source != Explicit {
					out[i].Private = true
				}
				return
			}
		}
		out = append(out, f)
		kept = append(kept, t)
	}

	for _, f := range explicit {
		f.Source = Explicit
		add(f)
	}
	for _, f := range deduced {
		if f.Source == "" {
			f.Source = Deduced
		}
		add(f)
	}
	return out
}

// tokens reduces a statement to a sorted set of significant lowercase words.
func tokens(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if stop[f] || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// similarity is Jaccard overlap: shared tokens over the union.
//
// Containment (shared over the smaller set) was tried first and was wrong:
// "deploys go to staging" and "deploys never go to production" share two of
// three significant tokens and collapsed into one fact. Penalising the tokens
// that differ is the whole point.
func similarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(b))
	for _, t := range b {
		set[t] = true
	}
	shared := 0
	for _, t := range a {
		if set[t] {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// stop holds the words that carry no identity for a fact. "I will record that
// X" and "X" must reduce to the same token set.
var stop = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "are": true, "was": true,
	"were": true, "be": true, "been": true, "being": true, "to": true, "of": true,
	"and": true, "or": true, "that": true, "this": true, "it": true, "its": true,
	"i": true, "we": true, "you": true, "will": true, "shall": true, "should": true,
	"record": true, "recording": true, "recorded": true, "remember": true,
	"remembering": true, "remembered": true, "note": true, "noting": true,
	"noted": true, "store": true, "storing": true, "stored": true, "save": true,
	"saving": true, "saved": true, "let": true, "me": true, "my": true, "for": true,
	"in": true, "on": true, "at": true, "as": true, "so": true, "now": true,
}

// Categories are the only values the decision model may use. The set is closed
// here, where the model's output can still be inspected, because a category is
// only worth storing if it still means the same thing on the way out, and a
// store handed a word it does not know is free to keep or rewrite it.
//
// Two of the four are split by who may state them rather than by what they
// say, which is the axis an agent-driven session needs: a subagent that
// decides "tests here use the standard library" has made a finding, and the
// same sentence from the person is a rule.
var Categories = map[string]bool{
	// finding: something this session established by looking - a bug located,
	// the state of a file or an environment, a measurement.
	"finding": true,
	// fact: a durable truth about the project or the machine - how something
	// is structured, what a command does, where things live.
	"fact": true,
	// rule: a constraint or decision that governs how work is done here.
	"rule": true,
	// preference: how this person wants work or communication done.
	"preference": true,
}

// legacyCategories are the names stores were given before the set above and
// still hold. They are accepted on the way in and mapped forward, never
// written back, so a file that mixes both generations reads as one.
var legacyCategories = map[string]string{
	"decision":   "rule",
	"constraint": "rule",
	"correction": "rule",
	"structure":  "fact",
	"reference":  "fact",
}

// Private reports whether a category describes the person rather than the
// code. A preference is theirs: it belongs in the file a backend keeps out of
// the repository, not in the conventions the team commits.
func Private(category string) bool { return CurrentName(category) == "preference" }

// UserOnly reports whether a category may only be stated by the person. A rule
// governs how work is done here and a preference is how they want it done;
// an agent that reaches either has concluded it, and a conclusion is a finding
// however much it is phrased as a rule.
func UserOnly(category string) bool {
	c := CurrentName(category)
	return c == "rule" || c == "preference"
}

// CurrentName returns the current name of a category. A legacy name maps
// forward; anything else, valid or not, is returned as given, so a read path
// can modernise what a store holds without deciding what to do with a value
// the set never contained.
func CurrentName(category string) string {
	c := strings.ToLower(strings.TrimSpace(category))
	if now, ok := legacyCategories[c]; ok {
		return now
	}
	return c
}

// NormaliseCategory returns the current name of the category if it is valid,
// and false otherwise. A legacy name is valid and comes back as the name it
// maps to. An invalid category is dropped rather than passed through, so the
// backend stores no category instead of a wrong one.
func NormaliseCategory(c string) (string, bool) {
	c = CurrentName(c)
	if c == "" {
		return "", true
	}
	if Categories[c] {
		return c, true
	}
	return "", false
}

// AgainstRecalled marks each fact that restates something already stored, so it
// supersedes that memory instead of being written alongside it.
//
// Reconcile only merges facts within one turn. The same fact restated three
// turns apart arrives as three separate writes, and no store can be relied on
// to recognise a paraphrase of what it already holds. Without this, a long
// session accumulates near-duplicates of its own conclusions.
//
// project is where a local fact would be filed, which is what makes a match
// checkable: a supersede is only a correction if it lands where the record it
// replaces already sits.
func AgainstRecalled(deduced []Fact, project string, recalled []Recalled) []Fact {
	if len(recalled) == 0 {
		return deduced
	}
	recTokens := make([][]string, len(recalled))
	for i, r := range recalled {
		recTokens[i] = tokens(r.Content)
	}

	out := make([]Fact, 0, len(deduced))
	for _, f := range deduced {
		if f.Supersedes == "" {
			ft := tokens(f.Content)
			best, bestScore := -1, 0.0
			for i, r := range recalled {
				if !r.Placed(f.Scope, project) {
					continue
				}
				if s := similarity(ft, recTokens[i]); s > bestScore {
					best, bestScore = i, s
				}
			}
			if best >= 0 && bestScore >= SimilarityThreshold {
				f.Supersedes = recalled[best].ID
			}
		}
		out = append(out, f)
	}
	return out
}

// Recalled is the minimum a stored memory needs to expose for reconciliation.
// Where it lives is part of that minimum: a supersede carries the new fact's
// placement onto the record it replaces, so what a record says is not enough to
// decide whether it may be replaced.
type Recalled struct {
	ID      string
	Content string
	Scope   scope.Scope

	// Project only has to be comparable with the project a fact would be filed
	// under. It is never shown or resolved here, so the caller picks the form
	// and uses the same one on both sides.
	Project string
}

// Placed reports whether r already sits exactly where a fact of this scope and
// project would be written.
//
// Superseding a record anywhere else does not correct it, it moves it: a global
// preference replaced by a local fact stops being visible in every project but
// one, and the user never asked for it to be narrowed.
func (r Recalled) Placed(s scope.Scope, project string) bool {
	if r.Scope != s {
		return false
	}
	if s == scope.Local {
		return r.Project == project
	}
	return true
}
