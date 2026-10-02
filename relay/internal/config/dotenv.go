package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// The env file is read with the grammar podman-compose reads it with, which is
// python-dotenv's, so that a value means the same thing to a daemon that reads
// the file itself, to the container compose builds from it, and to everything
// that compares the two. testdata/dotenv.json holds the cases, with the values
// python-dotenv gives for them; dotenv_test.go checks this reader against it
// and against python-dotenv itself where it is installed.
//
//   - blank lines and lines starting with # are skipped; `export ` is dropped
//   - an unquoted value runs to the end of the line, loses everything from the
//     first whitespace followed by #, and is trimmed
//   - a single-quoted value is literal except for \\ and \'; a double-quoted
//     one also takes \" \a \b \f \n \r \t \v; either may span lines, and a #
//     inside either is part of the value
//   - ${NAME} and ${NAME:-default} are expanded in every value, from the
//     lines above it and then from the environment
//   - a name with no = sets nothing, but stands for an empty value in a
//     ${NAME} below it
//   - a line that does not parse is skipped from where it stopped making sense
//     to its end, and the next line is read as usual
//
// Whitespace is Unicode whitespace, as Python's \s is.

// envBinding is one name the file sets. Value is nil for a name with no =.
// Start and End are the rune offsets of the lines it occupies, terminator
// included, which is what a writer replaces.
type envBinding struct {
	Key        string
	Value      *string
	Start, End int
}

// parseEnv is the file's values, expanded.
func parseEnv(src string, lookup func(string) (string, bool)) map[string]string {
	out := map[string]string{}
	bare := map[string]bool{}
	for _, b := range scanEnv(src) {
		if b.Value == nil {
			delete(out, b.Key)
			bare[b.Key] = true
			continue
		}
		v := expand(*b.Value, func(name string) (string, bool) {
			if s, ok := out[name]; ok {
				return s, true
			}
			if bare[name] {
				return "", true
			}
			return lookup(name)
		})
		delete(bare, b.Key)
		out[b.Key] = v
	}
	return out
}

// newlines is what Python's universal newlines does to the file as it opens
// it, and so what every offset below is counted in.
var newlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// scanEnv is every binding in the file, unexpanded, in order.
func scanEnv(src string) []envBinding {
	p := &envScanner{s: []rune(newlines.Replace(src))}
	var out []envBinding
	for {
		p.skip(isSpace)
		if p.i >= len(p.s) {
			return out
		}
		start := p.i
		for start > 0 && p.s[start-1] != '\n' {
			start--
		}
		key, val, ok := p.binding()
		if !ok {
			p.skip(notLineEnd)
			continue
		}
		if key != "" {
			out = append(out, envBinding{Key: key, Value: val, Start: start, End: p.i})
		}
	}
}

type envScanner struct {
	s []rune
	i int
}

// isSpace is Python's \s: Unicode whitespace and the four ASCII separators.
func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// isBlank is whitespace within a line. Every \r is a \n by the time it is
// asked, as below.
func isBlank(r rune) bool { return isSpace(r) && r != '\n' }

func notLineEnd(r rune) bool { return r != '\n' }

func (p *envScanner) skip(while func(rune) bool) {
	for p.i < len(p.s) && while(p.s[p.i]) {
		p.i++
	}
}

