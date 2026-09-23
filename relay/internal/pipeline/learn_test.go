package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/budget"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/metrics"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/settings"
)

// docModel answers one chunk at a time. byName lets a test give one document
// facts and another none, which is the difference --replace turns on.
type docModel struct {
	mu     sync.Mutex
	asked  []string
	reply  string
	byName map[string]string
	broken map[string]bool
}

func (m *docModel) Name() string { return "fake" }

func (m *docModel) Complete(_ context.Context, system, user string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, user)
	if system != prompts.Learn {
		return "", errors.New("learn must send the extraction prompt, not the turn one")
	}
	for name, broken := range m.broken {
		if broken && strings.Contains(user, "<document>"+name+"</document>") {
			return "", errors.New("the model fell over")
		}
	}
	for name, reply := range m.byName {
		if strings.Contains(user, "<document>"+name+"</document>") {
			return reply, nil
		}
	}
	return m.reply, nil
}

func (m *docModel) Chat(context.Context, []llm.Message, []llm.Tool) (llm.Message, error) {
	return llm.Message{}, errors.New("learn asks one question at a time")
}

// documents is what the model was shown, in the order it was shown them.
func (m *docModel) documents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, user := range m.asked {
		if _, rest, ok := strings.Cut(user, "<document>"); ok {
			name, _, _ := strings.Cut(rest, "</document>")
			out = append(out, name)
		}
	}
	return out
}

func learnPipe(t *testing.T, model llm.Provider) (*Pipeline, *fakeMemory) {
	t.Helper()
	mem := &fakeMemory{
		recalled: map[scope.Scope][]memory.Record{},
		listed:   map[scope.Scope][]memory.Record{},
	}
	cfg := config.Load()
	cfg.Budget = budget.Default()
	p := &Pipeline{
		Cfg:      cfg,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:  metrics.New(),
		Memory:   memory.Checked(mem),
		Settings: settings.ForProvider(model),
	}
	return p, mem
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// committed is a worktree with everything in it committed, which is the only
// state --replace will delete from.
func committed(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
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
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: git with the test's own arguments
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

const oneFact = `{"facts":[{"content":"Deploys go to eu-west-2.","category":"decision","scope":"local","tags":["deploy"]}]}`

func TestLearnReadsWhatARepositoryDocumentsAndLeavesTheRest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"DESIGN.md", "README.md", "CHANGELOG.md", "LICENSE.md", "CONTRIBUTING.md",
		"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "notes.txt",
		"docs/ARCHITECTURE.md", "docs/ARCHITECTURE.shoulder.md", "docs/USER.shoulder.md",
		"docs/.private/secret.md", "docs/node_modules/pkg/GUIDE.md", "docs/build/out.md",
		"doc/OLD.md", ".claude/commands/thing.md",
	} {
		write(t, filepath.Join(dir, name), "# Heading\n\nDeploys go to eu-west-2.\n")
	}

	model := &docModel{reply: oneFact}
	p, mem := learnPipe(t, model)
	got, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	var read []string
	for _, f := range got.Files {
		read = append(read, f.Path)
	}
	want := []string{"DESIGN.md", filepath.Join("doc", "OLD.md"), filepath.Join("docs", "ARCHITECTURE.md")}
	if strings.Join(read, ",") != strings.Join(want, ",") {
		t.Fatalf("read %v, want %v", read, want)
	}
	if names := model.documents(); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("the model was shown %v", names)
	}
	for _, f := range got.Files {
		if f.Chunks != 1 || f.Stored != 1 || f.Skipped != 0 || f.Failed != 0 || f.Deleted {
			t.Fatalf("%+v", f)
		}
	}

	// The scope was chosen at the command line. The model said local for every
	// one of them and is not the one deciding.
	stored, _, _ := mem.snapshot()
	if len(stored) != 3 {
		t.Fatalf("stored %d records, want one per document", len(stored))
	}
	for _, r := range stored {
		if r.Scope != scope.Global || r.Project != "" {
			t.Fatalf("stored %+v, want a global record", r)
		}
		if r.Category != "decision" || len(r.Tags) != 1 {
			t.Fatalf("stored %+v, want the category and tags the model gave", r)
		}
	}
	if p.Metrics.Get("shoulder_cli_learned_total") != 3 {
		t.Fatalf("counted %d facts learned", p.Metrics.Get("shoulder_cli_learned_total"))
	}
}

