package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

var _ Connector = (*Docs)(nil)

// Docs keeps facts as bullets in markdown files that live with the code they
// describe. It exists for the team that wants what the daemon learned about a
// repository to be reviewed, committed and read by the next person, which a
// JSON file under one person's home directory can never be. Local facts go in
// the worktree's docs directory; global ones, which follow the person rather
// than the code, go under their data directory.
//
// The files are the store. Nothing is held between calls except a parse of
// each file keyed on its modification time, so a fact somebody edited, moved
// or deleted in their editor is seen on the next read without anything being
// restarted. Working notes never touch these files: they are noise a day later
// and would be committed forever, so they go to an embedded Local.
type Docs struct {
	opts    DocsOptions
	session *Local

	// mu serialises writes to the docs files; reads take it shared.
	mu sync.RWMutex

	// places is keyed by scope and project identity, resolved by scope,
	// identity and the directory the call came with. The first answers a call
	// that brought no directory; the second keeps a directory that was
	// resolved once from being resolved again.
	placesMu sync.Mutex
	places   map[string]place
	resolved map[string]place

	filesMu sync.Mutex
	files   map[string]*docsFile

	cacheMu sync.Mutex
}

// DocsOptions configures a Docs store. Every field has a default, so the zero
// value is the store as the daemon would ship it.
type DocsOptions struct {
	// Roots locates the docs directory and the git worktree containing it for
	// a scope, a project identity and the directory the caller was working
	// in, which is empty when the caller had none to give. Nil resolves the
	// worktree with git from that directory and reuses an existing docs or doc
	// directory under it. The worktree is empty when there is none, which
	// turns off the .gitignore handling for private records.
	Roots func(s scope.Scope, project, dir string) (docsDir, worktree string)

	// GlobalDir is where global facts go under the default Roots; empty means
	// DefaultGlobalDocsDir. SHOULDER_GLOBAL_DOCS belongs here.
	GlobalDir string

	// DirName is the subdirectory of the worktree local facts go in; empty
	// means DefaultDocsDirName. SHOULDER_DOCS_DIR belongs here.
	DirName string

	// SessionPath is the file working notes are kept in; empty means
	// DefaultSessionPath.
	SessionPath string

	// CacheDir holds the vectors of docs entries, outside every repository;
	// empty means DefaultVectorCacheDir.
	CacheDir string

	Embedder Embedder
	Log      *slog.Logger
}

// place is where one scope's records live.
type place struct {
	scope    scope.Scope
	project  string
	dir      string
	worktree string
}

// docsFile is one parsed file, remembered until the file changes underneath.
type docsFile struct {
	modTime time.Time
	size    int64
	entries []docsEntry
}

// DefaultDocsDirName is the subdirectory local facts go in when nobody says
// otherwise. A repository that already has doc/ instead is honoured under the
// default; a name set on purpose is used as given.
const DefaultDocsDirName = "docs"

// DefaultGlobalDocsDir is where global facts go when nobody says otherwise.
func DefaultGlobalDocsDir() string {
	local := DefaultLocalPath()
	if local == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(local), "docs")
}

// DefaultSessionPath is where working notes go when the facts are in docs
// files: beside the store the notes would otherwise have shared.
func DefaultSessionPath() string {
	local := DefaultLocalPath()
	if local == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(local), "session.json")
}

// DefaultVectorCacheDir follows the XDG cache directory because that is what
// the vectors are: derivable from the files at any time and worth nothing to
// anybody else.
func DefaultVectorCacheDir() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "shoulder-daemon", "vectors")
}

