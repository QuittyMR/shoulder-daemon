package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/config"
)

const monitorUsage = `usage: shoulderd monitor [--log=PATH] [--all] [--no-follow] [--json]

Watch facts move: stored, superseded, merged, dropped, refused, and advice
queued and injected. Everything else in the log is left out.

  --log PATH   the daemon's log file (default: SHOULDER_LOG, or
               ~/.local/share/shoulder-daemon/shoulderd.log)
  --all        print every movement in the file, not only the last 20
  --no-follow  print what is there and exit rather than waiting for more
  --json       the raw log records instead of one line each

Ctrl-C leaves. A daemon started with SHOULDER_LOG=stderr writes no file and
cannot be watched this way.
`

// monitorTail is how much history opens the view: enough to see what the
// last session did, few enough that the screen is not a wall.
const monitorTail = 20

// monitorPoll is how often the file is re-read for new lines. There is no
// inotify in the standard library, and a quarter second is below what a person
// watching a terminal can notice.
const monitorPoll = 250 * time.Millisecond

// movements maps the daemon's log messages to the verb shown for each. A
// message not here is not a fact movement, whatever its level.
var movements = map[string]string{
	"fact stored":                      "stored",
	"fact superseded":                  "superseded",
	"facts merged":                     "merged",
	"fact dropped as no longer a rule": "dropped",
	"dry run: would store fact":        "dry-run",
	"advice queued":                    "queued",
	"advice injected":                  "injected",

	"fact dropped: no scope was decided":                                                                     "refused",
	"local fact dropped: no project to file it under":                                                        "refused",
	"fact refused as a near-duplicate but the collision was not named; the write is lost":                    "refused",
	"fact refused by a memory outside this scope; the write is dropped rather than moving that memory here":  "refused",
	"supersede refused: the named fact is in another scope; the write is dropped rather than moving it here": "refused",
	"fact refused as a near-duplicate and superseding the collision also failed":                             "failed",
	"supersede failed":  "failed",
	"fact write failed": "failed",
	"a fact the tidying pass wanted gone is still there": "failed",
}

// movementKeys is the order the record's fields are rendered in after the
// ones the verb line places itself. Whatever else a record carries follows
// in name order, so a new field shows up rather than vanishing.
var movementKeys = []string{"text", "id", "supersedes", "kept", "replaced", "collided", "why", "err"}

func (c *cli) monitor(args []string) int {
	fs := c.flags("monitor", monitorUsage)
	path := fs.String("log", "", "the daemon's log file")
	all := fs.Bool("all", false, "print every movement, not only the last 20")
	noFollow := fs.Bool("no-follow", false, "print what is there and exit")
	asJSON := fs.Bool("json", false, "raw log records")
	if code := c.parse(fs, args); code >= 0 {
		return code
	}
	if fs.NArg() > 0 {
		return c.reject(fmt.Errorf("monitor takes no arguments, got %q", fs.Arg(0)))
	}
	if *path == "" {
		*path = config.Load().LogPath
	}
	if *path == "" {
		return c.reject(errors.New("SHOULDER_LOG is stderr, so the daemon writes no file to watch; unset it or name a file"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.done != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-c.done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	f, err := os.Open(*path) //nolint:gosec // G304: the operator's own log
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) || *noFollow {
			fmt.Fprintf(c.err, "shoulderd: %v\n", err)
			return 1
		}
		fmt.Fprintf(c.err, "waiting for %s; the daemon writes it on its first start\n", *path)
		for f == nil {
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(monitorPoll):
			}
			f, err = os.Open(*path) //nolint:gosec // G304: the operator's own log
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(c.err, "shoulderd: %v\n", err)
				return 1
			}
		}
	}
	defer f.Close()

	// History first, in one pass: a file of the daemon's own lines is small,
	// and reading it whole is what makes --all and the tail the same code.
	var kept [][]byte
	offset, err := scanMovements(f, func(line []byte) { kept = append(kept, line) })
	if err != nil {
		fmt.Fprintf(c.err, "shoulderd: %v\n", err)
		return 1
	}
	if !*all && len(kept) > monitorTail {
		kept = kept[len(kept)-monitorTail:]
	}
	for _, line := range kept {
		c.printMovement(line, *asJSON)
	}
	if *noFollow {
		return 0
	}

	for {
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(monitorPoll):
		}
		st, err := f.Stat()
		if err != nil {
			// Moved aside by the daemon's own rotation, or removed by hand:
			// the name is what matters, so it is reopened from the top.
			_ = f.Close()
			if f, err = os.Open(*path); err != nil { //nolint:gosec // G304: the operator's own log
				continue
			}
			offset = 0
			st, err = f.Stat()
			if err != nil {
				continue
			}
		}
		if st.Size() < offset {
			offset = 0
		}
		if st.Size() == offset {
			continue
		}
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			fmt.Fprintf(c.err, "shoulderd: %v\n", err)
			return 1
		}
		n, err := scanMovements(f, func(line []byte) { c.printMovement(line, *asJSON) })
		if err != nil {
			fmt.Fprintf(c.err, "shoulderd: %v\n", err)
			return 1
		}
		offset += n
	}
}

