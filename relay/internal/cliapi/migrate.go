package cliapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// MigrateRequest asks for one scope of a built-in JSON store to be copied into
// whatever store the daemon is running now. From is the file to read; empty is
// the one this daemon would have kept its own facts in.
type MigrateRequest struct {
	Scope   string `json:"scope"`
	Project string `json:"project"`
	Dir     string `json:"dir,omitempty"`
	From    string `json:"from,omitempty"`
}

// What became of one record. The three are exhaustive: every record read is
// reported under exactly one of them.
const (
	MigrateStored  = "stored"
	MigrateSkipped = "skipped"
	MigrateFailed  = "failed"
)

// MigratedFact is one record's outcome. The content is echoed rather than only
// counted because the two stores give a record different ids, so the sentence
// is the only handle a person has on it while it is in both.
type MigratedFact struct {
	Content  string `json:"content"`
	Category string `json:"category,omitempty"`
	Outcome  string `json:"outcome"`

	// ID is what the destination called it, and is set only on a record that
	// was stored. Collided names the record already saying this, when one
	// could be identified. Error is the refusal, verbatim.
	ID       string `json:"id,omitempty"`
	Collided string `json:"collided,omitempty"`
	Error    string `json:"error,omitempty"`
}

type MigrateResponse struct {
	// From is echoed because the default is a path only the daemon knows, and
	// a migration that read the wrong file is otherwise indistinguishable from
	// one that found nothing.
	From    string         `json:"from"`
	Stored  int            `json:"stored"`
	Skipped int            `json:"skipped"`
	Failed  int            `json:"failed"`
	Facts   []MigratedFact `json:"facts,omitempty"`
}

// handleMigrate copies a scope out of the JSON store and into the running one.
// It exists because the built-in store was the only one there was: somebody who
// has been using the daemon for months and then sets SHOULDER_MEMORY=docs finds
// an empty repository and every fact they taught it still in a file nothing
// reads any more.
//
// Nothing is deleted and the source is only ever read, so a migration that goes
// wrong costs a rerun rather than the facts. Rerunning is the ordinary case:
// the destination refuses what it already holds, and those refusals are the
// answer rather than a failure.
func (s *Server) handleMigrate(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) || !s.methodIs(w, r, http.MethodPost) {
		return
	}
	var req MigrateRequest
	if !s.decode(w, r, &req) {
		return
	}
	sc, err := requireScope(req.Scope, req.Project)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// Refused before a single record is read: with nowhere to write, every one
	// of them would fail with the same sentence and the person would read a
	// list of failures instead of the one thing that is wrong.
	if s.Pipe.Memory.Name() == (memory.Nop{}).Name() {
		s.refusedRest(w, memory.ErrNoBackend)
		return
	}
	from := req.From
	if from == "" {
		from = s.Pipe.Cfg.MemoryPath
	}
	if err = s.migratable(from); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	// Opened with no embedder. This reads every record and ranks none, and it
	// is also what keeps the re-embedding pass — the one thing an open store
	// writes on its own — away from a file the migration promised not to
	// touch.
	src, err := memory.NewLocal(from, nil)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	defer func() { _ = src.Close() }()
	// A query that never mentions Kind asks for facts, which is what leaves the
	// working notes behind: they are the vocabulary of turns that ended months
	// ago and belong in nobody's repository.
	found, err := src.List(r.Context(), memory.Query{Scope: sc, Project: req.Project})
	if err != nil {
		s.failed(w, err)
		return
	}
	held, err := s.heldByContent(r.Context(), memory.Query{Scope: sc, Project: req.Project, Dir: req.Dir})
	if err != nil {
		s.failed(w, err)
		return
	}

	reply := MigrateResponse{From: from}
	// Oldest first: List answers newest first, and a store that keeps facts in
	// a file somebody reads should have them in the order they were learned.
	for i := len(found) - 1; i >= 0; i-- {
		reply.add(s.migrate(r.Context(), found[i], sc, req, held))
	}
	s.Pipe.Log.Info("facts migrated", "origin", "cli", "from", from, "scope", sc,
		"project", scope.Label(req.Project), "stored", reply.Stored,
		"skipped", reply.Skipped, "failed", reply.Failed)
	writeJSON(w, http.StatusOK, reply)
}

