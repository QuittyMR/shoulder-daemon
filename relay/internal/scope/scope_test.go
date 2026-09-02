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

func TestProjectReturnsTheWorktreeRootFromAnySubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	deep := filepath.Join(root, "internal", "memory")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	want := resolve(t, root)
	for _, dir := range []string{root, deep} {
		got, err := Project(dir)
		if err != nil {
			t.Fatalf("Project(%q): %v", dir, err)
		}
		if resolve(t, got) != want {
			t.Errorf("Project(%q) = %q; every directory in one checkout must share %q", dir, got, want)
		}
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
