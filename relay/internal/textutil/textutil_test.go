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
