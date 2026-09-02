// Package pipeline owns the one slow thing: the advisor call. It is
// deliberately a separate package from httpapi so that the hook path cannot
// reach it, and so the "advisor stalls, hooks do not" property can be tested
// directly.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/metrics"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/outbox"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/render"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/sanitize"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

type Pipeline struct {
	Cfg      config.Config
	Log      *slog.Logger
	Metrics  *metrics.Metrics
	Registry *session.Registry
	Outbox   *outbox.Box
	LLM      llm.Provider
	Memory   memory.Connector
	Queue    chan session.Event

	// IdleExit is a backstop for a harness that dies without saying goodbye.
	// Zero, the default, means the daemon waits to be told.
	IdleExit time.Duration

	// OnIdle is called once when the daemon has decided to stop.
	OnIdle func()

	// done is closed per consult in tests that need to await completion.
	OnConsulted func(sessionID string)
}

const (
	// shutdownSweep bounds the last thing the daemon does. It is short because
	// a process asked to stop has already been asked once and will be killed
	// if it dawdles.
	shutdownSweep = 5 * time.Second

	// noteOrphanAge is how old a note must be before another session may
	// remove it. It is well past IdleEviction so that a note still being
	// written by a live session in the same project can never match.
	noteOrphanAge = 6 * time.Hour
)

// IdleEviction is how long a session may go untouched before its state and
// pending advice are dropped.
const IdleEviction = time.Hour

// IdleExit is how long the daemon runs with no open session and no traffic
// before shutting itself down.
//
// The alternative was tying its life to the harness's SessionEnd, which cannot
// work: that event fires at the end of every `claude -p` and at every resume
// boundary, so it says nothing about whether anybody is still working. Counting
// live sessions is the same question asked where the answer is actually known,
// and it is correct with any number of editors open.
const IdleExit = 15 * time.Minute

// Run drains the queue until the context is cancelled. A turn boundary triggers
// an advisor call on its own goroutine so a slow advisor cannot back the queue
// up behind itself.
func (p *Pipeline) Run(ctx context.Context) {
	janitor := time.NewTicker(5 * time.Minute)
	defer janitor.Stop()
	for {
		select {
		case <-ctx.Done():
			// The notes of sessions still live at shutdown have no other
			// handle: the ids live in memory and die with this process. On a
			// fresh context, because the one that just ended is the reason we
			// are here and would cancel every delete before it left.
			sctx, cancel := context.WithTimeout(context.Background(), shutdownSweep)
			p.forgetNotes(sctx, p.Registry.Drain())
			cancel()
			return
		case now := <-janitor.C:
			if p.IdleExit > 0 {
				if idle, empty := p.Registry.Idle(now); empty && idle > p.IdleExit {
					p.Log.Info("no sessions and nothing to do; shutting down", "idle", idle.Round(time.Second))
					if p.OnIdle != nil {
						p.OnIdle()
					}
					return
				}
			}
			evicted := p.Registry.Evict(IdleEviction, now)
			for _, ev := range evicted {
				p.Outbox.Forget(ev.ID)
				p.Metrics.Inc("shoulder_sessions_evicted_total")
			}
			// Off the loop: this is one network write per dead session, and
			// what is queued behind this tick is a turn waiting to be advised.
			go p.forgetNotes(context.WithoutCancel(ctx), evicted)
			// An editor that is killed, crashes, or loses the machine under it
			// never sends its goodbye, so the daemon would otherwise sit here
			// holding a session nobody is in. Eviction is that session dying of
			// old age; if it was the last one there is nothing left to observe.
			if len(evicted) > 0 && p.Registry.Len() == 0 {
				p.Log.Info("last session evicted; shutting down", "evicted", len(evicted))
				if p.OnIdle != nil {
					p.OnIdle()
				}
				return
			}
		case ev := <-p.Queue:
			if ev.Kind == session.KindSessionEnd {
				// The harness has said this editor is done. If it was the last
				// one there is nothing left to observe, so the daemon stops
				// rather than sitting idle waiting to be noticed.
				gone, left := p.Registry.CloseSession(ev.SessionID)
				p.Outbox.Forget(ev.SessionID)
				go p.forgetNotes(context.WithoutCancel(ctx), []session.Evicted{gone})
				if left == 0 {
					p.Log.Info("last session ended; shutting down", "session", ev.SessionID)
					if p.OnIdle != nil {
						p.OnIdle()
					}
					return
				}
				continue
			}
			// Both ends of a turn, for different reasons. A prompt is where
			// advice can still change what happens: the assistant is thinking,
			// and the next PreToolUse of this same turn carries whatever the
			// advisor says. A turn end is where the turn can finally be read
			// whole, which is what facts are extracted from. Advising only at
			// the end means every note lands after the thing it was about.
			if ev.Kind != session.KindTurnEnd && ev.Kind != session.KindUserPrompt {
				continue
			}
			if !p.Registry.ClaimAdvisor(ev.SessionID) {
				p.Metrics.Inc("shoulder_advisor_skipped_inflight_total")
				continue
			}
			go func(sessionID string) {
				defer p.Registry.ReleaseAdvisor(sessionID)
				p.Consult(ctx, sessionID)
				if p.OnConsulted != nil {
					p.OnConsulted(sessionID)
				}
			}(ev.SessionID)
		}
	}
}