// NewDocs opens the store. Nothing is created until something is written; the
// session file is opened the way Local opens it, so a corrupt one is an error
// rather than a fresh start over somebody's notes.
func NewDocs(opts DocsOptions) (*Docs, error) {
	if opts.GlobalDir == "" {
		opts.GlobalDir = DefaultGlobalDocsDir()
	}
	if opts.SessionPath == "" {
		opts.SessionPath = DefaultSessionPath()
	}
	if opts.CacheDir == "" {
		opts.CacheDir = DefaultVectorCacheDir()
	}
	if opts.DirName == "" {
		opts.DirName = DefaultDocsDirName
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.GlobalDir == "" || opts.SessionPath == "" {
		return nil, errors.New("docs store has no home directory to put global facts or session notes in")
	}
	session, err := NewLocal(opts.SessionPath, opts.Embedder)
	if err != nil {
		return nil, err
	}
	session.SetLog(opts.Log)
	return &Docs{
		opts:     opts,
		session:  session,
		places:   map[string]place{},
		resolved: map[string]place{},
		files:    map[string]*docsFile{},
	}, nil
}

func (d *Docs) Name() string { return "docs" }

// Close ends the session store's background work. The docs files have none.
func (d *Docs) Close() error { return d.session.Close() }

// SessionPath is where working notes go, for the startup line.
func (d *Docs) SessionPath() string { return d.session.Path() }

// GlobalDir is where global facts go, for the startup line and the doctor.
func (d *Docs) GlobalDir() string { return d.opts.GlobalDir }

// DirName is the subdirectory local facts go in, for the doctor, which looks
// for it from the directory the command was typed in.
func (d *Docs) DirName() string { return d.opts.DirName }

func (d *Docs) Store(ctx context.Context, r Record) (string, error) {
	if r.Kind == KindSession {
		return d.session.Store(ctx, r)
	}
	p, err := d.place(r.Scope, r.Project, r.Dir)
	if err != nil {
		return "", err
	}
	r.Dir = ""
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	r.Content = docsSentence(r.Content)
	r.ID = contentID(r.Content)
	vec := embedWith(ctx, d.opts.Embedder, r.Content)

	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := d.scan(p)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.ID == r.ID {
			return "", ErrDuplicateExact
		}
	}
	recs, vecs := d.vectors(ctx, p, entries)
	if collided := newRanker(recs, vecs, d.opts.Embedder).similar(r, vec); collided != "" {
		return "", &ErrDuplicateSemantic{Collided: collided}
	}

	name := docsFileFor(r)
	path := filepath.Join(p.dir, name)
	lines, eol, _, err := readDocsFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		lines = strings.Split(strings.TrimSuffix(docsHeader(name), "\n"), "\n")
	case err != nil:
		return "", fmt.Errorf("docs store %s: %w", path, err)
	}
	lines = append(lines, formatDocsLine(r))
	if err := d.write(p, path, lines, eol); err != nil {
		return "", err
	}
	if vec != nil {
		vecs[r.ID] = *vec
		d.saveVectors(p, append(entries, docsEntry{Record: r}), vecs)
	}
	if r.Private && p.worktree != "" {
		d.ignorePrivate(p)
	}
	return r.ID, nil
}

