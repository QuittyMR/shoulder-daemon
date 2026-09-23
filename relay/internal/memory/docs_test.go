package memory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory/vectors"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// docsRoots keeps every file a test writes under base: one docs directory per
// project key, one for the global scope, and no worktree, so nothing here
// reaches for git.
func docsRoots(base string) func(scope.Scope, string, string) (string, string) {
	return func(s scope.Scope, project, _ string) (string, string) {
		if s == scope.Global {
			return filepath.Join(base, "global", "docs"), ""
		}
		return filepath.Join(base, "projects", scope.Key(project), "docs"), ""
	}
}

func newDocsIn(t *testing.T, base string, emb Embedder, roots func(scope.Scope, string, string) (string, string)) *Docs {
	t.Helper()
	if roots == nil {
		roots = docsRoots(base)
	}
	d, err := NewDocs(DocsOptions{
		Roots:       roots,
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		Embedder:    emb,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func newDocs(t *testing.T) (*Docs, string) {
	t.Helper()
	base := t.TempDir()
	return newDocsIn(t, base, vectors.Embedder{}, nil), base
}

func docsDirOf(base, project string) string {
	if project == "" {
		return filepath.Join(base, "global", "docs")
	}
	return filepath.Join(base, "projects", scope.Key(project), "docs")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func docsStore(t *testing.T, d *Docs, r Record) string {
	t.Helper()
	id, err := d.Store(context.Background(), r)
	if err != nil {
		t.Fatalf("store %q: %v", r.Content, err)
	}
	return id
}

func TestDocsConformance(t *testing.T) {
	base := t.TempDir()
	n := 0
	TestConnector(t, func() Connector {
		n++
		return newDocsIn(t, filepath.Join(base, strings.Repeat("x", n)), vectors.Embedder{}, nil)
	})
}

func TestDocsPlacesEachCategoryInItsFile(t *testing.T) {
	d, base := newDocs(t)
	const project = "/srv/app"
	for _, place := range []struct{ category, file, content string }{
		{"structure", "ARCHITECTURE.shoulder.md", "the api is versioned in the path"},
		{"decision", "DECISIONS.shoulder.md", "releases are cut on the last Thursday of the month"},
		{"constraint", "CONVENTIONS.shoulder.md", "the integration tests need a live Postgres"},
		{"preference", "CONVENTIONS.shoulder.md", "prefers terse answers with no preamble"},
		{"correction", "CONVENTIONS.shoulder.md", "the main branch is called master, not main"},
		{"reference", "REFERENCES.shoulder.md", "the release rota is kept in docs/rota.md"},
		{"", "NOTES.shoulder.md", "the office cat is called Biscuit"},
	} {
		category, file, content := place.category, place.file, place.content
		docsStore(t, d, Record{Content: content, Category: category, Scope: scope.Local, Project: project})
		got := readFile(t, filepath.Join(docsDirOf(base, project), file))
		if !strings.Contains(got, "- "+content+" <!-- sd id="+contentID(content)) {
			t.Errorf("category %q did not land in %s as a bullet:\n%s", category, file, got)
		}
	}
	// A private record goes to the person's file whatever it is about.
	docsStore(t, d, Record{Content: "prefers rebasing over merging", Category: "structure", Private: true, Scope: scope.Local, Project: project})
	if got := readFile(t, filepath.Join(docsDirOf(base, project), "USER.shoulder.md")); !strings.Contains(got, "prefers rebasing") {
		t.Errorf("the private record is not in USER.shoulder.md:\n%s", got)
	}
	if got := readFile(t, filepath.Join(docsDirOf(base, project), "ARCHITECTURE.shoulder.md")); strings.Contains(got, "prefers rebasing") {
		t.Error("the private record was also filed by category")
	}

	names, _ := filepath.Glob(filepath.Join(docsDirOf(base, project), "*"))
	for _, name := range names {
		if !strings.HasSuffix(name, docsSuffix) {
			t.Errorf("the store wrote a file it does not own: %s", name)
		}
	}
}

func TestDocsANewFileOpensWithItsTitleAndANote(t *testing.T) {
	d, base := newDocs(t)
	docsStore(t, d, Record{Content: "the api is versioned in the path", Category: "structure", Scope: scope.Local, Project: "/srv/app"})
	lines := strings.Split(readFile(t, filepath.Join(docsDirOf(base, "/srv/app"), "ARCHITECTURE.shoulder.md")), "\n")
	if lines[0] != "# Architecture" {
		t.Errorf("first line %q, want the title", lines[0])
	}
	if !strings.Contains(strings.Join(lines[1:4], "\n"), "shoulder-daemon") {
		t.Errorf("no note says who maintains the file:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.HasPrefix(lines[len(lines)-2], "- the api is versioned") || lines[len(lines)-1] != "" {
		t.Errorf("the record is not the last line, followed by a newline: %q", lines)
	}
}

func TestDocsWritesTheLineFormat(t *testing.T) {
	d, base := newDocs(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	docsStore(t, d, Record{
		Content: "releases ship\non Fridays", Category: "decision", Tags: []string{"release", "cadence"},
		CreatedAt: at, Scope: scope.Global,
	})
	got := readFile(t, filepath.Join(docsDirOf(base, ""), "DECISIONS.shoulder.md"))
	want := "- releases ship on Fridays <!-- sd id=" + contentID("releases ship on Fridays") +
		" category=decision tags=release,cadence at=2026-03-04T05:06:07Z -->"
	if !strings.Contains(got, want+"\n") {
		t.Fatalf("line not written as specified:\n%s\nwant\n%s", got, want)
	}
	rec, ok := parseDocsLine(want)
	if !ok || rec.Category != "decision" || len(rec.Tags) != 2 || !rec.CreatedAt.Equal(at) {
		t.Fatalf("the line does not read back: %+v", rec)
	}
}

// The files are edited by people. Nothing they are likely to do to one may
// cost the daemon a record or, worse, invent one.
func TestDocsToleratesHandEdits(t *testing.T) {
	d, base := newDocs(t)
	const project = "/srv/app"
	dir := docsDirOf(base, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const (
		kept     = "the main branch is called master"
		reworded = "the deploy script lives in bin/ship"
		orphan   = "a bullet somebody wrote by hand"
	)
	body := strings.Join([]string{
		"# Architecture",
		"",
		"Some prose a person wrote about the layout.",
		"",
		"- " + orphan,
		"",
		"  - the deployment script is bin/ship, run by hand <!-- sd id=" + contentID(reworded) + " category=structure at=2026-01-02T03:04:05Z -->",
		"## A heading in the middle",
		"* " + kept + " <!-- sd id=" + contentID(kept) + " category=structure at=2026-01-03T00:00:00Z -->",
		"- a bullet mentioning <!-- sd in passing",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "ARCHITECTURE.shoulder.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := d.List(context.Background(), Query{Scope: scope.Local, Project: project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d records from a file holding two: %+v", len(got), got)
	}
	if got[0].ID != contentID(kept) || got[1].ID != contentID(reworded) {
		t.Errorf("not newest first, or the id was not taken from the comment: %+v", got)
	}
	if got[1].Content != "the deployment script is bin/ship, run by hand" {
		t.Errorf("the reworded sentence was not read as written: %q", got[1].Content)
	}
	for _, r := range got {
		if r.Content == orphan {
			t.Error("a bullet with no sd comment was read as a record")
		}
	}

	// The id is what a person retypes off a digest, so the reworded record is
	// still the one it was.
	if _, err := d.Supersede(context.Background(), contentID(reworded), Record{
		Content: "the deploy script lives in tools/release", Category: "structure", Scope: scope.Local, Project: project,
	}); err != nil {
		t.Fatalf("supersede by the comment's id: %v", err)
	}
	after := readFile(t, filepath.Join(dir, "ARCHITECTURE.shoulder.md"))
	for _, line := range []string{"Some prose a person wrote", "- " + orphan, "## A heading in the middle", "* " + kept, "mentioning <!-- sd in passing"} {
		if !strings.Contains(after, line) {
			t.Errorf("a hand-written line was lost on write: %q\n%s", line, after)
		}
	}
	if strings.Contains(after, "run by hand") || !strings.Contains(after, "tools/release") {
		t.Errorf("the replacement did not take the original's place:\n%s", after)
	}
}

func TestDocsSupersedeStaysInFileAndPosition(t *testing.T) {
	d, base := newDocs(t)
	const project = "/srv/app"
	ctx := context.Background()
	facts := []string{
		"the billing service listens on port 8081",
		"the catalogue service listens on port 8082",
		"the search service listens on port 8084",
	}
	var ids []string
	for _, f := range facts {
		ids = append(ids, docsStore(t, d, Record{Content: f, Category: "structure", Scope: scope.Local, Project: project}))
	}
	path := filepath.Join(docsDirOf(base, project), "ARCHITECTURE.shoulder.md")
	before := strings.Split(readFile(t, path), "\n")
	at := -1
	for i, line := range before {
		if strings.Contains(line, ids[1]) {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("the record to be replaced is not in the file")
	}

	// A correction that changes the category would file elsewhere if it were
	// a new fact; a replacement is not one.
	replacement := Record{Content: "the catalogue service listens on port 8092", Category: "decision", Scope: scope.Local, Project: project}
	newID, err := d.Supersede(ctx, ids[1], replacement)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	after := strings.Split(readFile(t, path), "\n")
	if len(after) != len(before) {
		t.Fatalf("the file grew or shrank on a supersede: %d lines, was %d", len(after), len(before))
	}
	for i := range before {
		if i == at {
			if !strings.Contains(after[i], newID) || !strings.Contains(after[i], "port 8092") || !strings.Contains(after[i], "category=decision") {
				t.Errorf("line %d is not the replacement: %q", i, after[i])
			}
			continue
		}
		if after[i] != before[i] {
			t.Errorf("line %d changed on a supersede of another record: %q -> %q", i, before[i], after[i])
		}
	}
	if _, err := os.Stat(filepath.Join(docsDirOf(base, project), "DECISIONS.shoulder.md")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the replacement was moved to the file its category names")
	}

	// A replacement already stored elsewhere would leave two lines with one id.
	if _, err := d.Supersede(ctx, ids[0], Record{Content: facts[2], Category: "structure", Scope: scope.Local, Project: project}); !errors.Is(err, ErrDuplicateExact) {
		t.Errorf("got %v, want ErrDuplicateExact", err)
	}
}

func TestDocsRefusesDuplicatesLikeLocal(t *testing.T) {
	for name, emb := range map[string]Embedder{"embedding": vectors.Embedder{}, "lexical": nil} {
		t.Run(name, func(t *testing.T) {
			d := newDocsIn(t, t.TempDir(), emb, nil)
			ctx := context.Background()
			rec := Record{Content: "the deployment script lives in bin/ship and is run by hand", Scope: scope.Global}
			id := docsStore(t, d, rec)
			if _, err := d.Store(ctx, rec); !errors.Is(err, ErrDuplicateExact) {
				t.Fatalf("got %v, want ErrDuplicateExact", err)
			}
			_, err := d.Store(ctx, Record{Content: "the deployment script lives in bin/ship and is run by hand.", Scope: scope.Global})
			var dup *ErrDuplicateSemantic
			if !errors.As(err, &dup) || dup.Collided != id {
				t.Fatalf("got %v, want ErrDuplicateSemantic naming %s", err, id)
			}
			// Two facts that differ by a number are two facts.
			for _, content := range []string{"the billing service listens on port 8081", "the billing service listens on port 8082"} {
				docsStore(t, d, Record{Content: content, Scope: scope.Global})
			}
		})
	}
}

// One measure, in two stores: the same corpus asked the same question must
// score the same, or a daemon switched between them recalls differently.
func TestDocsRanksExactlyAsLocalDoes(t *testing.T) {
	ctx := context.Background()
	d, _ := newDocs(t)
	l := newLocal(t)
	for _, content := range []string{
		"the integration tests need a live Postgres on port 5544",
		"lunch is at one",
		"we ship to the staging cluster",
		"releases are cut on the last Thursday of the month",
	} {
		docsStore(t, d, Record{Content: content, Scope: scope.Global})
		if _, err := l.Store(ctx, Record{Content: content, Scope: scope.Global}); err != nil {
			t.Fatalf("local store: %v", err)
		}
	}
	for _, text := range []string{"where does this get deployed", "which port does Postgres run on for the tests"} {
		q := Query{Text: text, Limit: 5, Scope: scope.Global}
		fromDocs, err := d.Search(ctx, q)
		if err != nil {
			t.Fatalf("docs search: %v", err)
		}
		fromLocal, err := l.Search(ctx, q)
		if err != nil {
			t.Fatalf("local search: %v", err)
		}
		if len(fromDocs) != len(fromLocal) || len(fromDocs) == 0 {
			t.Fatalf("%q: docs %+v, local %+v", text, fromDocs, fromLocal)
		}
		for i := range fromDocs {
			// To the last few bits: summing a bag of words walks a map, and
			// the order of a map walk is not the same twice.
			if fromDocs[i].Content != fromLocal[i].Content || math.Abs(fromDocs[i].Score-fromLocal[i].Score) > 1e-9 {
				t.Errorf("%q rank %d: docs %q %v, local %q %v", text, i, fromDocs[i].Content, fromDocs[i].Score, fromLocal[i].Content, fromLocal[i].Score)
			}
		}
	}
}

func TestDocsCachesVectorsOutsideTheRepository(t *testing.T) {
	base := t.TempDir()
	d := newDocsIn(t, base, stubEmbedder{id: "stub-v1"}, nil)
	ctx := context.Background()
	const project = "/srv/app"
	for _, content := range []string{"zebra crossing the road", "postgres runs on 5544"} {
		docsStore(t, d, Record{Content: content, Scope: scope.Local, Project: project})
	}
	got, err := d.Search(ctx, Query{Text: "zebra unrelated words entirely", Limit: 5, Scope: scope.Local, Project: project})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || !strings.HasPrefix(got[0].Content, "zebra") {
		t.Fatalf("the embedding was not used to rank: %+v", got)
	}
	cache := filepath.Join(base, "cache", scope.Key(project)+".json")
	raw := readFile(t, cache)
	if !strings.Contains(raw, contentID("zebra crossing the road")) || !strings.Contains(raw, `"stub-v1"`) {
		t.Errorf("the cache is not keyed by id and tagged with the model:\n%s", raw)
	}
	names, _ := filepath.Glob(filepath.Join(docsDirOf(base, project), "*"))
	for _, name := range names {
		if strings.HasSuffix(name, ".json") {
			t.Errorf("a vector file was written into the docs directory: %s", name)
		}
	}

	// A record reworded by hand keeps its id and loses its vector, which is
	// then computed again rather than trusted.
	path := filepath.Join(docsDirOf(base, project), "NOTES.shoulder.md")
	body := strings.Replace(readFile(t, path), "- zebra crossing the road", "- antelope crossing the road", 1)
	if werr := os.WriteFile(path, []byte(body), 0o644); werr != nil {
		t.Fatal(werr)
	}
	got, err = d.Search(ctx, Query{Text: "antelope words unrelated entirely", Limit: 5, Scope: scope.Local, Project: project})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) == 0 || !strings.HasPrefix(got[0].Content, "antelope") {
		t.Fatalf("the stale vector was used for the reworded record: %+v", got)
	}
}

func TestDocsKeepsSessionNotesOutOfTheFiles(t *testing.T) {
	d, base := newDocs(t)
	ctx := context.Background()
	const (
		project = "/srv/app"
		note    = "session keywords: migration, rollback"
	)
	docsStore(t, d, Record{Content: "the migrations live in db/", Category: "structure", Scope: scope.Local, Project: project})
	id := docsStore(t, d, Record{Content: note, Kind: KindSession, Session: "s-1", Scope: scope.Local, Project: project})

	if err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, docsSuffix) {
			return err
		}
		if strings.Contains(readFile(t, path), note) {
			t.Errorf("a working note was written to %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, filepath.Join(base, "session.json")), note) {
		t.Error("the working note is not in the session file")
	}
	got, err := d.List(ctx, Query{Scope: scope.Local, Project: project, Kind: KindSession})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Session != "s-1" {
		t.Fatalf("the note does not read back from the session store: %+v", got)
	}
	if err := d.Forget(ctx, id, Query{Scope: scope.Local, Project: project, Kind: KindSession}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got, _ := d.List(ctx, Query{Scope: scope.Local, Project: project, Kind: KindSession}); len(got) != 0 {
		t.Fatalf("the note survived being forgotten: %+v", got)
	}
}

func TestDocsListSeesAnExternalEdit(t *testing.T) {
	d, base := newDocs(t)
	ctx := context.Background()
	const project = "/srv/app"
	first := docsStore(t, d, Record{Content: "the main branch is called master", Category: "decision", Scope: scope.Local, Project: project})
	if got, _ := d.List(ctx, Query{Scope: scope.Local, Project: project}); len(got) != 1 {
		t.Fatalf("list before the edit: %+v", got)
	}

	path := filepath.Join(docsDirOf(base, project), "DECISIONS.shoulder.md")
	const added = "tags are cut from master only"
	body := strings.Replace(readFile(t, path), "- the main branch", "- (see git history) the main branch", 1) +
		"- " + added + " <!-- sd id=" + contentID(added) + " category=decision at=2026-05-05T00:00:00Z -->\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := d.List(ctx, Query{Scope: scope.Local, Project: project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the edit was not seen: %+v", got)
	}
	if _, ok := conformanceFind(got, added); !ok {
		t.Errorf("the bullet added by hand is not listed: %+v", got)
	}
	if rec, ok := conformanceFind(got, "(see git history) the main branch is called master"); !ok || rec.ID != first {
		t.Errorf("the reworded record lost its identity: %+v", got)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := d.List(ctx, Query{Scope: scope.Local, Project: project}); len(got) != 0 {
		t.Fatalf("a deleted file is still listed: %+v", got)
	}
}

func TestDocsForgetRemovesTheLineAndKeepsTheFile(t *testing.T) {
	d, base := newDocs(t)
	ctx := context.Background()
	id := docsStore(t, d, Record{Content: "prefers terse answers", Category: "preference", Scope: scope.Global})
	path := filepath.Join(docsDirOf(base, ""), "CONVENTIONS.shoulder.md")
	if err := d.Forget(ctx, id, Query{Scope: scope.Global}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	got := readFile(t, path)
	if strings.Contains(got, id) {
		t.Errorf("the record is still in the file:\n%s", got)
	}
	if !strings.HasPrefix(got, "# Conventions\n") {
		t.Errorf("the file lost its header, or was deleted:\n%s", got)
	}
}

func TestDocsRefusesAKeyForALocalRead(t *testing.T) {
	d, _ := newDocs(t)
	_, err := d.List(context.Background(), Query{Scope: scope.Local, Project: scope.Key("/srv/app")})
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("got %v, want a refusal that says the path is needed", err)
	}
	if _, err := d.Store(context.Background(), Record{Content: "x", Scope: scope.Local, Project: scope.Key("/srv/app")}); err == nil {
		t.Fatal("a write under a key was accepted; whichever directory it went to was somebody else's")
	}
}

func TestDocsFileModes(t *testing.T) {
	d, base := newDocs(t)
	docsStore(t, d, Record{Content: "the api is versioned in the path", Category: "structure", Scope: scope.Local, Project: "/srv/app"})
	docsStore(t, d, Record{Content: "prefers terse answers", Category: "preference", Scope: scope.Global})
	for path, want := range map[string]os.FileMode{
		filepath.Join(docsDirOf(base, "/srv/app"), "ARCHITECTURE.shoulder.md"): 0o644,
		filepath.Join(docsDirOf(base, ""), "CONVENTIONS.shoulder.md"):          0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != want {
			t.Errorf("%s: mode %o, want %o", path, perm, want)
		}
	}
}

// gitRepo is a real repository, because the .gitignore rule is about what git
// reports and a fake would only test the fake.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	args = append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", args...).CombinedOutput() //nolint:gosec // G204: git with the test's own arguments
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func repoRoots(repo, base string) func(scope.Scope, string, string) (string, string) {
	return func(s scope.Scope, _, _ string) (string, string) {
		if s == scope.Global {
			return filepath.Join(base, "global", "docs"), ""
		}
		return filepath.Join(repo, "docs"), repo
	}
}

func TestDocsIgnoresThePrivateFile(t *testing.T) {
	private := Record{Content: "prefers rebasing over merging", Category: "preference", Private: true, Scope: scope.Local}

	t.Run("creates .gitignore when there is none", func(t *testing.T) {
		repo := gitRepo(t)
		d := newDocsIn(t, t.TempDir(), nil, repoRoots(repo, t.TempDir()))
		private.Project = repo
		docsStore(t, d, private)
		if got := readFile(t, filepath.Join(repo, ".gitignore")); got != "docs/USER.shoulder.md\n" {
			t.Fatalf(".gitignore is %q", got)
		}
		// The line is there; a second private fact must not add it again.
		docsStore(t, d, Record{Content: "likes commit messages in the imperative", Category: "preference", Private: true, Scope: scope.Local, Project: repo})
		if got := readFile(t, filepath.Join(repo, ".gitignore")); strings.Count(got, "USER.shoulder.md") != 1 {
			t.Fatalf(".gitignore holds the line more than once:\n%s", got)
		}
	})

	t.Run("appends to a committed .gitignore", func(t *testing.T) {
		repo := gitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", ".gitignore")
		git(t, repo, "commit", "-q", "-m", "ignore logs")
		d := newDocsIn(t, t.TempDir(), nil, repoRoots(repo, t.TempDir()))
		private.Project = repo
		docsStore(t, d, private)
		if got := readFile(t, filepath.Join(repo, ".gitignore")); got != "*.log\ndocs/USER.shoulder.md\n" {
			t.Fatalf(".gitignore is %q; the existing rule must survive and the line must be on its own", got)
		}
	})

	t.Run("refuses a .gitignore with uncommitted changes and stores the fact anyway", func(t *testing.T) {
		repo := gitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, repo, "add", ".gitignore")
		git(t, repo, "commit", "-q", "-m", "ignore logs")
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n*.tmp\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var logged strings.Builder
		d, err := NewDocs(DocsOptions{
			Roots:       repoRoots(repo, t.TempDir()),
			SessionPath: filepath.Join(t.TempDir(), "session.json"),
			CacheDir:    t.TempDir(),
			Log:         slog.New(slog.NewTextHandler(&logged, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Close() })
		private.Project = repo
		docsStore(t, d, private)
		if got := readFile(t, filepath.Join(repo, ".gitignore")); got != "*.log\n*.tmp\n" {
			t.Fatalf("a dirty .gitignore was edited: %q", got)
		}
		if !strings.Contains(logged.String(), "uncommitted") {
			t.Errorf("the refusal was not logged:\n%s", logged.String())
		}
		if got := readFile(t, filepath.Join(repo, "docs", "USER.shoulder.md")); !strings.Contains(got, private.Content) {
			t.Error("the fact was not stored")
		}
	})

	t.Run("leaves .gitignore alone outside a worktree", func(t *testing.T) {
		d, base := newDocs(t)
		private.Project = "/srv/app"
		docsStore(t, d, private)
		if _, err := os.Stat(filepath.Join(base, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a .gitignore was written where there is no repository")
		}
	})
}

// The default resolver is what the daemon runs with, and it has to find the
// worktree from a directory inside it and reuse a docs directory that already
// exists under whichever name the project chose.
func TestDocsDefaultRootsFindTheWorktreeAndAnExistingDocDir(t *testing.T) {
	repo := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "doc"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repo, "cmd", "tool")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	d, err := NewDocs(DocsOptions{
		GlobalDir:   filepath.Join(base, "global"),
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	docsStore(t, d, Record{Content: "the api is versioned in the path", Category: "structure", Scope: scope.Local, Project: inside})
	docsStore(t, d, Record{Content: "prefers rebasing", Category: "preference", Private: true, Scope: scope.Local, Project: inside})
	if !strings.Contains(readFile(t, filepath.Join(repo, "doc", "ARCHITECTURE.shoulder.md")), "versioned") {
		t.Error("the existing doc/ directory was not reused")
	}
	if _, err := os.Stat(filepath.Join(repo, "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a docs/ directory was created beside the existing doc/")
	}
	if got := readFile(t, filepath.Join(repo, ".gitignore")); got != "doc/USER.shoulder.md\n" {
		t.Errorf(".gitignore is %q; the line must be relative to the worktree, whatever the docs directory is", got)
	}
	docsStore(t, d, Record{Content: "prefers terse answers", Category: "preference", Scope: scope.Global})
	if _, err := os.Stat(filepath.Join(base, "global", "CONVENTIONS.shoulder.md")); err != nil {
		t.Errorf("the global fact did not land in GlobalDir: %v", err)
	}

	// A project identity that is not a path cannot be located, and the store
	// says so rather than picking a directory.
	if _, err := d.Store(context.Background(), Record{Content: "x", Scope: scope.Local, Project: "repo@0123abcd"}); err == nil {
		t.Fatal("a name@commit identity was accepted as a place to write")
	}
}

// A directory that is not a repository still gets its docs, under the path
// itself, with nothing to ignore.
func TestDocsDefaultRootsFallBackToThePath(t *testing.T) {
	dir := t.TempDir()
	base := t.TempDir()
	d, err := NewDocs(DocsOptions{
		GlobalDir:   filepath.Join(base, "global"),
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if gitToplevel(dir) != "" {
		t.Skip("the temporary directory is inside a git repository")
	}
	docsStore(t, d, Record{Content: "prefers rebasing", Category: "preference", Private: true, Scope: scope.Local, Project: dir})
	if _, err := os.Stat(filepath.Join(dir, "docs", "USER.shoulder.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a .gitignore was written outside a repository")
	}
}

// In production a local record names the project by the identity scope.Project
// produces, which for a repository is name@commit and not a path. The store
// has to find the checkout from the directory the call came with, and then go
// on finding it for the calls that come with none.
func TestDocsResolvesTheCheckoutFromTheDirectoryAndRemembersIt(t *testing.T) {
	repo := gitRepo(t)
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "root")
	inside := filepath.Join(repo, "cmd", "tool")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	project, err := scope.Project(inside)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(project, "@") {
		t.Fatalf("scope.Project gave %q, want a name@commit identity", project)
	}
	newStore := func() *Docs {
		base := t.TempDir()
		d, oerr := NewDocs(DocsOptions{
			GlobalDir:   filepath.Join(base, "global"),
			SessionPath: filepath.Join(base, "session.json"),
			CacheDir:    filepath.Join(base, "cache"),
			Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if oerr != nil {
			t.Fatal(oerr)
		}
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	ctx := context.Background()

	d := newStore()
	if _, serr := d.Store(ctx, Record{Content: "x", Scope: scope.Local, Project: project}); serr == nil {
		t.Fatal("an identity with no directory and no memory of one was accepted as a place to write")
	}
	id := docsStore(t, d, Record{
		Content: "the api is versioned in the path", Category: "structure",
		Scope: scope.Local, Project: project, Dir: inside,
	})
	if !strings.Contains(readFile(t, filepath.Join(repo, "docs", "ARCHITECTURE.shoulder.md")), "versioned") {
		t.Fatal("the fact did not land in the worktree's docs directory")
	}

	// Reads and deletes that arrive with the identity alone, as the janitor's
	// and the tidying pass's do, find the place the write established.
	got, err := d.List(ctx, Query{Scope: scope.Local, Project: project})
	if err != nil || len(got) != 1 || got[0].Dir != "" {
		t.Fatalf("list by identity alone: %+v, %v", got, err)
	}
	if err := d.Forget(ctx, id, Query{Scope: scope.Local, Project: project}); err != nil {
		t.Fatalf("forget by identity alone: %v", err)
	}
	if got := readFile(t, filepath.Join(repo, "docs", "ARCHITECTURE.shoulder.md")); strings.Contains(got, "versioned") {
		t.Fatal("the forgotten fact is still in the file")
	}

	// The boundary's own lookups carry the directory too: a daemon whose
	// first call is a correction has no memory of the checkout yet.
	fresh := Checked(newStore())
	kept := docsStore(t, newStore(), Record{Content: "the api is versioned in the path", Category: "structure", Scope: scope.Local, Project: project, Dir: inside})
	corrected := Record{Content: "the api is versioned in the header", Category: "structure", Scope: scope.Local, Project: project, Dir: inside}
	if _, err := fresh.Supersede(ctx, kept, corrected); err != nil {
		t.Fatalf("a supersede with the directory on a fresh store was refused: %v", err)
	}
	if got := readFile(t, filepath.Join(repo, "docs", "ARCHITECTURE.shoulder.md")); !strings.Contains(got, "header") || strings.Contains(got, "in the path") {
		t.Fatalf("the correction did not land in place:\n%s", got)
	}

	// A directory that is not on this machine is refused, not created: the
	// checkout is wherever the session is, and this daemon is not there.
	gone := Record{Content: "x", Scope: scope.Local, Project: project, Dir: filepath.Join(t.TempDir(), "gone")}
	if _, err := newStore().Store(ctx, gone); err == nil || !strings.Contains(err.Error(), "not on this machine") {
		t.Fatalf("a directory that does not exist was accepted: %v", err)
	}
}

// Two checkouts of one repository share an identity. A call from the second
// one is placed in the second one, not in whichever was seen first.
func TestDocsFollowsTheDirectoryBetweenCheckoutsOfOneRepository(t *testing.T) {
	first := filepath.Join(gitRepo(t), "repo")
	git(t, filepath.Dir(first), "init", "-q", first)
	git(t, first, "commit", "-q", "--allow-empty", "-m", "root")
	second := filepath.Join(t.TempDir(), "repo")
	git(t, first, "clone", "-q", first, second)
	project, err := scope.Project(second)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := scope.Project(first); again != project {
		t.Fatalf("the clone has identity %q and the origin %q; the test needs them to share one", project, again)
	}
	base := t.TempDir()
	d, err := NewDocs(DocsOptions{
		GlobalDir:   filepath.Join(base, "global"),
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	docsStore(t, d, Record{Content: "the first checkout builds with make", Scope: scope.Local, Project: project, Dir: first})
	docsStore(t, d, Record{Content: "the second checkout builds with mage", Scope: scope.Local, Project: project, Dir: second})
	if got := readFile(t, filepath.Join(first, "docs", "NOTES.shoulder.md")); strings.Contains(got, "mage") {
		t.Fatal("a fact from the second checkout was written into the first")
	}
	if got := readFile(t, filepath.Join(second, "docs", "NOTES.shoulder.md")); !strings.Contains(got, "mage") {
		t.Fatal("the second checkout's fact did not land in it")
	}
}

// The subdirectory name is a setting, and a name chosen on purpose is used as
// given rather than swapped for an existing doc/.
func TestDocsHonoursTheConfiguredDirectoryName(t *testing.T) {
	repo := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "doc"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	d, err := NewDocs(DocsOptions{
		GlobalDir:   filepath.Join(base, "global"),
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		DirName:     "notes",
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	docsStore(t, d, Record{Content: "the api is versioned in the path", Category: "structure", Scope: scope.Local, Project: repo, Dir: repo})
	if _, err := os.Stat(filepath.Join(repo, "notes", "ARCHITECTURE.shoulder.md")); err != nil {
		t.Fatalf("the configured name was not used: %v", err)
	}
	if dir, worktree := DocsDirFor(repo, "notes"); dir != filepath.Join(repo, "notes") || worktree != repo {
		t.Errorf("DocsDirFor = %q, %q", dir, worktree)
	}
	if dir, _ := DocsDirFor(repo, ""); dir != filepath.Join(repo, "doc") {
		t.Errorf("the default name did not reuse doc/: %q", dir)
	}
}

// The timestamp on the line keeps its sub-second part, so two facts written
// in the same second still list in the order they were written.
func TestDocsOrdersFactsWrittenInTheSameSecond(t *testing.T) {
	d, _ := newDocs(t)
	base := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	// Written out of order on purpose, so file order and time order disagree.
	docsStore(t, d, Record{Content: "prefers terse answers", Scope: scope.Global, CreatedAt: base.Add(500 * time.Millisecond)})
	docsStore(t, d, Record{Content: "runs the linter before pushing", Scope: scope.Global, CreatedAt: base.Add(200 * time.Millisecond)})
	docsStore(t, d, Record{Content: "the main branch is called master", Scope: scope.Global, CreatedAt: base.Add(900 * time.Millisecond)})
	got, err := d.List(context.Background(), Query{Scope: scope.Global})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range got {
		order = append(order, r.Content)
	}
	if want := "the main branch is called master,prefers terse answers,runs the linter before pushing"; strings.Join(order, ",") != want {
		t.Fatalf("listed %q, want %q", strings.Join(order, ","), want)
	}
	if !got[0].CreatedAt.Equal(base.Add(900 * time.Millisecond)) {
		t.Errorf("the sub-second part was lost: %s", got[0].CreatedAt)
	}
}

// Handed the composite the daemon builds, the store must not re-embed every
// entry with the fallback on the first read and again with the model once it
// arrives. Vectors from the model are kept through the wait and used after it.
func TestDocsKeepsCachedVectorsWhileTheModelLoads(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	primary := newLoadingEmbedder("big-model", 8)
	primary.arrive()
	settled := newDocsIn(t, base, NewFallback(primary, fixedEmbedder{id: "small-model", dims: 2}), nil)
	id := docsStore(t, settled, Record{Content: "the main branch is called master", Scope: scope.Global})

	cachePath := filepath.Join(base, "cache", "global.json")
	modelOf := func() string {
		t.Helper()
		var c docsCache
		if err := json.Unmarshal([]byte(readFile(t, cachePath)), &c); err != nil {
			t.Fatal(err)
		}
		return c.Vectors[id].Model
	}
	if got := modelOf(); got != "big-model" {
		t.Fatalf("written with the model ready, the vector is tagged %q", got)
	}

	// The daemon restarts: the model is loading again, the cache is from
	// before, and reads must leave it alone.
	loading := newLoadingEmbedder("big-model", 8)
	d := newDocsIn(t, base, NewFallback(loading, fixedEmbedder{id: "small-model", dims: 2}), nil)
	if _, err := d.Search(ctx, Query{Text: "which branch is main", Scope: scope.Global}); err != nil {
		t.Fatal(err)
	}
	if got := modelOf(); got != "big-model" {
		t.Fatalf("a read while the model was loading rewrote the cache with %q", got)
	}
	loading.arrive()
	got, err := d.Search(ctx, Query{Text: "which branch is main", Scope: scope.Global})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Score <= 0 {
		t.Fatalf("after the model arrived the search returned %+v", got)
	}
	if got := modelOf(); got != "big-model" {
		t.Fatalf("after the model arrived the vector is tagged %q", got)
	}
}

// Which file a record is in is what decides whether git carries it, so the one
// correction that cannot keep its position is the one that changes privacy.
// The person who ran it was told the fact is now theirs alone; leaving it in a
// committed file would make that a lie.
func TestDocsSupersedeMovesARecordThatChangedPrivacy(t *testing.T) {
	repo := gitRepo(t)
	d := newDocsIn(t, t.TempDir(), nil, repoRoots(repo, t.TempDir()))
	ctx := context.Background()
	arch := filepath.Join(repo, "docs", "ARCHITECTURE.shoulder.md")
	user := filepath.Join(repo, "docs", "USER.shoulder.md")

	rec := Record{Content: "the integration tests need a live Postgres", Category: "structure", Scope: scope.Local, Project: repo}
	id := docsStore(t, d, rec)
	// A second record in the same file, to show the move takes only its own line.
	kept := docsStore(t, d, Record{Content: "the billing service listens on port 8081", Category: "structure", Scope: scope.Local, Project: repo})

	private := rec
	private.Content, private.Private = "the integration tests need the Postgres on 5433 here", true
	newID, err := d.Supersede(ctx, id, private)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if got := readFile(t, arch); strings.Contains(got, "5433") || strings.Contains(got, id) {
		t.Fatalf("the fact is still in the committed file:\n%s", got)
	} else if !strings.Contains(got, kept) {
		t.Fatalf("the move took another record's line with it:\n%s", got)
	}
	if got := readFile(t, user); !strings.Contains(got, newID) || !strings.Contains(got, "5433") {
		t.Fatalf("the private replacement is not in the private file:\n%s", got)
	}
	// The private file is only kept out of git from the moment one exists, and
	// a move is the first moment here.
	if got := readFile(t, filepath.Join(repo, ".gitignore")); !strings.Contains(got, "USER.shoulder.md") {
		t.Fatalf(".gitignore is %q", got)
	}

	// And back: a record the person decides was about the project after all
	// has to leave the file git does not carry, or it is invisible to the team
	// that now owns it.
	public := private
	public.Content, public.Private = "the integration tests need Postgres on 5433", false
	backID, err := d.Supersede(ctx, newID, public)
	if err != nil {
		t.Fatalf("supersede back: %v", err)
	}
	if got := readFile(t, user); strings.Contains(got, backID) || strings.Contains(got, newID) {
		t.Fatalf("the record is still in the private file:\n%s", got)
	}
	if got := readFile(t, arch); !strings.Contains(got, backID) {
		t.Fatalf("the record did not come back to the committed file:\n%s", got)
	}
	held, err := d.List(ctx, Query{Scope: scope.Local, Project: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 {
		t.Fatalf("expected two records after two moves, got %d: %+v", len(held), held)
	}
	for _, h := range held {
		if h.Private {
			t.Errorf("record %q is still reported private: %+v", h.Content, h)
		}
	}
}

// The docs directory is somebody's checkout and a checkout is wherever they put
// it. A path holding a glob character used to read as an empty place: nothing
// was ever listed out of it, no duplicate was ever noticed, every write
// appended another copy of the same line, and a supersede could not find what
// it was told to replace.
func TestDocsReadsADirectoryWhosePathHoldsAGlobCharacter(t *testing.T) {
	base := filepath.Join(t.TempDir(), "[client]", "*work?")
	c := Checked(newDocsIn(t, base, nil, docsRoots(base)))
	ctx := context.Background()
	const (
		project = "/srv/app"
		fact    = "the deploy script lives in bin/ship"
	)
	rec := Record{Content: fact, Category: "structure", Scope: scope.Local, Project: project}
	id, err := c.Store(ctx, rec)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	got, err := c.List(ctx, Query{Scope: scope.Local, Project: project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Content != fact {
		t.Fatalf("the record was not read back from a path with a glob character: %+v", got)
	}
	if _, err := c.Store(ctx, rec); !errors.Is(err, ErrDuplicateExact) {
		t.Fatalf("a second store of the same sentence gave %v, want ErrDuplicateExact", err)
	}
	if _, err := c.Supersede(ctx, id, Record{
		Content: "the deploy script lives in tools/release", Category: "structure",
		Scope: scope.Local, Project: project,
	}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if body := readFile(t, filepath.Join(docsDirOf(base, project), "ARCHITECTURE.shoulder.md")); strings.Contains(body, "bin/ship") {
		t.Fatalf("the superseded line is still in the file:\n%s", body)
	}
}

// The identity and the directory decide together, once and for the life of the
// daemon, where a checkout's facts are kept. A pair naming two checkouts is
// refused rather than believed: believing it writes one project's facts into
// another's worktree, and every later call carrying that identity alone reads
// and writes there.
func TestDocsRefusesAProjectAndADirectoryThatNameDifferentCheckouts(t *testing.T) {
	a, b := gitRepo(t), gitRepo(t)
	// Different messages, or the two empty commits are byte for byte the same
	// object and the checkouts share an identity.
	git(t, a, "commit", "-q", "--allow-empty", "-m", "root of a")
	git(t, b, "commit", "-q", "--allow-empty", "-m", "root of b")
	projectA, err := scope.Project(a)
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := scope.Project(b)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Key(projectA) == scope.Key(projectB) {
		t.Fatal("the two checkouts share an identity; the test needs them apart")
	}
	base := t.TempDir()
	d, err := NewDocs(DocsOptions{
		GlobalDir:   filepath.Join(base, "global"),
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ctx := context.Background()

	mixed := Record{Content: "the deploy script lives in bin/ship", Scope: scope.Local, Project: projectA, Dir: b}
	_, err = d.Store(ctx, mixed)
	if err == nil || !strings.Contains(err.Error(), "different projects") {
		t.Fatalf("a project and a directory naming two checkouts were accepted: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(b, "docs")); !os.IsNotExist(serr) {
		t.Fatalf("the other checkout was written into: %v", serr)
	}
	if _, lerr := d.List(ctx, Query{Scope: scope.Local, Project: projectA}); lerr == nil {
		t.Fatal("the refused pair still bound the identity to a place")
	}

	docsStore(t, d, Record{Content: "the deploy script lives in bin/ship", Scope: scope.Local, Project: projectA, Dir: a})
	if body := readFile(t, filepath.Join(a, "docs", "NOTES.shoulder.md")); !strings.Contains(body, "bin/ship") {
		t.Fatalf("the matching pair did not land in its own checkout:\n%s", body)
	}
}

// countingEmbedder is stubEmbedder with a tally, so a test can tell a record
// embedded once from one embedded again on every read.
type countingEmbedder struct {
	stubEmbedder
	n int
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	c.n++
	return c.stubEmbedder.Embed(ctx, text)
}

// A fact can arrive with newlines in it and a file holds one line, so the
// sentence is flattened before it is hashed and embedded. Hashing the original
// instead puts an id on the line that is not the hash of the sentence beside
// it, so every read misses the cached vector and embeds the record again, and
// storing what the file actually says is refused as a near-duplicate rather
// than recognised as the same fact.
func TestDocsHashesAndEmbedsTheSentenceItWrites(t *testing.T) {
	base := t.TempDir()
	emb := &countingEmbedder{stubEmbedder: stubEmbedder{id: "stub-v1"}}
	d := newDocsIn(t, base, emb, docsRoots(base))
	ctx := context.Background()
	const project = "/srv/app"

	id := docsStore(t, d, Record{
		Content: "the api is versioned\nin the path", Category: "structure",
		Scope: scope.Local, Project: project,
	})
	if want := contentID("the api is versioned in the path"); id != want {
		t.Fatalf("the id is %s, want %s, the hash of the sentence on the line", id, want)
	}
	if emb.n != 1 {
		t.Fatalf("the store embedded %d times, want once", emb.n)
	}
	for i := 0; i < 3; i++ {
		if _, err := d.Search(ctx, Query{Text: "versioned api", Limit: 5, Scope: scope.Local, Project: project}); err != nil {
			t.Fatalf("search: %v", err)
		}
	}
	if emb.n != 4 {
		t.Errorf("three searches cost %d embeddings beyond the queries; the record is embedded again on every read", emb.n-4)
	}
	_, err := d.Store(ctx, Record{
		Content: "the api is versioned in the path", Category: "structure",
		Scope: scope.Local, Project: project,
	})
	if !errors.Is(err, ErrDuplicateExact) {
		t.Fatalf("storing the sentence the file holds gave %v, want ErrDuplicateExact", err)
	}
}

// A privacy move is the one write that has to touch two files, and it is the
// one place where a failure between them could take a fact and its correction
// with it. The replacement therefore goes down first, and this test makes the
// removal of the original fail to prove it: nothing is lost, and the retry
// finishes the move rather than reporting the replacement as a second fact.
func TestDocsSupersedeStoresTheReplacementBeforeItRemovesTheOriginal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file with no permission bits, so the drop cannot be made to fail this way")
	}
	base := t.TempDir()
	d := newDocsIn(t, base, nil, nil)
	ctx := context.Background()
	const project = "/repos/durable"
	dir := docsDirOf(base, project)
	arch := filepath.Join(dir, "ARCHITECTURE.shoulder.md")
	user := filepath.Join(dir, "USER.shoulder.md")

	rec := Record{Content: "the integration tests need a live Postgres", Category: "structure", Scope: scope.Local, Project: project}
	id := docsStore(t, d, rec)
	private := rec
	private.Content, private.Private = "the integration tests need the Postgres on 5433 here", true
	newID := contentID(private.Content)

	// A read first, so the parse of the committed file is cached and the
	// supersede meets the unreadable file only where it drops the original.
	if _, err := d.List(ctx, Query{Scope: scope.Local, Project: project, Limit: 10}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := os.Chmod(arch, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(arch, 0o600) })

	if _, err := d.Supersede(ctx, id, private); err == nil {
		t.Fatal("supersede reported success although the original could not be removed")
	}
	if got := readFile(t, user); !strings.Contains(got, "5433") || !strings.Contains(got, newID) {
		t.Fatalf("the replacement was not stored before the removal was attempted:\n%s", got)
	}
	held, err := d.List(ctx, Query{Scope: scope.Local, Project: project, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("the store holds %d records, want the original and its replacement: a failed move must lose neither", len(held))
	}

	// The retry is what closes the window, and it has to read the copy at the
	// destination as its own unfinished work rather than as a fact already
	// stored somewhere else.
	if chmodErr := os.Chmod(arch, 0o600); chmodErr != nil {
		t.Fatalf("chmod: %v", chmodErr)
	}
	got, err := d.Supersede(ctx, id, private)
	if err != nil {
		t.Fatalf("the retry did not finish the move: %v", err)
	}
	if got != newID {
		t.Fatalf("the retry returned %q, want %q", got, newID)
	}
	if body := readFile(t, arch); strings.Contains(body, id) {
		t.Fatalf("the original is still in the committed file:\n%s", body)
	}
	if body := readFile(t, user); strings.Count(body, newID) != 1 {
		t.Fatalf("the private file holds the replacement %d times, want once:\n%s", strings.Count(body, newID), body)
	}
	held, err = d.List(ctx, Query{Scope: scope.Local, Project: project, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(held) != 1 || held[0].ID != newID {
		t.Fatalf("the store holds %v, want the replacement alone", held)
	}
}

// Forget takes every copy of an id, not the first one it finds, because the
// state a failed move leaves behind is exactly the state where forgetting one
// of two copies would tell a person a fact is gone while the repository still
// carried it.
func TestDocsForgetTakesEveryCopyOfARecord(t *testing.T) {
	base := t.TempDir()
	d := newDocsIn(t, base, nil, nil)
	ctx := context.Background()
	const project = "/repos/copies"
	dir := docsDirOf(base, project)

	rec := Record{Content: "the release rota is kept in docs/rota.md", Scope: scope.Local, Project: project, CreatedAt: time.Unix(1, 0).UTC()}
	id := docsStore(t, d, rec)
	// The second copy by hand, which is what the window between the two writes
	// of a move looks like from the outside.
	rec.ID = id
	user := filepath.Join(dir, "USER.shoulder.md")
	if err := os.WriteFile(user, []byte(docsHeader("USER.shoulder.md")+formatDocsLine(rec)+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	held, err := d.List(ctx, Query{Scope: scope.Local, Project: project, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("the store lists %d records for one id, want one", len(held))
	}
	if held[0].Private {
		t.Fatal("the private copy was preferred: a fact still in a committed file must not read as kept out of it")
	}

	if err := d.Forget(ctx, id, Query{Kind: KindFact, Scope: scope.Local, Project: project}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	for _, name := range []string{"NOTES.shoulder.md", "USER.shoulder.md"} {
		if body := readFile(t, filepath.Join(dir, name)); strings.Contains(body, id) {
			t.Fatalf("%s still holds the record:\n%s", name, body)
		}
	}
}

// A repository that checks its files out with carriage returns gets one bullet
// changed, not every line of the file rewritten. The whole-file diff is worse
// than useless: it hides what the daemon did behind five hundred lines nobody
// can review, in a file the team owns.
func TestDocsKeepsTheLineEndingsAFileAlreadyUses(t *testing.T) {
	base := t.TempDir()
	d := newDocsIn(t, base, nil, nil)
	ctx := context.Background()
	const project = "/repos/crlf"
	dir := docsDirOf(base, project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "NOTES.shoulder.md")

	seeded := Record{Content: "the release rota is kept in docs/rota.md", Scope: scope.Local, Project: project, CreatedAt: time.Unix(1, 0).UTC()}
	seeded.ID = contentID(seeded.Content)
	const prose = "Some prose a person wrote."
	body := strings.Join([]string{"# Notes", "", prose, "", formatDocsLine(seeded)}, "\r\n") + "\r\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	crlf := func(t *testing.T, why string) string {
		t.Helper()
		got := readFile(t, path)
		if n, all := strings.Count(got, "\r\n"), strings.Count(got, "\n"); n != all {
			t.Fatalf("%s: %d of %d line endings kept their carriage return:\n%q", why, n, all, got)
		}
		if !strings.Contains(got, prose) {
			t.Fatalf("%s: the prose is gone:\n%q", why, got)
		}
		return got
	}

	id := docsStore(t, d, Record{Content: "lunch is at one", Scope: scope.Local, Project: project})
	if got := crlf(t, "after a store"); !strings.Contains(got, id) {
		t.Fatalf("the new record is not in the file:\n%q", got)
	}
	newID, err := d.Supersede(ctx, id, Record{Content: "lunch is at half past twelve", Scope: scope.Local, Project: project})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if got := crlf(t, "after a supersede"); !strings.Contains(got, newID) {
		t.Fatalf("the replacement is not in the file:\n%q", got)
	}
	if err := d.Forget(ctx, newID, Query{Kind: KindFact, Scope: scope.Local, Project: project}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got := crlf(t, "after a forget"); !strings.Contains(got, seeded.ID) {
		t.Fatalf("forgetting one record took the other:\n%q", got)
	}
}

// A file the connector creates is its own, and gets the ending everything else
// in the repository will be diffed against on the platform the daemon runs on.
func TestDocsWritesNewFilesWithNewlines(t *testing.T) {
	base := t.TempDir()
	d := newDocsIn(t, base, nil, nil)
	const project = "/repos/fresh"
	docsStore(t, d, Record{Content: "lunch is at one", Scope: scope.Local, Project: project})
	if got := readFile(t, filepath.Join(docsDirOf(base, project), "NOTES.shoulder.md")); strings.Contains(got, "\r") {
		t.Fatalf("a file the connector created has carriage returns in it:\n%q", got)
	}
}

func TestDominantEOLIsTheOneMostOfTheFileUses(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", docsLF},
		{"newlines", "a\nb\nc\n", docsLF},
		{"carriage returns", "a\r\nb\r\nc\r\n", docsCRLF},
		{"mostly carriage returns", "a\r\nb\r\nc\n", docsCRLF},
		{"mostly newlines", "a\r\nb\nc\n", docsLF},
		{"no ending at all", "a", docsLF},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := dominantEOL([]byte(c.raw)); got != c.want {
				t.Fatalf("dominantEOL(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestDocsJudgesARestatementByTheSameTableTheLocalStoreDoes exists because the
// two stores can be changed one at a time: a threshold that moved for facts in
// JSON and not for facts in Markdown would make the same write land in one
// install and be refused in the other, and the pipeline's answer to a refusal
// is to overwrite the record it collided with. The comparison is shared code
// and this is the test that says so.
func TestDocsJudgesARestatementByTheSameTableTheLocalStoreDoes(t *testing.T) {
	ctx := context.Background()
	const (
		stored = "the daemon keeps its log in one file"
		second = "the daemon keeps its log in one place"
	)
	// 0.945 by the embedding and 0.875 by words in common: over the compiled-in
	// table's threshold and under MiniLM's.
	angles := map[string]float64{stored: 0, second: math.Acos(0.945)}

	write := func(t *testing.T, model string) error {
		t.Helper()
		d := newDocsIn(t, t.TempDir(), angledEmbedder{id: model, angles: angles}, nil)
		if _, err := d.Store(ctx, Record{Content: stored, Scope: scope.Global}); err != nil {
			t.Fatalf("store: %v", err)
		}
		_, err := d.Store(ctx, Record{Content: second, Scope: scope.Global})
		return err
	}

	var dup *ErrDuplicateSemantic
	if err := write(t, "some-other-table-v1"); !errors.As(err, &dup) {
		t.Fatalf("got %v under a model with no thresholds of its own, want ErrDuplicateSemantic", err)
	}
	if err := write(t, "all-minilm-l6-v2-f32-v1"); err != nil {
		t.Fatalf("refused at 0.945 under MiniLM, want it kept: Docs and Local must draw the line in the same place: %v", err)
	}
}

// The docs store is the one whose records a person edits by hand, so it is the
// one most likely to be told the opposite of what it holds. It has to refuse
// the denial and name the claim, exactly as the JSON store does: the two share
// the rule and a divergence here would mean a daemon switched between them
// keeps a fact and its denial in one and not the other.
func TestDocsRefusesTheDenialOfAStoredClaim(t *testing.T) {
	ctx := context.Background()
	for _, pair := range []struct{ claim, denial string }{
		{"logs are kept for thirty days", "logs are not kept for thirty days"},
		{"we use rebase on shared branches", "we do not use rebase on shared branches"},
		{"releases ship on Fridays", "releases never ship on Fridays"},
		{"the test suite needs network access", "the test suite does not need network access"},
	} {
		t.Run(pair.denial, func(t *testing.T) {
			d, _ := newDocs(t)
			id := docsStore(t, d, Record{Content: pair.claim, Scope: scope.Global})
			_, err := d.Store(ctx, Record{Content: pair.denial, Scope: scope.Global})
			var dup *ErrDuplicateSemantic
			if !errors.As(err, &dup) {
				t.Fatalf("got %v, want ErrDuplicateSemantic: the file would hold the claim and its denial at once", err)
			}
			if dup.Collided != id {
				t.Fatalf("collided with %q, want the contradicted claim %q", dup.Collided, id)
			}
		})
	}
}

// TestTheDocsStoreAppliesTheGuardToo, because the ranker is shared and the
// two stores are the two places a fact can land.
func TestTheDocsStoreAppliesTheGuardToo(t *testing.T) {
	ctx := context.Background()
	d := newDocsIn(t, t.TempDir(), vectors.Embedder{}, nil)
	first := Record{Content: "the frontend is deployed to Cloudflare", Scope: scope.Global}
	if _, err := d.Store(ctx, first); err != nil {
		t.Fatalf("store: %v", err)
	}
	second := Record{Content: "the backend is deployed to Cloudflare", Scope: scope.Global}
	if _, err := d.Store(ctx, second); err != nil {
		t.Fatalf("the second fact was refused: %v", err)
	}
}
