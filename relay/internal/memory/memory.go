// Package memory is the connector boundary. shoulder-daemon stores what it
// learns in somebody else's memory service; which one is a configuration
// choice, and nothing above this package knows which one was chosen.
//
// The interface is deliberately small and backend-neutral. It names only
// operations any store can express — find, list, write, replace — and carries
// no concept belonging to one particular product. A backend that cannot
// deduplicate simply never returns the duplicate errors; a backend with no
// notion of similarity leaves Score at zero.
package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// Kind separates durable facts, which a digest is about, from per-session
// working notes that exist only to give the next turn continuity. A fact is
// worth recalling in a month; a working note is worth recalling in the next
// minute and is noise afterwards, so the two are never read together.
//
// The zero value is a fact, because a caller that has not heard of this
// distinction is asking about knowledge, and because that makes the safe
// reading the default one on both sides: an unset Kind on a Record stores a
// fact, an unset Kind on a Query asks for facts.
type Kind string

const (
	KindFact    Kind = ""
	KindSession Kind = "session"
)

// Record is one stored item, in the smallest shape every backend can express.
type Record struct {
	// ID is the backend's handle for this record and the value the rest of the
	// system passes around to talk about it: Supersede's oldID, a deduced
	// fact's Supersedes, the --id a person retypes off a digest, and the
	// Collided of ErrDuplicateSemantic. It is opaque — compare it and hand it
	// back, never parse it — and stays valid until the record is superseded.
	// Superseding does not preserve it: Supersede returns the replacement's id,
	// which is normally a different value, and the shipping connector makes it
	// a hash of the new content. A caller holding the old id after a supersede
	// is holding a handle to something that no longer exists.
	ID       string   `json:"id,omitempty"`
	Content  string   `json:"content"`
	Category string   `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Kind     Kind     `json:"kind,omitempty"`

	// Session names the session a working note belongs to, and is empty on a
	// fact. It exists because the id of a note is held only in the memory of
	// the process that wrote it, and that memory is lost routinely — the
	// daemon exits when idle and is relaunched by the editor, and a session's
	// state is dropped an hour after its last event. Without a mark in the
	// store, a session that comes back cannot tell its own note from anybody
	// else's and writes a second one, forever. With it, the store is the truth
	// and the in-memory id is only a shortcut to it.
	Session string `json:"session,omitempty"`

	// Scope and Project say where this belongs. Scope is mandatory. Project is
	// mandatory when Scope is local and must be empty when it is global.
	//
	// Project is the project path on write. On read it is that path when the
	// query named one, and otherwise the opaque scope.Key of it: a backend is
	// given the key rather than the directory so a memory service shared
	// between machines does not learn local layout, and the path cannot be
	// recovered from it afterwards. Callers that need to compare projects use
	// ProjectKey, which is the same value either way.
	Scope   scope.Scope `json:"scope"`
	Project string      `json:"project,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`

	// Score is populated by Search only, and is backend-defined. Treat it as an
	// ordering hint, not a probability. Leaving it zero throughout is legal and
	// callers must still work; a backend that does supply it must make the
	// values comparable between separate calls, because a local and a global
	// search are merged and ordered as one set.
	Score float64 `json:"score,omitempty"`
}

// projectKeyLen is measured rather than written out so a change to the width of
// scope.Key cannot leave this package recognising the old shape.
var projectKeyLen = len(scope.Key("/"))

// ProjectKey is the stable identifier of this record's project, whichever of
// the two forms Project came back in.
//
// The alternative was a second field carrying the key beside the path, which
// buys two fields that can contradict each other. One field plus a derivation
// is safe here because the two forms cannot be confused: a project is an
// absolute path, a key is a fixed run of hex digits.
func (r Record) ProjectKey() string {
	if isProjectKey(r.Project) {
		return r.Project
	}
	return scope.Key(r.Project)
}