// Supersede rewrites the target's line where it is. The replacement stays in
// the file and at the position the original had, whatever its category says,
// because a person who has read that file has a mental map of it and a fact
// that jumps between files on every correction breaks the map for nothing.
//
// Privacy is the exception, and has to be: which file a record is in is what
// decides whether git carries it. A replacement that became private and stayed
// in a committed file would report a fact as kept out of the repository while
// it sat in it.
func (d *Docs) Supersede(ctx context.Context, oldID string, r Record) (string, error) {
	if r.Kind == KindSession {
		return d.session.Supersede(ctx, oldID, r)
	}
	p, err := d.place(r.Scope, r.Project, r.Dir)
	if err != nil {
		return "", err
	}
	r.Dir = ""
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	r.Content = docsSentence(r.Content)
	r.ID = contentID(r.Content)
	vec := embedWith(ctx, d.opts.Embedder, r.Content)

	d.mu.Lock()
	defer d.mu.Unlock()

	entries, err := d.scan(p)
	if err != nil {
		return "", err
	}
	target := -1
	for i, e := range entries {
		if e.ID == oldID {
			target = i
			break
		}
	}
	if target < 0 {
		return "", &ErrCrossScopeSupersede{
			OldID: oldID, Scope: r.Scope, Project: r.Project, Elsewhere: d.heldElsewhere(oldID, p),
		}
	}
	// Where the replacement goes. A record that changed nothing about its
	// privacy stays in the file and at the line it was on; one that did has to
	// move, because which file a record is in is what decides whether git
	// carries it.
	dest := entries[target].path
	moved := entries[target].Private != r.Private
	if moved {
		dest = filepath.Join(p.dir, docsFileFor(r))
	}
	if r.ID != oldID {
		for i, e := range entries {
			// A copy of the replacement already sitting at the destination is
			// a move that did not finish rather than a second fact: it is
			// rewritten where it is below and the original swept after, which
			// is the same repair a first attempt would have made.
			if i == target || e.ID != r.ID || (moved && e.path == dest) {
				continue
			}
			return "", ErrDuplicateExact
		}
	}

	lines, eol, fresh, err := readDocsFile(dest)
	switch {
	case moved && errors.Is(err, os.ErrNotExist):
		lines = strings.Split(strings.TrimSuffix(docsHeader(filepath.Base(dest)), "\n"), "\n")
	case err != nil:
		return "", fmt.Errorf("docs store %s: %w", dest, err)
	}
	// A move looks for its own id, because the original is in the file it is
	// leaving and only an unfinished earlier attempt could have put the
	// replacement here. A rewrite in place looks for the original.
	at := lineOf(fresh, oldID)
	if moved {
		at = lineOf(fresh, r.ID)
	}
	switch {
	case at >= 0:
		lines[at] = formatDocsLine(r)
	case moved:
		lines = append(lines, formatDocsLine(r))
	default:
		return "", fmt.Errorf("docs store %s changed while %s was being replaced", dest, oldID)
	}
	if err := d.write(p, dest, lines, eol); err != nil {
		return "", err
	}
	if r.Private && p.worktree != "" {
		// Before the original goes, because from here the record is in a file
		// git would carry unless it is told not to.
		d.ignorePrivate(p)
	}
	// The replacement is on disk before the original is taken off it. The other
	// order loses both if it fails in the middle — the fact and the correction
	// that was meant to replace it — and leaves nothing to retry from. This
	// order costs a window in which the store holds the fact twice, which
	// dropRecord and oneEntryPerID are between them written to survive and which
	// the next write of either id closes.
	if moved {
		if err := d.dropRecord(p, oldID, dest); err != nil {
			return "", fmt.Errorf("%w; the replacement is stored in %s and the original stays readable until it can be removed", err, filepath.Base(dest))
		}
	}
	if d.opts.Embedder != nil {
		_, vecs := d.vectors(ctx, p, entries)
		delete(vecs, oldID)
		if vec != nil {
			vecs[r.ID] = *vec
		}
		entries[target].Record, entries[target].path = r, dest
		d.saveVectors(p, entries, vecs)
	}
	return r.ID, nil
}

// dropRecord removes every line carrying id from the files under p, except the
// one file named by keep, and leaves the rest of each file — prose and all —
// exactly as it was.
//
// It sweeps the directory rather than editing one known file because Supersede
// writes the replacement before it removes the original: a failure between the
// two leaves the record in two files at once, and an id naming one record is
// what Forget, Supersede and the vector cache all assume. Sweeping is what
// makes the next write of either kind repair that, and it is also what Forget
// means — the fact is gone from everywhere the caller can read it, not from
// the first file it was found in.
//
// A line that is not there is not an error. The record was moved, forgotten or
// edited away by hand meanwhile, and every one of those leaves the caller with
// what it asked for.
func (d *Docs) dropRecord(p place, id, keep string) error {
	names, err := docsFiles(p.dir)
	if err != nil {
		return err
	}
	for _, path := range names {
		if path == keep {
			continue
		}
		lines, eol, fresh, err := readDocsFile(path)
		if err != nil {
			return fmt.Errorf("docs store %s: %w", path, err)
		}
		at := lineOf(fresh, id)
		if at < 0 {
			continue
		}
		if err := d.write(p, path, append(lines[:at], lines[at+1:]...), eol); err != nil {
			return err
		}
	}
	return nil
}

// Forget removes the lines and nothing else. The file stays, header and prose
// included, even when the last record is gone: it is a committed file by then
// and deleting it is a decision for a person.
//
// Every copy of the id goes, not the first one found. There is normally one,
// and the case where there is not — a privacy move that failed between its two
// writes — is exactly the case where leaving the other behind would report a
// fact as forgotten while a copy of it sat in the repository.
func (d *Docs) Forget(ctx context.Context, id string, where Query) error {
	if id == "" {
		return ErrForgetUnidentified
	}
	if where.Kind == KindSession {
		return d.session.Forget(ctx, id, where)
	}
	if where.Kind != KindFact {
		return nil
	}
	places, err := d.placesFor(where)
	if err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, p := range places {
		entries, err := d.scan(p)
		if err != nil {
			return err
		}
		held := false
		for _, e := range entries {
			if e.ID == id {
				held = true
				break
			}
		}
		if !held {
			continue
		}
		if err := d.dropRecord(p, id, ""); err != nil {
			return err
		}
		if d.opts.Embedder != nil {
			kept := entries[:0:0]
			for _, e := range entries {
				if e.ID != id {
					kept = append(kept, e)
				}
			}
			_, vecs := d.vectors(ctx, p, kept)
			d.saveVectors(p, kept, vecs)
		}
		return nil
	}
	return nil
}

