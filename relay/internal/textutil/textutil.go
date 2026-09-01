// Package textutil holds the string helpers shared across the relay. They are
// here rather than duplicated per package so truncation behaves identically
// wherever a value is shortened for a log line, an error, or the advisor.
package textutil

// Clip shortens s to at most n bytes, marking the cut with an ellipsis. The
// bound is a byte bound: every caller is protecting a buffer or a line of
// output, not counting characters.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
