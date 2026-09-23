package cliapi

import (
	"net/http"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/pipeline"
)

// LearnRequest asks for documentation the person already wrote to be read into
// the store. Paths replace the default list rather than adding to it, and are
// resolved by the daemon, which is why the CLI sends them absolute.
type LearnRequest struct {
	Scope   string   `json:"scope"`
	Project string   `json:"project"`
	Dir     string   `json:"dir,omitempty"`
	Paths   []string `json:"paths,omitempty"`

	// Replace deletes each document whose content reached the store. It is
	// refused outright unless the worktree is clean, so nothing here can lose
	// a file that git could not give back.
	Replace bool `json:"replace,omitempty"`
}

// LearnedFile is one document and what it yielded. Chunks is reported because
// it is the only number that separates a document nothing was taken from — the
// ordinary result — from one that was never read.
type LearnedFile struct {
	Path    string `json:"path"`
	Chunks  int    `json:"chunks"`
	Stored  int    `json:"stored"`
	Skipped int    `json:"skipped"`
	Failed  int    `json:"failed"`
	Deleted bool   `json:"deleted,omitempty"`
	Error   string `json:"error,omitempty"`
}

// LearnResponse carries the per-file account and the totals of it. The totals
// are computed here rather than by the CLI so that every reader of this route
// adds them up the same way.
type LearnResponse struct {
	Files   []LearnedFile `json:"files"`
	Chunks  int           `json:"chunks"`
	Stored  int           `json:"stored"`
	Skipped int           `json:"skipped"`
	Failed  int           `json:"failed"`
	Deleted int           `json:"deleted"`
}

// handleLearn reads a repository's own documentation into the store.
//
// It is the slowest route the daemon has — a model call per section of every
// document — and it deliberately holds the request open for all of it, because
// the only useful answer is what became of each file. The CLI waits longer
// than the daemon will.
func (s *Server) handleLearn(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) || !s.methodIs(w, r, http.MethodPost) {
		return
	}
	var req LearnRequest
	if !s.decode(w, r, &req) {
		return
	}
	sc, err := requireScope(req.Scope, req.Project)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// Refused before the first document is read, for the reason a migration is:
	// with nowhere to write, every fact in the repository would fail with the
	// same sentence and the run would cost an hour to say so.
	if s.Pipe.Memory.Name() == (memory.Nop{}).Name() {
		s.refusedRest(w, memory.ErrNoBackend)
		return
	}
	if !s.requireModel(w) {
		return
	}

	result, err := s.Pipe.Learn(r.Context(), pipeline.LearnRequest{
		Scope: sc, Project: req.Project, Dir: req.Dir, Paths: req.Paths, Replace: req.Replace,
	})
	if err != nil {
		// Everything Learn refuses is the request as it was typed: a directory
		// it cannot read, a path outside the worktree, a tree with uncommitted
		// changes in it. A document that fails is reported against that
		// document instead, and the run carries on.
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, learned(result))
}

func learned(result pipeline.LearnResult) LearnResponse {
	out := LearnResponse{Files: make([]LearnedFile, 0, len(result.Files))}
	for _, f := range result.Files {
		out.Files = append(out.Files, LearnedFile{
			Path: f.Path, Chunks: f.Chunks, Stored: f.Stored,
			Skipped: f.Skipped, Failed: f.Failed, Deleted: f.Deleted, Error: f.Error,
		})
		out.Chunks += f.Chunks
		out.Stored += f.Stored
		out.Skipped += f.Skipped
		out.Failed += f.Failed
		if f.Deleted {
			out.Deleted++
		}
	}
	return out
}

// Learned reports whether anything was lost. A document nobody could read and
// a fact the store would not take are the same answer to the person who ran
// this: the repository still says something the memory does not.
func (r LearnResponse) Learned() bool { return r.Failed == 0 && r.unread() == 0 }

func (r LearnResponse) unread() int {
	n := 0
	for _, f := range r.Files {
		if f.Error != "" {
			n++
		}
	}
	return n
}