func TestLearnFilesEveryFactUnderTheProjectItWasRunIn(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "DESIGN.md"), "# Design\n\nDeploys go to eu-west-2.\n")

	p, mem := learnPipe(t, &docModel{
		reply: `{"facts":[{"content":"The user wants terse answers.","category":"preference","scope":"global"}]}`,
	})
	if _, err := p.Learn(context.Background(), LearnRequest{
		Scope: scope.Local, Project: "app@0123abcd", Dir: dir,
	}); err != nil {
		t.Fatal(err)
	}
	stored, _, _ := mem.snapshot()
	if len(stored) != 1 || stored[0].Scope != scope.Local || stored[0].Project != "app@0123abcd" {
		t.Fatalf("stored %+v", stored)
	}
	// A preference is the person's own wherever it was read, and a backend
	// that files by that must not commit it with the team's code.
	if !stored[0].Private {
		t.Fatal("a preference read out of a document was not marked private")
	}
}

func TestLearnNamedPathsOverrideTheDefaultListButNotTheProtectedFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "README.md"), "# Readme\n\nDeploys go to eu-west-2.\n")
	write(t, filepath.Join(dir, "CLAUDE.md"), "# Orders\n\nAlways run the linter.\n")
	write(t, filepath.Join(dir, "docs", "ARCHITECTURE.shoulder.md"), "- already stored\n")
	write(t, filepath.Join(dir, "elsewhere", "GUIDE.md"), "# Guide\n\nDeploys go to eu-west-2.\n")

	p, _ := learnPipe(t, &docModel{reply: oneFact})
	got, err := p.Learn(context.Background(), LearnRequest{
		Scope: scope.Global, Dir: dir,
		Paths: []string{"README.md", "CLAUDE.md", "docs", "elsewhere"},
	})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]LearnedFile{}
	for _, f := range got.Files {
		by[f.Path] = f
	}
	if f, ok := by["README.md"]; !ok || f.Stored != 1 {
		t.Fatalf("a file named outright must be read: %+v", got.Files)
	}
	if f, ok := by[filepath.Join("elsewhere", "GUIDE.md")]; !ok || f.Stored != 1 {
		t.Fatalf("a directory named outright must be walked: %+v", got.Files)
	}
	if f, ok := by["CLAUDE.md"]; !ok || f.Error == "" || f.Chunks != 0 {
		t.Fatalf("the agent's own instructions were read: %+v", by["CLAUDE.md"])
	}
	if _, ok := by[filepath.Join("docs", "ARCHITECTURE.shoulder.md")]; ok {
		t.Fatalf("the daemon read its own file back: %+v", got.Files)
	}
}

func TestLearnRefusesToDeleteAnythingFromADirtyWorktree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docs", "ARCHITECTURE.md"), "# Design\n\nDeploys go to eu-west-2.\n")
	committed(t, dir)
	write(t, filepath.Join(dir, "docs", "ARCHITECTURE.md"), "# Design\n\nDeploys go to eu-west-1.\n")

	p, mem := learnPipe(t, &docModel{reply: oneFact})
	_, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir, Replace: true})
	if err == nil {
		t.Fatal("--replace deleted from a worktree git could not restore")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("error %q does not say why", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "docs", "ARCHITECTURE.md")); statErr != nil {
		t.Fatalf("the document is gone: %v", statErr)
	}
	if stored, _, _ := mem.snapshot(); len(stored) != 0 {
		t.Fatalf("a refused run still wrote %+v", stored)
	}
}