func (p *envScanner) peek() rune {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

// binding reads one line, or one value spanning several. An empty key with ok
// is a comment.
func (p *envScanner) binding() (key string, val *string, ok bool) {
	if p.i+6 < len(p.s) && string(p.s[p.i:p.i+6]) == "export" && isBlank(p.s[p.i+6]) {
		p.i += 6
		p.skip(isBlank)
	}
	switch p.peek() {
	case '#':
	case '\'':
		end := p.i + 1
		for end < len(p.s) && p.s[end] != '\'' {
			end++
		}
		if end >= len(p.s) || end == p.i+1 {
			return "", nil, false
		}
		key = string(p.s[p.i+1 : end])
		p.i = end + 1
	default:
		start := p.i
		p.skip(func(r rune) bool { return r != '=' && r != '#' && !isSpace(r) })
		if p.i == start {
			return "", nil, false
		}
		key = string(p.s[start:p.i])
	}
	p.skip(isBlank)
	if p.peek() == '=' {
		p.i++
		p.skip(isBlank)
		v, good := p.value()
		if !good {
			return "", nil, false
		}
		val = &v
	}
	p.skip(isBlank)
	if p.peek() == '#' {
		p.skip(notLineEnd)
	}
	if p.i < len(p.s) {
		if p.s[p.i] != '\n' {
			return "", nil, false
		}
		p.i++
	}
	return key, val, true
}

func (p *envScanner) value() (string, bool) {
	q := p.peek()
	if q != '\'' && q != '"' {
		start := p.i
		p.skip(notLineEnd)
		v := p.s[start:p.i]
		for k := 0; k < len(v); k++ {
			if !isSpace(v[k]) {
				continue
			}
			m := k
			for m < len(v) && isSpace(v[m]) {
				m++
			}
			if m < len(v) && v[m] == '#' {
				v = v[:k]
				break
			}
			k = m - 1
		}
		return strings.TrimRightFunc(string(v), isSpace), true
	}
	// A backslash escapes the quote it stands before and nothing else, for
	// finding the end; which escapes then mean something depends on the quote.
	// Python finds the end with a regular expression, so a value that runs to
	// the end of the file without one backs off to the last escaped quote and
	// ends there, the backslash before it kept.
	j, last := p.i+1, -1
	for ; j < len(p.s); j++ {
		if p.s[j] == '\\' && j+1 < len(p.s) && p.s[j+1] == q {
			j++
			last = j
			continue
		}
		if p.s[j] == q {
			break
		}
	}
	if j >= len(p.s) {
		if last < 0 {
			return "", false
		}
		j = last
	}
	raw := p.s[p.i+1 : j]
	p.i = j + 1
	escapes := `\'`
	if q == '"' {
		escapes = `\'"abfnrtv`
	}
	var b strings.Builder
	for k := 0; k < len(raw); k++ {
		if raw[k] == '\\' && k+1 < len(raw) && strings.ContainsRune(escapes, raw[k+1]) {
			b.WriteRune(unescape[raw[k+1]])
			k++
			continue
		}
		b.WriteRune(raw[k])
	}
	return b.String(), true
}

var unescape = map[rune]rune{
	'\\': '\\', '\'': '\'', '"': '"',
	'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
}

var envVariable = regexp.MustCompile(`\$\{([^}:]*)(:-([^}]*))?\}`)

// expand replaces ${NAME} and ${NAME:-default}. The default stands in only for
// a name that is set nowhere: one set to nothing stays nothing.
func expand(v string, lookup func(string) (string, bool)) string {
	return envVariable.ReplaceAllStringFunc(v, func(m string) string {
		g := envVariable.FindStringSubmatch(m)
		if s, ok := lookup(g[1]); ok {
			return s
		}
		return g[3]
	})
}

// quoteEnv writes value so that the grammar above reads it back unchanged
// whatever comes after it in the file: single-quoted, or bare where single
// quotes cannot hold it, which is a value ending in a backslash. A value no
// form can hold - one with ${...} in it, which every form expands - is
// refused.
func quoteEnv(value string) (string, bool) {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	for _, form := range []string{"'" + r.Replace(value) + "'", value} {
		got := parseEnv("N="+form+"\nZ='z'\n", func(string) (string, bool) { return "", false })
		if len(got) == 2 && got["N"] == value && got["Z"] == "z" {
			return form, true
		}
	}
	return "", false
}

// SetInFile sets name to value in the env file at path, or removes it when
// value is nil, and leaves every other byte of the file as it was. The first
// line that set the name is replaced where it stands and any later ones go, so
// the file says it once and in the place somebody put it; a name the file did
// not set is appended.
func SetInFile(path, name string, value *string) error {
	raw, err := readFileIfAny(path)
	if err != nil {
		return err
	}
	var line string
	if value != nil {
		q, ok := quoteEnv(*value)
		if !ok {
			return errUnwritable
		}
		line = name + "=" + q + "\n"
	}
	src := []rune(newlines.Replace(raw))
	var out []rune
	at, placed := 0, line == ""
	for _, b := range scanEnv(raw) {
		if b.Key != name {
			continue
		}
		out = append(out, src[at:b.Start]...)
		if !placed {
			out = append(out, []rune(line)...)
			placed = true
		}
		at = b.End
	}
	out = append(out, src[at:]...)
	text := string(out)
	if !placed {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += line
	}
	return writeFilePrivate(path, text)
}

var errUnwritable = errors.New("no form of the env file's grammar reads this value back unchanged; a value with ${...} in it is always expanded")

func readFileIfAny(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: SHOULDER_ENV_FILE is the operator's own setting
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(raw), err
}

// writeFilePrivate replaces the file whole, by rename, so a reader never sees
// half of it, and keeps it to its owner: it holds keys. An env file that is a
// link - into a dotfiles checkout, say - is written where it points, and the
// link is left a link.
func writeFilePrivate(path, text string) error {
	for range 40 {
		target, err := os.Readlink(path)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
