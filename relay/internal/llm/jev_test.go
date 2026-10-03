package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

// jevCall is one request as the server saw it.
type jevCall struct {
	path, auth string
	raw        []byte
	body       struct {
		State struct {
			RecentEvents string    `json:"recent_events"`
			StoredFacts  []jevFact `json:"stored_facts"`
		} `json:"state"`
		Model     string                 `json:"model"`
		Questions map[string]jevQuestion `json:"questions"`
	}
}

// jevServer answers every request with status and body and keeps what it was
// sent.
func jevServer(t *testing.T, status int, body string) (*Jev, func() []jevCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []jevCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := jevCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), raw: raw}
		if err := json.Unmarshal(raw, &c.body); err != nil {
			t.Errorf("unparseable request: %v", err)
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	j := &Jev{BaseURL: srv.URL + "/", APIKey: "k-test", Model: "jev-test", Threshold: 0.6, HTTP: srv.Client()}
	return j, func() []jevCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]jevCall(nil), calls...)
	}
}

func answers(t *testing.T, a map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "jev-test", "answers": a, "usage": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func choice(option string, confidence float64) map[string]any {
	return map[string]any{
		"type": "choice", "choice": option, "confidence": confidence,
		"probabilities": map[string]float64{option: confidence},
	}
}

var twoFacts = []memory.Record{
	{ID: "f1", Scope: scope.Global, Category: "preference", Content: "Use pnpm, never npm."},
	{ID: "f2", Scope: scope.Local, Category: "convention", Content: "Branches are named feature/<ticket>."},
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	in := map[string]bool{}
	for _, g := range got {
		in[g] = true
	}
	for _, w := range want {
		if !in[w] {
			return false
		}
	}
	return true
}

func TestJevAsksBothQuestionsInOneCall(t *testing.T) {
	j, calls := jevServer(t, http.StatusOK, answers(t, map[string]any{
		"action": choice("inject", 0.9),
		"target": choice("f2", 0.7),
	}))

	v, err := j.Triage(context.Background(), "user: push it to main & <deploy>", twoFacts)
	if err != nil {
		t.Fatal(err)
	}
	if v.Action != Inject || v.FactID != "f2" {
		t.Fatalf("verdict = %+v", v)
	}
	// The less sure half bounds the whole: the right action on the wrong fact
	// is a wrong answer.
	if v.Confidence != 0.7 {
		t.Fatalf("confidence = %v, want the lower of the two", v.Confidence)
	}

	cs := calls()
	if len(cs) != 1 {
		t.Fatalf("%d calls, want one for both questions", len(cs))
	}
	c := cs[0]
	if c.path != "/v1/systemone" {
		t.Errorf("path = %q", c.path)
	}
	if c.auth != "Bearer k-test" {
		t.Errorf("Authorization = %q", c.auth)
	}
	if c.body.Model != "jev-test" {
		t.Errorf("model = %q", c.body.Model)
	}
	if c.body.State.RecentEvents != "user: push it to main & <deploy>" {
		t.Errorf("recent_events = %q", c.body.State.RecentEvents)
	}
	if strings.Contains(string(c.raw), `\u003c`) || strings.Contains(string(c.raw), `\u0026`) {
		t.Errorf("angle brackets were escaped, spending the window: %s", c.raw)
	}
	if len(c.body.State.StoredFacts) != 2 || c.body.State.StoredFacts[1] != (jevFact{
		ID: "f2", Scope: "local", Category: "convention", Content: "Branches are named feature/<ticket>.",
	}) {
		t.Errorf("stored facts = %+v", c.body.State.StoredFacts)
	}
	for name, q := range c.body.Questions {
		if q.Type != "choice" || q.Instructions == "" {
			t.Errorf("question %q = %+v; a choice without instructions is refused", name, q)
		}
		for opt, desc := range q.Criteria {
			if desc == "" {
				t.Errorf("question %q option %q has no description", name, opt)
			}
		}
	}
	if got := keys(c.body.Questions["action"].Criteria); !sameSet(got, "nothing", "create", "update", "inject") {
		t.Errorf("action options = %v", got)
	}
	if got := keys(c.body.Questions["target"].Criteria); !sameSet(got, "f1", "f2") {
		t.Errorf("target options = %v", got)
	}
}

// Update and inject are about a stored fact. With none to name, offering them
// invites an answer nothing can act on.
func TestJevOffersNoFactActionsWhenNothingWasRecalled(t *testing.T) {
	unnamed := []memory.Record{{Scope: scope.Global, Content: "a record the backend gave no id"}}
	for name, recalled := range map[string][]memory.Record{"none": nil, "no ids": unnamed} {
		t.Run(name, func(t *testing.T) {
			j, calls := jevServer(t, http.StatusOK, answers(t, map[string]any{"action": choice("nothing", 0.95)}))
			v, err := j.Triage(context.Background(), "user: hi", recalled)
			if err != nil {
				t.Fatal(err)
			}
			if v != (Verdict{Action: Nothing, Confidence: 0.95}) {
				t.Fatalf("verdict = %+v", v)
			}
			q := calls()[0].body.Questions
			if got := keys(q["action"].Criteria); !sameSet(got, "nothing", "create") {
				t.Fatalf("action options = %v", got)
			}
			if _, ok := q["target"]; ok {
				t.Fatal("a target question was asked with nothing to target")
			}
		})
	}
}

// One recalled fact is the target by elimination; asking would be a choice
// with a single option, which the API refuses.
func TestJevTargetsTheOnlyRecalledFactWithoutAsking(t *testing.T) {
	j, calls := jevServer(t, http.StatusOK, answers(t, map[string]any{"action": choice("update", 0.8)}))
	v, err := j.Triage(context.Background(), "user: we use yarn now", twoFacts[:1])
	if err != nil {
		t.Fatal(err)
	}
	if v != (Verdict{Action: Update, Confidence: 0.8, FactID: "f1"}) {
		t.Fatalf("verdict = %+v", v)
	}
	if _, ok := calls()[0].body.Questions["target"]; ok {
		t.Fatal("a target question was asked with one fact")
	}
}

// The target answer is ignored for actions that are not about a stored fact,
// so an odd one there must not make a usable verdict an error.
func TestJevIgnoresTheTargetForActionsWithoutOne(t *testing.T) {
	j, _ := jevServer(t, http.StatusOK, answers(t, map[string]any{
		"action": choice("create", 0.9),
		"target": choice("f1", 0.1),
	}))
	v, err := j.Triage(context.Background(), "user: we deploy on fridays", twoFacts)
	if err != nil {
		t.Fatal(err)
	}
	if v != (Verdict{Action: Create, Confidence: 0.9}) {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestJevRefusesAnswersItCannotTrust(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		recalled []memory.Record
		want     string
	}{
		{"server error", http.StatusUnprocessableEntity, `{"detail":"instructions required"}`, twoFacts, "422"},
		{"not JSON", http.StatusOK, `<html>`, twoFacts, "unparseable"},
		{"no action", http.StatusOK, answers(t, map[string]any{}), twoFacts, `"action"`},
		{"action not offered", http.StatusOK, answers(t, map[string]any{"action": choice("inject", 0.9)}), nil, "not offered"},
		{"unknown action", http.StatusOK, answers(t, map[string]any{"action": choice("delete", 0.9)}), twoFacts, "not offered"},
		{"wrong type", http.StatusOK, answers(t, map[string]any{"action": map[string]any{"type": "score", "choice": "nothing", "confidence": 0.9}}), twoFacts, "type"},
		{"no confidence", http.StatusOK, answers(t, map[string]any{"action": map[string]any{"type": "choice", "choice": "nothing"}}), twoFacts, "confidence"},
		{"confidence out of range", http.StatusOK, answers(t, map[string]any{"action": choice("nothing", 1.5)}), twoFacts, "confidence"},
		{"no target", http.StatusOK, answers(t, map[string]any{"action": choice("update", 0.9)}), twoFacts, `"target"`},
		{"target not offered", http.StatusOK, answers(t, map[string]any{"action": choice("inject", 0.9), "target": choice("f9", 0.9)}), twoFacts, "not offered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j, _ := jevServer(t, c.status, c.body)
			v, err := j.Triage(context.Background(), "user: hi", c.recalled)
			if err == nil {
				t.Fatalf("accepted %s as %+v", c.body, v)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not say %q", err, c.want)
			}
		})
	}
}

// The event window is what gives way to Jev's limit, and from the front: its end is
// what was just said.
func TestJevStateStaysInsideTheWindow(t *testing.T) {
	j, calls := jevServer(t, http.StatusOK, answers(t, map[string]any{"action": choice("nothing", 0.9)}))
	long := []memory.Record{{ID: "f1", Scope: scope.Global, Content: strings.Repeat("x", 10*maxJevFactBytes)}}
	window := "OLDEST" + strings.Repeat("\"é<\n", 40_000) + "NEWEST"

	if _, err := j.Triage(context.Background(), window, long); err != nil {
		t.Fatal(err)
	}
	c := calls()[0]
	b, err := encodeJSON(c.body.State)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxJevStateBytes {
		t.Fatalf("state is %d bytes, over %d", len(b), maxJevStateBytes)
	}
	if len(b) < maxJevStateBytes*9/10 {
		t.Fatalf("state is %d bytes: the event window was cut far past what the limit needs", len(b))
	}
	got := c.body.State.RecentEvents
	if !strings.HasPrefix(got, clippedMark) || !strings.HasSuffix(got, "NEWEST") || strings.Contains(got, "OLDEST") {
		t.Fatalf("the event window was not cut from the front: %d bytes, %q", len(got), textutil.Clip(got, 40))
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("the cut split a character")
	}
	if n := len(c.body.State.StoredFacts[0].Content); n > maxJevFactBytes+len(clippedMark) {
		t.Fatalf("one fact kept %d bytes", n)
	}
}

func clearJevEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SHOULDER_TRIAGE", "TYPESAFE_API_KEY", "TYPESAFE_BASE_URL", "SHOULDER_JEV_MODEL", "SHOULDER_JEV_MIN_CONFIDENCE"} {
		t.Setenv(k, "")
	}
}

