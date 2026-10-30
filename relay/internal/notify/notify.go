// Package notify carries what the daemon did to the person in the editor: a
// fact stored, advice injected, a model that refused. The daemon's log holds
// all of it, and nobody reads the log while they work - a key died once and
// refused every call for thirteen days with only the log to say so. A notice
// is queued for the session it concerns and handed to the next hook from that
// session, where the harness shows it. Which kinds are queued is the person's
// choice, from the env file.
package notify

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is what a notice is about. The names are the ones the setting takes.
type Kind string

const (
	Error       Kind = "errors"
	Injection   Kind = "injections"
	Stored      Kind = "stored"
	Superseded  Kind = "superseded"
	Warning     Kind = "warnings"
	Maintenance Kind = "maintenance"
)

// kinds is every kind, in the order the setting is printed back.
var kinds = []Kind{Error, Injection, Stored, Superseded, Warning, Maintenance}

// Notice is one thing the person is told.
type Notice struct {
	Kind Kind      `json:"kind"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Policy is which kinds reach the person.
type Policy map[Kind]bool

// Defaults is what "on" means: the four kinds worth seeing every time.
// Warnings are a fact the model wanted that could not be kept, which is often
// the model's fault and not news; maintenance is the store tidying itself.
// Both are there to switch on.
func Defaults() Policy {
	return Policy{Error: true, Injection: true, Stored: true, Superseded: true}
}

// Parse reads the setting: "off" or empty for nothing, "on" for Defaults,
// "all" for every kind, or a comma-separated list of kinds, in which "on"
// stands for the defaults so "on,warnings" adds one. A name it does not know
// is an error naming the names it does.
func Parse(s string) (Policy, error) {
	p := Policy{}
	for _, raw := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		switch name {
		case "", "off", "none":
		case "on", "default", "defaults":
			for k := range Defaults() {
				p[k] = true
			}
		case "all":
			for _, k := range kinds {
				p[k] = true
			}
		default:
			if !known(Kind(name)) {
				return nil, fmt.Errorf("SHOULDER_NOTIFY: %q is not a kind of notice; use off, on, all, or any of %s", raw, names())
			}
			p[Kind(name)] = true
		}
	}
	return p, nil
}

func known(k Kind) bool {
	for _, c := range kinds {
		if c == k {
			return true
		}
	}
	return false
}

func names() string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return strings.Join(out, ", ")
}

// Allows reports whether notices of this kind reach the person.
func (p Policy) Allows(k Kind) bool { return p[k] }

// Enabled reports whether any kind does.
func (p Policy) Enabled() bool { return len(p) > 0 }

// String is the setting read back: "off", or the kinds in a fixed order.
func (p Policy) String() string {
	var on []string
	for _, k := range kinds {
		if p[k] {
			on = append(on, string(k))
		}
	}
	if len(on) == 0 {
		return "off"
	}
	sort.Strings(on)
	return strings.Join(on, ",")
}

// perSession bounds what waits for a session that has gone quiet. The oldest
// goes first: a notice nobody collected in twenty events is stale news.
const perSession = 20

// Box queues notices per session until a hook from that session collects
// them. A nil Box accepts and drops everything, so a pipeline assembled
// without one needs no checks.
type Box struct {
	policy  Policy
	mu      sync.Mutex
	pending map[string][]Notice
}

func New(p Policy) *Box {
	return &Box{policy: p, pending: map[string][]Notice{}}
}

// Policy is what the box lets through.
func (b *Box) Policy() Policy {
	if b == nil {
		return Policy{}
	}
	return b.policy
}

// Push queues one notice for a session, unless its kind is switched off or
// there is no session to show it to: a write from the command line has its
// own terminal.
func (b *Box) Push(sessionID string, k Kind, text string) {
	if b == nil || sessionID == "" || sessionID == "cli" || !b.policy.Allows(k) || strings.TrimSpace(text) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[sessionID]
	q = append(q, Notice{Kind: k, Text: text, At: time.Now()})
	if len(q) > perSession {
		q = q[len(q)-perSession:]
	}
	b.pending[sessionID] = q
}

// PushAll queues the same notice for every session named, for news that is
// not about one session: the model going down, or coming back.
func (b *Box) PushAll(sessionIDs []string, k Kind, text string) {
	for _, id := range sessionIDs {
		b.Push(id, k, text)
	}
}

// Drain hands over everything waiting for a session, oldest first.
func (b *Box) Drain(sessionID string) []Notice {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.pending[sessionID]
	delete(b.pending, sessionID)
	return q
}

// Forget drops what waits for a session that is gone.
func (b *Box) Forget(sessionID string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, sessionID)
}

// Lines renders notices the way a harness shows them: one line each, the
// kind first in the singular a person reads, then the text.
func Lines(notices []Notice) []string {
	out := make([]string, 0, len(notices))
	for _, n := range notices {
		out = append(out, Label(n.Kind)+": "+n.Text)
	}
	return out
}

// Label is the word shown before a notice of this kind.
func Label(k Kind) string {
	switch k {
	case Error:
		return "error"
	case Injection:
		return "injected"
	case Stored:
		return "stored"
	case Superseded:
		return "superseded"
	case Warning:
		return "warning"
	case Maintenance:
		return "tidied"
	}
	return string(k)
}

// OneLine folds a provider's refusal, usually pretty-printed JSON, onto one
// line, which is all a notice has.
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