const (
	// RecallLimit caps how many stored records are put in front of the model.
	// More context is not better here: the model has to notice one
	// contradiction, not read a filing cabinet.
	RecallLimit = 8

	// DigestLimit caps how much of a scope a digest reads. A digest is prose
	// about what is known, not an export.
	DigestLimit = 200

	recallTimeout = 10 * time.Second
	writeTimeout  = 20 * time.Second

	// decisionSteps caps the tool loop. Four is one look at the prompt, two
	// lookups and an answer; a model still calling tools after that is not
	// converging and the turn is better served by whatever it has already said.
	decisionSteps = 4

	// searchToolLimit caps what the model may ask its own search for. The
	// number arrives from the model and sizes the next prompt, so an unbounded
	// one lets a single tool call spend the whole context window.
	searchToolLimit = 25

	// maxKeywordChars bounds one keyword. The count cap says how many may
	// arrive and nothing about how large each is, and the note is stored and
	// then read back into every prompt for the rest of the session, so one long
	// keyword is paid for on every turn that follows it. A keyword this long is
	// already a sentence.
	maxKeywordChars = 64

	// shortTurnTokens divides turns that carry one subject from turns that
	// wandered. Tokens are estimated at four characters each, which is close
	// enough for a threshold and costs nothing.
	shortTurnTokens = 500

	// shortTurnKeywords and longTurnKeywords bound the note a turn may add. The
	// model is told these numbers and they are enforced anyway: a note that
	// grows at whatever rate the model chooses is a prompt nobody sized.
	shortTurnKeywords = 8
	longTurnKeywords  = 25
)

// sessionScopes is what a session reads. Its own project is the obvious half;
// the global half is there because a preference the user stated in another
// repository is still true in this one, and would otherwise never surface.
var sessionScopes = []scope.Scope{scope.Local, scope.Global}

// Consult runs one full pass for a session: recall what is known, decide, queue
// any injection, and persist any new facts. Every outcome is counted separately
// so "nothing to say" is never confused with "the pipe is broken". Nothing is
// written to disk.
func (p *Pipeline) Consult(ctx context.Context, sessionID string) {
	events, turn, ok := p.Registry.Snapshot(sessionID)
	if !ok || len(events) == 0 {
		return
	}
	window := render.Window(events, p.Cfg.WindowEvents, p.Cfg.WindowChars)
	if p.LLM == nil {
		return
	}

	project := p.sessionProject(events)
	recalled := p.recall(ctx, render.RecallQuery(events), project, sessionScopes, RecallLimit, 0)

	cctx, cancel := context.WithTimeout(ctx, p.Cfg.AdvisorTimeout)
	defer cancel()

	// The decision is a tool loop rather than one question: a turn that says
	// only "do it" is unreadable without what came before, and the model is the
	// one that knows whether this is such a turn.
	counted := &countedSteps{Provider: p.LLM}
	start := time.Now()
	raw, err := llm.Run(cctx, counted, prompts.Decision, decisionPrompt(window, recalled),
		p.decisionTools(sessionID, project), decisionSteps)
	p.Metrics.ObserveAdvisor(time.Since(start))
	if err != nil {
		p.Metrics.Inc("shoulder_advisor_error_total")
		p.Log.Warn("decision failed; session unaffected", "session", sessionID, "err", err)
		return
	}
	if counted.exhausted(decisionSteps) {
		p.Metrics.Inc("shoulder_decision_steps_exhausted_total")
		p.Log.Warn("decision stopped at the step cap still asking for tools", "session", sessionID)
	}
	decision, err := llm.ParseDecision(raw)
	if err != nil {
		p.Metrics.Inc("shoulder_advisor_error_total")
		p.Log.Warn("decision failed; session unaffected", "session", sessionID, "err", err)
		return
	}

	p.queueInjection(sessionID, turn, decision.Inject)
	p.persist(ctx, sessionID, project, decision.Facts, recalled)
	p.rememberKeywords(ctx, sessionID, project, window+decision.Inject, decision.Keywords)
}

// countedSteps watches one tool loop go past. llm.Run returns the model's last
// text whether it finished or ran out of steps, which is right for the session
// and leaves nobody able to see the difference; this is how the difference is
// seen. The loop ended at the cap when the last thing the model did was ask for
// another tool and there was no step left to answer it in.
type countedSteps struct {
	llm.Provider
	mu      sync.Mutex
	steps   int
	pending bool
}

func (c *countedSteps) Chat(ctx context.Context, msgs []llm.Message, tools []llm.Tool) (llm.Message, error) {
	m, err := c.Provider.Chat(ctx, msgs, tools)
	if err != nil {
		return m, err
	}
	c.mu.Lock()
	c.steps++
	c.pending = len(m.ToolCalls) > 0
	c.mu.Unlock()
	return m, nil
}

func (c *countedSteps) exhausted(max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.steps >= max && c.pending
}

