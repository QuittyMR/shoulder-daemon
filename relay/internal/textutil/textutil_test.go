package textutil

import (
	"strings"
	"testing"
)

func TestClipIsAByteBoundMarkedWithAnEllipsis(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exact", 5, "exact"},
		{"too long by one", 7, "too lon…"},
		{"", 0, ""},
		{"x", 0, "…"},
	}
	for _, tc := range cases {
		if got := Clip(tc.in, tc.n); got != tc.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// Every caller is guarding a buffer, so the bound is bytes, not runes; a
// multi-byte character straddling the cut is split, and the marker is what
// says so.
func TestClipCountsBytesNotCharacters(t *testing.T) {
	s := strings.Repeat("é", 4) // 8 bytes
	got := Clip(s, 5)
	if len(got) != 5+len("…") {
		t.Fatalf("Clip cut at %d bytes, want 5", len(got)-len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("a cut string must end in the marker, got %q", got)
	}
}

func TestSentencesCutsAtTerminatorsAndLineBreaks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"blank", "  \n\t ", nil},
		{"one sentence without a terminator", "use the connection pool", []string{"use the connection pool"}},
		{
			"three terminators", "Use Postgres here. Never commit secrets! Which branch is main?",
			[]string{"Use Postgres here", "Never commit secrets", "Which branch is main"},
		},
		{
			"line breaks", "the record cap is eight\nthe digest limit is two hundred",
			[]string{"the record cap is eight", "the digest limit is two hundred"},
		},
		{
			"stacked terminators", "Did that really deploy?! It did not.",
			[]string{"Did that really deploy", "It did not"},
		},
		{
			"repeats collapse", "Prefer terse answers. Prefer terse answers.\nPrefer terse answers",
			[]string{"Prefer terse answers"},
		},
		{
			"surrounding whitespace", "   keep the migration reversible .  \n  ",
			[]string{"keep the migration reversible"},
		},
		{
			"a full stop inside a word is not a cut", "pin the runtime to version 5.3 for the migration. then fix internal/pool/conn.go and pipeline.go together",
			[]string{"pin the runtime to version 5.3 for the migration", "then fix internal/pool/conn.go and pipeline.go together"},
		},
		{
			"a file name ends a sentence", "the retry loop is in internal/sync/push.go. it re-sends on a 401",
			[]string{"the retry loop is in internal/sync/push.go", "it re-sends on a 401"},
		},
		{
			"a hostname and a version", "the relay listens on relay.local:8787 since v1.2.0",
			[]string{"the relay listens on relay.local:8787 since v1.2.0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Sentences(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("Sentences(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Sentences(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

// The floor is counted in words: a one-word piece is a reply or the stub a
// terminator leaves behind, and the shortest statement is two.
func TestSentencesDropsSingleWords(t *testing.T) {
	in := "ok. 5. fix it. Use Postgres. no testify! déjà vu again"
	want := []string{"fix it", "Use Postgres", "no testify", "déjà vu again"}
	got := Sentences(in)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("Sentences(%q) = %q, want %q", in, got, want)
	}
}