// List is every record in one scope, newest first. Records whose timestamps
// tie keep the order they were appended in, later first.
func (d *Docs) List(ctx context.Context, q Query) ([]Record, error) {
	if q.Kind == KindSession {
		return d.session.List(ctx, q)
	}
	if q.Kind != KindFact {
		return nil, nil
	}
	if q.Scope == scope.Any {
		return nil, ErrUnscopedList
	}
	p, err := d.place(q.Scope, q.Project, q.Dir)
	if err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	entries, err := d.scan(p)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, readable(entries[i].Record, q))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// Search ranks with the same measure Local uses, over the union of every
// scope the query admits. An unscoped query naming no project can only be
// answered from the global files: the local ones are one directory per
// checkout and nothing here can enumerate checkouts.
func (d *Docs) Search(ctx context.Context, q Query) ([]Record, error) {
	if q.Kind == KindSession {
		return d.session.Search(ctx, q)
	}
	if q.Kind != KindFact {
		return nil, nil
	}
	places, err := d.placesFor(q)
	if err != nil {
		return nil, err
	}
	qvec := embedWith(ctx, d.opts.Embedder, q.Text)

	d.mu.RLock()
	defer d.mu.RUnlock()

	var recs []Record
	vecs := map[string]vector{}
	for _, p := range places {
		entries, err := d.scan(p)
		if err != nil {
			return nil, err
		}
		here, theirs := d.vectors(ctx, p, entries)
		recs = append(recs, here...)
		for id, v := range theirs {
			vecs[id] = v
		}
	}
	return newRanker(recs, vecs, d.opts.Embedder).search(q, qvec), nil
}

// place resolves where a scope's files are. The answer is remembered for the
// life of the store, for two reasons: the worktree of a checkout does not move
// while a daemon is watching it, so asking git on every recall would put a
// process spawn on the hot path; and the identity alone names no path, so a
// call that arrives without a directory — the janitor forgetting a dead
// session's note, a tidying pass — can only be placed by what an earlier call
// with one established.
//
// A directory that was not seen before is resolved afresh even when the
// identity was: two checkouts of one repository share an identity, and the
// facts belong with the one the session is in.
func (d *Docs) place(s scope.Scope, project, dir string) (place, error) {
	switch s {
	case scope.Global:
		project, dir = "", ""
	case scope.Local:
		if project == "" {
			return place{}, errors.New("docs store: local query has no project")
		}
		// A key is one-way. Refusing is the only honest answer: any directory
		// picked here would be some other project's.
		if isProjectKey(project) {
			return place{}, fmt.Errorf("docs store needs the project's path to find its docs directory and was given only the key %s", project)
		}
	default:
		return place{}, fmt.Errorf("docs store: unknown scope %q", s)
	}

	d.placesMu.Lock()
	defer d.placesMu.Unlock()
	// By key, as everything above this package compares projects: a checkout
	// that was renamed is the same project with a new label.
	key := string(s) + "\x00" + scope.Key(project)
	if dir == "" {
		if p, ok := d.places[key]; ok {
			return p, nil
		}
	} else if p, ok := d.resolved[key+"\x00"+dir]; ok {
		return p, nil
	} else if err := ownsDir(project, dir); err != nil {
		return place{}, err
	}
	var docsDir, worktree string
	if d.opts.Roots != nil {
		docsDir, worktree = d.opts.Roots(s, project, dir)
	} else {
		docsDir, worktree = d.roots(s, project, dir)
	}
	if docsDir == "" {
		if dir == "" {
			return place{}, fmt.Errorf("docs store cannot locate a docs directory for %s %q: no working directory came with the request and none has been seen for it since the daemon started", s, project)
		}
		return place{}, fmt.Errorf("docs store cannot locate a docs directory for %s %q from %q: the directory is not on this machine, so the daemon is not where the checkout is", s, project, dir)
	}
	p := place{scope: s, project: project, dir: docsDir, worktree: worktree}
	d.places[key] = p
	if dir != "" {
		d.resolved[key+"\x00"+dir] = p
	}
	return p, nil
}

