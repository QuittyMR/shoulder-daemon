package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/facts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/llm"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/memory"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/prompts"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

const (
	// maxSourceBytes bounds one document. Past this it is not prose somebody
	// wrote for another person to read, and feeding it to a model a chunk at a
	// time would spend an afternoon finding that out.
	maxSourceBytes = 1 << 20

	// defaultChunkChars sizes a chunk when the window is unset. It is well
	// under any window a model has, because the cost of a chunk that is too
	// small is a rule split across two calls and the cost of one too large is
	// a call that returns nothing at all.
	defaultChunkChars = 8000
)

// LearnRequest is one ingest of documentation somebody already wrote. Paths
// replace the default list rather than adding to it; Replace deletes each
// document the store accepted.
type LearnRequest struct {
	Scope   scope.Scope
	Project string
	Dir     string
	Paths   []string
	Replace bool
}

// LearnedFile is what one document yielded. Chunks is how many pieces it was
// read in, which is the only number that says how much work was done when a
// document turns out to hold no rules at all.
type LearnedFile struct {
	Path    string
	Chunks  int
	Stored  int
	Skipped int
	Failed  int
	Deleted bool

	// Error is why this document was not read, or not read to the end. It is
	// separate from Failed, which counts chunks the model could not answer and
	// facts the store would not take: one is a document nobody finished
	// looking at, the other a document that was read and partly lost.
	Error string
}

type LearnResult struct {
	Files []LearnedFile
}

// Learn reads documentation into the store.
//
// It exists because a repository that has been worked in for years already
// states most of what this daemon would spend months overhearing: the
// decisions are in an architecture document, the conventions in a style guide,
// the commands in a runbook. Learning a turn at a time from a session is how
// the store stays current, not how it starts.
//
// The scope is the caller's and is stamped on every fact. A document is
// evidence about the codebase it sits in, and a model asked to judge, page by
// page, whether a sentence is about the person instead would file a third of a
// style guide in front of every other project.
func (p *Pipeline) Learn(ctx context.Context, req LearnRequest) (LearnResult, error) {
	prov := p.Settings.Provider()
	if prov == nil {
		return LearnResult{}, errors.New("no decision model is configured")
	}
	if !req.Scope.Valid() {
		return LearnResult{}, memory.ErrUnscoped
	}
	if req.Scope == scope.Local && req.Project == "" {
		return LearnResult{}, errors.New("a local ingest needs a project")
	}
	if req.Dir == "" {
		return LearnResult{}, errors.New("learn reads the files in front of the shell that typed it, and this request named no directory")
	}
	root := realPath(worktreeRoot(req.Dir))

	sources, err := sourcesFor(root, req.Dir, req.Paths)
	if err != nil {
		return LearnResult{}, err
	}
	if req.Replace {
		if err := replaceable(root, p.Cfg.Budget.DryRun, sources); err != nil {
			return LearnResult{}, err
		}
	}

	lctx, cancel := context.WithTimeout(ctx, p.Cfg.LearnTimeout)
	defer cancel()
	p.Metrics.Inc("shoulder_cli_learn_total")

	var out LearnResult
	for _, src := range sources {
		// Stopping is not an error. What was written stays written, a rerun
		// skips it, and the person is owed the account of the files that were
		// read rather than a failure covering all of them.
		if lctx.Err() != nil {
			break
		}
		out.Files = append(out.Files, p.learnFile(lctx, prov, req, src))
	}
	stored, deleted := 0, 0
	for _, f := range out.Files {
		stored += f.Stored
		if f.Deleted {
			deleted++
		}
	}
	p.Metrics.IncBy("shoulder_cli_learned_total", uint64(stored))
	p.Log.Info("documents learned", "origin", "cli", "scope", req.Scope,
		"project", scope.Label(req.Project), "files", len(out.Files),
		"stored", stored, "deleted", deleted)
	return out, nil
}