func TestJevFromEnvIsOffUnlessAskedFor(t *testing.T) {
	clearJevEnv(t)
	t.Setenv("TYPESAFE_API_KEY", "k")
	j, err := JevFromEnv()
	if err != nil || j != nil {
		t.Fatalf("JevFromEnv() = %v, %v; want nothing and no error", j, err)
	}
}

func TestJevFromEnvDefaultsAndOverrides(t *testing.T) {
	clearJevEnv(t)
	t.Setenv("SHOULDER_TRIAGE", " Jev ")
	t.Setenv("TYPESAFE_API_KEY", "k")
	j, err := JevFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if *j != (Jev{BaseURL: DefaultJevBaseURL, APIKey: "k", Model: DefaultJevModel, Threshold: DefaultJevMinConfidence}) {
		t.Fatalf("defaults = %+v", *j)
	}

	t.Setenv("TYPESAFE_BASE_URL", "http://127.0.0.1:9")
	t.Setenv("SHOULDER_JEV_MODEL", "jev-2")
	t.Setenv("SHOULDER_JEV_MIN_CONFIDENCE", "0.85")
	j, err = JevFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if j.BaseURL != "http://127.0.0.1:9" || j.Model != "jev-2" || j.MinConfidence() != 0.85 {
		t.Fatalf("overrides = %+v", *j)
	}
}

