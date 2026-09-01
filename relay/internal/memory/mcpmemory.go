package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
	"gitlab.com/quittymr/shoulder-daemon/relay/internal/textutil"
)

// Compile-time proof that this connector still satisfies the boundary. Without
// it a signature drift only surfaces at the first caller, which may be in
// another package entirely.
var _ Connector = (*MCPMemory)(nil)

// Placement is carried as tags because tags are the one classification every
// build of this server supports and can filter on. The prefix keeps them clear
// of tags a user set themselves.
const (
	tagScopePrefix   = "shoulder-scope:"
	tagProjectPrefix = "shoulder-project:"
	tagKindPrefix    = "shoulder-kind:"
	tagSessionPrefix = "shoulder-session:"
)

// placementTags renders a record's placement as backend tags.
func placementTags(sc scope.Scope, project string, k Kind) []string {
	tags := []string{tagScopePrefix + string(sc)}
	if sc == scope.Local {
		tags = append(tags, tagProjectPrefix+scope.Key(project))
	}
	return append(tags, kindTags(k)...)
}

// kindTags leaves a fact untagged. The absence is what makes every record
// written before this distinction existed still read as the fact it is.
func kindTags(k Kind) []string {
	if k == KindFact {
		return nil
	}
	return []string{tagKindPrefix + string(k)}
}

// queryTags renders a filter as the tags a record must carry to match it. An
// unscoped query has no scope requirement, and asking for facts is a tag's
// absence, which no tag query can express — so the kind is settled again on the
// records that come back rather than here.
func queryTags(q Query) []string {
	if q.Scope == scope.Any {
		return kindTags(q.Kind)
	}
	return placementTags(q.Scope, q.Project, q.Kind)
}

// writeTags is everything a record puts on the wire: the user's own tags plus
// the ones this package uses to place and identify it. The session mark is
// written here and nowhere else, because it belongs to a record rather than to
// a query — no read filters on it, and one that did would need a tag predicate
// the search endpoint does not have.
func writeTags(r Record) []string {
	tags := append(append([]string{}, r.Tags...), placementTags(r.Scope, r.Project, r.Kind)...)
	if r.Session != "" {
		tags = append(tags, tagSessionPrefix+r.Session)
	}
	return tags
}

func hasAll(got, want []string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

const maxBody = 4 << 20

// This server's semantic deduplication is not benign, which is why the generic
// ErrDuplicateSemantic carries the collided hash. A correction of a fact is by
// construction almost identical to the fact it corrects — same subject, same
// vocabulary — so deduplication rejects precisely the writes that matter most,
// and the stale fact keeps being recalled with no record that it was disputed.

// MCPMemory is a connector for doobidoo/mcp-memory-service (verified against
// v11.10.0).
//
// It uses the REST API for search and store, and the JSON-RPC endpoint only for
// versioned updates. That split is forced by the server: the MCP tools return
// human-readable prose rather than JSON, so parsing them means regexing English,
// while REST returns typed documents. memory_update is the one operation REST
// cannot express.
type MCPMemory struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	// Metrics is optional and may stay nil. What it reports is the shape of
	// what the server sent back rather than the answer this connector gave, so
	// it is the only place a caller can find out that its answer was short
	// because the store is full of something else.
	Metrics Counter

	id atomic.Uint64
}

