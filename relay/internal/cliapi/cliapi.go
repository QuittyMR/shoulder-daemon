// Package cliapi is the half of the HTTP surface a person talks to: the routes
// behind `shoulderd message`, `shoulderd fact` and `shoulderd digest`. It is a
// package of its own because httpapi may not import the advisor or the store —
// that ban is what keeps a hook fast while they are slow — and every route here
// does exactly that.
//
// Nothing here fails open. A hook that cannot be served must not disturb the
// session that sent it, so httpapi answers "no advice" and says nothing; a
// person who typed a command is owed the reason instead. Above all a request
// that never said local or global is refused, naming the flag it was missing,
// rather than filed wherever seems plausible.
package cliapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/pipeline"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// maxBodyBytes bounds a request body. These carry one typed sentence, not a
// session window.
const maxBodyBytes = 1 << 20

// DefaultListLimit is how much `fact list` shows when the user names no limit.
const DefaultListLimit = 50

// Server serves the CLI routes. It borrows the pipeline's connector and
// counters rather than being handed its own, so a fact typed at the terminal
// cannot end up in a different store from one learned in a session.
type Server struct {
	Pipe  *pipeline.Pipeline
	Token string
}

func New(pipe *pipeline.Pipeline, token string) *Server {
	return &Server{Pipe: pipe, Token: token}
}

// Mount adds the CLI routes to the mux the hooks are already served from, so
// the daemon listens on one address and honours one token.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/v1/cli/message", s.handleMessage)
	mux.HandleFunc("/v1/cli/facts", s.handleFacts)
	mux.HandleFunc("/v1/cli/digest", s.handleDigest)
	mux.HandleFunc("/v1/cli/consolidate", s.handleConsolidate)
	mux.HandleFunc("/v1/cli/config", s.handleConfig)
	mux.HandleFunc("/v1/cli/memory", s.handleMemory)
}

// The request and reply types below are the wire contract. They are exported
// because cmd/shoulderd encodes against them: one definition of each shape
// means the CLI and the daemon cannot drift apart field by field.
// ConsolidateRequest asks for one tidying pass over a scope. Unlike a digest,
// this one writes, so an unset scope is an omission rather than "everything".
type ConsolidateRequest struct {
	Scope   string `json:"scope"`
	Project string `json:"project"`
}

type ConsolidateResponse struct {
	Dropped int `json:"dropped"`
	Merged  int `json:"merged"`
}

type MessageRequest struct {
	Text    string `json:"text"`
	Scope   string `json:"scope"`
	Project string `json:"project"`
	Update  string `json:"update"`
}

type MessageResponse struct {
	Reply string       `json:"reply"`
	Facts []facts.Fact `json:"facts,omitempty"`
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) || !s.methodIs(w, r, http.MethodPost) {
		return
	}
	var req MessageRequest
	if !s.decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		s.fail(w, http.StatusBadRequest, errors.New("no message text"))
		return
	}
	sc, err := requireScope(req.Scope, req.Project)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	mode, err := parseUpdate(req.Update)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	if !s.requireModel(w) {
		return
	}
	reply, err := s.Pipe.Message(r.Context(), pipeline.MessageRequest{
		Text: req.Text, Scope: sc, Project: req.Project, Update: mode,
	})
	if err != nil {
		s.failed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, MessageResponse{Reply: reply.Reply, Facts: reply.Facts})
}