// decisionPrompt renders the turn and whatever the first search matched.
//
// The scope is shown because the model is asked which stored fact a new one
// replaces, and both scopes are in this list. Without it the only signal it has
// is the wording, which is identical either way.
func decisionPrompt(window string, recalled []memory.Record) string {
	var b strings.Builder
	b.WriteString("<recent-turn>\n")
	b.WriteString(window)
	b.WriteString("\n</recent-turn>\n\n<stored-facts>\n")
	if len(recalled) == 0 {
		b.WriteString("(none matched)")
	}
	writeRecalled(&b, recalled)
	b.WriteString("\n</stored-facts>")
	return b.String()
}

func writeRecalled(b *strings.Builder, recs []memory.Record) {
	for _, r := range recs {
		fmt.Fprintf(b, "id=%s scope=%s category=%s: %s\n", r.ID, r.Scope, r.Category, r.Content)
	}
}

// decisionTools are the two the decision model gets. Both answer about this
// session only: search_memory reads the scopes this session may read, and
// session_history reads the note this session has been keeping.
func (p *Pipeline) decisionTools(sessionID, project string) []llm.Binding {
	return []llm.Binding{{
		Tool: llm.Tool{
			Name:        "search_memory",
			Description: "Search stored facts again, more broadly than the search whose results are already in the prompt.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":     map[string]any{"type": "string", "description": "what to look for"},
					"limit":     map[string]any{"type": "integer", "description": "how many records to return"},
					"min_score": map[string]any{"type": "number", "description": "drop matches weaker than this"},
				},
				"required": []string{"query"},
			},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Query    string  `json:"query"`
				Limit    int     `json:"limit"`
				MinScore float64 `json:"min_score"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("search_memory arguments: %w", err)
			}
			p.Metrics.Inc("shoulder_decision_tool_call_total")
			if a.Limit <= 0 || a.Limit > searchToolLimit {
				a.Limit = searchToolLimit
			}
			found := p.recall(ctx, a.Query, project, sessionScopes, a.Limit, a.MinScore)
			if len(found) == 0 {
				return "(nothing matched)", nil
			}
			var b strings.Builder
			writeRecalled(&b, found)
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}, {
		Tool: llm.Tool{
			Name:        "session_history",
			Description: "The keywords from every earlier turn of this session, in order.",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		},
		Handler: func(context.Context, json.RawMessage) (string, error) {
			p.Metrics.Inc("shoulder_decision_tool_call_total")
			words := p.Registry.Keywords(sessionID)
			if len(words) == 0 {
				return "nothing yet", nil
			}
			return strings.Join(words, ", "), nil
		},
	}}
}

// rememberKeywords folds this turn's keywords into the session's running note
// and rewrites the one record that holds it.
//
// It is a session record rather than a fact: it is worth having on the next
// turn and noise a week later, and nothing that reads knowledge — recall, a
// digest, the fact list — asks for a kind, which is what keeps it out of them.
func (p *Pipeline) rememberKeywords(ctx context.Context, sessionID, project, turn string, words []string) {
	words = capKeywords(turn, words)
	if len(words) == 0 || p.Memory == nil {
		return
	}
	if project == "" {
		// The note is local by definition: it is about what this session is
		// doing in this checkout, and there is nowhere else to put it.
		p.Metrics.Inc("shoulder_session_keywords_no_project_total")
		return
	}
	prevID, all := p.Registry.AddKeywords(sessionID, words)
	if len(all) == 0 {
		return
	}
	if p.Cfg.Budget.DryRun {
		// Said out loud, like the fact path does. A dry run that skipped this
		// in silence was indistinguishable from a session whose turns produced
		// no keywords at all, which is the one thing a dry run is for finding
		// out.
		p.Metrics.Inc("shoulder_session_keywords_dry_run_total")
		p.Log.Info("dry run: would record what this session is about",
			"session", sessionID, "project", scope.Label(project), "keywords", strings.Join(all, ", "))
		return
	}

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	rec := memory.Record{
		Content: strings.Join(all, ", "),
		Kind:    memory.KindSession,
		Session: sessionID,
		Scope:   scope.Local,
		Project: project,
	}
	if prevID == "" {
		// The id held in memory is a shortcut, not the truth. It is dropped an
		// hour after a session goes quiet and again every time the daemon exits
		// on idle and the editor relaunches it, both of which happen inside the
		// life of an ordinary session. Believing it means writing a second note
		// for a session that already has one, and then a third, with nothing
		// able to tell those apart from three real sessions.
		prevID = p.findNote(wctx, sessionID, project)
	}
	id, counter, err := p.writeNote(wctx, sessionID, project, prevID, rec)
	if err != nil {
		if !errors.Is(err, memory.ErrNoBackend) {
			p.Metrics.Inc("shoulder_memory_write_error_total")
			p.Log.Warn("session keywords not written; the next turn loses this turn's context",
				"session", sessionID, "err", err)
		}
		return
	}
	p.Registry.SetKeywordRecord(sessionID, project, id)
	p.Metrics.Inc(counter)
}

// capKeywords bounds what one turn may add to the note, in count and in size.
// Both are applied here rather than asked of the model, because a model that
// ignores either decides how large the note grows and how large the prompts
// that carry it back are, for the rest of the session.
func capKeywords(turn string, words []string) []string {
	max := longTurnKeywords
	if len(turn)/4 < shortTurnTokens {
		max = shortTurnKeywords
	}
	out := make([]string, 0, min(len(words), max))
	for _, w := range words {
		w = textutil.Clip(strings.TrimSpace(w), maxKeywordChars)
		if w == "" {
			continue
		}
		out = append(out, w)
		if len(out) == max {
			break
		}
	}
	return out
}

// writeNote puts the note where the session left it, and recovers from the one
// failure that would otherwise last for the rest of the session.
//
// A supersede can commit at the backend and fail on the way home: the reply is
// prose that has to be parsed, and the write timeout can fire after the row has
// changed. The id this session is holding then names a record no read can see,
// so the boundary can only report that it is not in this scope — which of an id
// this session wrote to this project cannot be true of anything except a record
// that has gone. Taking that refusal at face value would leave the note frozen
// at the turn it broke on, while the log blamed a scope violation that never
// happened. The mark on the record is what makes the recovery exact: the
// replacement that did commit still carries it.
func (p *Pipeline) writeNote(ctx context.Context, sessionID, project, prevID string, rec memory.Record) (id, counter string, err error) {
	const (
		stored     = "shoulder_session_keywords_stored_total"
		superseded = "shoulder_session_keywords_superseded_total"
	)
	if prevID == "" {
		id, err = p.Memory.Store(ctx, rec)
		return id, stored, err
	}
	id, err = p.Memory.Supersede(ctx, prevID, rec)
	var cross *memory.ErrCrossScopeSupersede
	if !errors.As(err, &cross) || cross.Elsewhere {
		return id, superseded, err
	}

	p.Metrics.Inc("shoulder_session_note_relocated_total")
	p.Log.Info("this session's note is no longer where it was left; finding it again",
		"session", sessionID, "was", prevID)
	if again := p.findNote(ctx, sessionID, project); again != "" && again != prevID {
		id, err = p.Memory.Supersede(ctx, again, rec)
		return id, superseded, err
	}
	id, err = p.Memory.Store(ctx, rec)
	return id, stored, err
}

// findNote looks up the record already holding this session's note, by the mark
// the record carries rather than by any memory of having written it. The store
// is what survives an eviction and a restart; this process is not.
func (p *Pipeline) findNote(ctx context.Context, sessionID, project string) string {
	// No limit, for the reason checked.holds gives: a note past a cap reads as
	// absent, and here a false absent writes a second note for the same session
	// rather than merely refusing something.
	found, err := p.Memory.List(ctx, memory.Query{
		Scope: scope.Local, Project: project, Kind: memory.KindSession,
	})
	if err != nil {
		if !errors.Is(err, memory.ErrNoBackend) {
			p.Metrics.Inc("shoulder_memory_search_error_total")
			p.Log.Warn("could not look for this session's existing note; a second one may be written",
				"session", sessionID, "err", err)
		}
		return ""
	}
	mine := ""
	var stale []memory.Record
	for _, r := range found {
		switch {
		case r.Session == sessionID:
			p.Metrics.Inc("shoulder_session_keywords_rejoined_total")
			mine = r.ID
		case !r.CreatedAt.IsZero() && time.Since(r.CreatedAt) > noteOrphanAge:
			stale = append(stale, r)
		}
	}
	// A kill that skips the shutdown sweep - SIGKILL, an OOM, a suspended
	// laptop - leaves notes nothing holds a handle to. This is the one moment
	// something is already listing this project's notes, so it is where they
	// can be recognised: old enough that no live session could still be writing
	// one, and belonging to a session that is not this one.
	if len(stale) > 0 {
		go p.forgetStale(context.WithoutCancel(ctx), project, stale)
	}
	return mine
}

// forgetStale removes notes left behind by a process that did not get to clean
// up after itself.
func (p *Pipeline) forgetStale(ctx context.Context, project string, stale []memory.Record) {
	evicted := make([]session.Evicted, 0, len(stale))
	for _, r := range stale {
		evicted = append(evicted, session.Evicted{ID: r.Session, Project: project, KeywordRecord: r.ID})
	}
	p.Metrics.IncBy("shoulder_session_notes_orphaned_total", uint64(len(evicted)))
	p.forgetNotes(ctx, evicted)
}

// forgetNotes removes the working notes of sessions that have ended, and is the
// only thing that ever removes them. A note is superseded turn after turn while
// its session lives, and superseding replaces rather than deletes, so without
// this every session every project has ever run leaves one record behind — each
// of them worded out of that project's own turns, and therefore ranked
// alongside its facts in every search that project makes afterwards.
func (p *Pipeline) forgetNotes(ctx context.Context, evicted []session.Evicted) {
	if p.Memory == nil {
		return
	}
	for _, ev := range evicted {
		if ev.KeywordRecord == "" {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		err := p.Memory.Forget(wctx, ev.KeywordRecord, memory.Query{
			Scope: scope.Local, Project: ev.Project, Kind: memory.KindSession,
		})
		cancel()
		if err != nil {
			if !errors.Is(err, memory.ErrNoBackend) {
				p.Metrics.Inc("shoulder_memory_forget_error_total")
				p.Log.Warn("a dead session's note could not be removed; it will keep competing with this project's facts",
					"session", ev.ID, "record", ev.KeywordRecord, "err", err)
			}
			continue
		}
		p.Metrics.Inc("shoulder_session_keywords_forgotten_total")
	}
}

// sessionProject resolves the directory the session is working in to the
// project its local knowledge belongs to. An unresolvable directory leaves the
// session with global memory only, which is counted: silently filing its facts
// somewhere else would be the worse failure.
func (p *Pipeline) sessionProject(events []session.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].CWD == "" {
			continue
		}
		project, err := scope.Project(events[i].CWD)
		if err != nil {
			p.Metrics.Inc("shoulder_session_project_unresolved_total")
			p.Log.Warn("session directory does not resolve to a project; local memory is unreachable",
				"cwd", events[i].CWD, "err", err)
			return ""
		}
		return project
	}
	p.Metrics.Inc("shoulder_session_project_unresolved_total")
	return ""
}

// recall searches each scope separately and merges the results. Separately,
// because one unfiltered query would also match every other project's local
// knowledge, which is exactly what scoping exists to prevent.
//
// The search text is the session's most recent prose. The whole rendered window
// is a poor query: it is mostly tool noise, which drags a semantic search
// towards whatever files were touched rather than what was said.
func (p *Pipeline) recall(ctx context.Context, text, project string, scopes []scope.Scope, limit int, minScore float64) []memory.Record {
	if p.Memory == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()

	perScope := make([][]memory.Record, 0, len(scopes))
	seen := map[string]bool{}
	for _, s := range scopes {
		q := memory.Query{Text: text, Limit: limit, Scope: s, MinScore: minScore}
		if s == scope.Local {
			if project == "" {
				continue
			}
			q.Project = project
		}
		found, err := p.Memory.Search(rctx, q)
		if err != nil {
			p.Metrics.Inc("shoulder_memory_search_error_total")
			p.Log.Warn("memory search failed; continuing without that scope", "scope", s, "err", err)
			continue
		}
		hits := make([]memory.Record, 0, len(found))
		for _, r := range found {
			if r.ID != "" && seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			hits = append(hits, r)
		}
		perScope = append(perScope, hits)
	}

	// Take one hit from each scope in turn rather than ranking them together.
	// Score is optional at this boundary, so a backend that does not rank
	// returns zeros and any ordering by it is a no-op: the first scope searched
	// would fill the limit on its own and the global preferences a busy project
	// most needs would never reach the model.
	var merged []memory.Record
	for round := 0; len(merged) < limit; round++ {
		before := len(merged)
		for _, hits := range perScope {
			if round >= len(hits) || len(merged) >= limit {
				continue
			}
			merged = append(merged, hits[round])
		}
		if len(merged) == before {
			break
		}
	}
	if len(merged) == 0 {
		return nil
	}
	p.Metrics.Inc("shoulder_memory_recalled_total")
	return merged
}

func (p *Pipeline) queueInjection(sessionID string, turn uint64, raw string) {
	if sanitize.IsSilent(raw) {
		p.Metrics.Inc("shoulder_advice_silent_total")
		return
	}
	text := sanitize.Advice(raw, p.Cfg.Budget.MaxChars)
	if text == "" {
		p.Metrics.Inc("shoulder_advice_silent_total")
		return
	}
	a := session.Advice{
		ID:          "adv_" + randomID(),
		SessionID:   sessionID,
		Kind:        session.AdviceNote,
		Text:        text,
		CreatedTurn: turn,
		TTLTurns:    2,
		CreatedAt:   time.Now().UTC(),
	}
	p.Outbox.Push(a)
	p.Metrics.Inc("shoulder_advice_queued_total")
	// Logged in full, and at Info. This is the one thing the daemon exists to
	// produce, and it lands somewhere only the model reads; without this line
	// the only evidence a person can get is a counter going up.
	p.Log.Info("advice queued", "id", a.ID, "session", sessionID, "turn", turn, "text", text)
}

// persist reconciles the model's deduced facts against any the agent recorded
// explicitly this turn, then writes what survives.
func (p *Pipeline) persist(ctx context.Context, sessionID, project string, deduced []facts.Fact, recalled []memory.Record) {
	if p.Memory == nil {
		return
	}
	explicit := p.Registry.TakeFacts(sessionID)
	p.store(ctx, sessionID, project, facts.Reconcile(explicit, deduced), recalled)
}

// store applies the scope rule and writes what survives it, returning the facts
// that were accepted for writing.
//
// A fact whose scope was never decided dies here rather than being defaulted.
// Guessing is what puts a project's branch layout in front of every other
// repository the user opens, and the guess is invisible once it is stored.
func (p *Pipeline) store(ctx context.Context, origin, project string, merged []facts.Fact, recalled []memory.Record) []facts.Fact {
	if p.Memory == nil || len(merged) == 0 {
		return nil
	}

	// Link restatements of already-stored facts so they supersede rather than
	// accumulate. Reconcile only sees one turn; this sees the whole store.
	//
	// Placement travels with each record, and by key on both sides: a read is
	// entitled to hand back either form of a project, and a comparison that
	// mistook one for the other would decide two projects are the same one.
	key := scope.Key(project)
	seen := make([]facts.Recalled, 0, len(recalled))
	for _, r := range recalled {
		seen = append(seen, facts.Recalled{ID: r.ID, Content: r.Content, Scope: r.Scope, Project: r.ProjectKey()})
	}
	merged = facts.AgainstRecalled(merged, key, seen)

	kept := merged[:0]
	for _, f := range merged {
		switch {
		case !f.Scope.Valid():
			p.Metrics.Inc("shoulder_facts_missing_scope_total")
			p.Log.Warn("fact dropped: no scope was decided", "origin", origin, "content", f.Content)
		case f.Scope == scope.Local && project == "":
			p.Metrics.Inc("shoulder_facts_no_project_total")
			p.Log.Warn("local fact dropped: no project to file it under", "origin", origin, "content", f.Content)
		default:
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	if p.Cfg.Budget.DryRun {
		for _, f := range kept {
			p.Log.Info("dry run: would store fact", "origin", origin, "content", f.Content,
				"category", f.Category, "scope", f.Scope, "supersedes", f.Supersedes)
		}
		p.Metrics.Inc("shoulder_facts_dry_run_total")
		return kept
	}

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	for _, f := range kept {
		p.writeFact(wctx, origin, project, key, f, seen)
	}
	return kept
}

// writeFact persists one fact, converting a refused write into a supersede.
//
// A store that deduplicates refuses whatever it finds too close to something it
// already holds, and a correction is almost identical to what it corrects, so
// that refusal lands hardest on exactly the writes worth keeping while the
// stale fact goes on being recalled. Replacing the memory that blocked it is
// the correct recovery: if the new text really was redundant the supersede is
// near enough a no-op, and if it was a correction it now lands.
//
// recalled is what this session read for its own scopes, and it is what makes
// the recovery safe; see recalledHere.
func (p *Pipeline) writeFact(ctx context.Context, origin, project, key string, f facts.Fact, recalled []facts.Recalled) {
	category, ok := facts.NormaliseCategory(f.Category)
	if !ok {
		// Send no category rather than one nothing downstream agrees on.
		p.Metrics.Inc("shoulder_facts_bad_category_total")
		p.Log.Warn("decision model used an unknown category", "category", f.Category, "origin", origin)
	}
	rec := memory.Record{Content: f.Content, Category: category, Tags: f.Tags, Scope: f.Scope}
	if f.Scope == scope.Local {
		rec.Project = project
	}

	if f.Supersedes != "" {
		// The id came from the decision model, which is shown both scopes and is
		// perfectly capable of naming a global record as the thing its new local
		// fact replaces. The boundary refuses that; here it is only counted.
		_, err := p.Memory.Supersede(ctx, f.Supersedes, rec)
		var cross *memory.ErrCrossScopeSupersede
		switch {
		case err == nil:
			p.Metrics.Inc("shoulder_facts_superseded_total")
			p.Log.Info("fact superseded", "origin", origin, "scope", f.Scope,
				"project", scope.Label(project), "supersedes", f.Supersedes,
				"category", category, "content", f.Content)
		case errors.As(err, &cross):
			p.Metrics.Inc("shoulder_facts_refused_cross_scope_total")
			p.Log.Warn("supersede refused: the named fact is in another scope; the write is dropped rather than moving it here",
				"origin", origin, "supersedes", f.Supersedes, "scope", f.Scope,
				"project", scope.Label(project), "content", f.Content)
		case errors.Is(err, memory.ErrNoBackend):
			p.Metrics.Inc("shoulder_facts_nowhere_total")
		default:
			p.Metrics.Inc("shoulder_memory_write_error_total")
			p.Log.Warn("supersede failed", "origin", origin, "supersedes", f.Supersedes, "err", err)
		}
		return
	}

	id, err := p.Memory.Store(ctx, rec)
	if err == nil {
		p.Metrics.Inc("shoulder_facts_stored_total")
		p.Log.Info("fact stored", "id", id, "origin", origin, "scope", f.Scope,
			"project", scope.Label(project), "category", category, "content", f.Content)
		return
	}

	var semantic *memory.ErrDuplicateSemantic
	switch {
	case errors.Is(err, memory.ErrNoBackend):
		// Distinct from a write error: nothing is broken, there is simply
		// nowhere to write. Counting it as a failure would make the store-broken
		// alarm fire steadily in the default configuration.
		p.Metrics.Inc("shoulder_facts_nowhere_total")
	case errors.Is(err, memory.ErrDuplicateExact):
		p.Metrics.Inc("shoulder_facts_duplicate_total")
	case errors.As(err, &semantic):
		if semantic.Collided == "" {
			p.Metrics.Inc("shoulder_facts_refused_unattributed_total")
			p.Log.Warn("fact refused as a near-duplicate but the collision was not named; the write is lost",
				"origin", origin, "content", f.Content)
			return
		}
		if !recalledHere(recalled, f, key, semantic.Collided) {
			p.Metrics.Inc("shoulder_facts_refused_cross_scope_total")
			p.Log.Warn("fact refused by a memory this scope cannot see; the write is dropped rather than moving that memory here",
				"origin", origin, "scope", f.Scope, "project", scope.Label(project),
				"collided", semantic.Collided, "content", f.Content)
			return
		}
		if _, serr := p.Memory.Supersede(ctx, semantic.Collided, rec); serr != nil {
			p.Metrics.Inc("shoulder_memory_write_error_total")
			p.Log.Warn("fact refused as a near-duplicate and superseding the collision also failed",
				"origin", origin, "collided", semantic.Collided, "err", serr)
			return
		}
		p.Metrics.Inc("shoulder_facts_auto_superseded_total")
		p.Log.Info("fact superseded", "origin", origin, "scope", f.Scope,
			"project", scope.Label(project), "supersedes", semantic.Collided,
			"category", category, "content", f.Content,
			"why", "the backend refused it as a near-duplicate of that record")
	default:
		p.Metrics.Inc("shoulder_memory_write_error_total")
		p.Log.Warn("fact write failed", "origin", origin, "err", err)
	}
}

// recalledHere reports whether id names a memory this session actually read for
// the scope f is being written to.
//
// The id in a refusal comes from the store's own similarity search, which spans
// everything it holds and answers to no scope. Superseding it on that word
// alone re-tags another project's knowledge as this one's, which is the failure
// the whole scheme exists to prevent, so an id we did not recall is treated as
// belonging to somebody else.
func recalledHere(recalled []facts.Recalled, f facts.Fact, key, id string) bool {
	for _, r := range recalled {
		if r.ID == id && r.Placed(f.Scope, key) {
			return true
		}
	}
	return false
}

// UpdateMode says what a message the user typed may write back.
type UpdateMode string

const (
	// UpdateAuto lets the extraction pass decide whether the exchange
	// established anything durable. It is what an unflagged message does.
	UpdateAuto UpdateMode = "auto"
	// UpdateForce records the exchange even when the model was unmoved by it.
	// The user asked for this in so many words.
	UpdateForce UpdateMode = "force"
	// UpdateNever answers and writes nothing.
	UpdateNever UpdateMode = "never"
)

// MessageRequest is one question typed at the CLI. Scope is not optional: it
// says both which knowledge answers the question and where anything learned
// from the answer belongs.
type MessageRequest struct {
	Text    string
	Scope   scope.Scope
	Project string
	Update  UpdateMode
}

// MessageReply carries the prose the user sees and the facts the exchange
// yielded, so the CLI can show what it just committed them to.
type MessageReply struct {
	Reply string
	Facts []facts.Fact
}

// Message answers one question from the user, grounded in what is stored, and
// then records whatever the exchange established.
//
// Unlike Consult it is allowed to be slow: it runs on a CLI request, not on the
// hook path, and nothing about a session depends on it returning.
func (p *Pipeline) Message(ctx context.Context, req MessageRequest) (MessageReply, error) {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return MessageReply{}, errors.New("message is empty")
	}
	if !req.Scope.Valid() {
		return MessageReply{}, memory.ErrUnscoped
	}
	if req.Scope == scope.Local && req.Project == "" {
		return MessageReply{}, errors.New("a local message needs a project")
	}
	switch req.Update {
	case "", UpdateAuto, UpdateForce, UpdateNever:
	default:
		return MessageReply{}, fmt.Errorf("unknown update mode %q", req.Update)
	}
	if p.LLM == nil {
		return MessageReply{}, errors.New("no decision model is configured")
	}
	p.Metrics.Inc("shoulder_cli_message_total")

	// A global preference holds inside every project, so a question asked about
	// one project is answered with the global set alongside it. The reverse does
	// not hold: a question asked globally must not be answered with one
	// project's private details.
	scopes := []scope.Scope{scope.Local, scope.Global}
	if req.Scope == scope.Global {
		scopes = []scope.Scope{scope.Global}
	}
	recalled := p.recall(ctx, text, req.Project, scopes, RecallLimit, 0)

	// No budget gate here. The gate exists to stop unrequested advice
	// interrupting somebody's turn; this answer was asked for and is being
	// waited on.
	mctx, cancel := context.WithTimeout(ctx, p.Cfg.MessageTimeout)
	defer cancel()

	var b strings.Builder
	writeKnown(&b, recalled)
	b.WriteString("\n\n<question>\n")
	b.WriteString(text)
	b.WriteString("\n</question>")

	raw, err := p.LLM.Complete(mctx, prompts.Message, b.String())
	if err != nil {
		p.Metrics.Inc("shoulder_cli_message_error_total")
		return MessageReply{}, fmt.Errorf("message: %w", err)
	}
	reply := strings.TrimSpace(raw)

	if req.Update == UpdateNever {
		return MessageReply{Reply: reply}, nil
	}
	return MessageReply{Reply: reply, Facts: p.learn(ctx, req, text, reply, recalled)}, nil
}

// learn extracts what the exchange established and stores it. It goes through
// the same reconciliation the session path uses, so a fact typed at the CLI
// supersedes the stored version of itself instead of landing beside it.
func (p *Pipeline) learn(ctx context.Context, req MessageRequest, question, reply string, recalled []memory.Record) []facts.Fact {
	ectx, cancel := context.WithTimeout(ctx, p.Cfg.MessageTimeout)
	defer cancel()

	window := "<user>" + question + "</user>\n<assistant>" + reply + "</assistant>"
	decision, err := llm.Decide(ectx, p.LLM, window, recalled)
	if err != nil {
		p.Metrics.Inc("shoulder_cli_extract_error_total")
		p.Log.Warn("fact extraction failed; the answer still stands", "err", err)
	}

	deduced := decision.Facts
	if len(deduced) == 0 && req.Update == UpdateForce {
		// The user said to record this. Deferring to a model that found the
		// exchange unremarkable would be overruling them with a guess.
		// The scope is the one the person typed, for the words they typed.
		deduced = []facts.Fact{{Content: question, Source: facts.Explicit, Scope: req.Scope}}
		p.Metrics.Inc("shoulder_cli_facts_forced_total")
	}
	// The model's per-fact scope stands. "I always want terse answers,
	// everywhere" is global however it was typed, and stamping the request's
	// scope over it would file a statement about the user inside one project.
	// The request supplies only the project a local fact is bound to.
	return p.store(ctx, "cli", req.Project, facts.Reconcile(nil, deduced), recalled)
}

// DigestRequest selects what to describe. Scope Any means both, which is what
// a bare `digest` asks for.
type DigestRequest struct {
	Scope   scope.Scope
	Project string
}

// Digest describes everything a scope holds, in prose. It lists nothing: the
// CLI could print the records itself, and a list of them tells the user only
// what they already handed over one line at a time.
func (p *Pipeline) Digest(ctx context.Context, req DigestRequest) (string, error) {
	if req.Scope != scope.Any && !req.Scope.Valid() {
		return "", fmt.Errorf("unknown scope %q", req.Scope)
	}
	if req.Scope == scope.Local && req.Project == "" {
		return "", errors.New("a local digest needs a project")
	}
	p.Metrics.Inc("shoulder_cli_digest_total")

	local, global, err := p.listFor(ctx, req)
	if err != nil {
		return "", err
	}
	if len(local)+len(global) == 0 {
		// A model handed an empty list writes about it anyway. The honest
		// sentence is cheaper and cannot invent knowledge that does not exist.
		p.Metrics.Inc("shoulder_cli_digest_empty_total")
		return nothingKnown(req), nil
	}
	if p.LLM == nil {
		return "", errors.New("no decision model is configured")
	}

	var b strings.Builder
	if len(local) > 0 {
		fmt.Fprintf(&b, "<project name=%q>\n", scope.Label(req.Project))
		writeItems(&b, local)
		b.WriteString("</project>\n")
	}
	if len(global) > 0 {
		b.WriteString("<global>\n")
		writeItems(&b, global)
		b.WriteString("</global>\n")
	}

	dctx, cancel := context.WithTimeout(ctx, p.Cfg.DigestTimeout)
	defer cancel()

	raw, err := p.LLM.Complete(dctx, prompts.Digest, b.String())
	if err != nil {
		p.Metrics.Inc("shoulder_cli_digest_error_total")
		return "", fmt.Errorf("digest: %w", err)
	}
	return strings.TrimSpace(raw), nil
}

// listFor reads each requested scope in full. Unlike a recall, a failure here
// is returned: the user asked a direct question and a half-read digest would
// read as "nothing is stored".
func (p *Pipeline) listFor(ctx context.Context, req DigestRequest) (local, global []memory.Record, err error) {
	if p.Memory == nil {
		return nil, nil, nil
	}
	lctx, cancel := context.WithTimeout(ctx, p.Cfg.DigestTimeout)
	defer cancel()

	if req.Scope != scope.Global && req.Project != "" {
		local, err = p.Memory.List(lctx, memory.Query{Limit: DigestLimit, Scope: scope.Local, Project: req.Project})
		if err != nil {
			return nil, nil, fmt.Errorf("list project knowledge: %w", err)
		}
	}
	if req.Scope != scope.Local {
		global, err = p.Memory.List(lctx, memory.Query{Limit: DigestLimit, Scope: scope.Global})
		if err != nil {
			return nil, nil, fmt.Errorf("list global knowledge: %w", err)
		}
	}
	return local, global, nil
}

func nothingKnown(req DigestRequest) string {
	switch {
	case req.Scope == scope.Global:
		return "Nothing is recorded globally yet."
	case req.Scope == scope.Local:
		return fmt.Sprintf("Nothing is recorded for %s yet.", scope.Label(req.Project))
	case req.Project != "":
		return fmt.Sprintf("Nothing is recorded for %s or globally yet.", scope.Label(req.Project))
	}
	return "Nothing is recorded yet."
}

// writeKnown renders recalled records for a question, naming where each one
// came from so the model can say "in this project" rather than asserting a
// project detail as universal.
func writeKnown(b *strings.Builder, recs []memory.Record) {
	b.WriteString("<known>\n")
	if len(recs) == 0 {
		b.WriteString("(nothing stored matched this question)\n")
	}
	for _, r := range recs {
		where := string(r.Scope)
		if r.Scope == scope.Local {
			where = "project " + scope.Label(r.Project)
		}
		fmt.Fprintf(b, "- [%s] %s\n", where, r.Content)
	}
	b.WriteString("</known>")
}

func writeItems(b *strings.Builder, recs []memory.Record) {
	for _, r := range recs {
		if r.Category != "" {
			fmt.Fprintf(b, "- (%s) %s\n", r.Category, r.Content)
			continue
		}
		fmt.Fprintf(b, "- %s\n", r.Content)
	}
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
