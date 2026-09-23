package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

const (
	// consolidateEvery is how many completed turns pass between tidying runs.
	// The write path is deliberately near-silent, so a scope changes slowly and
	// a pass on every turn would spend a model call to find nothing.
	consolidateEvery = 5

	// consolidateFloor is the size below which a scope is left alone. A handful
	// of facts cannot be cluttered, and a model asked to tidy them will invent
	// work to justify the call.
	consolidateFloor = 8

	// consolidateCeiling bounds what one pass may remove, as a fraction of the
	// scope. A model that misreads the instruction and returns every id would
	// otherwise empty a memory that took months to build, in one call, with no
	// way back: Forget deletes.
	consolidateCeiling = 0.4
)

// consolidation is what the model asks for.
type consolidation struct {
	Drop  []string `json:"drop"`
	Merge []struct {
		Keep     string   `json:"keep"`
		Replaces []string `json:"replaces"`
		Content  string   `json:"content"`
	} `json:"merge"`
}

// ConsolidateRequest selects the scope to tidy. Unlike a digest it writes, so
// an unset scope is an omission rather than "everything".
type ConsolidateRequest struct {
	Scope   scope.Scope
	Project string
	Dir     string
}

// Consolidate tidies one scope: it drops facts that have stopped being rules
// and collapses several wordings of one rule into a single record.
//
// It exists because the write path cannot see this. That path judges one turn
// against a handful of recalled facts, so it cannot tell that it is writing the
// fourth phrasing of something already stored, nor that a fact written last
// week has since decayed into a note about history. Both are only visible from
// above the whole scope.
func (p *Pipeline) Consolidate(ctx context.Context, req ConsolidateRequest) (dropped, merged int, err error) {
	sc, project := req.Scope, req.Project
	prov := p.Settings.Provider()
	if p.Memory == nil || prov == nil {
		return 0, 0, nil
	}
	if !sc.Valid() {
		return 0, 0, fmt.Errorf("consolidate needs a scope, got %q", sc)
	}
	if sc == scope.Local && project == "" {
		return 0, 0, errors.New("a local consolidation needs a project")
	}

	lctx, cancel := context.WithTimeout(ctx, p.Cfg.DigestTimeout)
	defer cancel()

	held, err := p.Memory.List(lctx, memory.Query{Scope: sc, Project: project, Dir: req.Dir, Kind: memory.KindFact})
	if err != nil {
		p.Metrics.Inc("shoulder_memory_search_error_total")
		return 0, 0, err
	}
	if len(held) < consolidateFloor {
		return 0, 0, nil
	}

	var b strings.Builder
	byID := make(map[string]memory.Record, len(held))
	for _, r := range held {
		byID[r.ID] = r
		fmt.Fprintf(&b, "%s | %s | %s\n", r.ID, r.Category, textutil.Clip(r.Content, 400))
	}

	dctx, dcancel := context.WithTimeout(ctx, p.Cfg.DigestTimeout)
	defer dcancel()
	raw, err := prov.Complete(dctx, prompts.Consolidate, b.String())
	if err != nil {
		p.Metrics.Inc("shoulder_consolidate_error_total")
		return 0, 0, err
	}

	var plan consolidation
	if err := json.Unmarshal([]byte(llm.Unfence(strings.TrimSpace(raw))), &plan); err != nil {
		p.Metrics.Inc("shoulder_consolidate_unparsed_total")
		return 0, 0, fmt.Errorf("consolidate: %w", err)
	}

	// Every id is checked against what was sent. A hallucinated id would
	// otherwise be handed to Forget, which deletes, and the boundary can only
	// confirm the scope - not that this pass ever saw the record.
	budget := int(float64(len(held)) * consolidateCeiling)
	// A merge is a supersede and then its forgets; one cut off between the
	// two leaves both wordings standing, which is what this pass is for.
	ctx, done := Decided(ctx)
	defer done()
	where := memory.Query{Scope: sc, Project: project, Dir: req.Dir, Kind: memory.KindFact}

	for _, m := range plan.Merge {
		// Between merges is the one place a stop leaves nothing half done;
		// past the grace every write would fail, and each failure would be
		// counted and reported as the store refusing.
		if ctx.Err() != nil {
			break
		}
		keep, ok := byID[m.Keep]
		if !ok || strings.TrimSpace(m.Content) == "" {
			continue
		}
		gone := make([]string, 0, len(m.Replaces))
		// One sentence replaces all of them, so it inherits the strictest
		// placement any of them had: merging a private fact into a public one
		// would otherwise publish it under a wording nobody reviewed.
		private := keep.Private
		for _, id := range m.Replaces {
			r, ours := byID[id]
			if ours && id != m.Keep {
				gone = append(gone, id)
				private = private || r.Private
			}
		}
		if len(gone) == 0 || dropped+len(gone) > budget {
			continue
		}
		rec := keep
		rec.Content = strings.TrimSpace(m.Content)
		rec.Private = private
		rec.Dir = req.Dir
		if _, err := p.Memory.Supersede(ctx, m.Keep, rec); err != nil {
			p.Metrics.Inc("shoulder_memory_write_error_total")
			continue
		}
		for _, id := range gone {
			if p.forget(ctx, id, where) {
				dropped++
			}
		}
		merged++
		p.Log.Info("facts merged", "scope", sc, "project", scope.Label(project),
			"kept", m.Keep, "replaced", strings.Join(gone, ","), "content", rec.Content)
	}

	for _, id := range plan.Drop {
		if ctx.Err() != nil {
			break
		}
		r, ok := byID[id]
		if !ok || dropped >= budget {
			continue
		}
		if p.forget(ctx, id, where) {
			dropped++
			p.Log.Info("fact dropped as no longer a rule", "scope", sc,
				"project", scope.Label(project), "id", id, "content", r.Content)
		}
	}

	p.Metrics.IncBy("shoulder_facts_consolidated_total", uint64(dropped)) //nolint:gosec // G115: a count of records, never negative
	return dropped, merged, nil
}

func (p *Pipeline) forget(ctx context.Context, id string, where memory.Query) bool {
	fctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := p.Memory.Forget(fctx, id, where); err != nil {
		p.Metrics.Inc("shoulder_memory_forget_error_total")
		p.Log.Warn("a fact the tidying pass wanted gone is still there", "id", id, "err", err)
		return false
	}
	return true
}

// consolidateBoth tidies the project and the global scope together, off the
// hook path. Errors are logged rather than returned: nothing the session is
// waiting on depends on this.
func (p *Pipeline) consolidateBoth(ctx context.Context, at site) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, req := range []ConsolidateRequest{
		{Scope: scope.Local, Project: at.project, Dir: at.dir}, {Scope: scope.Global},
	} {
		if req.Scope == scope.Local && at.project == "" {
			continue
		}
		if _, _, err := p.Consolidate(cctx, req); err != nil {
			p.Log.Warn("tidying pass failed; the store is unchanged", "scope", req.Scope, "err", err)
		}
	}
}
