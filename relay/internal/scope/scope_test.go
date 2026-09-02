package scope

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseAcceptsBothScopesWhateverTheCasing(t *testing.T) {
	for _, in := range []string{"local", "LOCAL", " Local ", "lOcAl"} {
		got, err := Parse(in)
		if err != nil || got != Local {
			t.Errorf("Parse(%q) = %q, %v; want local", in, got, err)
		}
	}
	for _, in := range []string{"global", "GLOBAL", "\tGlobal\n"} {
		got, err := Parse(in)
		if err != nil || got != Global {
			t.Errorf("Parse(%q) = %q, %v; want global", in, got, err)
		}
	}
}

// The empty string is the shape an omitted flag arrives in, so it must not be
// merely invalid: the error has to tell the user which two words to choose
// between, because that choice is the one thing the system will never make.
func TestParseRejectsAnUnsetScopeNamingBothOptions(t *testing.T) {
	for _, in := range []string{"", "   "} {
		got, err := Parse(in)
		if err == nil {
			t.Fatalf("Parse(%q) = %q; an unset scope must be an error, never a default", in, got)
		}
		msg := err.Error()
		if !strings.Contains(msg, "local") || !strings.Contains(msg, "global") {
			t.Errorf("Parse(%q) error %q must name both local and global", in, msg)
		}
	}
}

func TestParseRejectsJunk(t *testing.T) {
	for _, in := range []string{"any", "team", "LOCALE", "loca l", "both", "-"} {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q; want an error", in, got)
		}
	}
}

func TestValidIsOnlyTheTwoStorableScopes(t *testing.T) {
	if !Local.Valid() || !Global.Valid() {
		t.Error("local and global must be storable")
	}
	if Any.Valid() {
		t.Error("Any is a query filter, not a scope a record may be stored under")
	}
	if Scope("team").Valid() {
		t.Error("an unknown scope must not be valid")
	}
}

// git resolves symlinks in --show-toplevel while filepath.Abs does not, so the
// two halves of Project are only comparable after the same resolution.
func resolve(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// repo makes a checkout with one commit, which is what gives it an identity.
// The commit has content, because two empty commits made in the same second by
// the same author carry the same hash.
func repo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte(dir), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
		{"add", "seed"}, {"commit", "-m", "root"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestEveryDirectoryInOneCheckoutSharesAProject(t *testing.T) {
	root := t.TempDir()
	repo(t, root)
	deep := filepath.Join(root, "internal", "memory")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	top, err := Project(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Project(deep)
	if err != nil {
		t.Fatal(err)
	}
	if got != top {
		t.Errorf("Project(%q) = %q, want %q", deep, got, top)
	}
	if Label(top) != filepath.Base(resolve(t, root)) {
		t.Errorf("Label(%q) = %q, want the checkout's name", top, Label(top))
	}
}

// A path-derived key orphans every fact the moment a checkout is renamed or
// cloned elsewhere: the store still holds them and no read can see them.
func TestRenamingACheckoutKeepsItsKey(t *testing.T) {
	parent := t.TempDir()
	before := filepath.Join(parent, "before")
	if err := os.Mkdir(before, 0o755); err != nil {
		t.Fatal(err)
	}
	repo(t, before)
	was, err := Project(before)
	if err != nil {
		t.Fatal(err)
	}

	after := filepath.Join(parent, "after")
	if err := os.Rename(before, after); err != nil {
		t.Fatal(err)
	}
	now, err := Project(after)
	if err != nil {
		t.Fatal(err)
	}

	if Key(now) != Key(was) {
		t.Fatalf("Key changed on rename: %q -> %q", Key(was), Key(now))
	}
	if Label(now) != "after" {
		t.Errorf("Label(%q) = %q; the name should follow the directory", now, Label(now))
	}
}

// Two checkouts of different repositories must never share a key, however
// similarly they are named.
func TestTwoRepositoriesAreDifferentProjects(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	repo(t, a)
	repo(t, b)
	pa, err := Project(a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := Project(b)
	if err != nil {
		t.Fatal(err)
	}
	if Key(pa) == Key(pb) {
		t.Fatalf("two repositories share the key %q", Key(pa))
	}
}

func TestProjectFallsBackToTheAbsolutePathOutsideARepository(t *testing.T) {
	dir := t.TempDir()
	got, err := Project(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("Project(%q) = %q; want an absolute path", dir, got)
	}
	if resolve(t, got) != resolve(t, dir) {
		t.Errorf("Project(%q) = %q; want the directory itself", dir, got)
	}
}

func TestProjectAcceptsADirectoryThisMachineCannotSee(t *testing.T) {
	// The daemon is routinely not on the machine the session is: a container
	// sees none of the host's paths. Refusing a directory it cannot stat drops
	// every fact that session produces, so an unreachable path is still a
	// project, identified by what it is called.
	const remote = "/home/someone/work/their-repo"
	got, err := Project(remote)
	if err != nil {
		t.Fatalf("a path this machine cannot see must still resolve: %v", err)
	}
	if got != remote {
		t.Fatalf("got %q, want %q", got, remote)
	}
	if Key(got) == "" {
		t.Fatal("an unreachable project must still have a stable key")
	}
}

func TestProjectAgreesBetweenRelativeAndAbsoluteSpellings(t *testing.T) {
	dir := t.TempDir()
	abs, err := Project(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	rel, err := Project(".")
	if err != nil {
		t.Fatal(err)
	}
	if Key(rel) != Key(abs) {
		t.Fatalf("Key differs by spelling: %q -> %q, %q -> %q", rel, Key(rel), abs, Key(abs))
	}
}

var hex12 = regexp.MustCompile(`^[0-9a-f]{12}$`)

func TestKeyIsTwelveStableHexCharacters(t *testing.T) {
	const path = "/home/someone/src/shoulder-daemon"
	got := Key(path)
	if !hex12.MatchString(got) {
		t.Fatalf("Key(%q) = %q; want 12 hex characters", path, got)
	}
	if again := Key(path); again != got {
		t.Errorf("Key is not stable: %q then %q", got, again)
	}
	// A backend keyed on this must not merge two checkouts.
	if other := Key("/home/someone/src/other-project"); other == got {
		t.Errorf("distinct projects share key %q", got)
	}
	if Key("") != "" {
		t.Errorf("Key(\"\") = %q; an absent project has no key", Key(""))
	}
}

func TestKeyIgnoresPathSpelling(t *testing.T) {
	want := Key("/srv/app")
	for _, spelling := range []string{"/srv/app/", "/srv/./app", "/srv/other/../app"} {
		if got := Key(spelling); got != want {
			t.Errorf("Key(%q) = %q; want %q", spelling, got, want)
		}
	}
}

func TestLabelIsTheDirectoryName(t *testing.T) {
	if got := Label("/home/someone/src/shoulder-daemon"); got != "shoulder-daemon" {
		t.Errorf("got %q", got)
	}
	if got := Label("/home/someone/src/shoulder-daemon/"); got != "shoulder-daemon" {
		t.Errorf("got %q", got)
	}
	if got := Label(""); got != "" {
		t.Errorf("got %q", got)
	}
}
