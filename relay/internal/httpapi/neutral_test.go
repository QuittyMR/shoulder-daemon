package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/session"
)

func postNeutral(h http.Handler, body string, token ...string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body))
	for _, tok := range token {
		req.Header.Set("X-Shoulder-Token", tok)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoutineEventsAreTheOnesAWorkingInstallMustHaveSeen(t *testing.T) {
	got := RoutineEvents()
	want := []string{"PostToolUse", "PreToolUse", "SessionEnd", "Stop", "UserPromptSubmit"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("RoutineEvents = %v, want %v (sorted, without the events a session may never produce)", got, want)
	}
}

func TestFlattenReadsWhatClaudeCodeSendsAsAToolResponse(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"nothing", ``, ""},
		{"string", `"ok"`, "ok"},
		{"stdout first", `{"stderr":"warn","stdout":"built"}`, "built"},
		{"content", `{"content":"file text"}`, "file text"},
		{"stderr alone", `{"stderr":"boom"}`, "boom"},
		{"unknown shape", `{"weird":1}`, `{"weird":1}`},
		{"empty strings fall through", `{"stdout":"","result":"done"}`, "done"},
	}
	for _, tc := range cases {
		if got := flatten(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: flatten(%s) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestANeutralEventIsRecordedWithDefaultsFilledIn(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()

	rec := postNeutral(h, `{"session_id":"n1","event":"user_prompt","prompt":"hello"}`)
	if rec.Body.String() != string(noAdviceJSON) {
		t.Fatalf("an event with nothing pending must get the empty answer, got %s", rec.Body.String())
	}
	events, _, ok := s.Registry.Snapshot("n1")
	if !ok || len(events) != 1 {
		t.Fatalf("event not recorded: %v %v", ok, events)
	}
	if events[0].Harness != "unknown" || events[0].TS.IsZero() {
		t.Fatalf("a neutral event without a harness or a stamp gets both filled in, got %+v", events[0])
	}
	if s.Metrics.Get("shoulder_events_total") != 1 {
		t.Fatal("the event was not counted")
	}
}

// An adapter that names the agent has said where the event came from, whether
// or not it also sent origin: without it the agent's turn end would count as
// the user's and its prompt would read as the user's words.
func TestANeutralEventWithAnAgentIdComesFromTheAgent(t *testing.T) {
	s, box := newTestServer(t)
	h := s.Handler()

	postNeutral(h, `{"session_id":"n3","event":"user_prompt","prompt":"find the leak","agent_id":"agent-1","agent_type":"explore"}`)
	postNeutral(h, `{"session_id":"n3","event":"turn_end","assistant":"found it","agent_id":"agent-1","agent_type":"explore"}`)
	events, turn, ok := s.Registry.Snapshot("n3")
	if !ok || len(events) != 2 {
		t.Fatalf("got %+v", events)
	}
	for _, ev := range events {
		if ev.Origin != session.OriginAgent {
			t.Fatalf("an event naming an agent was recorded as the user's: %+v", ev)
		}
	}
	if turn != 0 {
		t.Fatalf("an agent's turn end advanced the user's turn to %d", turn)
	}

	box.Push(session.Advice{ID: "a1", SessionID: "n3", Kind: session.AdviceNote, Level: session.LevelAction, Text: "for agent-1", AgentID: "agent-1", TTLTurns: 5})
	if rec := postNeutral(h, `{"session_id":"n3","event":"tool_call","tool_name":"read"}`); strings.Contains(rec.Body.String(), "for agent-1") {
		t.Fatal("the main thread was handed a subagent's note")
	}
	rec := postNeutral(h, `{"session_id":"n3","event":"tool_call","tool_name":"read","agent_id":"agent-1","agent_type":"explore"}`)
	if !strings.Contains(rec.Body.String(), "for agent-1") {
		t.Fatalf("the agent did not get its note: %s", rec.Body.String())
	}
}

func TestANeutralEventThatIsNotOneIsCountedAndAnsweredAnyway(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	for _, body := range []string{`{not json`, `{"event":"user_prompt"}`} {
		rec := postNeutral(h, body)
		if rec.Code != http.StatusOK || rec.Body.String() != string(noAdviceJSON) {
			t.Fatalf("%s: a bad event must still get the well-formed empty answer, got %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if got := s.Metrics.Get("shoulder_malformed_total"); got != 2 {
		t.Fatalf("shoulder_malformed_total = %d, want 2", got)
	}
}

func TestANeutralPromptCollectsPendingAdvice(t *testing.T) {
	s, box := newTestServer(t)
	h := s.Handler()
	postNeutral(h, `{"session_id":"n2","event":"user_prompt","prompt":"first"}`)

	box.Push(session.Advice{ID: "a1", SessionID: "n2", Kind: session.AdviceNote, Level: session.LevelPlan, Text: "the branch is master", CreatedTurn: 1, TTLTurns: 5})

	rec := postNeutral(h, `{"session_id":"n2","event":"user_prompt","prompt":"rebase onto main"}`)
	var out struct {
		Advice session.Advice `json:"advice"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Advice.Text != "the branch is master" {
		t.Fatalf("advice not delivered on the next prompt: %s", rec.Body.String())
	}
	if s.Metrics.Get("shoulder_advice_emitted_total") != 1 {
		t.Fatal("delivered advice was not counted as emitted")
	}
}

func TestSessionsAndNeutralEventsHonourTheToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.Token = "secret"
	h := s.Handler()

	rec := postNeutral(h, `{"session_id":"n3","event":"user_prompt"}`)
	if rec.Code != http.StatusOK || rec.Body.String() != string(silentJSON) {
		t.Fatalf("a rejected hook must still get a well-formed empty answer, got %d %s", rec.Code, rec.Body.String())
	}
	if _, _, ok := s.Registry.Snapshot("n3"); ok {
		t.Fatal("a rejected event was recorded")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if s.Metrics.Get("shoulder_unauthorised_total") != 2 {
		t.Fatalf("unauthorised = %d, want 2", s.Metrics.Get("shoulder_unauthorised_total"))
	}

	postNeutral(h, `{"session_id":"n3","event":"user_prompt"}`, "secret")
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("X-Shoulder-Token", "secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "n3") {
		t.Fatalf("/v1/sessions with the token = %d %s", rec.Code, rec.Body.String())
	}
}

func TestHealthAndMetricsNeedNoToken(t *testing.T) {
	s, _ := newTestServer(t)
	s.Token = "secret"
	h := s.Handler()
	postNeutral(h, `{"session_id":"m1","event":"user_prompt"}`, "secret")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Body.String() != `{"ok":true}` {
		t.Fatalf("healthz = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), "shoulder_events_total 1") {
		t.Fatalf("metrics scrape = %q %s", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestSuppressLabelKeepsTheReasonNotTheNumber(t *testing.T) {
	for in, want := range map[string]string{"turn_gap:6": "turn_gap", "session_cap:4000": "session_cap", "expired": "expired"} {
		if got := suppressLabel(in); got != want {
			t.Errorf("suppressLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteJSONFallsBackToSilenceWhenTheValueCannotBeEncoded(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]any{"bad": make(chan int)})
	if rec.Body.String() != string(silentJSON) {
		t.Fatalf("an unencodable reply must become the silent answer, got %s", rec.Body.String())
	}
}

// The daemon generates its own token, and the editor that started it read its
// environment before that value existed. Enforcing immediately would turn away
// every hook of the session that started the daemon, silently, which is the
// failure this whole mechanism exists to remove.
func TestAGeneratedTokenIsNotEnforcedUntilTheHarnessHasIt(t *testing.T) {
	s, _ := newTestServer(t)
	s.Token = "generated"
	s.Adopting = true
	h := s.Handler()

	if rec := postNeutral(h, `{"session_id":"a1","event":"user_prompt"}`); rec.Code != http.StatusOK {
		t.Fatalf("a hook with no token was turned away during adoption: %d", rec.Code)
	}
	if _, _, ok := s.Registry.Snapshot("a1"); !ok {
		t.Fatal("the event was not observed")
	}

	// One correct header proves the harness has been restarted and holds the
	// value; from here the window is closed for good.
	if rec := postNeutral(h, `{"session_id":"a2","event":"user_prompt"}`, "generated"); rec.Code != http.StatusOK {
		t.Fatalf("a correct token was rejected: %d", rec.Code)
	}
	before := s.Metrics.Get("shoulder_unauthorised_total")
	postNeutral(h, `{"session_id":"a3","event":"user_prompt"}`)
	if s.Metrics.Get("shoulder_unauthorised_total") != before+1 {
		t.Error("a hook with no token was still accepted after the harness proved it has the token")
	}
}

// A token somebody set by hand is not being adopted, and a wrong one is wrong
// from the first request.
func TestATokenGivenToTheDaemonIsEnforcedImmediately(t *testing.T) {
	s, _ := newTestServer(t)
	s.Token = "chosen-by-hand"
	h := s.Handler()
	postNeutral(h, `{"session_id":"b1","event":"user_prompt"}`)
	if s.Metrics.Get("shoulder_unauthorised_total") != 1 {
		t.Error("a hook with no token was accepted against a token the operator set")
	}
}

// exchange posts a prompt and the end of its answer, and returns what the
// prompt was answered with.
func exchange(h http.Handler, sid string) string {
	body := postNeutral(h, `{"session_id":"`+sid+`","event":"user_prompt","prompt":"go on"}`).Body.String()
	postNeutral(h, `{"session_id":"`+sid+`","event":"turn_end","assistant":"done"}`)
	return body
}

// The gate counts prompts and answer ends alike, and its default gap is
// sized so that a note is followed by two prompts without one.
func TestTheDefaultGapKeepsTwoPromptsFreeOfNotesAfterOne(t *testing.T) {
	s, box := newTestServer(t)
	h := s.Handler()
	queue := func(id string) {
		box.Push(session.Advice{
			ID: id, SessionID: "g1", Kind: session.AdviceNote, Level: session.LevelPlan,
			Text: id, CreatedTurn: s.Registry.Turn("g1"), TTLTurns: 4,
		})
	}

	exchange(h, "g1")
	queue("first")
	if got := exchange(h, "g1"); !strings.Contains(got, "first") {
		t.Fatalf("the note was not delivered at the prompt after it was written: %s", got)
	}
	for _, id := range []string{"second", "third"} {
		queue(id)
		if got := exchange(h, "g1"); got != string(noAdviceJSON) {
			t.Fatalf("%q was delivered inside the gap: %s", id, got)
		}
	}
	if n := s.Metrics.Get("shoulder_advice_suppressed_turn_gap_total"); n != 2 {
		t.Fatalf("%d notes counted as suppressed by the gap, want 2", n)
	}
	queue("fourth")
	if got := exchange(h, "g1"); !strings.Contains(got, "fourth") {
		t.Fatalf("the note at the third prompt after the last one was not delivered: %s", got)
	}
}

// A note written at a prompt, with the lifetime the pipeline gives it, is
// still current at the second prompt after that one and stale at the third.
func TestANoteOutlivesTwoFurtherPromptsAndNoMore(t *testing.T) {
	for _, tc := range []struct {
		sid       string
		exchanges int
		delivered bool
	}{
		{"t1", 2, true},
		{"t2", 3, false},
	} {
		s, box := newTestServer(t)
		h := s.Handler()
		postNeutral(h, `{"session_id":"`+tc.sid+`","event":"user_prompt","prompt":"start"}`)
		box.Push(session.Advice{
			ID: "a", SessionID: tc.sid, Kind: session.AdviceNote, Level: session.LevelAction,
			Text: "mind the lock", CreatedTurn: s.Registry.Turn(tc.sid), TTLTurns: 4,
		})
		postNeutral(h, `{"session_id":"`+tc.sid+`","event":"turn_end","assistant":"done"}`)
		for i := 1; i < tc.exchanges; i++ {
			exchange(h, tc.sid)
		}
		postNeutral(h, `{"session_id":"`+tc.sid+`","event":"user_prompt","prompt":"go on"}`)
		got := postNeutral(h, `{"session_id":"`+tc.sid+`","event":"tool_call","tool_name":"Bash"}`).Body.String()
		if strings.Contains(got, "mind the lock") != tc.delivered {
			t.Fatalf("%d prompts after the note was written, delivered = %v: %s", tc.exchanges, !tc.delivered, got)
		}
	}
}