// learnFile reads one document a chunk at a time. A chunk the model could not
// answer for counts as a failure of that document rather than of the run: the
// rest of it is still worth reading, and the failure is what keeps the file
// from being deleted.
func (p *Pipeline) learnFile(ctx context.Context, prov llm.Provider, req LearnRequest, src source) LearnedFile {
	out := LearnedFile{Path: src.name}
	if src.err != nil {
		out.Error = src.err.Error()
		return out
	}
	body, err := readSource(src.path)
	if err != nil {
		p.Metrics.Inc("shoulder_cli_learn_unread_total")
		out.Error = err.Error()
		return out
	}

	at := site{project: req.Project, dir: req.Dir}
	chunks := chunkMarkdown(body, p.chunkChars())
	out.Chunks = len(chunks)
	for _, chunk := range chunks {
		// A cancelled run reads no further. The error, not a failure per
		// chunk, is what says so, and it keeps --replace from deleting a
		// document only partly learned.
		if ctx.Err() != nil {
			out.Error = "stopped before the whole document was read"
			break
		}
		found, err := p.extract(ctx, prov, src.name, chunk)
		if err != nil {
			// A call cut off by the stop is the run ending, not this chunk
			// failing, and on the last chunk nothing after it would say so.
			if ctx.Err() != nil {
				out.Error = "stopped before the whole document was read"
				break
			}
			p.Metrics.Inc("shoulder_cli_learn_chunk_error_total")
			p.Log.Warn("a piece of a document was not read", "file", src.name, "err", err)
			out.Failed++
			continue
		}
		for i := range found {
			found[i].Scope = req.Scope
			// The model was shown no stored ids, so anything it named here it
			// invented. Corrections against what is already held are the
			// tidying pass's job, not this one's.
			found[i].Supersedes = ""
		}
		// A document is evidence about the codebase, not somebody correcting
		// it: a sentence the store refuses as too close to a fact it holds
		// leaves that fact alone.
		// Read and paid for, so a cancellation from here on keeps what this
		// chunk found; the loop then stops before the next chunk.
		wctx, done := Decided(ctx)
		_, wrote := p.store(wctx, "learn", at, facts.Reconcile(nil, found), nil, keepCollision)
		done()
		out.Stored += wrote.stored
		out.Skipped += wrote.skipped
		out.Failed += wrote.failed
	}
	if req.Replace {
		p.remove(&out, src.path)
	}
	return out
}

// extract asks for the rules in one chunk. The reply is the decision model's
// fact shape and is read by the decision model's own parser, because the thing
// that makes that parser worth having — a small model fencing its JSON, or
// prefacing it with a sentence — has nothing to do with which prompt was sent.
func (p *Pipeline) extract(ctx context.Context, prov llm.Provider, name, chunk string) ([]facts.Fact, error) {
	ectx, cancel := context.WithTimeout(ctx, p.Cfg.MessageTimeout)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "<document>%s</document>\n\n<markdown>\n", name)
	b.WriteString(chunk)
	b.WriteString("\n</markdown>")

	raw, err := prov.Complete(ectx, prompts.Learn, b.String())
	if err != nil {
		return nil, err
	}
	decision, err := llm.ParseDecision(raw)
	if err != nil {
		return nil, err
	}
	return decision.Facts, nil
}

// remove deletes a document the store has taken. Every branch here is a reason
// not to: --replace moves a document into the memory, and a deletion that
// moved nothing is just a deletion.
func (p *Pipeline) remove(f *LearnedFile, path string) {
	if f.Failed > 0 || f.Error != "" || f.Stored+f.Skipped == 0 {
		return
	}
	if err := os.Remove(path); err != nil {
		f.Error = err.Error()
		return
	}
	f.Deleted = true
	p.Metrics.Inc("shoulder_cli_learn_deleted_total")
	p.Log.Info("document deleted after the store took what it said", "file", f.Path)
}

// chunkChars is how much markdown one call sees. The window the daemon was
// configured with is the honest bound: it is the same model, and a chunk it
// cannot hold is a chunk it answers about the half it read.
func (p *Pipeline) chunkChars() int {
	if p.Cfg.WindowChars > 0 {
		return p.Cfg.WindowChars
	}
	return defaultChunkChars
}

// source is one document to read, with the name the person will recognise it
// by. err is set for a path that was named but must not be read, so the answer
// says so against that file rather than silently returning fewer.
type source struct {
	path string
	name string
	err  error
}

// sourcesFor is the list of documents to read: the ones named, or the default
// list under the worktree when none were.
func sourcesFor(root, dir string, paths []string) ([]source, error) {
	var found []source
	if len(paths) == 0 {
		defaults, err := defaultSources(root)
		if err != nil {
			return nil, err
		}
		found = defaults
	} else {
		for _, raw := range paths {
			named, err := namedSources(raw, dir)
			if err != nil {
				return nil, err
			}
			found = append(found, named...)
		}
	}

	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	seen := make(map[string]bool, len(found))
	out := make([]source, 0, len(found))
	for _, s := range found {
		if seen[s.path] {
			continue
		}
		seen[s.path] = true
		s.name = displayName(root, s.path)
		out = append(out, s)
	}
	return out, nil
}

