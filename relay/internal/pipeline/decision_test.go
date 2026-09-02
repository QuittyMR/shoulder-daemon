package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// keywordBody is a decision that says nothing and remembers nothing, carrying
// only the keywords this turn was about.
func keywordBody(t *testing.T, keywords ...string) string {
	t.Helper()
	inner, err := json.Marshal(map[string]any{
		"inject": "", "facts": []any{}, "keywords": keywords,
	})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{"content": string(inner)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(outer)
}

// toolCallBody is the model asking for one tool instead of answering.
func toolCallBody(t *testing.T, name, args string) string {
	t.Helper()
	outer, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(outer)
}

// recordingAdvisor serves one body per call, repeating the last, and keeps
// every request it was sent. A tool result is only visible in the request that
// follows it, so seeing what the model was told means reading these.
type recordingAdvisor struct {
	mu     sync.Mutex
	bodies []string
	n      int
	seen   []string
}

func newRecordingAdvisor(t *testing.T, bodies ...string) (*recordingAdvisor, *httptest.Server) {
	t.Helper()
	a := &recordingAdvisor{bodies: bodies}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.seen = append(a.seen, string(body))
		out := a.bodies[min(a.n, len(a.bodies)-1)]
		a.n++
		a.mu.Unlock()
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(ts.Close)
	return a, ts
}

func (a *recordingAdvisor) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// notes are the session records among everything written, which is what the
// running keyword note is stored as.
func notes(recs []memory.Record) []memory.Record {
	var out []memory.Record
	for _, r := range recs {
		if r.Kind == memory.KindSession {
			out = append(out, r)
		}
	}
	return out
}

func turnIn(t *testing.T, s *stack, sid, dir, userText, assistantText string) {
	t.Helper()
	s.post(t, "UserPromptSubmit", promptIn(sid, userText, dir))
	s.post(t, "Stop", stop(sid, assistantText))
	select {
	case <-s.consults:
	case <-time.After(5 * time.Second):
		t.Fatal("the advisor was never consulted")
	}
}

func TestShortTurnKeepsEightKeywords(t *testing.T) {
	dir := t.TempDir()
	ts := advisorServer(t, 0, keywordBody(t,
		"k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9", "k10", "k11", "k12"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "do it", "Done.")

	stored, _, _ := mem.snapshot()
	note := notes(stored)
	if len(note) != 1 {
		t.Fatalf("expected one session note, got %d of %d records", len(note), len(stored))
	}
	if got := strings.Split(note[0].Content, ", "); len(got) != shortTurnKeywords {
		t.Fatalf("a short turn keeps %d keywords, got %d: %q", shortTurnKeywords, len(got), note[0].Content)
	}
	if note[0].Scope != scope.Local || note[0].Project != projectOf(t, dir) {
		t.Errorf("the note belongs to the session's project: %+v", note[0])
	}
	if s.srv.Metrics.Get("shoulder_session_keywords_stored_total") != 1 {
		t.Error("writing the note must be counted")
	}
}

func TestLongTurnKeepsTwentyFiveKeywords(t *testing.T) {
	dir := t.TempDir()
	words := make([]string, 40)
	for i := range words {
		words[i] = "k" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	ts := advisorServer(t, 0, keywordBody(t, words...))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	// Over 500 estimated tokens of turn, which is what buys the larger note.
	turnIn(t, s, "s1", dir, strings.Repeat("rewrite the parser and the loader. ", 80), "Done.")

	stored, _, _ := mem.snapshot()
	note := notes(stored)
	if len(note) != 1 {
		t.Fatalf("expected one session note, got %d of %d records", len(note), len(stored))
	}
	if got := strings.Split(note[0].Content, ", "); len(got) != longTurnKeywords {
		t.Fatalf("a long turn keeps %d keywords, got %d: %q", longTurnKeywords, len(got), note[0].Content)
	}
}

// The note is one record that keeps being rewritten. Three turns that each
// stored one would leave three of them, and a search for what the session is
// about would answer with its own history three times over.
func TestSessionNoteIsSupersededNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	ts := sequencedAdvisor(t,
		keywordBody(t, "parser"),
		keywordBody(t, "loader"),
		keywordBody(t, "renderer"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")
	turnIn(t, s, "s1", dir, "now the loader", "Done.")
	turnIn(t, s, "s1", dir, "now the renderer", "Done.")

	stored, superseded, _ := mem.snapshot()
	note := notes(stored)
	if len(note) != 3 {
		t.Fatalf("expected three writes of one note, got %d", len(note))
	}
	if len(superseded) != 2 {
		t.Fatalf("expected the second and third turns to supersede, got %v", superseded)
	}
	// Each supersede names the id the previous write returned, not the one
	// before it: the handle dies with every rewrite.
	if superseded[0] != "mem_1" || superseded[1] != "mem_2" {
		t.Errorf("each turn must replace the record the last one wrote, got %v", superseded)
	}
	if got := note[2].Content; got != "parser, loader, renderer" {
		t.Errorf("the note accumulates every turn, got %q", got)
	}
	if s.srv.Metrics.Get("shoulder_session_keywords_superseded_total") != 2 {
		t.Error("each rewrite must be counted")
	}
}

// The note must be invisible to everything that reads knowledge. That falls out
// of Kind: a query that never mentions one asks for facts, and the note is not
// one.
func TestSessionNoteStaysOutOfRecallAndDigest(t *testing.T) {
	dir := t.TempDir()
	ts := sequencedAdvisor(t, keywordBody(t, "parser"), proseBody(t, "A paragraph."))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{listed: map[scope.Scope][]memory.Record{
		scope.Global: {{ID: "g1", Content: "prefers terse answers", Scope: scope.Global}},
	}}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")
	if _, err := s.pipe.Digest(context.Background(), DigestRequest{Project: projectOf(t, dir)}); err != nil {
		t.Fatal(err)
	}

	stored, _, _ := mem.snapshot()
	if n := len(notes(stored)); n != 1 {
		t.Fatalf("expected the note to have been written as a session record, got %d", n)
	}
	searched, listed := mem.reads()
	if len(searched) == 0 || len(listed) == 0 {
		t.Fatal("expected the recall and the digest to have read something")
	}
	// A recall is a read of knowledge whichever scope it is aimed at, and there
	// is no reason for one ever to ask for a working note.
	for _, q := range searched {
		if q.Kind != memory.KindFact {
			t.Errorf("a recall must ask for facts, got kind %q", q.Kind)
		}
	}
	own := 0
	for _, q := range listed {
		if q.Kind == memory.KindFact {
			continue
		}
		// The single exception, and it is confined to the session's own
		// project: the session locating the note it is about to rewrite.
		own++
		if q.Kind != memory.KindSession || q.Scope != scope.Local || q.Project != projectOf(t, dir) {
			t.Errorf("the only read of a working note is a session finding its own, got %+v", q)
		}
	}
	if own != 1 {
		t.Errorf("expected exactly one lookup of this session's own note, got %d", own)
	}
}

func TestSessionHistoryToolAnswersWithEarlierKeywords(t *testing.T) {
	dir := t.TempDir()
	rec, ts := newRecordingAdvisor(t,
		keywordBody(t, "parser", "tokenizer"),
		toolCallBody(t, "session_history", "{}"),
		keywordBody(t, "parser"))
	s := newStack(t, ts.URL, 2*time.Second)
	s.pipe.Memory = &fakeMemory{}

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")
	turnIn(t, s, "s1", dir, "do it", "Done.")

	reqs := rec.requests()
	if len(reqs) != 3 {
		t.Fatalf("expected three model calls, got %d", len(reqs))
	}
	if !strings.Contains(reqs[2], "parser, tokenizer") {
		t.Fatalf("the earlier turn's keywords must reach the model: %s", reqs[2])
	}
	if s.srv.Metrics.Get("shoulder_decision_tool_call_total") != 1 {
		t.Error("the tool call must be counted")
	}
}

func TestSearchMemoryToolSearchesAgainAndTheResultReachesTheModel(t *testing.T) {
	dir := t.TempDir()
	rec, ts := newRecordingAdvisor(t,
		toolCallBody(t, "search_memory", `{"query":"release branch","limit":3,"min_score":0.2}`),
		keywordBody(t, "branch"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{recalled: map[scope.Scope][]memory.Record{
		scope.Global: {{ID: "g1", Content: "releases go out from release/stable", Scope: scope.Global}},
	}}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "which branch", "Not sure.")

	_, _, queries := mem.snapshot()
	var asked *memory.Query
	for i, q := range queries {
		if q.Text == "release branch" {
			asked = &queries[i]
		}
	}
	if asked == nil {
		t.Fatalf("the tool must run its own search, got %+v", queries)
	}
	if asked.Limit != 3 || asked.MinScore != 0.2 {
		t.Errorf("the model's limit and floor must be honoured, got %+v", *asked)
	}
	reqs := rec.requests()
	if len(reqs) != 2 {
		t.Fatalf("expected two model calls, got %d", len(reqs))
	}
	if !strings.Contains(reqs[1], "releases go out from release/stable") {
		t.Fatalf("the tool result must reach the model: %s", reqs[1])
	}
}

// A model that will not stop calling tools must not take the turn down with it.
func TestStepCapEndsTheTurnAndIsCounted(t *testing.T) {
	dir := t.TempDir()
	rec, ts := newRecordingAdvisor(t, toolCallBody(t, "session_history", "{}"))
	s := newStack(t, ts.URL, 2*time.Second)
	s.pipe.Memory = &fakeMemory{}

	turnIn(t, s, "s1", dir, "do it", "Done.")

	if n := len(rec.requests()); n != decisionSteps {
		t.Fatalf("the loop must stop at %d steps, got %d", decisionSteps, n)
	}
	if s.srv.Metrics.Get("shoulder_decision_steps_exhausted_total") != 1 {
		t.Error("hitting the cap must be counted")
	}
	if s.srv.Metrics.Get("shoulder_advisor_error_total") != 0 {
		t.Error("running out of steps is not an error the session should see")
	}
}

// A note is local by definition. A session whose directory does not resolve has
// nowhere to file one, and that has to be visible: the next turn silently loses
// its history.
func TestKeywordsWithNoProjectAreDroppedAndCounted(t *testing.T) {
	ts := advisorServer(t, 0, keywordBody(t, "parser"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	s.post(t, "UserPromptSubmit", prompt("s1", "fix the parser"))
	s.post(t, "Stop", stop("s1", "Fixed."))
	<-s.consults

	stored, _, _ := mem.snapshot()
	if n := len(notes(stored)); n != 0 {
		t.Fatalf("a session with no project must not file a note, got %d", n)
	}
	if s.srv.Metrics.Get("shoulder_session_keywords_no_project_total") != 1 {
		t.Error("the dropped note must be counted")
	}
}

// noteIn is the record a store would hand back for a session's working note,
// which is how a session that has forgotten everything finds it again.
func noteIn(t *testing.T, id, sessionID, dir, content string) memory.Record {
	t.Helper()
	return memory.Record{
		ID: id, Content: content, Kind: memory.KindSession, Session: sessionID,
		Scope: scope.Local, Project: projectOf(t, dir),
	}
}

// The daemon exits after fifteen idle minutes and the editor plugin relaunches
// it, and a session's state is dropped an hour after its last event. Both
// happen inside the life of an ordinary session, and both leave the note's id
// behind while the note itself lives on. Believing the empty id writes a second
// record, then a third, none of them distinguishable from a real second
// session.
func TestASessionThatLostItsStateRejoinsItsNoteInsteadOfWritingASecond(t *testing.T) {
	dir := t.TempDir()
	ts := sequencedAdvisor(t, keywordBody(t, "parser"), keywordBody(t, "loader"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")
	stored, _, _ := mem.snapshot()
	written := notes(stored)
	if len(written) != 1 {
		t.Fatalf("expected the first turn to write one note, got %d", len(written))
	}
	if written[0].Session != "s1" {
		t.Fatalf("the note must name the session that keeps it, got %q", written[0].Session)
	}
	mem.notes = []memory.Record{noteIn(t, "mem_1", "s1", dir, written[0].Content)}

	// Everything this process knew about the session, gone; the session itself
	// carries on.
	if gone := s.pipe.Registry.Evict(0, time.Now().Add(time.Hour)); len(gone) != 1 {
		t.Fatalf("expected the session's state to be dropped, got %+v", gone)
	}
	turnIn(t, s, "s1", dir, "now the loader", "Done.")

	stored, superseded, _ := mem.snapshot()
	if n := len(notes(stored)); n != 2 {
		t.Fatalf("expected two writes of one note, got %d", n)
	}
	if len(superseded) != 1 || superseded[0] != "mem_1" {
		t.Fatalf("the second turn must replace the note already in the store, got %v", superseded)
	}
	if s.srv.Metrics.Get("shoulder_session_keywords_rejoined_total") != 1 {
		t.Error("finding a note the process had forgotten must be counted")
	}
}

// A note is superseded for as long as its session lives and never removed, and
// one is written per session per project. Left alone the population grows
// without bound, and every one of them is worded out of that project's own
// turns, so they rank alongside the facts they are buried beside.
func TestAnEvictedSessionsNoteIsForgotten(t *testing.T) {
	dir := t.TempDir()
	ts := advisorServer(t, 0, keywordBody(t, "parser"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")

	gone := s.pipe.Registry.Evict(time.Hour, time.Now().Add(2*time.Hour))
	if len(gone) != 1 || gone[0].ID != "s1" {
		t.Fatalf("expected the idle session to be evicted, got %+v", gone)
	}
	if gone[0].KeywordRecord != "mem_1" {
		t.Fatalf("eviction must hand over the record the session leaves behind, got %q", gone[0].KeywordRecord)
	}
	s.pipe.forgetNotes(context.Background(), gone)

	if got := mem.forgets(); len(got) != 1 || got[0] != "mem_1" {
		t.Fatalf("the dead session's note must be removed, got %v", got)
	}
	if s.srv.Metrics.Get("shoulder_session_keywords_forgotten_total") != 1 {
		t.Error("tidying up must be counted")
	}
}

// vanishingMemory is a backend that has already replaced the record the caller
// is still naming: the supersede committed and the reply was lost, so every
// later attempt names something no read can see.
type vanishingMemory struct {
	fakeMemory
	dead string
}

func (v *vanishingMemory) Supersede(ctx context.Context, old string, r memory.Record) (string, error) {
	if old == v.dead {
		return "", &memory.ErrCrossScopeSupersede{OldID: old, Scope: r.Scope, Project: r.Project}
	}
	return v.fakeMemory.Supersede(ctx, old, r)
}

// Taking that refusal at face value freezes the note at the turn it broke on
// for the rest of the session, while the log blames a scope violation that
// never happened.
func TestAHalfLandedSupersedeDoesNotWedgeTheNote(t *testing.T) {
	dir := t.TempDir()
	ts := sequencedAdvisor(t,
		keywordBody(t, "parser"),
		keywordBody(t, "loader"),
		keywordBody(t, "renderer"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &vanishingMemory{dead: "mem_1"}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")
	// What the lost reply actually wrote: the replacement is in the store, and
	// it still carries this session's mark.
	mem.notes = []memory.Record{noteIn(t, "mem_committed", "s1", dir, "parser")}

	turnIn(t, s, "s1", dir, "now the loader", "Done.")
	turnIn(t, s, "s1", dir, "now the renderer", "Done.")

	stored, superseded, _ := mem.snapshot()
	written := notes(stored)
	if len(written) != 3 {
		t.Fatalf("every turn must still write the note, got %d", len(written))
	}
	if got := written[2].Content; got != "parser, loader, renderer" {
		t.Errorf("the note must keep accumulating, got %q", got)
	}
	// The dead id is refused and never reaches the backend again; what the
	// session rewrites from there is the record its lost write actually left.
	if len(superseded) != 2 || superseded[0] != "mem_committed" || superseded[1] != "mem_2" {
		t.Fatalf("the session must rejoin the record its lost write left behind, got %v", superseded)
	}
	if s.srv.Metrics.Get("shoulder_session_note_relocated_total") != 1 {
		t.Error("losing track of the note must be counted, not silently absorbed")
	}
	if s.srv.Metrics.Get("shoulder_memory_write_error_total") != 0 {
		t.Error("a recovered write is not a failed one")
	}
}

// The count cap says how many keywords a turn may add and nothing about how
// large each is. The note is stored and then read back into every later prompt,
// so one long keyword is paid for on every turn that follows it.
func TestAnEnormousKeywordIsClamped(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("boilerplate ", 8000)
	ts := advisorServer(t, 0, keywordBody(t, huge, "parser"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem

	turnIn(t, s, "s1", dir, "do it", "Done.")

	stored, _, _ := mem.snapshot()
	note := notes(stored)
	if len(note) != 1 {
		t.Fatalf("expected one note, got %d", len(note))
	}
	for _, w := range strings.Split(note[0].Content, ", ") {
		if len(w) > maxKeywordChars+len("…") {
			t.Fatalf("a keyword of %d bytes reached the store and every prompt after it", len(w))
		}
	}
}

// A dry run that skipped the note in silence was indistinguishable from a
// session whose turns produced no keywords, which is the one thing a dry run
// exists to find out.
func TestDryRunSaysTheNoteWasNotWritten(t *testing.T) {
	dir := t.TempDir()
	ts := advisorServer(t, 0, keywordBody(t, "parser"))
	s := newStack(t, ts.URL, 2*time.Second)
	mem := &fakeMemory{}
	s.pipe.Memory = mem
	s.pipe.Cfg.Budget.DryRun = true

	turnIn(t, s, "s1", dir, "fix the parser", "Fixed.")

	stored, _, _ := mem.snapshot()
	if n := len(notes(stored)); n != 0 {
		t.Fatalf("a dry run must not write: %d", n)
	}
	if s.srv.Metrics.Get("shoulder_session_keywords_dry_run_total") != 1 {
		t.Error("the skipped write must be counted, as the fact path counts its own")
	}
}

// The registry holds the sessions this process has heard from, not the sessions
// that are open. A daemon restarted a moment ago knows about one editor however
// many are running, so exiting on the first goodbye takes observation away from
// every other live one - which restarts it, which exits again on the next.
func TestAGoodbyeDoesNotStopADaemonAnotherSessionIsStillUsing(t *testing.T) {
	restore := lastSessionGrace
	lastSessionGrace = 300 * time.Millisecond
	t.Cleanup(func() { lastSessionGrace = restore })

	ts := advisorServer(t, 0, decisionBody(t, ""))
	s := newStack(t, ts.URL, 2*time.Second)
	idle := make(chan struct{}, 1)
	s.pipe.OnIdle = func() { idle <- struct{}{} }

	s.post(t, "UserPromptSubmit", prompt("s1", "working"))
	s.post(t, "SessionEnd", `{"session_id":"s1","hook_event_name":"SessionEnd"}`)
	// A second editor speaks up inside the grace period.
	time.Sleep(20 * time.Millisecond)
	s.post(t, "UserPromptSubmit", prompt("s2", "also working"))

	select {
	case <-idle:
		t.Fatal("the daemon stopped while another session was still working")
	case <-time.After(lastSessionGrace + 300*time.Millisecond):
	}
}

// The grace period must not become a daemon that never stops: with nothing else
// open, the goodbye still ends it.
func TestTheLastGoodbyeStillStopsTheDaemon(t *testing.T) {
	restore := lastSessionGrace
	lastSessionGrace = 300 * time.Millisecond
	t.Cleanup(func() { lastSessionGrace = restore })

	ts := advisorServer(t, 0, decisionBody(t, ""))
	s := newStack(t, ts.URL, 2*time.Second)
	idle := make(chan struct{}, 1)
	s.pipe.OnIdle = func() { idle <- struct{}{} }

	s.post(t, "UserPromptSubmit", prompt("s1", "working"))
	s.post(t, "SessionEnd", `{"session_id":"s1","hook_event_name":"SessionEnd"}`)

	select {
	case <-idle:
	case <-time.After(3 * time.Second):
		t.Fatal("the daemon outlived its only session")
	}
}
