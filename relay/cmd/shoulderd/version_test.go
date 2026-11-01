package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewerComparesReleasesOnly(t *testing.T) {
	cases := []struct {
		candidate, current string
		want               bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.1", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v0.10.0", "v0.9.0", true},
		// A candidate is later than the release before it and earlier than the
		// release it is a candidate for; among candidates the number decides.
		{"v0.7.0-rc", "v0.6.3", true},
		{"v0.7.0", "v0.7.0-rc", true},
		{"v0.7.0-rc", "v0.7.0", false},
		{"v0.7.0-rc2", "v0.7.0-rc1", true},
		{"v0.7.0-rc2", "v0.7.0-rc", true},
		{"v0.7.0-rc", "v0.7.0-rc", false},
		{"v0.7.1", "v0.7.0-rc", true},
		// A pseudo-version, a devel build and garbage are never told to update:
		// there is nothing to compare them against.
		{"v0.2.0", "devel", false},
		{"v0.2.0", "v0.0.0-20260903131609-7e184aaace8d", false},
		{"v0.0.0-20260903131609-7e184aaace8d", "v0.1.0", false},
		{"latest", "v0.1.0", false},
	}
	for _, tc := range cases {
		if got := newer(tc.candidate, tc.current); got != tc.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.want)
		}
	}
}

func TestIsTagged(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.1.0":                             true,
		"v12.0.3":                            true,
		"0.1.0":                              true,
		"devel":                              false,
		"(devel)":                            false,
		"v0.0.0-20260903131609-7e184aaace8d": false,
		"v0.1":                               false,
		"v0.1.0-rc1":                         true,
		"v0.1.0-rc":                          true,
		"v0.1.0-rc.1":                        false,
		"v0.1.0-rc0":                         false,
		"v0.1.0-beta":                        false,
	} {
		if got := isTagged(v); got != want {
			t.Errorf("isTagged(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestLatestReleaseReadsTheProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Version": "v0.3.1", "Time": "2026-09-03T00:00:00Z"})
	}))
	defer srv.Close()
	old := latestURL
	latestURL = srv.URL
	defer func() { latestURL = old }()

	got, err := latestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v0.3.1" {
		t.Fatalf("latest = %q, want v0.3.1", got)
	}
}

func TestLatestReleaseRefusesAPseudoVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Version": "v0.0.0-20260903131609-7e184aaace8d"})
	}))
	defer srv.Close()
	old := latestURL
	latestURL = srv.URL
	defer func() { latestURL = old }()

	if _, err := latestRelease(context.Background()); err == nil {
		t.Fatal("a pseudo-version is not a release and must not be offered as one")
	}
}

// proxyLists stands in for the module proxy's list of every tag.
func proxyLists(t *testing.T, versions ...string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Join(versions, "\n") + "\n"))
	}))
	t.Cleanup(srv.Close)
	old := listURL
	listURL = srv.URL
	t.Cleanup(func() { listURL = old })
}

func TestLatestCandidateIsTheNewestTagOfAnyKind(t *testing.T) {
	proxyLists(t, "v0.6.2", "v0.7.0-rc", "v0.6.3", "v0.0.0-20260903131609-7e184aaace8d")
	if got, err := latestCandidate(context.Background()); err != nil || got != "v0.7.0-rc" {
		t.Fatalf("latest candidate = %q, %v; want v0.7.0-rc", got, err)
	}

	proxyLists(t, "v0.7.0-rc", "v0.7.0")
	if got, err := latestCandidate(context.Background()); err != nil || got != "v0.7.0" {
		t.Fatalf("latest candidate = %q, %v; want the release over its candidate", got, err)
	}

	proxyLists(t, "v0.0.0-20260903131609-7e184aaace8d")
	if _, err := latestCandidate(context.Background()); err == nil {
		t.Fatal("a list with no tag must not name one")
	}
}

func TestVersionCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	c := &cli{out: &out, err: &errOut}

	if code := c.dispatch("version", nil); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	text := out.String()
	for _, want := range []string{"shoulderd ", "go1.", "/"} {
		if !strings.Contains(text, want) {
			t.Errorf("version output %q lacks %q", text, want)
		}
	}

	out.Reset()
	if code := c.dispatch("version", []string{"--json"}); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut.String())
	}
	var b build
	if err := json.Unmarshal(out.Bytes(), &b); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if b.Version == "" || b.Origin == "" || b.Go == "" || b.Platform == "" {
		t.Errorf("incomplete build description: %+v", b)
	}
}

func TestUpdateHintFollowsOrigin(t *testing.T) {
	for origin, want := range map[string]string{
		"go install": "shoulderd update",
		"container":  "image",
		"plugin":     "shoulderd update",
		"release":    "shoulderd update",
		"checkout":   "make update",
	} {
		if got := updateHint(origin); !strings.Contains(got, want) {
			t.Errorf("updateHint(%q) = %q, want it to mention %q", origin, got, want)
		}
	}
}
