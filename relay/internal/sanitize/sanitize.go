// Package sanitize neutralises advisor output before it is framed and shown to
// a model. Advisor text is untrusted input: it arrives from a service the user
// can swap for anything, and it lands inside a coding agent's context.
package sanitize

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	ansiCSI  = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	ansiOSC  = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	ansiEsc  = regexp.MustCompile(`\x1b[@-Z\\-_]`)
	blankRun = regexp.MustCompile(`\n{3,}`)
)

// Advice returns text safe to place inside the advisory envelope, truncated to
// maxChars runes.
//
// Every angle bracket is entity-escaped. That is deliberately blunt: it is the
// only rule that cannot be defeated by a novel framing token, and it makes the
// envelope, harness system-reminder framing, and tool-call syntax all
// unforgeable by advisor output. Advisory text is prose; losing literal markup
// is an acceptable price for an unbypassable boundary.
func Advice(raw string, maxChars int) string {
	s := raw

	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiEsc.ReplaceAllString(s, "")

	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\t':
			return r
		case '\r':
			return '\n'
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		// Strip bidi overrides and zero-width joiners used to disguise text.
		if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) || r == 0x200b || r == 0x200e || r == 0x200f || r == 0xfeff {
			return -1
		}
		if r == utf8.RuneError {
			return -1
		}
		return r
	}, s)

	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")

	s = blankRun.ReplaceAllString(s, "\n\n")
	s = strings.TrimSpace(s)

	return truncate(s, maxChars)
}

func truncate(s string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	cut := maxChars
	if cut > 1 {
		cut--
	}
	return strings.TrimRight(string(runes[:cut]), " \t\n") + "…"
}

// IsSilent reports whether an advisor reply means "say nothing". Empty,
// whitespace-only and the sentinels NOOP and none all mean stay quiet. It is
// the single definition: the decision parser and the pipeline both ask it,
// rather than each keeping its own list of ways a model declines to speak.
func IsSilent(raw string) bool {
	t := strings.TrimSpace(raw)
	return t == "" || strings.EqualFold(t, "NOOP") || strings.EqualFold(t, "none")
}
