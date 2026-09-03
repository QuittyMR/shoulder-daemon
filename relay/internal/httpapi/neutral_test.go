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
	for in, want := range map[string]string{"turn_gap:3": "turn_gap", "session_cap:4000": "session_cap", "expired": "expired"} {
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