// ownsDir refuses a directory that is on this machine and belongs to a
// different project from the identity it arrived with. The pair is what
// decides where a checkout's facts are written and is then remembered for the
// life of the daemon, so a mismatched one costs more than the write it came
// with: the identity is bound to somebody else's checkout, and every later
// call carrying that identity alone reads and writes there.
//
// A directory this machine cannot see is not judged. That is the daemon
// running somewhere the session is not — a container, another host — which is
// exactly what Record.Dir is allowed to be, and the resolver refuses it on its
// own terms.
func ownsDir(project, dir string) error {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	at, err := scope.Project(dir)
	if err != nil || scope.Key(at) == scope.Key(project) {
		return nil
	}
	return fmt.Errorf("docs store was given project %s together with directory %q, which is checkout %s: the two name different projects and nothing here can tell which of them the caller meant",
		scope.Label(project), dir, scope.Label(at))
}

// roots is the default resolver. It works from the caller's directory, and
// falls back to the identity only when that is itself a path, which is what
// scope.Project produces for a directory outside git. A directory that does
// not exist here is refused rather than created: the checkout is on another
// machine, and a docs tree grown inside a container is one nobody will ever
// commit.
func (d *Docs) roots(s scope.Scope, project, dir string) (string, string) {
	if s == scope.Global {
		return d.opts.GlobalDir, ""
	}
	if dir == "" && filepath.IsAbs(project) {
		dir = project
	}
	if !filepath.IsAbs(dir) {
		return "", ""
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", ""
	}
	return DocsDirFor(dir, d.opts.DirName)
}

// DocsDirFor is where the docs store keeps local facts for a directory: name
// under the git worktree containing dir, or under dir itself outside git. The
// worktree is empty in the second case. Under the default name an existing
// doc/ is reused, because a project that chose that spelling should not gain
// a second directory beside it.
func DocsDirFor(dir, name string) (docsDir, worktree string) {
	if name == "" {
		name = DefaultDocsDirName
	}
	worktree = gitToplevel(dir)
	root := worktree
	if root == "" {
		root = dir
	}
	candidates := []string{name}
	if name == DefaultDocsDirName {
		candidates = append(candidates, "doc")
	}
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(root, c)); err == nil && info.IsDir() {
			return filepath.Join(root, c), worktree
		}
	}
	return filepath.Join(root, name), worktree
}