// defaultSources is what a repository keeps its documentation in: the
// documents at the top of the worktree, and everything under its docs
// directory. Everything else is source code, and a model asked to find the
// rules in a repository would spend the afternoon reading it.
func defaultSources(root string) ([]source, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("nothing to read: %w", err)
	}
	var out []source
	for _, e := range entries {
		if !e.Type().IsRegular() || !markdown(e.Name()) || excluded(e.Name()) {
			continue
		}
		out = append(out, source{path: filepath.Join(root, e.Name())})
	}
	// Both spellings, because a repository that has one has never had the
	// other and naming the wrong one reads as a repository with no docs.
	for _, name := range []string{"docs", "doc"} {
		dir := filepath.Join(root, name)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		under, err := walkMarkdown(dir, excluded)
		if err != nil {
			return nil, err
		}
		out = append(out, under...)
	}
	return out, nil
}

// namedSources is one path the person typed. Naming a file overrides the
// default list, so the exclusions that only kept a document out of that list
// no longer apply; the two that are not about the list still do.
func namedSources(raw, dir string) ([]source, error) {
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	path = realPath(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("nothing to read: %w", err)
	}
	if info.IsDir() {
		return walkMarkdown(path, protected)
	}
	if protected(filepath.Base(path)) {
		return []source{{path: path, err: errors.New("this is the daemon's own file or the agent's instructions, which it must never learn from")}}, nil
	}
	return []source{{path: path}}, nil
}

// walkMarkdown collects the markdown under a directory. The directories it
// steps over are not exclusions but traversal: a repository's dependencies and
// its build output hold thousands of other people's documents.
func walkMarkdown(dir string, skip func(string) bool) ([]source, error) {
	var out []source
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			if path != dir && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		case !d.Type().IsRegular() || !markdown(d.Name()) || skip(d.Name()):
			return nil
		}
		out = append(out, source{path: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("nothing to read: %w", err)
	}
	return out, nil
}

func markdown(name string) bool { return strings.EqualFold(filepath.Ext(name), ".md") }

// protected is the two exclusions that hold however a file was asked for. The
// daemon's own files are what it has already stored, and reading them back
// would file every fact a second time in another store's words; the private
// one among them is the file a backend deliberately keeps out of the
// repository. The agent instruction files are orders to a harness, not
// knowledge about a codebase, and they are already read on every turn.
func protected(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".shoulder.md") {
		return true
	}
	switch lower {
	case "claude.md", "claude.local.md", "agents.md":
		return true
	}
	return false
}

// excluded is protected plus the documents every repository has and none of
// them state a rule in: a description of the project, a list of its releases,
// its licence, and instructions for contributing to it.
func excluded(name string) bool {
	if protected(name) {
		return true
	}
	lower := strings.ToLower(name)
	return lower == "readme.md" ||
		strings.HasPrefix(lower, "changelog") ||
		strings.HasPrefix(lower, "license") ||
		strings.HasPrefix(lower, "contributing")
}

// skipDir names the directories a walk never enters. A dot directory covers
// .claude and everything else a tool keeps its own state in.
func skipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "dist", "build":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// displayName is what the person called the file, which is the path relative
// to their worktree wherever there is one.
func displayName(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// realPath resolves the symlinks in a path, because everything here is
// compared against a root git resolved and the shell's idea of the same
// directory is routinely spelled differently: /tmp is a symlink on some
// machines, and a home directory reached through one is common.
func realPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}

// readSource reads a document, or says why it will not.
func readSource(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxSourceBytes {
		return "", fmt.Errorf("%d bytes is past the %d a document is read up to", info.Size(), maxSourceBytes)
	}
	body, err := os.ReadFile(path) //nolint:gosec // G304: the path is the person's own worktree, named at their own terminal
	if err != nil {
		return "", err
	}
	if !utf8.Valid(body) {
		return "", errors.New("this is not text")
	}
	return string(body), nil
}

// replaceable decides whether anything may be deleted at all, once, before the
// first document is read. Once the run starts the store itself may be writing
// into this worktree, so the tree is never clean again to ask a second time.
func replaceable(root string, dryRun bool, sources []source) error {
	if dryRun {
		return errors.New("--replace would delete documents while SHOULDER_DRY_RUN keeps the daemon from storing anything, which loses them; unset it in the daemon's environment or run this without --replace")
	}
	// A document outside the worktree is one the check below says nothing
	// about, and it is named rather than its own relative path.
	for _, s := range sources {
		if s.name == s.path {
			return fmt.Errorf("--replace deletes only what is inside %s, and %s is outside it; run this without --replace", root, s.path)
		}
	}
	// --untracked-files=all rather than the default, because
	// status.showUntrackedFiles=no is a setting people have and it hides
	// exactly the files this refuses to delete.
	out, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=all").Output() //nolint:gosec // G204: git with literal arguments in the worktree the command was typed in
	if err != nil {
		return fmt.Errorf("--replace deletes the documents it read, and %s is not a git worktree, so nothing could bring them back; run this without --replace", root)
	}
	if dirty := strings.TrimSpace(string(out)); dirty != "" {
		return fmt.Errorf("--replace refuses to delete anything while %s has uncommitted changes, because committing first is the only thing that makes a deletion undoable; git status says:\n%s",
			root, textutil.Clip(dirty, 400))
	}
	return tracked(root, sources)
}