type FactRequest struct {
	ID       string   `json:"id,omitempty"`
	Content  string   `json:"content"`
	Category string   `json:"category,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Scope    string   `json:"scope"`
	Project  string   `json:"project,omitempty"`
}

type FactResponse struct {
	ID string `json:"id"`

	// AlreadyKnown reports a write the backend refused because it already holds
	// this exact content. The state the caller asked for is the state there is,
	// so it is an answer rather than a failure.
	AlreadyKnown bool `json:"already_known,omitempty"`
}

type FactsResponse struct {
	Facts []memory.Record `json:"facts"`
}

func (s *Server) handleFacts(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listFacts(w, r)
	case http.MethodPost, http.MethodPatch:
		s.writeFact(w, r)
	default:
		s.fail(w, http.StatusMethodNotAllowed, errors.New("use GET to list, POST to add, PATCH to update"))
	}
}

// writeFact stores what the user typed verbatim. No model is consulted: they
// wrote the sentence themselves, and a fact worth typing out is not a fact
// worth having second-guessed.
func (s *Server) writeFact(w http.ResponseWriter, r *http.Request) {
	var req FactRequest
	if !s.decode(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		s.fail(w, http.StatusBadRequest, errors.New("no fact content"))
		return
	}
	if r.Method == http.MethodPatch && req.ID == "" {
		s.fail(w, http.StatusBadRequest, errors.New("no id to update: pass --id"))
		return
	}
	sc, err := requireScope(req.Scope, req.Project)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// The pipeline drops a category the backend would silently rewrite. Here the
	// user chose it, so the typo is worth reporting rather than swallowing.
	category, ok := facts.NormaliseCategory(req.Category)
	if !ok {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("unknown category %q: use one of %s",
			req.Category, strings.Join(categoryNames(), ", ")))
		return
	}

	rec := memory.Record{Content: req.Content, Category: category, Tags: req.Tags, Scope: sc}
	if sc == scope.Local {
		rec.Project = req.Project
	}
	var id string
	if r.Method == http.MethodPatch {
		id, err = s.Pipe.Memory.Supersede(r.Context(), req.ID, rec)
	} else {
		id, err = s.Pipe.Memory.Store(r.Context(), rec)
	}
	if err != nil {
		s.refused(w, err)
		return
	}
	// Logged exactly as the pipeline logs its own writes, so `shoulderd monitor`
	// shows a fact typed at the terminal beside the ones the model deduced.
	if r.Method == http.MethodPatch {
		s.Pipe.Metrics.Inc("shoulder_cli_fact_superseded_total")
		s.Pipe.Log.Info("fact superseded", "origin", "cli", "scope", sc,
			"project", scope.Label(rec.Project), "supersedes", req.ID,
			"category", category, "content", rec.Content)
	} else {
		s.Pipe.Metrics.Inc("shoulder_cli_fact_stored_total")
		s.Pipe.Log.Info("fact stored", "id", id, "origin", "cli", "scope", sc,
			"project", scope.Label(rec.Project), "category", category, "content", rec.Content)
	}
	writeJSON(w, http.StatusOK, FactResponse{ID: id})
}

func (s *Server) listFacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := q.Get("project")
	sc, err := requireScope(q.Get("scope"), project)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	limit := DefaultListLimit
	if raw := q.Get("limit"); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit <= 0 {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("limit %q is not a positive number", raw))
			return
		}
	}
	query := memory.Query{Limit: limit, Scope: sc}
	if sc == scope.Local {
		query.Project = project
	}
	found, err := s.Pipe.Memory.List(r.Context(), query)
	if err != nil {
		s.failed(w, err)
		return
	}
	if found == nil {
		found = []memory.Record{}
	}
	writeJSON(w, http.StatusOK, FactsResponse{Facts: found})
}

type DigestRequest struct {
	Scope   string `json:"scope"`
	Project string `json:"project"`
}

type DigestResponse struct {
	Digest string `json:"digest"`
}

func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) || !s.methodIs(w, r, http.MethodPost) {
		return
	}
	var req DigestRequest
	if !s.decode(w, r, &req) {
		return
	}
	// The one route where an unset scope is an answer rather than an omission:
	// `shoulderd digest` with no flag asks about everything that is known.
	sc := scope.Any
	if strings.TrimSpace(req.Scope) != "" {
		parsed, err := scope.Parse(req.Scope)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		sc = parsed
	}
	if sc == scope.Local && req.Project == "" {
		s.fail(w, http.StatusBadRequest, errors.New("--local names no project: run this inside one"))
		return
	}

	if !s.requireModel(w) {
		return
	}
	digest, err := s.Pipe.Digest(r.Context(), pipeline.DigestRequest{Scope: sc, Project: req.Project})
	if err != nil {
		s.failed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DigestResponse{Digest: digest})
}

// requireModel refuses a request that has no advisor to answer it. The daemon
// says which variable to set when it starts, but its log is on a terminal the
// person who typed the command is not looking at, so the hint travels with the
// refusal instead.
func (s *Server) requireModel(w http.ResponseWriter) bool {
	if s.Pipe.Settings.Provider() != nil {
		return true
	}
	// The variable has to be set where the daemon reads it, which is not the
	// shell that typed the command. Saying only "set SHOULDER_LLM" sends anyone
	// running a containerised daemon round the loop of setting it correctly, in
	// the wrong process, and getting the same refusal back.
	s.fail(w, http.StatusServiceUnavailable, fmt.Errorf(
		"the daemon has no decision model configured; give it one now with `shoulderd config set --provider=NAME`, or set SHOULDER_LLM in the daemon's own environment (not this shell) so it survives a restart. Either takes one of %s",
		strings.Join(llm.Presets(), ", ")))
	return false
}

// requireScope resolves the scope of a write or a scoped read. The message
// names the flag rather than the JSON field: the person reading it typed a
// command, not a request body.
func requireScope(raw, project string) (scope.Scope, error) {
	if strings.TrimSpace(raw) == "" {
		return scope.Any, errors.New("no scope: pass --local or --global")
	}
	s, err := scope.Parse(raw)
	if err != nil {
		return scope.Any, err
	}
	if s == scope.Local && project == "" {
		return scope.Any, errors.New("--local names no project: run this inside one")
	}
	return s, nil
}

func parseUpdate(raw string) (pipeline.UpdateMode, error) {
	switch mode := pipeline.UpdateMode(strings.ToLower(strings.TrimSpace(raw))); mode {
	case "":
		return pipeline.UpdateAuto, nil
	case pipeline.UpdateAuto, pipeline.UpdateForce, pipeline.UpdateNever:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown update mode %q: expected auto, force or never", raw)
	}
}

func categoryNames() []string {
	names := make([]string, 0, len(facts.Categories))
	for c := range facts.Categories {
		names = append(names, c)
	}
	sort.Strings(names)
	return names
}

func (s *Server) authorised(w http.ResponseWriter, r *http.Request) bool {
	if s.Token == "" {
		return true
	}
	got := r.Header.Get("X-Shoulder-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1 {
		return true
	}
	s.Pipe.Metrics.Inc("shoulder_cli_unauthorised_total")
	// Absent and wrong are told apart deliberately. Both are one loopback hop
	// from a person who can read the daemon's own environment anyway, and
	// collapsing them produces the useless answer: telling somebody who just set
	// SHOULDER_TOKEN to set SHOULDER_TOKEN.
	msg := "the daemon requires a token; set SHOULDER_TOKEN in this shell to the value the daemon was started with"
	if got != "" {
		msg = "SHOULDER_TOKEN here does not match the token the daemon was started with"
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": msg})
	return false
}

func (s *Server) methodIs(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	s.fail(w, http.StatusMethodNotAllowed, fmt.Errorf("use %s here, not %s", want, r.Method))
	return false
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("unreadable body: %w", err))
		return false
	}
	if err := json.Unmarshal(body, into); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("malformed JSON body: %w", err))
		return false
	}
	return true
}

// fail answers a request the caller got wrong.
func (s *Server) fail(w http.ResponseWriter, status int, err error) {
	if status == http.StatusBadRequest {
		s.Pipe.Metrics.Inc("shoulder_cli_bad_request_total")
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// failed answers a request the daemon got wrong. Unscoped work reaching this
// far is a bug above it, not a caller mistake, so it is reported as one.
func (s *Server) failed(w http.ResponseWriter, err error) {
	s.Pipe.Metrics.Inc("shoulder_cli_error_total")
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// refused answers a write the backend would not take. An exact duplicate is
// not a refusal the caller has to act on: what they asked for is already true,
// and a script that files the same fact twice is doing nothing wrong. A
// semantic collision is different, and naming the record that blocked it is
// what lets the user turn the write into an update.
// refused turns a boundary or backend refusal into the answer a person typing a
// command should get. A cross-scope supersede is a 404 rather than a 400: the
// fact they named is real, it is just not here.
func (s *Server) refused(w http.ResponseWriter, err error) {
	var cross *memory.ErrCrossScopeSupersede
	if errors.As(err, &cross) {
		s.Pipe.Metrics.Inc("shoulder_cli_fact_wrong_scope_total")
		writeJSON(w, http.StatusNotFound, map[string]string{"error": cross.Error()})
		return
	}
	s.refusedRest(w, err)
}

func (s *Server) refusedRest(w http.ResponseWriter, err error) {
	var semantic *memory.ErrDuplicateSemantic
	switch {
	case errors.Is(err, memory.ErrNoBackend):
		s.Pipe.Metrics.Inc("shoulder_cli_fact_nowhere_total")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "nothing was written: this daemon has no store at all, which happens when its own file could not be opened; the startup log says why, and SHOULDER_MEMORY_PATH or SHOULDER_MEMORY_URL points it somewhere it can write",
		})
	case errors.Is(err, memory.ErrDuplicateExact):
		s.Pipe.Metrics.Inc("shoulder_cli_fact_duplicate_total")
		writeJSON(w, http.StatusOK, FactResponse{AlreadyKnown: true})
	case errors.As(err, &semantic):
		s.Pipe.Metrics.Inc("shoulder_cli_fact_duplicate_total")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("too similar to fact %s: correct it with `fact update --id=%s`",
				semantic.Collided, semantic.Collided),
		})
	default:
		s.failed(w, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":"response could not be encoded"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func (s *Server) handleConsolidate(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) || !s.methodIs(w, r, http.MethodPost) {
		return
	}
	var req ConsolidateRequest
	if !s.decode(w, r, &req) {
		return
	}
	sc, err := scope.Parse(req.Scope)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if sc == scope.Local && req.Project == "" {
		s.fail(w, http.StatusBadRequest, errors.New("--local names no project: run this inside one"))
		return
	}
	if !s.requireModel(w) {
		return
	}
	dropped, merged, err := s.Pipe.Consolidate(r.Context(), sc, req.Project)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, ConsolidateResponse{Dropped: dropped, Merged: merged})
}

// memoryProbeTimeout bounds the probe below. It is generous because the first
// read of a cold store can be slow — an embedding model may still be loading —
// and a doctor that calls that unreachable would be lying about the one thing
// it was asked.
const memoryProbeTimeout = 10 * time.Second

// MemoryStatus answers the question no metric can: is anything actually being
// remembered. Configured separates a daemon told about no store at all from one
// pointed at a store that will not answer, because the two are different
// mistakes; OK is the result of a real read, since a URL that resolves proves
// nothing about a backend refusing every request.
type MemoryStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// handleMemory probes the store and reports what happened. It lives on the
// daemon rather than in the CLI because the store is named in the daemon's
// environment, which is routinely not the shell anybody types in: a container,
// a service file, or an editor's idea of the environment.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		s.fail(w, http.StatusMethodNotAllowed, errors.New("use GET to check the memory backend"))
		return
	}

	st := MemoryStatus{Name: s.Pipe.Memory.Name()}
	st.Configured = st.Name != memory.Nop{}.Name()
	if !st.Configured {
		writeJSON(w, http.StatusOK, st)
		return
	}

	// A read, not a write. The probe must be able to run on a healthy daemon as
	// often as somebody types the command without leaving anything in the store
	// to explain later, and a backend that refuses reads is already broken for
	// every purpose this daemon has.
	ctx, cancel := context.WithTimeout(r.Context(), memoryProbeTimeout)
	defer cancel()
	if _, err := s.Pipe.Memory.Search(ctx, memory.Query{Text: "reachability probe", Limit: 1, Scope: scope.Global}); err != nil {
		st.Error = err.Error()
	} else {
		st.OK = true
	}
	writeJSON(w, http.StatusOK, st)
}

// ConfigResponse is what the daemon is doing now. It is the same shape whether
// the request read the settings or changed them, so a caller that changed one
// knob sees the state of all four without asking twice.
type ConfigResponse struct {
	settings.Snapshot
}

// handleConfig reads the live settings with GET and turns them with PATCH.
// PATCH rather than POST because a request names only the knobs it wants moved:
// there is no way to submit the whole set, and one that omitted a field would
// otherwise be asking to clear it.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if !s.authorised(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, ConfigResponse{s.Pipe.Settings.Snapshot()})
	case http.MethodPatch:
		var req settings.Change
		if !s.decode(w, r, &req) {
			return
		}
		now, err := s.Pipe.Settings.Apply(req)
		if err != nil {
			// Every refusal from here is the caller naming something that does
			// not exist — a level, a level of pickiness, a provider, or a model
			// its provider has no key for. The daemon is unchanged, and the
			// sentence already says which one.
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		s.Pipe.Metrics.Inc("shoulder_cli_config_changed_total")
		s.Pipe.Log.Info("settings changed at the terminal",
			"log_level", now.LogLevel, "pickiness", now.Pickiness,
			"provider", now.Provider, "model", now.Model)
		writeJSON(w, http.StatusOK, ConfigResponse{now})
	default:
		s.fail(w, http.StatusMethodNotAllowed, errors.New("use GET to read the settings, PATCH to change them"))
	}
}