func isProjectKey(s string) bool {
	if len(s) != projectKeyLen {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Query selects records. A zero Scope means "either scope"; it is a filter, not
// a storage decision, so unlike a Record a Query may leave it unset.
//
// The two reads read it differently. Search treats Any as "do not filter".
// List requires a scope and is refused without one, because listing is
// exhaustive: an unscoped list is every project's knowledge in one answer.
type Query struct {
	Text    string
	Limit   int
	Scope   scope.Scope
	Project string

	// Kind is matched exactly, and its zero value asks for facts. Session
	// records are opt-in rather than opt-out: recall, digests and the fact list
	// all want knowledge, and none of them would notice working notes arriving
	// until one was quoted back to a person as something the system had
	// learned.
	Kind Kind `json:"kind,omitempty"`

	// MinScore drops hits the backend scored below it. Zero is no floor, and a
	// record the backend left unscored is never dropped: a connector with no
	// notion of similarity must not answer everything with nothing.
	MinScore float64 `json:"min_score,omitempty"`
}

// Connector is what a memory backend must provide.
//
// Supersede exists separately from Store because "this replaces that" is the
// operation shoulder-daemon needs and most backends implement it as
// delete-then-write rather than an update. List exists separately from Search
// because a digest needs everything a scope holds, and a semantic search given
// an empty query returns arbitrary results rather than all of them.
//
// Supersede must make oldID unreachable: after it returns, neither Search nor
// List may ever produce that record again, by whatever means the backend has —
// deleting it, hiding it behind the replacement, filtering it on the way out.
// This is the invariant the reconciliation loop rests on. A backend that leaves
// the old record visible has every later turn recall the stale fact, supersede
// it again, and write another replacement, without end.
//
// Both reads must match Query.Kind exactly, which makes a query that never
// mentioned Kind a query for facts. It is enforced in each connector rather
// than once in Checked so that the conformance suite measures the connector
// itself: a wrapper that filtered on the way out would let a backend leaking
// working notes pass the test it exists to fail.
//
// Forget is deletion, and it is here because without it nothing this system
// writes is ever bounded. Supersede replaces a record with another one, so a
// store only ever grows; the working note of every session in every project
// would accumulate for the life of the backend, and those notes are by
// construction the exact vocabulary of the turns they came from, so they
// outrank the facts in any search over the same project. Forgetting one when
// its session dies is what keeps recall from being buried under the history of
// how it was reached.
//
// Forget is idempotent: an id that is already gone is not an error, because the
// caller's intent — that this record no longer exist — is satisfied. It refuses
// an empty id rather than treating it as "nothing to do", since an empty id is
// a caller that lost track of what it meant to delete.
type Connector interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Record, error)
	List(ctx context.Context, q Query) ([]Record, error)
	Store(ctx context.Context, r Record) (string, error)
	Supersede(ctx context.Context, oldID string, r Record) (string, error)
	Forget(ctx context.Context, id string, where Query) error
}

// Counter is the whole of what this package needs from metrics: somewhere for a
// connector to report the shape of what it read on the way to an answer, which
// nobody above it can see. It is an interface rather than the metrics type so
// the connector boundary keeps no dependency on the exposition format, and a
// nil one is legal — a connector counts nothing rather than refusing to work.
type Counter interface{ Inc(name string) }

func count(m Counter, name string) {
	if m != nil {
		m.Inc(name)
	}
}

// countN raises a counter by n. Counter has only Inc because that is all the
// exposition offers; a tally that arrives already summed is spent here rather
// than widening the interface for one caller.
func countN(m Counter, name string, n int) {
	for i := 0; i < n; i++ {
		count(m, name)
	}
}

// ErrUnscoped is returned for a write whose scope was never decided. It is a
// programming error rather than a runtime condition: the decision is supposed
// to be made where the knowledge enters the system.
var ErrUnscoped = errors.New("record has no scope: every record must be local or global")

// ErrUnscopedList is returned by List for a query with no scope. Callers are
// expected to match it with errors.Is and distinguish it from a backend that
// failed: one means the person never said which knowledge they wanted, the
// other means the store is broken, and the two deserve different answers.
var ErrUnscopedList = errors.New("list has no scope: a digest reads one scope, never all of them")

// ErrNoBackend is returned by a write when there is nowhere to write it,
// because no memory service is configured. Callers test for it with errors.Is
// and tell the person which setting is missing: reporting a write that went
// nowhere as success is how somebody ends up trusting a memory that was never
// kept.
var ErrNoBackend = errors.New("no memory backend configured")

// Validate rejects a record that could not be recalled correctly later.
func Validate(r Record) error {
	if r.Content == "" {
		return errors.New("record has no content")
	}
	if !r.Scope.Valid() {
		return ErrUnscoped
	}
	if r.Scope == scope.Local && r.Project == "" {
		return errors.New("local record has no project")
	}
	if r.Scope == scope.Global && r.Project != "" {
		return fmt.Errorf("global record must not name a project, got %q", r.Project)
	}
	return nil
}