func NewMCPMemory(baseURL, apiKey string, timeout time.Duration) *MCPMemory {
	return &MCPMemory{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

func (m *MCPMemory) Name() string { return "mcp-memory-service" }

func (m *MCPMemory) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.APIKey != "" {
		req.Header.Set("X-API-Key", m.APIKey)
	}

	resp, err := m.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &statusError{
			Status: resp.StatusCode,
			msg:    fmt.Sprintf("%s %s: status %d: %s", m.Name(), path, resp.StatusCode, textutil.Clip(string(raw), 200)),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: unparseable response: %w", m.Name(), path, err)
	}
	return nil
}

// statusError carries the HTTP status alongside the message so one caller can
// tell an outcome apart from a fault. Only Forget needs it: a record that is
// already gone answers 404, and that is the state it asked for.
type statusError struct {
	Status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

type restMemory struct {
	Content     string         `json:"content"`
	ContentHash string         `json:"content_hash"`
	Tags        []string       `json:"tags"`
	MemoryType  string         `json:"memory_type"`
	Metadata    map[string]any `json:"metadata"`
	CreatedISO  string         `json:"created_at_iso"`
}

func (r restMemory) supersededBy() string {
	if r.Metadata == nil {
		return ""
	}
	s, _ := r.Metadata["superseded_by"].(string)
	return s
}

// toRecord rebuilds a Record, splitting shoulder-daemon's placement tags back
// out of the user-visible ones. project is the path the query asked about,
// which the tags cannot carry: they hold its key, not the directory.
func (r restMemory) toRecord(score float64, project string) Record {
	rec := Record{
		ID: r.ContentHash, Content: r.Content, Category: r.MemoryType,
		Score: score, Project: project,
	}
	for _, t := range r.Tags {
		switch {
		case strings.HasPrefix(t, tagScopePrefix):
			rec.Scope = scope.Scope(strings.TrimPrefix(t, tagScopePrefix))
		case strings.HasPrefix(t, tagProjectPrefix):
			if rec.Project == "" {
				rec.Project = strings.TrimPrefix(t, tagProjectPrefix)
			}
		case strings.HasPrefix(t, tagKindPrefix):
			rec.Kind = Kind(strings.TrimPrefix(t, tagKindPrefix))
		case strings.HasPrefix(t, tagSessionPrefix):
			rec.Session = strings.TrimPrefix(t, tagSessionPrefix)
		default:
			rec.Tags = append(rec.Tags, t)
		}
	}
	if rec.Scope == scope.Global {
		rec.Project = ""
	}
	if t, err := time.Parse(time.RFC3339, r.CreatedISO); err == nil {
		rec.CreatedAt = t
	}
	return rec
}

// Search returns the current records of the kind the query asked for, which is
// facts unless it said otherwise.
//
// Superseded memories are filtered here rather than by the server. In v11.10.0
// a versioned update writes the marker into the metadata JSON blob, while the
// query filters on a `superseded_by` column that stays NULL, so the server
// returns superseded facts on both surfaces and `include_superseded` has no
// effect. Observed directly: after superseding "releases ship on Fridays" with
// "releases never ship on Fridays", the server returned both and ranked the
// superseded one higher. Handing a model a fact and its own retraction is worse
// than handing it nothing.
func (m *MCPMemory) Search(ctx context.Context, q Query) ([]Record, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	// Over-fetch so that filtering superseded and out-of-scope rows does not
	// silently shrink the result set below what the caller asked for. Scope is
	// filtered here rather than server-side because the search endpoint ranks
	// semantically and offers no tag predicate; asking for more and discarding
	// is the only way to combine the two.
	fetch := limit * 2
	if q.Scope != scope.Any {
		fetch = limit * 4
	}

	// A fixed multiple is only ever a guess about how much of the index is
	// something else, and the guess degrades as the store fills: working notes
	// are written one per session per project and are made of that project's
	// own vocabulary, so they rank alongside its facts and grow without bound.
	// Past a few dozen of them a fixed over-fetch returns a page that is
	// entirely discarded, the answer is empty, and the caller cannot tell that
	// from a store with nothing in it. Asking again for more, up to a ceiling,
	// is what keeps a full store from reading as an empty one.
	var (
		recs      []Record
		wrongKind map[Kind]int
		rows      int
		asked     bool // a wider page has been requested at least once
		capped    bool // the server would not widen it
	)
	for {
		page, err := m.searchPage(ctx, q.Text, fetch)
		if err != nil {
			return nil, err
		}
		prev := rows
		rows = len(page)
		recs, wrongKind = selectMatching(q, page, limit)
		if len(recs) >= limit || fetch >= searchFetchCeiling {
			break
		}
		// A server that answers a larger request with no more rows than the
		// last one is capping the page itself, and asking again only repeats
		// the question. Distinguishing that from a genuinely exhausted store
		// matters: exhausted means the answer is short because that is all
		// there is, capped means the rest is unreachable from here and the
		// caller is being handed silence that looks like emptiness.
		if rows < fetch {
			if asked && rows <= prev {
				capped = true
			}
			break
		}
		asked = true
		fetch *= searchFetchGrowth
		if fetch > searchFetchCeiling {
			fetch = searchFetchCeiling
		}
	}

	for k, n := range wrongKind {
		countN(m.Metrics, discardCounter(k), n)
	}
	if len(recs) < limit && (capped || rows >= fetch) {
		// The ceiling was reached with the server still willing to return more,
		// so the answer is short because of what the store holds rather than
		// because that is all there is. Without this the failure is a caller
		// quietly receiving nothing.
		count(m.Metrics, "shoulder_memory_search_ceiling_total")
	}
	return recs, nil
}

const (
	// searchFetchCeiling bounds the widening. The point of the search is to put
	// a handful of records in front of a model; a request large enough to walk
	// the whole index would cost more than the answer is worth, and reaching
	// this is itself the signal that the store needs tidying rather than
	// re-reading.
	searchFetchCeiling = 500

	// searchFetchGrowth widens fast because the cost of another round trip is
	// paid whether it helps or not, and because the population being skipped
	// grows with the number of sessions rather than one record at a time.
	searchFetchGrowth = 4
)

type searchHit struct {
	Memory     restMemory `json:"memory"`
	Similarity *float64   `json:"similarity_score"`
}

func (m *MCPMemory) searchPage(ctx context.Context, text string, n int) ([]searchHit, error) {
	var out struct {
		Results []searchHit `json:"results"`
	}
	if err := m.do(ctx, http.MethodPost, "/api/search",
		map[string]any{"query": text, "n_results": n}, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// selectMatching narrows one ranked page to the records the query asked for,
// and says how many were thrown away for being the wrong kind.
//
// Kind is settled before placement so that the tally is complete on both
// surfaces: asking for working notes expresses the kind as a tag the facts do
// not carry, so a placement test would have discarded them before anything
// knew what they were.
func selectMatching(q Query, page []searchHit, limit int) ([]Record, map[Kind]int) {
	want := queryTags(q)
	recs := make([]Record, 0, limit)
	var wrongKind map[Kind]int
	for _, r := range page {
		if r.Memory.supersededBy() != "" {
			continue
		}
		var score float64
		if r.Similarity != nil {
			score = *r.Similarity
		}
		rec := r.Memory.toRecord(score, q.Project)
		if rec.Kind != q.Kind {
			if wrongKind == nil {
				wrongKind = map[Kind]int{}
			}
			wrongKind[rec.Kind]++
			continue
		}
		if !hasAll(r.Memory.Tags, want) {
			continue
		}
		if q.MinScore > 0 && score > 0 && score < q.MinScore {
			continue
		}
		recs = append(recs, rec)
		if len(recs) == limit {
			break
		}
	}
	return recs, wrongKind
}

// discardCounter names the counter for a record dropped from an answer because
// it was not the kind that was asked for. The kind is part of the name rather
// than a label because this exposition carries no labels, and it is chosen from
// a closed set rather than interpolated from the record so that a tag somebody
// invents in the backend cannot mint counter series.
func discardCounter(k Kind) string {
	switch k {
	case KindSession:
		return "shoulder_memory_discarded_session_total"
	case KindFact:
		return "shoulder_memory_discarded_fact_total"
	default:
		return "shoulder_memory_discarded_other_total"
	}
}

// List returns everything held under a scope, newest first. A digest needs the
// whole set rather than the nearest few, so it goes to the tag endpoint: a
// semantic search with no query text ranks against nothing and returns an
// arbitrary slice.
//
// The scope a listing needs is required of every backend at the boundary, in
// Checked, rather than here: reading one project's knowledge alongside
// another's must be impossible whichever connector is configured.
func (m *MCPMemory) List(ctx context.Context, q Query) ([]Record, error) {
	want := queryTags(q)

	var out struct {
		Memories []restMemory `json:"memories"`
		Results  []restMemory `json:"results"`
	}
	if err := m.do(ctx, http.MethodPost, "/api/search/by-tag",
		map[string]any{"tags": want, "match_all": true}, &out); err != nil {
		return nil, err
	}
	found := out.Memories
	if len(found) == 0 {
		found = out.Results
	}

	recs := make([]Record, 0, len(found))
	for _, r := range found {
		if r.supersededBy() != "" {
			continue
		}
		if !hasAll(r.Tags, want) {
			continue
		}
		rec := r.toRecord(0, q.Project)
		if rec.Kind != q.Kind {
			continue
		}
		recs = append(recs, rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].CreatedAt.After(recs[j].CreatedAt) })
	if q.Limit > 0 && len(recs) > q.Limit {
		recs = recs[:q.Limit]
	}
	return recs, nil
}

func (m *MCPMemory) Store(ctx context.Context, r Record) (string, error) {
	in := map[string]any{"content": r.Content, "tags": writeTags(r), "metadata": map[string]any{}}
	if r.Category != "" {
		in["memory_type"] = r.Category
	}
	// conversation_id is deliberately omitted: supplying it disables the
	// server's own semantic deduplication, which backs up fact reconciliation.

	var out struct {
		Success     bool   `json:"success"`
		Message     string `json:"message"`
		ContentHash string `json:"content_hash"`
	}
	if err := m.do(ctx, http.MethodPost, "/api/memories", in, &out); err != nil {
		return "", err
	}
	if !out.Success && out.ContentHash == "" {
		// An exact duplicate is the expected outcome for a fact already known,
		// not a failure. Reporting it as an error would fill the log with the
		// most common result.
		msg := strings.ToLower(out.Message)
		if strings.Contains(msg, "semantically similar") {
			collided := ""
			if m := collidedRe.FindStringSubmatch(out.Message); len(m) == 2 {
				collided = m[1]
			}
			return "", &ErrDuplicateSemantic{Collided: collided}
		}
		if strings.Contains(msg, "duplicate") {
			return "", ErrDuplicateExact
		}
		return "", fmt.Errorf("%s store refused: %s", m.Name(), textutil.Clip(out.Message, 200))
	}
	return out.ContentHash, nil
}

// Forget deletes a record. It is the only way the working notes this daemon
// writes ever leave the store: they are superseded turn after turn, and a
// supersede replaces rather than removes, so the population of one note per
// session per project only ever grows.
//
// A record that is already absent answers 404, which is reported as success:
// the caller asked for it to be gone and it is, and a janitor that logged a
// failure for every already-tidy session would train whoever reads the log to
// ignore it.
// where is unused here: the boundary has already confirmed the record is in
// that scope, and this connector deletes by content hash.
func (m *MCPMemory) Forget(ctx context.Context, id string, _ Query) error {
	if id == "" {
		return ErrForgetUnidentified
	}
	err := m.do(ctx, http.MethodDelete, "/api/memories/"+url.PathEscape(id), nil, nil)
	var status *statusError
	if errors.As(err, &status) && status.Status == http.StatusNotFound {
		return nil
	}
	return err
}

var (
	newHashRe  = regexp.MustCompile(`New hash:\s*([0-9a-f]{16,})`)
	collidedRe = regexp.MustCompile(`similar to ([0-9a-f]{16,})`)
)

// Supersede records the new content as a version of the old fact. This is the
// one operation REST cannot express, so it goes over JSON-RPC, whose reply is
// prose of the form "Versioned update successful. New hash: <hash>, parent
// hash: <hash>".
//
// Requires the sqlite_vec backend; other backends reject versioned updates.
func (m *MCPMemory) Supersede(ctx context.Context, oldID string, r Record) (string, error) {
	if oldID == "" {
		return m.Store(ctx, r)
	}
	updates := map[string]any{
		"content": r.Content,
		// Placement tags are rewritten with the content: a supersede that
		// dropped them would leave the replacement invisible to every scoped
		// read, which looks exactly like the fact having been deleted.
		"tags": writeTags(r),
	}
	if r.Category != "" {
		updates["memory_type"] = r.Category
	}

	text, err := m.callTool(ctx, "memory_update", map[string]any{
		"content_hash": oldID,
		"updates":      updates,
		"versioned":    true,
	})
	if err != nil {
		return "", err
	}
	if match := newHashRe.FindStringSubmatch(text); len(match) == 2 {
		return match[1], nil
	}
	return "", fmt.Errorf("%s supersede: no new hash in reply: %s", m.Name(), textutil.Clip(text, 200))
}

// callTool invokes an MCP tool and returns its text content. The tools answer
// in prose, so callers must parse what they need out of it.
func (m *MCPMemory) callTool(ctx context.Context, tool string, args map[string]any) (string, error) {
	in := map[string]any{
		"jsonrpc": "2.0",
		"id":      m.id.Add(1),
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	var out struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := m.do(ctx, http.MethodPost, "/mcp", in, &out); err != nil {
		return "", err
	}
	if out.Error != nil {
		return "", fmt.Errorf("%s %s: rpc error %d: %s", m.Name(), tool, out.Error.Code, out.Error.Message)
	}
	if out.Result == nil || len(out.Result.Content) == 0 {
		return "", nil
	}
	text := out.Result.Content[0].Text
	if out.Result.IsError {
		return "", fmt.Errorf("%s %s: %s", m.Name(), tool, textutil.Clip(text, 200))
	}
	return text, nil
}
