package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/sanitize"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// Decision is the only thing the model is allowed to return: what to say to the
// session, what to remember from it, and the keywords that carry this event
// forward to the next one.
type Decision struct {
	Inject string `json:"inject"`
	// Level decides where the injection is delivered: "action" at the tool call
	// it is about, anything else at the next prompt. Empty is the common case.
	Level    string       `json:"level,omitempty"`
	Facts    []facts.Fact `json:"facts"`
	Keywords []string     `json:"keywords,omitempty"`
}

// Decide runs one decision pass. A model that returns unparseable output is
// treated as silence, never as an error worth disturbing the session over.
//
// pick is passed rather than read from anywhere because it may have been
// changed since the daemon started, and the caller is the one holding the
// current value.
func Decide(ctx context.Context, p Provider, pick prompts.Pickiness, eventWindow string, recalled []memory.Record) (Decision, error) {
	var b strings.Builder
	b.WriteString("<recent-events>\n")
	b.WriteString(eventWindow)
	b.WriteString("\n</recent-events>\n\n<stored-facts>\n")
	if len(recalled) == 0 {
		b.WriteString("(none matched)")
	}
	for _, r := range recalled {
		// The scope is shown because the model is asked which stored fact a new
		// one replaces, and both scopes are in this list. Without it the only
		// signal it has is the wording, which is identical either way.
		fmt.Fprintf(&b, "id=%s scope=%s category=%s: %s\n", r.ID, r.Scope, r.Category, r.Content)
	}
	b.WriteString("\n</stored-facts>")

	raw, err := p.Complete(ctx, prompts.Decision(pick), b.String())
	if err != nil {
		return Decision{}, err
	}
	return ParseDecision(raw)
}

var injectRe = regexp.MustCompile(`"inject"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// salvageInject recovers the injection from output that was cut off before the
// closing brace.
func salvageInject(s string) (string, bool) {
	m := injectRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return "", false
	}
	var out string
	if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &out); err != nil {
		return "", false
	}
	return out, out != ""
}

// Unfence strips a code fence from around model output. Small models wrap JSON
// in one however plainly they are told not to.
func Unfence(s string) string {
	i := strings.Index(s, "```")
	if i < 0 {
		return s
	}
	s = s[i+3:]
	if j := strings.IndexByte(s, '\n'); j >= 0 {
		s = s[j+1:]
	}
	if j := strings.Index(s, "```"); j >= 0 {
		s = s[:j]
	}
	return s
}

// ParseObject reads one JSON object into v. It is for the answers that are one
// object and nothing else: a message, a digest. A fence is stripped because a
// model asked for JSON at the wire still wraps one on occasion; prose around
// the object is not tolerated, because every prompt forbids it and the wire
// asks for JSON, so prose is the model refusing the shape rather than decorating it.
// Every key listed must be present: valid JSON of the wrong shape is a refusal.
func ParseObject(raw string, v any, keys ...string) error {
	s := strings.TrimSpace(Unfence(strings.TrimSpace(raw)))
	if !strings.HasPrefix(s, "{") {
		return fmt.Errorf("no JSON object in model output")
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("model output JSON: %w", err)
	}
	if len(keys) == 0 {
		return nil
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &present); err != nil {
		return fmt.Errorf("model output JSON: %w", err)
	}
	for _, k := range keys {
		if _, ok := present[k]; !ok {
			return fmt.Errorf("model output JSON lacks key %q", k)
		}
	}
	return nil
}

// ShapeError is a model answer that arrived but was not a decision: prose, a
// cut-off object, an object with none of the decision's keys. It is the one
// failure worth asking again about, because the model is answering; it has
// answered the wrong thing.
type ShapeError struct{ Err error }

func (e *ShapeError) Error() string { return e.Err.Error() }
func (e *ShapeError) Unwrap() error { return e.Err }

// ParseDecision tolerates the things small models do to JSON: code fences,
// leading prose, trailing commentary. Every error it returns is a *ShapeError.
func ParseDecision(raw string) (Decision, error) {
	d, err := parseDecision(raw)
	if err != nil {
		return Decision{}, &ShapeError{Err: err}
	}
	return d, nil
}

func parseDecision(raw string) (Decision, error) {
	s := strings.TrimSpace(raw)
	if sanitize.IsSilent(s) {
		return Decision{}, nil
	}
	s = Unfence(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 {
		return Decision{}, fmt.Errorf("no JSON object in model output")
	}
	if end <= start {
		// Cut off before any closing brace. Recover the injection if one was
		// already complete.
		if inject, ok := salvageInject(s[start:]); ok {
			return Decision{Inject: strings.TrimSpace(inject)}, nil
		}
		return Decision{}, fmt.Errorf("no JSON object in model output")
	}

	var d Decision
	if err := json.Unmarshal([]byte(s[start:end+1]), &d); err != nil {
		// A model that runs out of output budget mid-object leaves valid text
		// followed by a truncated tail. Salvaging the injection is better than
		// discarding a decision that had already been made; the facts are
		// dropped because a half-written fact must never be stored.
		if inject, ok := salvageInject(s[start:]); ok {
			return Decision{Inject: strings.TrimSpace(inject)}, nil
		}
		return Decision{}, fmt.Errorf("decision JSON: %w", err)
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s[start:end+1]), &present); err != nil {
		return Decision{}, fmt.Errorf("decision JSON: %w", err)
	}
	_, hasInject := present["inject"]
	_, hasFacts := present["facts"]
	_, hasKeywords := present["keywords"]
	if !hasInject && !hasFacts && !hasKeywords {
		return Decision{}, fmt.Errorf("decision JSON has none of the expected keys inject, facts, keywords")
	}

	d.Inject = strings.TrimSpace(d.Inject)
	if sanitize.IsSilent(d.Inject) {
		d.Inject = ""
	}
	kept := d.Facts[:0]
	for _, f := range d.Facts {
		f.Content = strings.TrimSpace(f.Content)
		if f.Content == "" {
			continue
		}
		// A model that answers "Local" has made the decision; only an absent or
		// unrecognised scope is a refusal to decide, and that is judged where
		// the fact is written rather than silently repaired here.
		f.Scope = scope.Scope(strings.ToLower(strings.TrimSpace(string(f.Scope))))
		kept = append(kept, f)
	}
	d.Facts = kept

	words := d.Keywords[:0]
	for _, w := range d.Keywords {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		words = append(words, w)
	}
	d.Keywords = words
	return d, nil
}