func gitToplevel(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output() //nolint:gosec // G204: git with literal arguments in the project's own directory
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// placesFor is every place a query reads: one for a scoped query, and for an
// unscoped one the global files plus the named project's, if any was named.
func (d *Docs) placesFor(q Query) ([]place, error) {
	var out []place
	if q.Scope == scope.Global || q.Scope == scope.Any {
		p, err := d.place(scope.Global, "", "")
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if q.Scope == scope.Local || (q.Scope == scope.Any && q.Project != "") {
		p, err := d.place(scope.Local, q.Project, q.Dir)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if q.Scope != scope.Global && q.Scope != scope.Local && q.Scope != scope.Any {
		return nil, fmt.Errorf("docs store: unknown scope %q", q.Scope)
	}
	return out, nil
}

// docsFiles is every file under dir this connector owns, in name order. The
// directory is read rather than globbed: a checkout under a path holding a
// glob metacharacter matches nothing, and a place that reads as empty is one
// where the duplicate checks pass, Store appends another copy, and a supersede
// cannot find what it was told to replace.
func docsFiles(dir string) ([]string, error) {
	read, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("docs store %s: %w", dir, err)
	}
	names := make([]string, 0, len(read))
	for _, e := range read {
		if !e.IsDir() && strings.HasSuffix(e.Name(), docsSuffix) {
			names = append(names, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(names)
	return names, nil
}

// scan is every record under a place, in file name order and then line order,
// one entry per id. Files are re-parsed only when their size or modification
// time changed, which is what lets a hand edit be seen without making every
// recall re-read a directory it read a second ago.
func (d *Docs) scan(p place) ([]docsEntry, error) {
	names, err := docsFiles(p.dir)
	if err != nil {
		return nil, err
	}

	d.filesMu.Lock()
	defer d.filesMu.Unlock()

	var out []docsEntry
	for _, path := range names {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		f, ok := d.files[path]
		if !ok || !f.modTime.Equal(info.ModTime()) || f.size != info.Size() {
			_, _, entries, err := readDocsFile(path)
			if err != nil {
				return nil, fmt.Errorf("docs store %s: %w", path, err)
			}
			private := filepath.Base(path) == "USER"+docsSuffix
			for i := range entries {
				entries[i].Scope = p.scope
				entries[i].Project = p.project
				entries[i].Private = private
			}
			f = &docsFile{modTime: info.ModTime(), size: info.Size(), entries: entries}
			d.files[path] = f
		}
		out = append(out, f.entries...)
	}
	return oneEntryPerID(out), nil
}

// oneEntryPerID keeps a single entry for each id, preferring the copy that is
// not private.
//
// There is normally one anyway: an id is the hash of the content and Store
// refuses a sentence the place already holds. Two arise in one situation, a
// privacy move whose second write failed, and the public copy is the one to
// show because it is the one that is true — the fact is still in a file git
// carries. Preferring the private copy would report the move as done while the
// repository still held the record, which is the only reading of the two that
// nobody can act on.
func oneEntryPerID(entries []docsEntry) []docsEntry {
	at := make(map[string]int, len(entries))
	out := entries[:0:0]
	for _, e := range entries {
		i, seen := at[e.ID]
		if !seen {
			at[e.ID] = len(out)
			out = append(out, e)
			continue
		}
		if out[i].Private && !e.Private {
			out[i] = e
		}
	}
	return out
}

// heldElsewhere reports whether a local supersede's target is current in the
// global files, which is the one other place readable from here. It decides
// only the wording of a refusal that stands either way.
func (d *Docs) heldElsewhere(oldID string, p place) bool {
	if p.scope != scope.Local {
		return false
	}
	global, err := d.place(scope.Global, "", "")
	if err != nil {
		return false
	}
	entries, err := d.scan(global)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.ID == oldID {
			return true
		}
	}
	return false
}

// lineOf finds a record's line in a fresh parse rather than trusting the index
// remembered from an earlier one, so an edit made between the two cannot have
// the daemon rewrite somebody's prose.
func lineOf(entries []docsEntry, id string) int {
	for _, e := range entries {
		if e.ID == id {
			return e.line
		}
	}
	return -1
}

// write replaces a file through a temporary one in the same directory. Files
// in a repository are readable by the group and the world like any other
// source file; the global ones hold what a person said in front of an agent
// and are theirs alone.
//
// eol is the ending the file was read with, so that a repository whose files
// are checked out with carriage returns gets one bullet changed rather than
// every line of the file rewritten the first time the daemon touches it.
func (d *Docs) write(p place, path string, lines []string, eol string) error {
	dirMode, fileMode := os.FileMode(0o755), os.FileMode(0o644)
	if p.scope == scope.Global {
		dirMode, fileMode = 0o700, 0o600
	}
	if eol == "" {
		eol = docsLF
	}
	if err := writeAtomically(path, []byte(strings.Join(lines, eol)+eol), dirMode, fileMode); err != nil {
		return fmt.Errorf("docs store %s: %w", path, err)
	}
	d.filesMu.Lock()
	delete(d.files, path)
	d.filesMu.Unlock()
	return nil
}

func writeAtomically(path string, body []byte, dirMode, fileMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if err := tmp.Chmod(fileMode); err != nil {
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ignorePrivate keeps the private file out of the repository. It touches
// .gitignore only when that is safe to do without asking: the line is absent,
// and the file has no changes of the person's own that a commit would then
// sweep up with the daemon's. Otherwise it says so and leaves the fact where
// it was stored; the person adds the line, or commits, and the next private
// fact will.
func (d *Docs) ignorePrivate(p place) {
	rel, err := filepath.Rel(p.worktree, filepath.Join(p.dir, "USER"+docsSuffix))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	line := filepath.ToSlash(rel)
	path := filepath.Join(p.worktree, ".gitignore")
	log := d.opts.Log.With("path", path, "line", line)

	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the worktree's own .gitignore
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("private facts are stored but cannot be kept out of git; add the line by hand", "error", err)
		return
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l == line || l == "/"+line {
			return
		}
	}
	status, err := exec.Command("git", "-C", p.worktree, "status", "--porcelain", "--", ".gitignore").Output() //nolint:gosec // G204: git with literal arguments in the worktree
	if err != nil {
		log.Warn("private facts are stored but cannot be kept out of git; add the line by hand", "error", err)
		return
	}
	if len(bytes.TrimSpace(status)) > 0 {
		log.Warn("private facts are stored but .gitignore has uncommitted changes; add the line by hand or commit and the next private fact will")
		return
	}
	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"
	if err := writeAtomically(path, []byte(body), 0o755, mode); err != nil {
		log.Warn("private facts are stored but cannot be kept out of git; add the line by hand", "error", err)
		return
	}
	log.Info("added the private facts file to .gitignore")
}

// docsCache is the on-disk shape of one place's vectors. Each vector remembers
// the model and the content it was computed from, because the file it belongs
// to is edited by people: an id whose sentence was reworded keeps the id and
// needs a new vector.
type docsCache struct {
	Version int                   `json:"version"`
	Vectors map[string]docsVector `json:"vectors"`
}

type docsVector struct {
	Model   string    `json:"model"`
	Content string    `json:"content"`
	Values  []float32 `json:"values"`
}

func (d *Docs) cachePath(p place) string {
	name := "global"
	if p.scope == scope.Local {
		name = scope.Key(p.project)
	}
	return filepath.Join(d.opts.CacheDir, name+".json")
}

// vectors is the entries as records plus a vector for each, embedding the ones
// the cache does not hold and saving the cache when it did. A cache that
// cannot be read is an empty one: everything is re-embedded and nothing is
// lost but time.
//
// A vector from another model is kept, unused, while the embedder is still
// loading, for the reason Local's pass waits: re-embedding everything with the
// fallback and again with the model minutes later, on every start, is work
// that ends where it began. Those entries are scored on words until then.
func (d *Docs) vectors(ctx context.Context, p place, entries []docsEntry) ([]Record, map[string]vector) {
	recs := make([]Record, len(entries))
	for i, e := range entries {
		recs[i] = e.Record
	}
	if d.opts.Embedder == nil {
		return recs, nil
	}
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	cached := d.loadVectors(p)
	model := d.opts.Embedder.ID()
	loading := false
	if s, ok := d.opts.Embedder.(Settler); ok {
		select {
		case <-s.Settled():
		default:
			loading = true
		}
	}
	out := make(map[string]vector, len(entries))
	changed := false
	for _, r := range recs {
		hash := contentID(r.Content)
		if v, ok := cached[r.ID]; ok && v.Content == hash && (v.Model == model || loading) {
			out[r.ID] = vector{Model: v.Model, Values: v.Values}
			continue
		}
		if v := embedWith(ctx, d.opts.Embedder, r.Content); v != nil {
			out[r.ID] = *v
			changed = true
		}
	}
	if changed || len(cached) != len(out) {
		d.saveVectorsLocked(p, recs, out)
	}
	return recs, out
}

func (d *Docs) loadVectors(p place) map[string]docsVector {
	raw, err := os.ReadFile(d.cachePath(p)) //nolint:gosec // G304: the store's own cache directory
	if err != nil {
		return nil
	}
	var c docsCache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	return c.Vectors
}

// saveVectors writes the cache for exactly the records given, so a vector for
// a record that is gone does not outlive it.
func (d *Docs) saveVectors(p place, entries []docsEntry, vecs map[string]vector) {
	recs := make([]Record, len(entries))
	for i, e := range entries {
		recs[i] = e.Record
	}
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()
	d.saveVectorsLocked(p, recs, vecs)
}

func (d *Docs) saveVectorsLocked(p place, recs []Record, vecs map[string]vector) {
	c := docsCache{Version: fileVersion, Vectors: make(map[string]docsVector, len(recs))}
	for _, r := range recs {
		if v, ok := vecs[r.ID]; ok {
			c.Vectors[r.ID] = docsVector{Model: v.Model, Content: contentID(r.Content), Values: v.Values}
		}
	}
	body, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := writeAtomically(d.cachePath(p), body, 0o700, 0o600); err != nil {
		d.opts.Log.Warn("the vector cache could not be written; entries will be re-embedded on the next read", "path", d.cachePath(p), "error", err)
	}
}
