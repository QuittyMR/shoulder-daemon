package cliapi

import (
	"context"
	"encoding/json"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/metrics"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/pipeline"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// fakeMemory records what the handlers asked of the store, so a test can assert
// on the scope a request was actually filed or read under.
type fakeMemory struct {
	mu         sync.Mutex
	held       map[scope.Scope][]memory.Record
	stored     []memory.Record
	superseded []string
	queries    []memory.Query
	forgotten  []string
	storeErr   error
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{held: map[scope.Scope][]memory.Record{}}
}

func (f *fakeMemory) Name() string { return "fake" }

func (f *fakeMemory) Search(_ context.Context, q memory.Query) ([]memory.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	return f.held[q.Scope], nil
}

// List filters by project the way a real backend must: a local read of one
// project may never see another one's records.
func (f *fakeMemory) List(_ context.Context, q memory.Query) ([]memory.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	var out []memory.Record
	for _, r := range f.held[q.Scope] {
		if q.Scope == scope.Local && r.Project != q.Project {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeMemory) Store(_ context.Context, r memory.Record) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.storeErr != nil {
		return "", f.storeErr
	}
	f.stored = append(f.stored, r)
	return "mem_1", nil
}

func (f *fakeMemory) Supersede(_ context.Context, old string, r memory.Record) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.superseded = append(f.superseded, old)
	f.stored = append(f.stored, r)
	return "mem_2", nil
}

func (f *fakeMemory) Forget(_ context.Context, id string, _ memory.Query) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeMemory) Health(context.Context) error { return nil }

func (f *fakeMemory) writes() []memory.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]memory.Record(nil), f.stored...)
}

func (f *fakeMemory) asked() []memory.Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]memory.Query(nil), f.queries...)
}

// fakeLLM answers by which job it was given: the two CLI prompts want prose,
// the decision prompt wants the extraction JSON.
type fakeLLM struct {
	prose    string
	decision string
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) Complete(_ context.Context, system, _ string) (string, error) {
	if system == prompts.Decision {
		return f.decision, nil
	}
	return f.prose, nil
}

// Chat exists so the fake still satisfies Provider. The CLI paths ask one
// question and want one answer, so they never reach it; a fake that pretended
// to run a tool loop would be modelling something these tests do not exercise.
func (f *fakeLLM) Chat(_ context.Context, msgs []llm.Message, _ []llm.Tool) (llm.Message, error) {
	for _, m := range msgs {
		if m.Role == "system" && m.Content == prompts.Decision {
			return llm.Message{Role: "assistant", Content: f.decision}, nil
		}
	}
	return llm.Message{Role: "assistant", Content: f.prose}, nil
}

func newTestServer(t *testing.T, token string, model llm.Provider) (http.Handler, *fakeMemory, *metrics.Metrics) {
	t.Helper()
	mem := newFakeMemory()
	m := metrics.New()
	cfg := config.Load()
	cfg.Budget = budget.Default()

	pipe := &pipeline.Pipeline{
		Cfg:     cfg,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: m,
		LLM:     model,
		// Wrapped exactly as main.go wraps it. The scope rules live in Checked,
		// so a test holding a bare connector would be testing a configuration
		// that never ships.
		Memory: memory.Checked(mem),
	}
	mux := http.NewServeMux()
	New(pipe, token).Mount(mux)
	return mux, mem, m
}

func do(t *testing.T, h http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("reply %q is not JSON: %v", rec.Body.String(), err)
	}
	return v
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decode[struct {
		Error string `json:"error"`
	}](t, rec).Error
}

