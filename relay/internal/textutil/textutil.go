// Package textutil holds the string helpers shared across the relay. They are
// here rather than duplicated per package so truncation behaves identically
// wherever a value is shortened for a log line, an error, or the advisor.
package textutil

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clip shortens s to at most n bytes, marking the cut with an ellipsis. The
// bound is a byte bound: every caller is protecting a buffer or a line of
// output, not counting characters.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// SentenceFloor is the fewest words Sentences keeps a piece with. A single
// word ("ok", "5") is a reply or the stub a terminator leaves behind, and
// gives a semantic search nothing to match on; two ("fix it", "use tabs") is
// the shortest statement there is.
const SentenceFloor = 2

// Sentences cuts text at line breaks and at a full stop, an exclamation mark
// or a question mark that ends a word, and returns the trimmed pieces in
// order, stripped of their terminators, without blanks, without pieces under
// SentenceFloor and without repeats. A full stop inside a word - "5.3",
// "conn.go", "relay.local" - is part of it and is not a cut; one that ends a
// word is, so an abbreviation such as "e.g." still cuts.
func Sentences(text string) []string {
	var out []string
	seen := map[string]bool{}
	start := 0
	keep := func(piece string) {
		piece = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(piece), ".!?"))
		if len(strings.Fields(piece)) < SentenceFloor || seen[piece] {
			return
		}
		seen[piece] = true
		out = append(out, piece)
	}
	for i, r := range text {
		switch r {
		case '\n':
			keep(text[start:i])
			start = i + 1
		case '.', '!', '?':
			if !endsWord(text, i+1) {
				continue
			}
			keep(text[start:i])
			start = i + 1
		}
	}
	keep(text[start:])
	return out
}

// endsWord reports whether position i in text is the end of the text or the
// start of whitespace or of another terminator.
func endsWord(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[i:])
	return unicode.IsSpace(r) || r == '.' || r == '!' || r == '?'
}
