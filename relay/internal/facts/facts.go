// Package facts reconciles what an agent said it would remember with what it
// explicitly asked to be remembered.
//
// An agent that calls record_fact(fact="the best number is 1") usually also
// writes "I will record that the best number is 1" in the same turn. Both reach
// shoulder-daemon. Without reconciliation the memory backend gets the same fact
// twice in slightly different words, which is worse than getting it once.
package facts

import (
	"sort"
	"strings"
	"unicode"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

type Source string

const (
	// Explicit came from a record_fact tool call. It is authoritative: the
	// agent chose those words and those tags deliberately.
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
var Categories = map[string]bool{
	"decision":   true,
	"constraint": true,
	"preference": true,
	"correction": true,
	"structure":  true,
	"reference":  true,
}

// NormaliseCategory returns the category if it is valid, and false otherwise.
// An invalid category is dropped rather than passed through, so the backend
// stores no category instead of a wrong one.
func NormaliseCategory(c string) (string, bool) {
	c = strings.ToLower(strings.TrimSpace(c))
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