func TestMessageAnswersFromBothScopesAndRecordsWhatItLearns(t *testing.T) {
	model := &fakeLLM{
		prose:    "main branch is master",
		decision: `{"inject":"","facts":[{"content":"the main branch is master","category":"structure","scope":"global"}]}`,
	}
	h, mem, _ := newTestServer(t, "", model)
	mem.held[scope.Local] = []memory.Record{{ID: "a", Content: "branch is master", Scope: scope.Local, Project: "/p"}}

	rec := do(t, h, http.MethodPost, "/v1/cli/message",
		`{"text":"this is my git repository","scope":"local","project":"/p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[MessageResponse](t, rec)
	if got.Reply != "main branch is master" {
		t.Fatalf("reply = %q", got.Reply)
	}

	var scopes []scope.Scope
	for _, q := range mem.asked() {
		scopes = append(scopes, q.Scope)
	}
	if len(scopes) != 2 || scopes[0] != scope.Local || scopes[1] != scope.Global {
		t.Fatalf("a local question must read local then global, read %v", scopes)
	}

	// The command line said local; the model said the fact is about the user.
	// The fact's own scope decides where it is filed, and a global one carries
	// no project out of the directory the command happened to run in.
	writes := mem.writes()
	if len(writes) != 1 || writes[0].Scope != scope.Global || writes[0].Project != "" {
		t.Fatalf("stored %+v", writes)
	}
	if len(got.Facts) != 1 {
		t.Fatalf("the reply must say what it recorded, got %+v", got.Facts)
	}
}

func TestMessageWithNoUpdateWritesNothing(t *testing.T) {
	model := &fakeLLM{
		prose:    "nothing is stored about that",
		decision: `{"inject":"","facts":[{"content":"something","category":"structure","scope":"local"}]}`,
	}
	h, mem, _ := newTestServer(t, "", model)

	rec := do(t, h, http.MethodPost, "/v1/cli/message",
		`{"text":"anything?","scope":"global","update":"never"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if w := mem.writes(); len(w) != 0 {
		t.Fatalf("update=never wrote %+v", w)
	}
	for _, q := range mem.asked() {
		if q.Scope == scope.Local {
			t.Fatal("a global question must not read one project's private knowledge")
		}
	}
}

func TestUnscopedRequestsNameTheFlag(t *testing.T) {
	h, mem, m := newTestServer(t, "", &fakeLLM{prose: "unreachable"})

	cases := []struct{ name, method, path, body string }{
		{"message", http.MethodPost, "/v1/cli/message", `{"text":"hello"}`},
		{"fact add", http.MethodPost, "/v1/cli/facts", `{"content":"a fact"}`},
		{"fact update", http.MethodPatch, "/v1/cli/facts", `{"id":"x","content":"a fact"}`},
		{"fact list", http.MethodGet, "/v1/cli/facts", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, c.method, c.path, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if msg := errorOf(t, rec); !strings.Contains(msg, "--local or --global") {
				t.Fatalf("error %q does not name the flag the user should have passed", msg)
			}
		})
	}
	if len(mem.writes()) != 0 || len(mem.asked()) != 0 {
		t.Fatal("an unscoped request reached the store")
	}
	if m.Get("shoulder_cli_bad_request_total") != uint64(len(cases)) {
		t.Fatalf("rejections counted %d", m.Get("shoulder_cli_bad_request_total"))
	}
}

func TestLocalRequestWithoutAProjectIsRefused(t *testing.T) {
	h, _, _ := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodPost, "/v1/cli/facts", `{"content":"a fact","scope":"local"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "project") {
		t.Fatalf("error %q does not say what is missing", msg)
	}
}

func TestFactAddStoresVerbatim(t *testing.T) {
	h, mem, m := newTestServer(t, "", &fakeLLM{})

	rec := do(t, h, http.MethodPost, "/v1/cli/facts",
		`{"content":"deploys go to staging first","category":"constraint","tags":["deploy"],"scope":"local","project":"/p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if id := decode[FactResponse](t, rec).ID; id != "mem_1" {
		t.Fatalf("id = %q", id)
	}
	writes := mem.writes()
	if len(writes) != 1 {
		t.Fatalf("stored %+v", writes)
	}
	got := writes[0]
	if got.Content != "deploys go to staging first" || got.Category != "constraint" ||
		got.Scope != scope.Local || got.Project != "/p" || len(got.Tags) != 1 {
		t.Fatalf("stored %+v", got)
	}
	if m.Get("shoulder_cli_fact_stored_total") != 1 {
		t.Fatal("the write was not counted")
	}
}

func TestGlobalFactCarriesNoProject(t *testing.T) {
	h, mem, _ := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodPost, "/v1/cli/facts",
		`{"content":"prefers terse answers","scope":"global","project":"/p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := mem.writes()[0]; got.Project != "" {
		t.Fatalf("a global fact was filed under project %q", got.Project)
	}
}

func TestFactAddRejectsAnUnknownCategory(t *testing.T) {
	h, mem, _ := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodPost, "/v1/cli/facts",
		`{"content":"a fact","category":"observation","scope":"global"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "constraint") {
		t.Fatalf("error %q does not list the categories", msg)
	}
	if len(mem.writes()) != 0 {
		t.Fatal("the fact was stored with a category the backend would rewrite")
	}
}

func TestFactUpdateSupersedes(t *testing.T) {
	h, mem, m := newTestServer(t, "", &fakeLLM{})
	mem.held[scope.Global] = []memory.Record{{ID: "old", Content: "the main branch is main", Scope: scope.Global}}

	rec := do(t, h, http.MethodPatch, "/v1/cli/facts",
		`{"id":"old","content":"the main branch is master","scope":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(mem.superseded) != 1 || mem.superseded[0] != "old" {
		t.Fatalf("superseded %v", mem.superseded)
	}
	if m.Get("shoulder_cli_fact_superseded_total") != 1 {
		t.Fatal("the supersede was not counted")
	}
}

func TestFactUpdateNeedsAnID(t *testing.T) {
	h, _, _ := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodPatch, "/v1/cli/facts", `{"content":"a fact","scope":"global"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "--id") {
		t.Fatalf("error %q does not name the flag", msg)
	}
}

func TestFactListReadsOneScope(t *testing.T) {
	h, mem, _ := newTestServer(t, "", &fakeLLM{})
	mem.held[scope.Local] = []memory.Record{{ID: "a", Content: "one", Scope: scope.Local, Project: "/p"}}
	mem.held[scope.Global] = []memory.Record{{ID: "b", Content: "two", Scope: scope.Global}}

	rec := do(t, h, http.MethodGet, "/v1/cli/facts?scope=local&project=/p&limit=3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[FactsResponse](t, rec).Facts
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("facts = %+v", got)
	}
	if q := mem.asked()[0]; q.Limit != 3 || q.Project != "/p" {
		t.Fatalf("query = %+v", q)
	}
}

func TestFactListRejectsANonsenseLimit(t *testing.T) {
	h, _, _ := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodGet, "/v1/cli/facts?scope=global&limit=none", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestDigestWithNoScopeCoversBoth(t *testing.T) {
	h, mem, _ := newTestServer(t, "", &fakeLLM{prose: "You mostly work on one Go relay."})
	mem.held[scope.Local] = []memory.Record{{ID: "a", Content: "one", Scope: scope.Local, Project: "/p"}}
	mem.held[scope.Global] = []memory.Record{{ID: "b", Content: "two", Scope: scope.Global}}

	rec := do(t, h, http.MethodPost, "/v1/cli/digest", `{"project":"/p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[DigestResponse](t, rec).Digest; got != "You mostly work on one Go relay." {
		t.Fatalf("digest = %q", got)
	}
	if len(mem.asked()) != 2 {
		t.Fatalf("a bare digest must read both scopes, read %+v", mem.asked())
	}
}

func TestDigestOfAnEmptyStoreSaysSoWithoutAModel(t *testing.T) {
	h, _, _ := newTestServer(t, "", &fakeLLM{prose: "the model must not be asked"})
	rec := do(t, h, http.MethodPost, "/v1/cli/digest", `{"scope":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := decode[DigestResponse](t, rec).Digest; !strings.Contains(got, "Nothing is recorded") {
		t.Fatalf("digest = %q", got)
	}
}

func TestTokenIsRequiredWhenConfigured(t *testing.T) {
	h, mem, m := newTestServer(t, "s3cret", &fakeLLM{prose: "unreachable"})

	rec := do(t, h, http.MethodPost, "/v1/cli/message", `{"text":"hello","scope":"global"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "SHOULDER_TOKEN") {
		t.Fatalf("error %q does not say how to authenticate", msg)
	}
	if len(mem.asked()) != 0 {
		t.Fatal("an unauthenticated request reached the store")
	}
	if m.Get("shoulder_cli_unauthorised_total") != 1 {
		t.Fatal("the rejection was not counted")
	}

	ok := do(t, h, http.MethodPost, "/v1/cli/message", `{"text":"hello","scope":"global"}`,
		"X-Shoulder-Token", "s3cret")
	if ok.Code != http.StatusOK {
		t.Fatalf("status %d with the right token: %s", ok.Code, ok.Body.String())
	}
}

func TestWrongMethodIsRefused(t *testing.T) {
	h, _, _ := newTestServer(t, "", &fakeLLM{})
	for _, path := range []string{"/v1/cli/message", "/v1/cli/digest"} {
		if rec := do(t, h, http.MethodGet, path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", path, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodDelete, "/v1/cli/facts", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1/cli/facts = %d, want 405", rec.Code)
	}
}

func TestFactUpdateRefusesAnIDFromAnotherScope(t *testing.T) {
	cases := []struct {
		name string
		held map[scope.Scope][]memory.Record
		body string
		want string
	}{
		{
			name: "a global preference retagged as one project's",
			held: map[scope.Scope][]memory.Record{scope.Global: {{ID: "g1", Content: "prefers terse answers", Scope: scope.Global}}},
			body: `{"id":"g1","content":"prefers terse answers","scope":"local","project":"/p"}`,
			want: "not in local scope",
		},
		{
			name: "one project's detail published everywhere",
			held: map[scope.Scope][]memory.Record{scope.Local: {{ID: "l1", Content: "the tests need docker", Scope: scope.Local, Project: "/p"}}},
			body: `{"id":"l1","content":"the tests need docker","scope":"global"}`,
			want: "not in global scope",
		},
		{
			name: "another project's record corrected from here",
			held: map[scope.Scope][]memory.Record{scope.Local: {{ID: "a1", Content: "the tests need docker", Scope: scope.Local, Project: "/a"}}},
			body: `{"id":"a1","content":"the tests need postgres","scope":"local","project":"/b"}`,
			want: "not in local scope",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, mem, m := newTestServer(t, "", &fakeLLM{})
			mem.held = c.held

			rec := do(t, h, http.MethodPatch, "/v1/cli/facts", c.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404: %s", rec.Code, rec.Body.String())
			}
			if msg := errorOf(t, rec); !strings.Contains(msg, c.want) {
				t.Fatalf("error %q does not say the fact is in another scope", msg)
			}
			if len(mem.superseded) != 0 {
				t.Fatalf("a record in another scope was superseded: %v", mem.superseded)
			}
			if len(mem.writes()) != 0 {
				t.Fatal("the replacement was written anyway")
			}
			if m.Get("shoulder_cli_fact_wrong_scope_total") != 1 {
				t.Fatal("the refusal was not counted")
			}
		})
	}
}

func TestFactUpdateReadsTheScopeItWasGiven(t *testing.T) {
	h, mem, _ := newTestServer(t, "", &fakeLLM{})
	mem.held[scope.Local] = []memory.Record{{ID: "l1", Content: "the tests need docker", Scope: scope.Local, Project: "/p"}}

	rec := do(t, h, http.MethodPatch, "/v1/cli/facts",
		`{"id":"l1","content":"the tests need postgres","scope":"local","project":"/p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	q := mem.asked()[0]
	if q.Scope != scope.Local || q.Project != "/p" {
		t.Fatalf("the id was looked for in %+v, not the scope the caller claimed", q)
	}
	if len(mem.superseded) != 1 || mem.superseded[0] != "l1" {
		t.Fatalf("superseded %v", mem.superseded)
	}
}

func TestAWriteWithNowhereToGoIsRefused(t *testing.T) {
	h, mem, m := newTestServer(t, "", &fakeLLM{})
	mem.storeErr = memory.ErrNoBackend

	rec := do(t, h, http.MethodPost, "/v1/cli/facts", `{"content":"a fact","scope":"global"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: a write that went nowhere must not read as success", rec.Code)
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "SHOULDER_MEMORY_URL") {
		t.Fatalf("error %q does not name the variable that fixes it", msg)
	}
	if m.Get("shoulder_cli_fact_nowhere_total") != 1 {
		t.Fatal("the lost write was not counted")
	}
}

func TestAFactAlreadyStoredVerbatimIsAnAnswer(t *testing.T) {
	h, mem, m := newTestServer(t, "", &fakeLLM{})
	mem.storeErr = memory.ErrDuplicateExact

	rec := do(t, h, http.MethodPost, "/v1/cli/facts", `{"content":"a fact","scope":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: the store already holds what was asked for", rec.Code)
	}
	got := decode[FactResponse](t, rec)
	if !got.AlreadyKnown {
		t.Fatalf("reply %+v does not say the fact was already known", got)
	}
	if m.Get("shoulder_cli_fact_duplicate_total") != 1 {
		t.Fatal("the duplicate was not counted")
	}
}

func TestARequestWithNoModelNamesTheVariable(t *testing.T) {
	h, mem, _ := newTestServer(t, "", nil)

	for _, c := range []struct{ path, body string }{
		{"/v1/cli/message", `{"text":"hello","scope":"global"}`},
		{"/v1/cli/digest", `{"scope":"global"}`},
	} {
		t.Run(c.path, func(t *testing.T) {
			rec := do(t, h, http.MethodPost, c.path, c.body)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
			}
			msg := errorOf(t, rec)
			if !strings.Contains(msg, "SHOULDER_LLM") {
				t.Fatalf("error %q drops the hint the daemon prints at startup", msg)
			}
			if !strings.Contains(msg, "openai") {
				t.Fatalf("error %q names no preset to choose from", msg)
			}
		})
	}
	if len(mem.asked()) != 0 {
		t.Fatal("a request that cannot be answered still read the store")
	}
}