// ValidateQuery rejects a query that would silently read the wrong project's
// knowledge. It accepts scope.Any, which is a legal filter for Search; the
// stricter rule List needs is applied by List itself.
func ValidateQuery(q Query) error {
	if q.Scope == scope.Local && q.Project == "" {
		return errors.New("local query has no project")
	}
	if q.Scope != scope.Any && !q.Scope.Valid() {
		return fmt.Errorf("unknown scope %q", q.Scope)
	}
	return nil
}

// ErrDuplicateExact reports that the backend already holds this exact content.
// It is an expected outcome, not a fault. Backends without deduplication never
// return it.
var ErrDuplicateExact = errors.New("memory already stored verbatim")

// ErrDuplicateSemantic reports that the backend refused a write because an
// existing record is above its similarity threshold. Collided names the record
// that blocked it, when the backend says which, so callers can supersede that
// record instead of losing the write. Backends without semantic deduplication
// never return it.
type ErrDuplicateSemantic struct {
	Collided string
}

func (e *ErrDuplicateSemantic) Error() string {
	return "memory refused as semantically similar to " + e.Collided
}

// ErrCrossScopeSupersede reports a supersede whose target is not a current
// record in the scope the replacement claims. It is not a permission failure;
// it means the caller asked to correct a record that is not the one it thinks
// it is, and honouring it would relocate a record rather than fix it.
//
// Elsewhere separates the two ways that happens, because they are not the same
// fault and the difference decides what a caller should do next. Set, the
// target was seen, current, in another scope: the caller named the wrong
// record and writing anyway would move somebody else's knowledge. Unset, the
// target was not found at all, which covers both a project that cannot be
// enumerated from here and a record that has already been replaced and is
// gone. The message says only what was established: claiming a record is in
// another project when it may simply no longer exist sends whoever reads the
// log hunting a placement bug that never happened.
type ErrCrossScopeSupersede struct {
	OldID     string
	Scope     scope.Scope
	Project   string
	Elsewhere bool
}

func (e *ErrCrossScopeSupersede) Error() string {
	where := "global scope"
	if e.Scope == scope.Local {
		where = "local scope of " + scope.Label(e.Project)
	}
	if e.Elsewhere {
		return fmt.Sprintf("%s is not in %s and is current in another one, so replacing it would move it rather than correct it", e.OldID, where)
	}
	return fmt.Sprintf("%s is not in %s and was not found anywhere readable from here, so it is either another scope's record or one already replaced; replacing it would move a record rather than correct this one", e.OldID, where)
}

// ErrForgetUnidentified is returned by Forget with no id. Deleting is the one
// operation with nothing to fall back on, so a caller that has lost track of
// what it meant to delete is told so rather than quietly doing nothing.
var ErrForgetUnidentified = errors.New("forget has no id: there is nothing to delete")

// Checked wraps a connector so no unscoped record can reach a backend, whatever
// the caller does. Every connector is wrapped at construction: enforcing the
// rule once here is what makes "you must decide local or global" a property of
// the system rather than a convention.
func Checked(c Connector) Connector { return checked{inner: c} }

type checked struct{ inner Connector }

func (c checked) Name() string { return c.inner.Name() }

func (c checked) Search(ctx context.Context, q Query) ([]Record, error) {
	if err := ValidateQuery(q); err != nil {
		return nil, err
	}
	return c.inner.Search(ctx, q)
}

func (c checked) List(ctx context.Context, q Query) ([]Record, error) {
	if err := ValidateQuery(q); err != nil {
		return nil, err
	}
	// Refused at the boundary rather than inside a connector, so that a backend
	// nobody here has audited cannot answer it either. A digest is the one read
	// that returns a scope whole, which makes it the place where one project's
	// knowledge appearing in another's answer would be least visible.
	if q.Scope == scope.Any {
		return nil, fmt.Errorf("%s: %w", c.Name(), ErrUnscopedList)
	}
	return c.inner.List(ctx, q)
}

func (c checked) Store(ctx context.Context, r Record) (string, error) {
	if err := Validate(r); err != nil {
		return "", err
	}
	return c.inner.Store(ctx, r)
}

