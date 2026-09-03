package cliapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestConsolidateInsistsOnAPostWithAScope(t *testing.T) {
	h, _, m := newTestServer(t, "", &fakeLLM{})

	if rec := do(t, h, http.MethodGet, "/v1/cli/consolidate", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
	rec := do(t, h, http.MethodPost, "/v1/cli/consolidate", `{"scope":"sideways"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sideways") {
		t.Fatalf("bad scope = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/v1/cli/consolidate", `{"scope":"local"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "project") {
		t.Fatalf("local without a project = %d %s", rec.Code, rec.Body.String())
	}
	if got := m.Get("shoulder_cli_bad_request_total"); got != 2 {
		t.Fatalf("bad requests counted = %d, want 2", got)
	}
}

func TestAMalformedBodyIsRefusedBeforeAnythingIsRead(t *testing.T) {
	h, mem, m := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodPost, "/v1/cli/consolidate", `{"scope":`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "malformed JSON") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if m.Get("shoulder_cli_bad_request_total") != 1 {
		t.Fatal("a malformed body was not counted")
	}
	if len(mem.asked()) != 0 {
		t.Fatal("the store was consulted for a request that could not be parsed")
	}
}

func TestConsolidateNeedsAModelBecauseItRewrites(t *testing.T) {
	h, _, _ := newTestServer(t, "", nil)
	rec := do(t, h, http.MethodPost, "/v1/cli/consolidate", `{"scope":"global"}`)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "SHOULDER_LLM") {
		t.Fatalf("no model = %d %s; the refusal must say where to set the provider", rec.Code, rec.Body.String())
	}
}

func TestConsolidatingAnEmptyScopeChangesNothing(t *testing.T) {
	h, mem, _ := newTestServer(t, "", &fakeLLM{})
	rec := do(t, h, http.MethodPost, "/v1/cli/consolidate", `{"scope":"global"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	out := decode[ConsolidateResponse](t, rec)
	if out.Dropped != 0 || out.Merged != 0 {
		t.Fatalf("an empty scope reported %+v", out)
	}
	if len(mem.writes()) != 0 {
		t.Fatal("something was written while consolidating nothing")
	}
}
