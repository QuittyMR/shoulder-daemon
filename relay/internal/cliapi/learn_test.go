package cliapi

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

const learnedFact = `{"facts":[{"content":"Deploys go to eu-west-2.","category":"decision","scope":"local"}]}`

// repoWith is a committed worktree holding the documents named, which is the
// state --replace is allowed to delete from.
func repoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "docs"}} {
		args = append([]string{
			"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "commit.gpgsign=false",
		}, args...)
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: git with the test's own arguments
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestLearnReportsWhatEachDocumentYielded(t *testing.T) {
	dir := repoWith(t, map[string]string{
		"docs/ARCHITECTURE.md": "# Design\n\nDeploys go to eu-west-2.\n",
		"README.md":            "# Project\n\nA description.\n",
	})
	h, mem, m := newTestServer(t, "", &fakeLLM{learn: learnedFact})

	rec := do(t, h, http.MethodPost, "/v1/cli/learn", `{"scope":"global","dir":"`+dir+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decode[LearnResponse](t, rec)
	if len(got.Files) != 1 || got.Files[0].Path != filepath.Join("docs", "ARCHITECTURE.md") {
		t.Fatalf("read %+v, want the docs directory alone", got.Files)
	}
	if got.Chunks != 1 || got.Stored != 1 || got.Skipped != 0 || got.Failed != 0 || got.Deleted != 0 {
		t.Fatalf("%+v", got)
	}
	if !got.Learned() {
		t.Fatal("a run that lost nothing reports that it did")
	}
	writes := mem.writes()
	if len(writes) != 1 || writes[0].Scope != scope.Global {
		t.Fatalf("stored %+v, want the scope the request named", writes)
	}
	if m.Get("shoulder_cli_learned_total") != 1 {
		t.Fatalf("counted %d facts learned", m.Get("shoulder_cli_learned_total"))
	}
}

func TestLearnDeletesADocumentOnlyWhenAskedAndOnlyFromACleanTree(t *testing.T) {
	docs := map[string]string{"docs/ARCHITECTURE.md": "# Design\n\nDeploys go to eu-west-2.\n"}
	h, _, _ := newTestServer(t, "", &fakeLLM{learn: learnedFact})

	kept := repoWith(t, docs)
	got := decode[LearnResponse](t, do(t, h, http.MethodPost, "/v1/cli/learn",
		`{"scope":"global","dir":"`+kept+`"}`))
	if got.Deleted != 0 {
		t.Fatalf("%+v: a run nobody asked to replace anything deleted a document", got)
	}
	if _, err := os.Stat(filepath.Join(kept, "docs", "ARCHITECTURE.md")); err != nil {
		t.Fatalf("the document is gone: %v", err)
	}

	replaced := repoWith(t, docs)
	got = decode[LearnResponse](t, do(t, h, http.MethodPost, "/v1/cli/learn",
		`{"scope":"global","dir":"`+replaced+`","replace":true}`))
	if got.Deleted != 1 || !got.Files[0].Deleted {
		t.Fatalf("%+v: --replace kept the document it stored", got)
	}
	if _, err := os.Stat(filepath.Join(replaced, "docs", "ARCHITECTURE.md")); !os.IsNotExist(err) {
		t.Fatalf("the document is still there: %v", err)
	}
}

func TestLearnRefusesADirtyWorktreeAsSomethingToFix(t *testing.T) {
	dir := repoWith(t, map[string]string{"docs/ARCHITECTURE.md": "# Design\n\nDeploys go to eu-west-2.\n"})
	if err := os.WriteFile(filepath.Join(dir, "docs", "ARCHITECTURE.md"), []byte("# Design\n\nedited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, mem, _ := newTestServer(t, "", &fakeLLM{learn: learnedFact})

	rec := do(t, h, http.MethodPost, "/v1/cli/learn", `{"scope":"global","dir":"`+dir+`","replace":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "uncommitted") {
		t.Fatalf("error %q does not say what to fix", msg)
	}
	if len(mem.writes()) != 0 {
		t.Fatal("a refused run still wrote to the store")
	}
}

func TestLearnWithoutAModelNamesTheSettingToChange(t *testing.T) {
	dir := repoWith(t, map[string]string{"docs/ARCHITECTURE.md": "# Design\n\nDeploys go to eu-west-2.\n"})
	h, _, _ := newTestServer(t, "", nil)

	rec := do(t, h, http.MethodPost, "/v1/cli/learn", `{"scope":"global","dir":"`+dir+`"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "config set --provider") {
		t.Fatalf("error %q does not say how to give it a model", msg)
	}
}

func TestLearnWithNowhereToWriteIsRefusedBeforeAnythingIsRead(t *testing.T) {
	dir := repoWith(t, map[string]string{"docs/ARCHITECTURE.md": "# Design\n\nDeploys go to eu-west-2.\n"})
	model := &fakeLLM{learn: learnedFact}
	h, _ := newTestServerWith(t, "", model, memory.Nop{})

	rec := do(t, h, http.MethodPost, "/v1/cli/learn", `{"scope":"global","dir":"`+dir+`"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "no store at all") {
		t.Fatalf("error %q", msg)
	}
	if len(model.asked()) != 0 {
		t.Fatal("a daemon with nowhere to write still spent model calls reading documents")
	}
}