// migrate writes one record and says what happened to it. A duplicate is not a
// failure: the destination already says this, which is the state the caller
// asked for.
func (s *Server) migrate(ctx context.Context, src memory.Record, sc scope.Scope, req MigrateRequest, held map[string]string) MigratedFact {
	rec := memory.Record{
		Content:  src.Content,
		Category: src.Category,
		Tags:     src.Tags,
		// The timestamp is the fact's, not the migration's. A store that dates
		// what it holds would otherwise report a decade of decisions as taken
		// this afternoon.
		CreatedAt: src.CreatedAt,
		Scope:     sc,
		// A preference is the person's own wherever it was written down. The
		// JSON store had nowhere to put that and never marked one, so a store
		// that files by it is told here rather than committing somebody's
		// habits with the team's code.
		Private: src.Private || facts.Private(src.Category),
	}
	if sc == scope.Local {
		rec.Project, rec.Dir = req.Project, req.Dir
	}

	out := MigratedFact{Content: src.Content, Category: src.Category}
	id, err := s.Pipe.Memory.Store(ctx, rec)
	var semantic *memory.ErrDuplicateSemantic
	switch {
	case err == nil:
		s.Pipe.Metrics.Inc("shoulder_cli_migrated_total")
		out.Outcome, out.ID = MigrateStored, id
		held[sentence(rec.Content)] = id
	case errors.Is(err, memory.ErrDuplicateExact):
		out.Outcome, out.Collided = MigrateSkipped, held[sentence(rec.Content)]
	case errors.As(err, &semantic):
		out.Outcome, out.Collided = MigrateSkipped, semantic.Collided
	default:
		out.Outcome, out.Error = MigrateFailed, err.Error()
	}
	return out
}

func (m *MigrateResponse) add(f MigratedFact) {
	switch f.Outcome {
	case MigrateStored:
		m.Stored++
	case MigrateSkipped:
		m.Skipped++
	default:
		m.Failed++
	}
	m.Facts = append(m.Facts, f)
}

// heldByContent is what the destination already holds, by sentence. It is read
// once, up front, so that a record refused as a verbatim duplicate can still be
// reported with the id of the record that refused it: a backend names one for a
// semantic collision and has no reason to for an exact one.
//
// The key is the content with its runs of whitespace collapsed, because a store
// that keeps one record per line writes it back that way, and a fact that came
// out of the JSON store with a newline in it is still the same fact.
func (s *Server) heldByContent(ctx context.Context, q memory.Query) (map[string]string, error) {
	found, err := s.Pipe.Memory.List(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(found))
	for _, r := range found {
		out[sentence(r.Content)] = r.ID
	}
	return out, nil
}

func sentence(content string) string { return strings.Join(strings.Fields(content), " ") }

// migratable refuses a source before anything is read from it. An unreadable
// path is refused rather than reported as an empty store, because a typo in
// --from and a store with nothing in it would otherwise print the same line.
func (s *Server) migratable(from string) error {
	cfg := s.Pipe.Cfg
	if cfg.MemoryURL == "" && cfg.Memory == config.MemoryLocal && sameFile(from, cfg.MemoryPath) {
		return fmt.Errorf(
			"%s is the store this daemon is already keeping its facts in, so there is nothing to move it into; point the daemon somewhere else first with SHOULDER_MEMORY=docs or SHOULDER_MEMORY_URL, restart it, and run this again",
			from)
	}
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("nothing to migrate: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory; --from names the JSON file the built-in store wrote", from)
	}
	return nil
}

// sameFile compares two paths by the file they name, so a store reached
// through a symlink or a different spelling of the same path is still
// recognised as the one the daemon is holding open.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}