func TestLearnDeletesOnlyTheDocumentsTheStoreTook(t *testing.T) {
	dir := t.TempDir()
	kept := filepath.Join(dir, "docs", "EMPTY.md")
	gone := filepath.Join(dir, "docs", "RULES.md")
	broke := filepath.Join(dir, "docs", "BROKEN.md")
	write(t, kept, "# Empty\n\nThis document explains nothing.\n")
	write(t, gone, "# Rules\n\nDeploys go to eu-west-2.\n")
	write(t, broke, "# Broken\n\nDeploys go to eu-west-3.\n")
	committed(t, dir)

	model := &docModel{
		reply:  `{"facts":[]}`,
		byName: map[string]string{filepath.Join("docs", "RULES.md"): oneFact},
		broken: map[string]bool{filepath.Join("docs", "BROKEN.md"): true},
	}
	p, _ := learnPipe(t, model)
	got, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir, Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Files {
		switch f.Path {
		case filepath.Join("docs", "RULES.md"):
			if !f.Deleted {
				t.Fatalf("%+v: a document the store took was kept", f)
			}
		case filepath.Join("docs", "BROKEN.md"):
			if f.Failed != 1 || f.Deleted {
				t.Fatalf("%+v: a document the model choked on was not reported or was deleted", f)
			}
		default:
			if f.Stored != 0 || f.Deleted {
				t.Fatalf("%+v: a document nothing was taken from was deleted", f)
			}
		}
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("RULES.md is still there: %v", err)
	}
	for _, path := range []string{kept, broke} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was deleted: %v", path, err)
		}
	}
}

func TestLearnRefusesToDeleteWhatIsOutsideTheWorktree(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "NOTES.md")
	write(t, filepath.Join(dir, "docs", "ARCHITECTURE.md"), "# Design\n\nDeploys go to eu-west-2.\n")
	write(t, outside, "# Notes\n\nDeploys go to eu-west-2.\n")
	committed(t, dir)

	p, _ := learnPipe(t, &docModel{reply: oneFact})
	_, err := p.Learn(context.Background(), LearnRequest{
		Scope: scope.Global, Dir: dir, Paths: []string{outside}, Replace: true,
	})
	if err == nil {
		t.Fatal("--replace deleted a file from outside the worktree it checked")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("the document is gone: %v", statErr)
	}
}

func TestLearnNeedsAModelAndAScope(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "DESIGN.md"), "# Design\n\nDeploys go to eu-west-2.\n")

	if p, _ := learnPipe(t, nil); true {
		if _, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir}); err == nil {
			t.Fatal("learn ran with no model to read the documents")
		}
	}
	p, _ := learnPipe(t, &docModel{reply: oneFact})
	for _, req := range []LearnRequest{
		{Dir: dir},
		{Scope: scope.Local, Dir: dir},
		{Scope: scope.Global},
	} {
		if _, err := p.Learn(context.Background(), req); err == nil {
			t.Fatalf("%+v was accepted", req)
		}
	}
}

func TestChunkingCutsAtHeadingsAndNeverInsideAFence(t *testing.T) {
	doc := "# Title\n\nintro\n\n## One\n\n```sh\n# not a heading\necho hi\n```\n\n## Two\n\nbody\n"

	if whole := chunkMarkdown(doc, 4096); len(whole) != 1 || whole[0] != strings.TrimRight(doc, "\n") {
		t.Fatalf("a document inside the window must arrive whole, got %d chunks", len(whole))
	}

	got := chunkMarkdown(doc, 45)
	if len(got) != 3 {
		t.Fatalf("got %d chunks: %q", len(got), got)
	}
	for i, want := range []string{"# Title", "## One", "## Two"} {
		if !strings.HasPrefix(got[i], want) {
			t.Fatalf("chunk %d is %q, want it to start at %q", i, got[i], want)
		}
	}
	// The shell comment is a comment. Splitting there hands the model half a
	// command and calls it a document.
	if !strings.Contains(got[1], "# not a heading\necho hi") {
		t.Fatalf("the fenced block was cut: %q", got[1])
	}
	// Four spaces of indent are a code block too, fence or no fence.
	if one := sections("# T\n\nrun:\n\n    # not a heading\n    make build\n"); len(one) != 1 {
		t.Fatalf("an indented code block was read as a heading: %q", one)
	}
}