func TestJevFromEnvRefusesWhatItCannotRun(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unknown triage", map[string]string{"SHOULDER_TRIAGE": "gpt", "TYPESAFE_API_KEY": "k"}, "jev"},
		{"no key", map[string]string{"SHOULDER_TRIAGE": "jev"}, "TYPESAFE_API_KEY"},
		{"not a number", map[string]string{"SHOULDER_TRIAGE": "jev", "TYPESAFE_API_KEY": "k", "SHOULDER_JEV_MIN_CONFIDENCE": "high"}, "SHOULDER_JEV_MIN_CONFIDENCE"},
		{"above one", map[string]string{"SHOULDER_TRIAGE": "jev", "TYPESAFE_API_KEY": "k", "SHOULDER_JEV_MIN_CONFIDENCE": "60"}, "SHOULDER_JEV_MIN_CONFIDENCE"},
		{"NaN", map[string]string{"SHOULDER_TRIAGE": "jev", "TYPESAFE_API_KEY": "k", "SHOULDER_JEV_MIN_CONFIDENCE": "NaN"}, "SHOULDER_JEV_MIN_CONFIDENCE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearJevEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			j, err := JevFromEnv()
			if err == nil {
				t.Fatalf("built %+v", j)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not name %q", err, c.want)
			}
		})
	}
}

// A cut that lands inside a character would reach Jev as a replacement
// character: in the state, where a fact is clipped, and in a target option.
func TestJevCutsFactsAtACharacter(t *testing.T) {
	j, calls := jevServer(t, http.StatusOK, answers(t, map[string]any{"action": choice("nothing", 0.9)}))
	odd := "x" + strings.Repeat("é", maxJevFactBytes)
	recalled := []memory.Record{
		{ID: "f1", Scope: scope.Global, Content: odd},
		{ID: "f2", Scope: scope.Global, Content: odd},
	}
	if _, err := j.Triage(context.Background(), "user: hi", recalled); err != nil {
		t.Fatal(err)
	}
	c := calls()[0]
	for _, f := range c.body.State.StoredFacts {
		if strings.ContainsRune(f.Content, utf8.RuneError) || !strings.HasSuffix(f.Content, clippedMark) {
			t.Fatalf("fact %s was cut inside a character: %q", f.ID, textutil.Clip(f.Content, 40))
		}
	}
	for id, desc := range c.body.Questions["target"].Criteria {
		if strings.ContainsRune(desc, utf8.RuneError) || !strings.HasSuffix(desc, clippedMark) {
			t.Fatalf("option %s was cut inside a character: %q", id, desc)
		}
	}
}