// tracked refuses --replace for a document git has never held. A clean status
// is not the same promise as `git checkout` being able to give a file back: it
// says nothing about an ignored file, and nothing at all about untracked ones
// where the person has set status.showUntrackedFiles to no. A dotfiles
// repository that ignores everything but what it tracks is the ordinary way to
// meet the first, and its documents are on the default list.
func tracked(root string, sources []source) error {
	args := []string{"-C", root, "ls-files", "-z", "--"}
	var want []string
	for _, s := range sources {
		// A source that will not be read will not be deleted either.
		if s.err != nil {
			continue
		}
		rel := filepath.ToSlash(s.name)
		want = append(want, rel)
		// Literal, because a document whose name holds a glob character is a
		// pathspec that matches some other file or none.
		args = append(args, ":(literal)"+rel)
	}
	if len(want) == 0 {
		return nil
	}
	out, err := exec.Command("git", args...).Output() //nolint:gosec // G204: git with the sources' own paths, inside the worktree they were found in
	if err != nil {
		return fmt.Errorf("--replace deletes the documents it read, and git in %s could not say which of them it tracks, so nothing could promise them back; run this without --replace", root)
	}
	held := make(map[string]bool, len(want))
	for _, name := range strings.Split(string(out), "\x00") {
		held[name] = true
	}
	for _, name := range want {
		if !held[name] {
			return fmt.Errorf("--replace refuses to delete %s in %s: git does not track it, so it is ignored or was never added and no checkout can bring it back; commit it first or run this without --replace", name, root)
		}
	}
	return nil
}

// worktreeRoot is the top of the checkout the command was typed in, and that
// directory itself when it is not a repository: a directory outside git is a
// project like any other, and its documents are the ones in front of it.
func worktreeRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output() //nolint:gosec // G204: git with literal arguments in the directory the command was typed in
	if err != nil {
		return dir
	}
	if root := strings.TrimSpace(string(out)); root != "" {
		return root
	}
	return dir
}

// chunkMarkdown cuts a document at its headings and packs the sections back up
// to limit characters, so one call sees whole sections and as many of them as
// the window allows. A section too long on its own is cut at line boundaries;
// nothing else is.
//
// A heading inside a fenced code block is not a heading. A shell comment in an
// example is written exactly like one, and cutting there hands the model half
// a command and calls it a document.
func chunkMarkdown(text string, limit int) []string {
	if limit <= 0 {
		limit = defaultChunkChars
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			out = append(out, strings.TrimRight(cur.String(), "\n"))
		}
		cur.Reset()
	}
	for _, section := range sections(text) {
		for _, part := range fit(section, limit) {
			if cur.Len() > 0 && cur.Len()+len(part) > limit {
				flush()
			}
			cur.WriteString(part)
		}
	}
	flush()
	return out
}

// sections splits at every heading. Which headings are worth splitting at is
// decided afterwards by the size, because the level that carries a document is
// ## in one and ### in the next.
func sections(text string) []string {
	var out []string
	var cur strings.Builder
	fenced := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if fence(line) {
			fenced = !fenced
		}
		if !fenced && heading(line) && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// heading is an ATX heading. Four spaces of indent make it an indented code
// block instead, which is where a shell comment goes when nobody fenced it.
func heading(line string) bool {
	s := strings.TrimLeft(line, " ")
	if len(line)-len(s) > 3 {
		return false
	}
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return false
	}
	rest := s[n:]
	return rest == "" || rest == "\n" || rest[0] == ' ' || rest[0] == '\t'
}

func fence(line string) bool {
	s := strings.TrimLeft(line, " ")
	return strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~")
}

// fit breaks a section that is too long to be a chunk, at line boundaries.
func fit(section string, limit int) []string {
	if len(section) <= limit {
		return []string{section}
	}
	var out []string
	var cur strings.Builder
	for _, line := range strings.SplitAfter(section, "\n") {
		for _, part := range cutRunes(line, limit) {
			if cur.Len() > 0 && cur.Len()+len(part) > limit {
				out = append(out, cur.String())
				cur.Reset()
			}
			cur.WriteString(part)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// cutRunes breaks a single line longer than a whole chunk, on a rune boundary
// so the model is never handed half a character. Generated markdown has lines
// like this; prose does not.
func cutRunes(s string, limit int) []string {
	var out []string
	for len(s) > limit {
		n := limit
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		if n == 0 {
			break
		}
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}