func TestAChunkIsNeverLargerThanTheWindow(t *testing.T) {
	var b strings.Builder
	b.WriteString("# One long section\n\n")
	for i := 0; i < 200; i++ {
		b.WriteString("a line of documentation that says something about the project\n")
	}
	b.WriteString("\n## Then a short one\n\nbody\n")

	for _, limit := range []int{200, 1000, 4096} {
		for _, chunk := range chunkMarkdown(b.String(), limit) {
			if len(chunk) > limit {
				t.Fatalf("limit %d produced a chunk of %d", limit, len(chunk))
			}
		}
	}
	// A single line longer than the window is cut rather than dropped.
	long := "# T\n\n" + strings.Repeat("x", 500) + "\n"
	var joined string
	for _, chunk := range chunkMarkdown(long, 100) {
		if len(chunk) > 100 {
			t.Fatalf("a long line produced a chunk of %d", len(chunk))
		}
		joined += chunk
	}
	if !strings.Contains(joined, strings.Repeat("x", 500)) {
		t.Fatal("the long line was lost rather than cut")
	}
}

// A document says what it said when it was written, which can be years before
// anything a session learned. A sentence the store refuses as too close to a
// fact it holds is counted and dropped: replacing that fact would let a page
// nobody has reread undo a correction made last week.
func TestLearnLeavesTheFactADocumentCollidesWith(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docs", "GUIDE.md"), "# Guide\n\nDeploys go to eu-west-2.\n")

	p, _ := learnPipe(t, &docModel{reply: oneFact})
	mem := &refusingMemory{refuseWith: "mem_held"}
	p.Memory = memory.Checked(mem)

	got, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0].Skipped != 1 || got.Files[0].Stored != 0 {
		t.Fatalf("a collision was not counted as skipped: %+v", got.Files)
	}
	stored, superseded, _ := mem.snapshot()
	if len(superseded) != 0 {
		t.Fatalf("a document superseded the fact it collided with: %v", superseded)
	}
	if len(stored) != 0 {
		t.Fatalf("the collision was written anyway: %+v", stored)
	}
}

// A clean `git status` is not the promise --replace needs. It says nothing
// about a file git is ignoring, so a document that has never been committed
// reads as safe to delete while `git checkout` has nothing to give back.
func TestLearnRefusesToDeleteAnIgnoredDocument(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docs", "ARCHITECTURE.md"), "# Design\n\nDeploys go to eu-west-2.\n")
	write(t, filepath.Join(dir, ".gitignore"), "docs/DRAFT.md\n")
	committed(t, dir)
	draft := filepath.Join(dir, "docs", "DRAFT.md")
	write(t, draft, "# Draft\n\nDeploys go to eu-west-2.\n")

	p, mem := learnPipe(t, &docModel{reply: oneFact})
	_, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir, Replace: true})
	if err == nil {
		t.Fatal("--replace accepted a document git has never held")
	}
	if !strings.Contains(err.Error(), filepath.Join("docs", "DRAFT.md")) {
		t.Fatalf("the refusal does not name the document: %v", err)
	}
	if _, statErr := os.Stat(draft); statErr != nil {
		t.Fatalf("the document is gone: %v", statErr)
	}
	if stored, _, _ := mem.snapshot(); len(stored) != 0 {
		t.Fatalf("a refused run still wrote %+v", stored)
	}
}

// status.showUntrackedFiles=no is a setting people have, and the refusal above
// promises the worktree holds nothing a commit has not taken. The flag on the
// command line is what keeps that promise true whatever the person's config
// says.
func TestLearnLooksForUntrackedFilesGitStatusWasToldToHide(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docs", "ARCHITECTURE.md"), "# Design\n\nDeploys go to eu-west-2.\n")
	committed(t, dir)
	gitIn(t, dir, "config", "status.showUntrackedFiles", "no")
	write(t, filepath.Join(dir, "scratch.txt"), "notes to self\n")

	p, mem := learnPipe(t, &docModel{reply: oneFact})
	_, err := p.Learn(context.Background(), LearnRequest{Scope: scope.Global, Dir: dir, Replace: true})
	if err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("--replace ran in a worktree holding work git had not been told about: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "docs", "ARCHITECTURE.md")); statErr != nil {
		t.Fatalf("the document is gone: %v", statErr)
	}
	if stored, _, _ := mem.snapshot(); len(stored) != 0 {
		t.Fatalf("a refused run still wrote %+v", stored)
	}
}