// Supersede is where the scope rule is easiest to lose. Store validates the
// record being written, but a supersede also names a record that is already
// somewhere, and rewriting it with a replacement from another scope does not
// correct that record - it moves it, out of every context that could still read
// it. So the target's placement is checked here rather than in each caller: a
// backend nobody here has audited cannot be trusted to refuse it either, and
// this is the operation the callers reach for precisely when a fact was wrong.
func (c checked) Supersede(ctx context.Context, oldID string, r Record) (string, error) {
	if err := Validate(r); err != nil {
		return "", err
	}
	if oldID == "" {
		return c.inner.Supersede(ctx, oldID, r)
	}
	held, err := c.holds(ctx, oldID, r)
	if err != nil {
		return "", err
	}
	if held {
		return c.inner.Supersede(ctx, oldID, r)
	}
	// The refusal stands either way — a record this scope cannot see must not
	// be rewritten from here — but it says which of the two it observed. A
	// caller holding an id it wrote here itself can read "not found anywhere"
	// as what it is, a record that has gone, and write afresh; the boundary
	// cannot, because it does not know where the id came from.
	elsewhere, err := c.elsewhere(ctx, oldID, r)
	if err != nil {
		return "", err
	}
	return "", &ErrCrossScopeSupersede{OldID: oldID, Scope: r.Scope, Project: r.Project, Elsewhere: elsewhere}
}

// Forget is the one verb that cannot be undone, so it is the one that most
// needs the check Supersede already has. where says which scope the caller
// believes the record is in, and the deletion does not happen unless it is.
func (c checked) Forget(ctx context.Context, id string, where Query) error {
	if id == "" {
		return ErrForgetUnidentified
	}
	held, err := c.heldBy(ctx, id, where)
	if err != nil {
		return err
	}
	if !held {
		// Already gone, or never here. Both mean the caller has nothing to
		// delete, and Forget is documented as idempotent.
		return nil
	}
	return c.inner.Forget(ctx, id, where)
}

// holds reports whether oldID sits in the same place as the replacement. The
// listing is unbounded on purpose: a record past a cap would read as absent,
// and refusing a correction to a fact that exists is the worse failure.
func (c checked) holds(ctx context.Context, oldID string, r Record) (bool, error) {
	// Kind is carried into the lookup because a session record supersedes
	// itself every turn: asked for as a fact, the record being replaced reads
	// as absent and the correction is refused as a cross-scope one.
	return c.heldBy(ctx, oldID, Query{Scope: r.Scope, Project: r.Project, Kind: r.Kind})
}

func (c checked) heldBy(ctx context.Context, oldID string, q Query) (bool, error) {
	q.Limit = 0
	found, err := c.List(ctx, q)
	if err != nil {
		return false, err
	}
	for _, rec := range found {
		if rec.ID == oldID {
			return true, nil
		}
	}
	return false, nil
}

// elsewhere reports whether oldID was seen as a current record in some scope
// other than the one the replacement claims. It is what lets the refusal say
// "that record belongs to somebody else" only when that was observed.
//
// It answers no when it cannot tell, and the error is worded to admit as much.
// Global is a single list and can be read whole; local is one list per project
// and this boundary knows only the project the replacement named, so a record
// belonging to a third project is indistinguishable from one that has been
// deleted. Nothing is decided on the difference here, so an unprovable case
// costs a vaguer sentence rather than a wrong outcome.
func (c checked) elsewhere(ctx context.Context, oldID string, r Record) (bool, error) {
	if r.Scope != scope.Local {
		return false, nil
	}
	found, err := c.List(ctx, Query{Scope: scope.Global, Kind: r.Kind})
	if err != nil {
		return false, err
	}
	for _, rec := range found {
		if rec.ID == oldID {
			return true, nil
		}
	}
	return false, nil
}

// Nop is the connector used when no memory backend is configured.
// shoulder-daemon stays fully functional without one: it observes and answers,
// it simply has nothing to recall and nowhere to write.
//
// Reads succeed empty; writes fail with ErrNoBackend. Having nothing to recall
// is an ordinary state, whereas accepting a write that goes nowhere would let
// the daemon tell somebody it had remembered a fact it discarded.
type Nop struct{}

func (Nop) Name() string                                    { return "none" }
func (Nop) Search(context.Context, Query) ([]Record, error) { return nil, nil }
func (Nop) List(context.Context, Query) ([]Record, error)   { return nil, nil }

func (Nop) Store(context.Context, Record) (string, error) { return "", ErrNoBackend }

func (Nop) Supersede(context.Context, string, Record) (string, error) { return "", ErrNoBackend }

// Forget succeeds. Nothing was ever stored, so the record the caller wants gone
// is gone; reporting ErrNoBackend would have a janitor log a failure every time
// it tidied up after a session on a daemon with no memory service.
func (Nop) Forget(context.Context, string, Query) error { return nil }
