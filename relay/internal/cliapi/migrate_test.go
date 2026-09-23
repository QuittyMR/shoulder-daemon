package cliapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// docsIn is a docs store whose files all live under base: one directory for
// the global scope and one per project, and no worktree, so nothing here
// reaches for git or for the machine this runs on.
func docsIn(t *testing.T, base string) *memory.Docs {
	t.Helper()
	d, err := memory.NewDocs(memory.DocsOptions{
		Roots: func(s scope.Scope, project, _ string) (string, string) {
			if s == scope.Global {
				return filepath.Join(base, "global"), ""
			}
			return filepath.Join(base, "projects", scope.Key(project)), ""
		},
		SessionPath: filepath.Join(base, "session.json"),
		CacheDir:    filepath.Join(base, "cache"),
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("open docs store: %v", err)
	}
	return d
}

// oldStore is a JSON store as a daemon would have left it before the person
// pointed theirs at another backend.
func oldStore(t *testing.T, path string, recs ...memory.Record) {
	t.Helper()
	l, err := memory.NewLocal(path, nil)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	for _, r := range recs {
		if _, err := l.Store(context.Background(), r); err != nil {
			t.Fatalf("store %q: %v", r.Content, err)
		}
	}
}

// unsetMemoryEnv keeps the machine's own settings out of a test about which
// store the daemon is holding.
func unsetMemoryEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SHOULDER_ENV_FILE", filepath.Join(t.TempDir(), "absent"))
	config.ResetEnvFile()
	t.Cleanup(config.ResetEnvFile)
	t.Setenv("SHOULDER_MEMORY", "")
	t.Setenv("SHOULDER_MEMORY_URL", "")
	t.Setenv("SHOULDER_MEMORY_PATH", filepath.Join(t.TempDir(), "facts.json"))
}

func migrate(t *testing.T, h http.Handler, body string) MigrateResponse {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/cli/migrate", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	return decode[MigrateResponse](t, rec)
}

const migrateProject = "app@0123abcd"

func TestMigrateMovesOneScopeOfTheJSONStoreIntoTheRunningOne(t *testing.T) {
	unsetMemoryEnv(t)
	base := t.TempDir()
	source := filepath.Join(base, "old-facts.json")
	first := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	oldStore(t, source,
		memory.Record{
			Content: "the api listens on 8081", Category: "structure",
			Tags: []string{"api"}, Scope: scope.Local, Project: migrateProject, CreatedAt: first,
		},
		memory.Record{
			Content: "deployments run from ci", Category: "structure",
			Scope: scope.Local, Project: migrateProject, CreatedAt: first.Add(time.Hour),
		},
		memory.Record{
			Content: "prefers terse answers", Category: "preference",
			Scope: scope.Local, Project: migrateProject, CreatedAt: first.Add(2 * time.Hour),
		},
		memory.Record{Content: "always runs the linter", Category: "preference", Scope: scope.Global},
		memory.Record{
			Content: "was looking at the queue", Kind: memory.KindSession, Session: "s1",
			Scope: scope.Local, Project: migrateProject,
		},
	)
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	h, m := newTestServerWith(t, "", nil, docsIn(t, base))

	got := migrate(t, h, `{"scope":"local","project":"`+migrateProject+`","from":"`+source+`"}`)
	if got.Stored != 3 || got.Skipped != 0 || got.Failed != 0 {
		t.Fatalf("%+v: want three facts stored and nothing else", got)
	}
	if got.From != source {
		t.Fatalf("from = %q, want the file that was read (%s)", got.From, source)
	}
	if m.Get("shoulder_cli_migrated_total") != 3 {
		t.Fatalf("counted %d migrations", m.Get("shoulder_cli_migrated_total"))
	}

	project := filepath.Join(base, "projects", scope.Key(migrateProject))
	arch := read(t, filepath.Join(project, "ARCHITECTURE.shoulder.md"))
	if !strings.Contains(arch, "the api listens on 8081") || !strings.Contains(arch, "deployments run from ci") {
		t.Fatalf("the structure facts are not in the architecture file:\n%s", arch)
	}
	// Oldest first, so the file reads in the order the facts were learned.
	if strings.Index(arch, "the api listens on 8081") > strings.Index(arch, "deployments run from ci") {
		t.Fatalf("the facts were written newest first:\n%s", arch)
	}
	if !strings.Contains(arch, "category=structure") || !strings.Contains(arch, "tags=api") {
		t.Fatalf("the category and tags did not survive the move:\n%s", arch)
	}
	if !strings.Contains(arch, "at=2024-01-02T03:04:05Z") {
		t.Fatalf("the fact was redated by the migration:\n%s", arch)
	}
	// A preference is the person's own: it belongs in the private file, not in
	// the one that is committed with the code.
	user := read(t, filepath.Join(project, "USER.shoulder.md"))
	if !strings.Contains(user, "prefers terse answers") {
		t.Fatalf("the preference is not in the private file:\n%s", user)
	}
	for _, name := range docsFiles(t, project) {
		body := read(t, name)
		if strings.Contains(body, "was looking at the queue") {
			t.Fatalf("a working note was migrated into %s", name)
		}
		if strings.Contains(body, "always runs the linter") {
			t.Fatalf("a global fact was migrated into the project's %s", name)
		}
	}

	// Running it again writes nothing and names the record that already says
	// each thing.
	again := migrate(t, h, `{"scope":"local","project":"`+migrateProject+`","from":"`+source+`"}`)
	if again.Stored != 0 || again.Skipped != 3 {
		t.Fatalf("%+v: a second run must change nothing", again)
	}
	stored := map[string]bool{}
	for _, f := range got.Facts {
		stored[f.ID] = true
	}
	for _, f := range again.Facts {
		if f.Outcome != MigrateSkipped || !stored[f.Collided] {
			t.Fatalf("%+v does not name the record the first run wrote (%v)", f, stored)
		}
	}

	global := migrate(t, h, `{"scope":"global","from":"`+source+`"}`)
	if global.Stored != 1 {
		t.Fatalf("%+v: the global half was not migrated", global)
	}
	if body := read(t, filepath.Join(base, "global", "USER.shoulder.md")); !strings.Contains(body, "always runs the linter") {
		t.Fatalf("the global preference is not where it belongs:\n%s", body)
	}

	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the store that was migrated was written to")
	}
}

