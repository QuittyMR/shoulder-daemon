package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// envCase is one entry of testdata/dotenv.json: an env file, and the values
// python-dotenv 1.2.2 - the reader podman-compose uses - gives for it with
// HOME=/home/u and EMPTY set to nothing in its environment. The values were
// taken from python-dotenv, not written to agree with parseEnv, and the
// OpenCode adapter's tests read the same file.
type envCase struct {
	Name string            `json:"name"`
	In   string            `json:"in"`
	Want map[string]string `json:"want"`
}

var caseEnv = map[string]string{"HOME": "/home/u", "EMPTY": ""}

func envCases(t *testing.T) []envCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "dotenv.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []envCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestParseEnvReadsTheFileAsComposeDoes(t *testing.T) {
	lookup := func(k string) (string, bool) { v, ok := caseEnv[k]; return v, ok }
	for _, c := range envCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			if got := parseEnv(c.In, lookup); !reflect.DeepEqual(got, c.Want) {
				t.Fatalf("parseEnv(%q)\n got %q\nwant %q", c.In, got, c.Want)
			}
		})
	}
}

// The table is only as good as the python-dotenv it was taken from, so where
// python-dotenv is installed - wherever podman-compose is - the table is
// checked against it again. `make check-stack` sets SHOULDER_DOTENV_REQUIRED,
// which makes a missing python-dotenv a failure there instead of a skip.
func TestTheTableIsWhatPythonDotenvSays(t *testing.T) {
	required := os.Getenv("SHOULDER_DOTENV_REQUIRED") != ""
	if err := exec.Command("python3", "-c", "import dotenv").Run(); err != nil {
		if required {
			t.Fatalf("python-dotenv is not importable: %v", err)
		}
		t.Skip("python-dotenv is not installed here; the table is checked against it by make check-stack")
	}
	cases := envCases(t)
	ins := make([]string, len(cases))
	for i, c := range cases {
		ins[i] = c.In
	}
	payload, err := json.Marshal(ins)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "-c", `
import io, json, sys
from dotenv import dotenv_values
out = []
for src in json.load(sys.stdin):
    vals = dotenv_values(stream=io.StringIO(src, newline=None))
    out.append({k: v for k, v in vals.items() if v is not None})
json.dump(out, sys.stdout)
`)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/u", "EMPTY="}
	cmd.Stdin = strings.NewReader(string(payload))
	cmd.Stderr = nil
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("python-dotenv: %v", err)
	}
	var got []map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		if !reflect.DeepEqual(got[i], c.Want) {
			t.Errorf("%s: python-dotenv says %q, the table %q", c.Name, got[i], c.Want)
		}
	}
}

// A value the daemon writes - the token, a setting from `shoulderd env set` -
// has to read back as itself through the same grammar, whatever is in it and
// whatever the file holds after it.
func TestSetInFileWritesWhatParseEnvReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	before := "# kept\nexport A=1 # kept too\nB='multi\nline'\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	none := func(string) (string, bool) { return "", false }
	for _, v := range []string{
		"plain", "", "it's", `back\slash`, `ends in \`, `\'`, "a # b", " padded ", "two\nlines",
		`"quoted"`, "tab\there", "ünïcödé", "$HOME", "#start",
	} {
		if err := SetInFile(path, "V", &v); err != nil {
			t.Fatalf("SetInFile(%q): %v", v, err)
		}
		// Something with quotes after it, which a badly closed value would run into.
		if err := SetInFile(path, "Z", ptr("'z'")); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		got := parseEnv(string(raw), none)
		if got["V"] != v || got["Z"] != "'z'" || got["A"] != "1" || got["B"] != "multi\nline" {
			t.Fatalf("after writing %q the file reads %q:\n%s", v, got, raw)
		}
		if n := strings.Count(string(raw), "\nV="); n != 1 {
			t.Fatalf("V is set %d times:\n%s", n, raw)
		}
	}
	if err := SetInFile(path, "V", ptr("${HOME}")); err == nil {
		t.Fatal("a value every form expands was written")
	}
	if err := SetInFile(path, "V", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if _, ok := parseEnv(string(raw), none)["V"]; ok || !strings.HasPrefix(string(raw), "# kept\nexport A=1 # kept too\n") {
		t.Fatalf("removing V left:\n%s", raw)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", st.Mode(), err)
	}
}

func ptr(s string) *string { return &s }

// A setting is changed where it stands, so comments and order around it - and
// the ${...} references that depend on order - stay as somebody wrote them.
func TestSetInFileReplacesTheFirstAndDropsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	before := "# models\nV=old # first\nW=${V}/x\n# keys\nV=dup\nZ=1\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetInFile(path, "V", ptr("new")); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if want := "# models\nV='new'\nW=${V}/x\n# keys\nZ=1\n"; string(raw) != want {
		t.Fatalf("got\n%s\nwant\n%s", raw, want)
	}
	if got := parseEnv(string(raw), func(string) (string, bool) { return "", false }); got["W"] != "new/x" {
		t.Fatalf("W = %q, want the reference to read the new value", got["W"])
	}
}

// A dotfiles setup links the env file somewhere else; writing it must change
// that file and leave the link in place.
func TestSetInFileWritesThroughALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "env")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "env")
	if err := os.Symlink(filepath.Join("dotfiles", "env"), link); err != nil {
		t.Fatal(err)
	}
	if err := SetInFile(link, "B", ptr("2")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", st.Mode(), err)
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "A=1\nB='2'\n" {
		t.Fatalf("the target reads %q", raw)
	}
}

func TestEnvFilePathExpandsAHomePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHOULDER_ENV_FILE", "~/conf/env")
	if got := EnvFilePath(); got != filepath.Join(home, "conf", "env") {
		t.Fatalf("EnvFilePath = %q", got)
	}
}
