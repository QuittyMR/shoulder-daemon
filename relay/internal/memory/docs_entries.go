package memory

import (
	"bufio"
	"bytes"
	"net/url"
	"os"
	"strings"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
)

// docsSuffix marks the files the connector owns. Nothing else under a docs
// directory is ever opened for writing: a person's own README or design note
// sits beside these, and the suffix is what keeps the daemon out of it.
const docsSuffix = ".shoulder.md"

// docsFileFor is the whole of the placement rule. It is a table rather than a
// model because a person reading the directory has to be able to predict where
// a fact went, and because the categories were closed upstream for exactly
// this: a category is only worth having if it still means the same thing on
// the way out.
//
// A legacy name files where its current name does, so a store that was written
// under the old set keeps growing in the files it has rather than forking. The
// files the old set created are still read: the connector lists by suffix, and
// a supersede lands in place whatever the file is called.
func docsFileFor(r Record) string {
	if r.Private {
		return "USER" + docsSuffix
	}
	switch facts.CurrentName(r.Category) {
	case "finding":
		return "FINDINGS" + docsSuffix
	case "fact":
		return "FACTS" + docsSuffix
	case "rule":
		return "RULES" + docsSuffix
	case "preference":
		return "USER" + docsSuffix
	}
	return "NOTES" + docsSuffix
}

// docsHeader opens a file the connector creates. The one-line note is there so
// somebody who finds the file in a diff knows what writes to it and that the
// bullets are safe to edit by hand.
func docsHeader(name string) string {
	title := map[string]string{
		"FINDINGS": "Findings",
		"FACTS":    "Facts",
		"RULES":    "Rules",
		"NOTES":    "Notes",
		"USER":     "User notes",
	}[strings.TrimSuffix(name, docsSuffix)]
	if title == "" {
		title = strings.TrimSuffix(name, docsSuffix)
	}
	return "# " + title + "\n\n" +
		"Maintained by shoulder-daemon: bullets ending in an `sd` comment are its records and may be edited or removed by hand, everything else is yours.\n\n"
}

// The comment carried on every record line. The id inside it is the record's
// identity, so a person can reword the sentence and the daemon still knows
// which fact it is; a line with no such comment is prose, however much it
// looks like a bullet.
const (
	docsMarkOpen  = "<!-- sd "
	docsMarkClose = "-->"
)

// docsEntry is one record and where it was read from, so a supersede can put
// the replacement exactly there.
type docsEntry struct {
	Record
	path string
	line int
}

// docsSentence is the content as a file holds it. Newlines are flattened
// because one record is one line: that is what makes the file diffable,
// greppable and parseable without a markdown parser, and a fact is a sentence
// anyway. A write flattens before it hashes and embeds, because an id or a
// vector taken from the unflattened text names something no read of the file
// can produce: the record is re-embedded on every search and a rewrite of the
// line's own sentence is refused as a near-duplicate of itself.
func docsSentence(content string) string {
	return strings.Join(strings.Fields(content), " ")
}

// formatDocsLine renders a record as a bullet. The category is written under
// its current name: a file is read by people as well as by the daemon, and one
// that mixed two generations of names would make the reader learn both.
func formatDocsLine(r Record) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(docsSentence(r.Content))
	b.WriteString(" ")
	b.WriteString(docsMarkOpen)
	b.WriteString("id=" + r.ID)
	if c := facts.CurrentName(r.Category); c != "" {
		b.WriteString(" category=" + url.QueryEscape(c))
	}
	if len(r.Tags) > 0 {
		escaped := make([]string, len(r.Tags))
		for i, t := range r.Tags {
			escaped[i] = url.QueryEscape(t)
		}
		b.WriteString(" tags=" + strings.Join(escaped, ","))
	}
	b.WriteString(" at=" + r.CreatedAt.UTC().Format(time.RFC3339Nano))
	b.WriteString(" " + docsMarkClose)
	return b.String()
}

// parseDocsLine reads one line back, tolerating what a person does to a
// markdown file: indentation, a trailing carriage return, extra spaces. The
// last sd comment on the line wins so content that happens to mention one is
// not mistaken for it.
func parseDocsLine(line string) (Record, bool) {
	trimmed := strings.TrimRight(strings.TrimSpace(line), "\r")
	if !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "* ") {
		return Record{}, false
	}
	open := strings.LastIndex(trimmed, docsMarkOpen)
	if open < 0 {
		return Record{}, false
	}
	rest := trimmed[open+len(docsMarkOpen):]
	end := strings.Index(rest, docsMarkClose)
	if end < 0 {
		return Record{}, false
	}
	r := Record{Content: strings.TrimSpace(trimmed[2:open])}
	for _, field := range strings.Fields(rest[:end]) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "id":
			r.ID = value
		case "category":
			r.Category = facts.CurrentName(unescapeDocs(value))
		case "tags":
			for _, t := range strings.Split(value, ",") {
				if t = unescapeDocs(t); t != "" {
					r.Tags = append(r.Tags, t)
				}
			}
		case "at":
			if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
				r.CreatedAt = at.UTC()
			}
		}
	}
	if r.ID == "" || r.Content == "" {
		return Record{}, false
	}
	return r, true
}

func unescapeDocs(s string) string {
	if out, err := url.QueryUnescape(s); err == nil {
		return out
	}
	return s
}

// docsLF is the line ending a file the connector creates gets, and the one a
// file that has no opinion keeps.
const docsLF = "\n"

// docsCRLF is what a file committed from Windows uses. It is carried through a
// write rather than normalised: the connector edits one bullet of a file a
// team owns, and rewriting every line ending of it converts a one-line change
// into a whole-file diff that hides what the daemon actually did and that
// somebody has to review.
const docsCRLF = "\r\n"

// readDocsFile returns the file's lines, the ending to put them back with, and
// the records among them. Lines are kept whole, prose included, because a
// write puts them back exactly as they were: the daemon owns the records, not
// the file.
//
// The ending is the dominant one rather than the first, because a file that
// has been edited on two machines has both and the majority is the one a diff
// is smallest against. A file that is empty, or that cannot be read at all,
// reports a newline, so a caller creating the file needs no case of its own.
func readDocsFile(path string) (lines []string, eol string, entries []docsEntry, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a file under the docs directory this connector was pointed at
	if err != nil {
		return nil, docsLF, nil, err
	}
	eol = dominantEOL(raw)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(nil, len(raw)+1)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, docsLF, nil, err
	}
	for i, line := range lines {
		if r, ok := parseDocsLine(line); ok {
			entries = append(entries, docsEntry{Record: r, path: path, line: i})
		}
	}
	return lines, eol, entries, nil
}

// dominantEOL is the line ending most of raw uses.
func dominantEOL(raw []byte) string {
	crlf := bytes.Count(raw, []byte(docsCRLF))
	if crlf > bytes.Count(raw, []byte(docsLF))-crlf {
		return docsCRLF
	}
	return docsLF
}