// A record the destination refuses for a reason of its own is reported rather
// than stopping the ones behind it.
func TestMigrateReportsWhatTheStoreWouldNotTake(t *testing.T) {
	unsetMemoryEnv(t)
	base := t.TempDir()
	source := filepath.Join(base, "old-facts.json")
	oldStore(t, source,
		memory.Record{Content: "the api listens on 8081", Scope: scope.Global},
		memory.Record{Content: "the linter runs before every push", Scope: scope.Global},
	)
	mem := newFakeMemory()
	mem.storeErr = errors.New("the store is on fire")
	h, _ := newTestServerWith(t, "", nil, mem)

	got := migrate(t, h, `{"scope":"global","from":"`+source+`"}`)
	if got.Stored != 0 || got.Failed != 2 || len(got.Facts) != 2 {
		t.Fatalf("%+v: both records should be reported as failed", got)
	}
	for _, f := range got.Facts {
		if f.Outcome != MigrateFailed || !strings.Contains(f.Error, "on fire") {
			t.Fatalf("%+v does not carry the reason it was refused", f)
		}
	}
}

// A record the destination refuses as a restatement of one it holds is skipped
// like a verbatim duplicate, and carries the record that blocked it so the
// person can go and look at the pair.
func TestMigrateSkipsWhatTheStoreCallsARestatement(t *testing.T) {
	unsetMemoryEnv(t)
	source := filepath.Join(t.TempDir(), "old-facts.json")
	oldStore(t, source, memory.Record{Content: "the api listens on 8081", Scope: scope.Global})
	mem := newFakeMemory()
	mem.storeErr = &memory.ErrDuplicateSemantic{Collided: "mem_9"}
	h, _ := newTestServerWith(t, "", nil, mem)

	got := migrate(t, h, `{"scope":"global","from":"`+source+`"}`)
	if got.Skipped != 1 || got.Stored != 0 || got.Failed != 0 {
		t.Fatalf("%+v: a restatement is skipped, not lost", got)
	}
	if got.Facts[0].Collided != "mem_9" {
		t.Fatalf("%+v does not name the record that blocked it", got.Facts[0])
	}
}

// Migrating the running store into itself would refuse every record as a
// duplicate of itself and read as a migration that found nothing.
func TestMigrateRefusesTheStoreTheDaemonIsAlreadyRunning(t *testing.T) {
	unsetMemoryEnv(t)
	source := filepath.Join(t.TempDir(), "facts.json")
	oldStore(t, source, memory.Record{Content: "the api listens on 8081", Scope: scope.Global})
	t.Setenv("SHOULDER_MEMORY_PATH", source)
	h, _ := newTestServerWith(t, "", nil, named{newFakeMemory(), "local"})

	rec := do(t, h, http.MethodPost, "/v1/cli/migrate", `{"scope":"global"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	msg := errorOf(t, rec)
	if !strings.Contains(msg, source) || !strings.Contains(msg, "SHOULDER_MEMORY") {
		t.Fatalf("error %q says neither which file nor how to fix it", msg)
	}
}

// A source that is not there is refused rather than reported as an empty
// store: a typo in --from would otherwise print the same line as a successful
// migration of nothing.
func TestMigrateRefusesASourceItCannotRead(t *testing.T) {
	unsetMemoryEnv(t)
	dir := t.TempDir()
	h, _ := newTestServerWith(t, "", nil, docsIn(t, dir))

	for _, from := range []string{filepath.Join(dir, "nowhere.json"), dir} {
		rec := do(t, h, http.MethodPost, "/v1/cli/migrate", `{"scope":"global","from":"`+from+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400: %s", from, rec.Code, rec.Body.String())
		}
		if msg := errorOf(t, rec); !strings.Contains(msg, from) {
			t.Fatalf("error %q does not name the file it could not read", msg)
		}
	}
}

func TestMigrateWithNowhereToWriteIsRefused(t *testing.T) {
	unsetMemoryEnv(t)
	source := filepath.Join(t.TempDir(), "old-facts.json")
	oldStore(t, source, memory.Record{Content: "the api listens on 8081", Scope: scope.Global})
	h, _ := newTestServerWith(t, "", nil, memory.Nop{})

	rec := do(t, h, http.MethodPost, "/v1/cli/migrate", `{"scope":"global","from":"`+source+`"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func docsFiles(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.shoulder.md"))
	if err != nil {
		t.Fatal(err)
	}
	return names
}