// scanMovements reads whole lines from r, hands the fact movements among them
// to emit, and reports how many bytes it consumed. A final line without its
// newline is left for the next call: the daemon writes a record in one write
// but the reader can arrive in the middle of it.
func scanMovements(r io.Reader, emit func([]byte)) (int64, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var consumed int64
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return consumed, nil
			}
			return consumed, err
		}
		consumed += int64(len(line))
		if isMovement(line) {
			emit(bytes.TrimRight(line, "\r\n"))
		}
	}
}

// record is one slog line as the daemon writes it. Only the three fields the
// handler always emits are typed; the rest are the attributes of that message.
type record struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

func isMovement(line []byte) bool {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return false
	}
	_, ok := movements[rec.Msg]
	return ok
}

func (c *cli) printMovement(line []byte, asJSON bool) {
	if asJSON {
		fmt.Fprintf(c.out, "%s\n", line)
		return
	}
	fmt.Fprintln(c.out, renderMovement(line))
}

// renderMovement turns one record into one line: the time, the verb, where
// it happened, and what moved.
func renderMovement(line []byte) string {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return string(line)
	}
	attrs := map[string]any{}
	_ = json.Unmarshal(line, &attrs)
	delete(attrs, "time")
	delete(attrs, "level")
	delete(attrs, "msg")

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %-11s", rec.Time.Local().Format("15:04:05"), movements[rec.Msg])

	// Where: a scope with its project for a fact, a session for advice.
	if sc, ok := attrs["scope"]; ok {
		where := fmt.Sprint(sc)
		if p, ok := attrs["project"]; ok && p != "" {
			where += " " + fmt.Sprint(p)
		}
		fmt.Fprintf(&b, " %-24s", where)
		delete(attrs, "scope")
		delete(attrs, "project")
	} else if s, ok := attrs["session"]; ok {
		where := "session " + short(fmt.Sprint(s))
		if t, ok := attrs["turn"]; ok {
			where += fmt.Sprintf(" turn %v", t)
		}
		if e, ok := attrs["event"]; ok {
			where += " " + fmt.Sprint(e)
		}
		fmt.Fprintf(&b, " %-24s", where)
		delete(attrs, "session")
		delete(attrs, "turn")
		delete(attrs, "event")
	}
	// The origin is a session id or "cli"; only the latter says anything.
	if o, ok := attrs["origin"]; ok {
		if fmt.Sprint(o) == "cli" {
			b.WriteString(" [cli]")
		}
		delete(attrs, "origin")
	}

	// The category sits in front of the content it classifies, as `fact list`
	// prints it.
	category, _ := attrs["category"].(string)
	delete(attrs, "category")
	if content, ok := attrs["content"].(string); ok && content != "" {
		delete(attrs, "content")
		if category != "" {
			fmt.Fprintf(&b, "  (%s) %q", category, content)
		} else {
			fmt.Fprintf(&b, "  %q", content)
		}
	}
	for _, k := range movementKeys {
		writeAttr(&b, k, attrs)
	}
	rest := make([]string, 0, len(attrs))
	for k := range attrs {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		writeAttr(&b, k, attrs)
	}
	if rec.Level == "WARN" || rec.Level == "ERROR" {
		fmt.Fprintf(&b, "  (%s)", rec.Msg)
	}
	return b.String()
}

func writeAttr(b *strings.Builder, k string, attrs map[string]any) {
	v, ok := attrs[k]
	if !ok {
		return
	}
	delete(attrs, k)
	s := fmt.Sprint(v)
	if s == "" {
		return
	}
	switch k {
	case "content", "text":
		fmt.Fprintf(b, "  %q", s)
	default:
		fmt.Fprintf(b, "  %s=%s", k, s)
	}
}

// short is the first eight characters of a session id, which is what the
// eye can match between two lines.
func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
