package notify

import (
	"strings"
	"testing"
)

func TestParseReadsTheSetting(t *testing.T) {
	cases := map[string]string{
		"":                    "off",
		"off":                 "off",
		"on":                  "errors,injections,stored,superseded",
		"on,warnings":         "errors,injections,stored,superseded,warnings",
		"all":                 "errors,injections,maintenance,stored,superseded,warnings",
		"errors, maintenance": "errors,maintenance",
		"Stored":              "stored",
	}
	for in, want := range cases {
		p, err := Parse(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := p.String(); got != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := Parse("on,typo"); err == nil || !strings.Contains(err.Error(), "typo") || !strings.Contains(err.Error(), "maintenance") {
		t.Fatalf("an unknown kind must be refused by name, with the names it could be: %v", err)
	}
}

func TestBoxKeepsOnlyWhatThePolicyAllows(t *testing.T) {
	b := New(Defaults())
	b.Push("s1", Stored, "a fact")
	b.Push("s1", Warning, "dropped")
	b.Push("s1", Maintenance, "tidied")
	b.Push("s1", Error, "the key is dead")
	b.Push("cli", Stored, "from the terminal")
	b.Push("", Stored, "nowhere")
	b.Push("s1", Stored, "   ")

	got := Lines(b.Drain("s1"))
	want := []string{"stored: a fact", "error: the key is dead"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("drained %v, want %v", got, want)
	}
	if again := b.Drain("s1"); len(again) != 0 {
		t.Fatalf("a drain empties the queue, got %v", again)
	}
	if left := b.Drain("cli"); len(left) != 0 {
		t.Fatalf("the terminal has its own output, got %v", left)
	}
}

func TestBoxIsBoundedAndKeepsTheNewest(t *testing.T) {
	b := New(Policy{Stored: true})
	for i := 0; i < perSession+5; i++ {
		b.Push("s1", Stored, strings.Repeat("x", i+1))
	}
	got := b.Drain("s1")
	if len(got) != perSession || len(got[0].Text) != 6 || len(got[len(got)-1].Text) != perSession+5 {
		t.Fatalf("got %d notices, first %d long, last %d long", len(got), len(got[0].Text), len(got[len(got)-1].Text))
	}
}

func TestANilBoxDropsEverything(t *testing.T) {
	var b *Box
	b.Push("s1", Error, "x")
	b.PushAll([]string{"s1", "s2"}, Error, "x")
	b.Forget("s1")
	if got := b.Drain("s1"); got != nil || b.Policy().Enabled() {
		t.Fatalf("a nil box must be silent: %v", got)
	}
}

func TestPushAllReachesEverySession(t *testing.T) {
	b := New(Defaults())
	b.PushAll([]string{"s1", "s2"}, Error, "the model stopped answering")
	for _, id := range []string{"s1", "s2"} {
		if got := Lines(b.Drain(id)); len(got) != 1 || got[0] != "error: the model stopped answering" {
			t.Fatalf("%s: %v", id, got)
		}
	}
}
