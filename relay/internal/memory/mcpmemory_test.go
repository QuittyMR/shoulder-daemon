package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

type capture struct {
	path string
	body map[string]any
}

func serve(t *testing.T, handler func(path string, body map[string]any) (int, string)) (*MCPMemory, *[]capture) {
	t.Helper()
	seen := &[]capture{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*seen = append(*seen, capture{path: r.URL.Path, body: body})
		code, payload := handler(r.URL.Path, body)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(ts.Close)
	return NewMCPMemory(ts.URL, "k", 5*time.Second), seen
}

// The tag spellings are written out rather than built from the connector's own
// constants: they are a wire format shared with whatever else reads this
// backend, so a rename has to break a test.
const (
	projectA = "/srv/app"
	projectB = "/srv/other"
)

func localTags(project string) []string {
	return []string{"shoulder-scope:local", "shoulder-project:" + scope.Key(project)}
}

func hit(hash, content string, score float64, iso string, tags ...string) string {
	b, _ := json.Marshal(tags)
	return fmt.Sprintf(`{"memory":{"content":%q,"content_hash":%q,"tags":%s,"memory_type":"decision","metadata":{},"created_at_iso":%q},"similarity_score":%v}`,
		content, hash, b, iso, score)
}

func listed(hash, content, iso string, tags ...string) string {
	b, _ := json.Marshal(tags)
	return fmt.Sprintf(`{"content":%q,"content_hash":%q,"tags":%s,"memory_type":"decision","metadata":{},"created_at_iso":%q}`,
		content, hash, b, iso)
}

func results(entries ...string) string { return `{"results":[` + strings.Join(entries, ",") + `]}` }

func bodyTags(t *testing.T, body map[string]any, key string) []string {
	t.Helper()
	raw, ok := body[key].([]any)
	if !ok {
		t.Fatalf("no %q array in %+v", key, body)
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("non-string tag %v", v)
		}
		out = append(out, s)
	}
	return out
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

const searchTwo = `{"results":[
 {"memory":{"content":"releases ship on Fridays","content_hash":"old","tags":["t"],"memory_type":"decision","metadata":{"superseded_by":"new"},"created_at_iso":"2026-08-30T10:00:00Z"},"similarity_score":0.9},
 {"memory":{"content":"releases never ship on Fridays","content_hash":"new","tags":["t"],"memory_type":"decision","metadata":{},"created_at_iso":"2026-08-30T11:00:00Z"},"similarity_score":0.7}
]}`

// The server returns superseded facts on every surface and ranks them by
// similarity alone, so the higher-scoring stale fact would win without this
// filter. Handing a model a fact and its own retraction is worse than nothing.
func TestSearchDropsSupersededFacts(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, searchTwo })
	got, err := c.Search(context.Background(), Query{Text: "when do releases ship", Limit: 5, Scope: scope.Any})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the superseded fact to be dropped, got %+v", got)
	}
	if got[0].ID != "new" || got[0].Category != "decision" || got[0].Score != 0.7 {
		t.Fatalf("wrong record survived: %+v", got[0])
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("created_at_iso should have parsed")
	}
}

func TestSearchOverFetchesToSurviveFiltering(t *testing.T) {
	unscoped, seen := serve(t, func(string, map[string]any) (int, string) { return 200, searchTwo })
	if _, err := unscoped.Search(context.Background(), Query{Text: "q", Limit: 5, Scope: scope.Any}); err != nil {
		t.Fatal(err)
	}
	plain := (*seen)[0].body["n_results"].(float64)
	if plain <= 5 {
		t.Fatalf("should over-fetch so filtering does not shrink the result set, asked for %v", plain)
	}

	// A scoped search discards more rows than an unscoped one, so it has to ask
	// for more of them.
	scoped, seen := serve(t, func(string, map[string]any) (int, string) { return 200, searchTwo })
	if _, err := scoped.Search(context.Background(), Query{Text: "q", Limit: 5, Scope: scope.Global}); err != nil {
		t.Fatal(err)
	}
	if n := (*seen)[0].body["n_results"].(float64); n <= plain {
		t.Fatalf("a scope filter also discards rows; asked for %v, same as unfiltered %v", n, plain)
	}
}

func TestSearchClampsLimit(t *testing.T) {
	c, seen := serve(t, func(string, map[string]any) (int, string) { return 200, `{"results":[]}` })
	if _, err := c.Search(context.Background(), Query{Text: "q", Limit: 5000, Scope: scope.Any}); err != nil {
		t.Fatal(err)
	}
	if n := (*seen)[0].body["n_results"].(float64); n > 200 {
		t.Fatalf("limit must be clamped, asked for %v", n)
	}
}

func TestStoreWritesThePlacementTags(t *testing.T) {
	t.Run("local carries scope and project", func(t *testing.T) {
		c, seen := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":true,"content_hash":"h1"}`
		})
		_, err := c.Store(context.Background(), Record{
			Content: "the main branch is master", Tags: []string{"vcs"},
			Scope: scope.Local, Project: projectA,
		})
		if err != nil {
			t.Fatal(err)
		}
		tags := bodyTags(t, (*seen)[0].body, "tags")
		for _, want := range append([]string{"vcs"}, localTags(projectA)...) {
			if !hasTag(tags, want) {
				t.Errorf("missing tag %q in %v", want, tags)
			}
		}
	})

	t.Run("global carries no project", func(t *testing.T) {
		c, seen := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":true,"content_hash":"h1"}`
		})
		_, err := c.Store(context.Background(), Record{Content: "prefers terse answers", Scope: scope.Global})
		if err != nil {
			t.Fatal(err)
		}
		tags := bodyTags(t, (*seen)[0].body, "tags")
		if !hasTag(tags, "shoulder-scope:global") {
			t.Errorf("missing global scope tag in %v", tags)
		}
		for _, tag := range tags {
			if strings.HasPrefix(tag, "shoulder-project:") {
				t.Errorf("a global record must not be pinned to a project, got %q", tag)
			}
		}
	})
}

func TestStoreUsesRESTFieldNames(t *testing.T) {
	c, seen := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"success":true,"content_hash":"h9"}`
	})
	id, err := c.Store(context.Background(), Record{
		Content: "the best number is 1", Category: "preference", Tags: []string{"numbers"},
		Scope: scope.Global,
	})
	if err != nil || id != "h9" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	b := (*seen)[0].body
	if (*seen)[0].path != "/api/memories" {
		t.Fatalf("wrong path %q", (*seen)[0].path)
	}
	if b["memory_type"] != "preference" {
		t.Errorf("REST uses memory_type at the top level, got %+v", b)
	}
	if _, present := b["conversation_id"]; present {
		t.Error("conversation_id must be omitted; sending it disables server-side semantic deduplication")
	}
}

// A caller that set one tag must get one tag back. Leaking the placement tags
// into Tags would put them in front of the model and back into the next write.
func TestReadStripsThePlacementTagsBackOff(t *testing.T) {
	tags := append([]string{"vcs"}, localTags(projectA)...)
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, results(hit("h1", "the main branch is master", 0.8, "2026-08-30T10:00:00Z", tags...))
	})
	got, err := c.Search(context.Background(), Query{Text: "branch", Scope: scope.Local, Project: projectA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if len(got[0].Tags) != 1 || got[0].Tags[0] != "vcs" {
		t.Errorf("placement tags leaked into Tags: %v", got[0].Tags)
	}
	if got[0].Scope != scope.Local {
		t.Errorf("scope not recovered: %q", got[0].Scope)
	}
	// The tags hold the project's key, not its path, so the readable path can
	// only come from what the caller asked for.
	if got[0].Project != projectA {
		t.Errorf("Project = %q; a query that named a project should get it back", got[0].Project)
	}
	if got[0].ProjectKey() != scope.Key(projectA) {
		t.Errorf("ProjectKey = %q; it is the one value a caller can compare whichever form Project arrived in", got[0].ProjectKey())
	}
}

func TestReadFallsBackToTheProjectKeyWhenTheQueryNamedNone(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, results(hit("h1", "x", 0.8, "2026-08-30T10:00:00Z", localTags(projectA)...))
	})
	got, err := c.Search(context.Background(), Query{Text: "x", Scope: scope.Any})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Project != scope.Key(projectA) {
		t.Fatalf("Project = %+v; want the key from the tag", got)
	}
	// The same identifier as the path form yields: a key is not hashed twice.
	if got[0].ProjectKey() != scope.Key(projectA) {
		t.Errorf("ProjectKey = %q, want %q", got[0].ProjectKey(), scope.Key(projectA))
	}
}

func TestGlobalRecordsNeverComeBackWithAProject(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, results(hit("h1", "prefers terse answers", 0.8, "2026-08-30T10:00:00Z", "shoulder-scope:global"))
	})
	got, err := c.Search(context.Background(), Query{Text: "x", Scope: scope.Any, Project: projectA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].Project != "" {
		t.Errorf("Project = %q; a global record belongs to no project", got[0].Project)
	}
}

// The search endpoint ranks semantically and has no tag predicate, so nothing
// but this client-side filter stops one project's memory reaching another's.
func TestSearchFiltersOutTheWrongScope(t *testing.T) {
	payload := results(
		hit("a", "local to A", 0.9, "2026-08-30T10:00:00Z", localTags(projectA)...),
		hit("b", "local to B", 0.8, "2026-08-30T10:00:00Z", localTags(projectB)...),
		hit("g", "global", 0.7, "2026-08-30T10:00:00Z", "shoulder-scope:global"),
	)

	t.Run("local sees only its own project", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, payload })
		got, err := c.Search(context.Background(), Query{Text: "q", Scope: scope.Local, Project: projectA})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("got %+v; want only the record belonging to %s", got, projectA)
		}
	})

	t.Run("global sees only global", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, payload })
		got, err := c.Search(context.Background(), Query{Text: "q", Scope: scope.Global})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "g" {
			t.Fatalf("got %+v; want only the global record", got)
		}
	})

	t.Run("an unfiltered query sees everything", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, payload })
		got, err := c.Search(context.Background(), Query{Text: "q", Scope: scope.Any})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d records, want all 3", len(got))
		}
	})
}

// A digest reads everything a scope holds, which is exactly where mixing two
// projects would be hardest to notice. The refusal belongs to every backend, so
// it is asserted through the wrapper every caller goes through.
func TestListRefusesAnUnscopedRequest(t *testing.T) {
	c, seen := serve(t, func(string, map[string]any) (int, string) { return 200, `{"memories":[]}` })
	got, err := Checked(c).List(context.Background(), Query{Scope: scope.Any})
	if !errors.Is(err, ErrUnscopedList) {
		t.Fatalf("got %+v, %v; an unscoped list must not mean everything", got, err)
	}
	if len(*seen) != 0 {
		t.Fatalf("the request should never have been sent: %+v", *seen)
	}
}

func TestListQueriesTheTagEndpoint(t *testing.T) {
	c, seen := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"memories":[` + listed("h1", "x", "2026-08-30T10:00:00Z", localTags(projectA)...) + `]}`
	})
	got, err := c.List(context.Background(), Query{Scope: scope.Local, Project: projectA})
	if err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].path != "/api/search/by-tag" {
		t.Fatalf("a digest needs every record, not the nearest few; got path %q", (*seen)[0].path)
	}
	if (*seen)[0].body["match_all"] != true {
		t.Errorf("scope and project must both match, not either: %+v", (*seen)[0].body)
	}
	for _, want := range localTags(projectA) {
		if !hasTag(bodyTags(t, (*seen)[0].body, "tags"), want) {
			t.Errorf("missing tag %q in the request", want)
		}
	}
	if len(got) != 1 || got[0].Scope != scope.Local || len(got[0].Tags) != 0 {
		t.Errorf("list results must be stripped and scoped like search results: %+v", got)
	}
}

func TestListDropsTheWrongProjectAndSupersededRecords(t *testing.T) {
	stale := fmt.Sprintf(`{"content":"stale","content_hash":"s","tags":%s,"metadata":{"superseded_by":"h1"},"created_at_iso":"2026-08-30T10:00:00Z"}`,
		mustJSON(localTags(projectA)))
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"memories":[` + strings.Join([]string{
			listed("a", "mine", "2026-08-30T10:00:00Z", localTags(projectA)...),
			listed("b", "someone else's", "2026-08-30T10:00:00Z", localTags(projectB)...),
			stale,
		}, ",") + `]}`
	})
	got, err := c.List(context.Background(), Query{Scope: scope.Local, Project: projectA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("got %+v; want only the current record for %s", got, projectA)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestListReturnsNewestFirstAndHonoursTheLimit(t *testing.T) {
	tags := localTags(projectA)
	payload := `{"memories":[` + strings.Join([]string{
		listed("mid", "b", "2026-08-29T10:00:00Z", tags...),
		listed("old", "c", "2026-08-28T10:00:00Z", tags...),
		listed("new", "a", "2026-08-30T10:00:00Z", tags...),
	}, ",") + `]}`

	c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, payload })
	got, err := c.List(context.Background(), Query{Scope: scope.Local, Project: projectA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "new" || got[1].ID != "mid" || got[2].ID != "old" {
		t.Fatalf("want newest first, got %+v", got)
	}

	c, _ = serve(t, func(string, map[string]any) (int, string) { return 200, payload })
	got, err = c.List(context.Background(), Query{Scope: scope.Local, Project: projectA, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "new" {
		t.Fatalf("limit ignored: %+v", got)
	}
}

// The by-tag endpoint answers with the ranked envelope the search endpoint
// uses, wrapping each record under "memory", as well as with a flat "memories"
// array. Decoding the ranked one as if it were flat does not fail: it yields an
// element per record with every field empty, so a scope that holds records
// reads as a scope that holds none, and nothing this listing feeds - the
// digest, the scope check behind every supersede, the lookup for a session's
// working note - can tell that from an empty store.
func TestListAcceptsTheRankedResponseEnvelope(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"results":[{"memory":` + listed("h1", "x", "2026-08-30T10:00:00Z", "shoulder-scope:global") + `}]}`
	})
	got, err := c.List(context.Background(), Query{Scope: scope.Global})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "h1" || got[0].Content != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestSupersedeParsesTheProseReply(t *testing.T) {
	c, seen := serve(t, func(path string, _ map[string]any) (int, string) {
		return 200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Versioned update successful. New hash: abc123def4567890, parent hash: old. Memory versioned successfully"}]}}`
	})
	id, err := c.Supersede(context.Background(), "old", Record{
		Content: "new content", Scope: scope.Local, Project: projectA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "abc123def4567890" {
		t.Fatalf("got %q", id)
	}
	if (*seen)[0].path != "/mcp" {
		t.Fatalf("supersede must use the JSON-RPC endpoint, got %q", (*seen)[0].path)
	}
	args := (*seen)[0].body["params"].(map[string]any)["arguments"].(map[string]any)
	if args["versioned"] != true {
		t.Errorf("versioned must be set: %+v", args)
	}
	// A replacement that lost its placement tags would be invisible to every
	// scoped read, which is indistinguishable from the fact being deleted.
	updates := args["updates"].(map[string]any)
	for _, want := range localTags(projectA) {
		if !hasTag(bodyTags(t, updates, "tags"), want) {
			t.Errorf("supersede dropped placement tag %q", want)
		}
	}
}

func TestSupersedeWithoutAHashFallsBackToStore(t *testing.T) {
	c, seen := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"success":true,"content_hash":"h2"}`
	})
	if _, err := c.Supersede(context.Background(), "", Record{Content: "x", Scope: scope.Global}); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].path != "/api/memories" {
		t.Fatalf("expected a plain store, got %q", (*seen)[0].path)
	}
}

func TestSupersedeFailsLoudlyOnAnUnrecognisedReply(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"something changed, probably"}]}}`
	})
	if _, err := c.Supersede(context.Background(), "old", Record{Content: "x", Scope: scope.Global}); err == nil {
		t.Fatal("an unparseable supersede reply must not be reported as success")
	}
}

func TestErrorsSurface(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) { return 500, `boom` })
	if _, err := c.Search(context.Background(), Query{Text: "q", Limit: 5, Scope: scope.Any}); err == nil {
		t.Error("a 500 must not be swallowed")
	}
	if _, err := c.List(context.Background(), Query{Scope: scope.Global}); err == nil {
		t.Error("a failed list must not be swallowed")
	}
	if _, err := c.Store(context.Background(), Record{Content: "x", Scope: scope.Global}); err == nil {
		t.Error("a failed store must not be swallowed")
	}
	rpc, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
	})
	if _, err := rpc.Supersede(context.Background(), "old", Record{Content: "x", Scope: scope.Global}); err == nil {
		t.Error("a JSON-RPC error must not be swallowed")
	}
}

func TestStoreDistinguishesDuplicateKinds(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":false,"message":"Duplicate content detected (exact match)","content_hash":null}`
		})
		_, err := c.Store(context.Background(), Record{Content: "x", Scope: scope.Global})
		if !errors.Is(err, ErrDuplicateExact) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("semantic names the collision", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":false,"message":"Duplicate content detected (semantically similar to 47dc9903346401927dadb5fcb6156790fb644f4b15ad2bde369cbea15afd6815)","content_hash":null}`
		})
		_, err := c.Store(context.Background(), Record{Content: "x", Scope: scope.Global})
		var sem *ErrDuplicateSemantic
		if !errors.As(err, &sem) {
			t.Fatalf("got %v", err)
		}
		if sem.Collided != "47dc9903346401927dadb5fcb6156790fb644f4b15ad2bde369cbea15afd6815" {
			t.Fatalf("collided hash not extracted: %q", sem.Collided)
		}
	})

	t.Run("semantic without a hash", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":false,"message":"Duplicate content detected (semantically similar)","content_hash":null}`
		})
		_, err := c.Store(context.Background(), Record{Content: "x", Scope: scope.Global})
		var sem *ErrDuplicateSemantic
		if !errors.As(err, &sem) || sem.Collided != "" {
			t.Fatalf("got %v", err)
		}
	})
}

// The kind rides on the same tag channel as the placement, so a build of the
// server that only understands tags still tells the two apart.
func TestWritesCarryTheKindTagOnlyForSessionRecords(t *testing.T) {
	t.Run("a session record is tagged", func(t *testing.T) {
		c, seen := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":true,"content_hash":"h1"}`
		})
		_, err := c.Store(context.Background(), Record{
			Content: "keywords: deploy, rollback", Kind: KindSession,
			Scope: scope.Local, Project: projectA,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !hasTag(bodyTags(t, (*seen)[0].body, "tags"), "shoulder-kind:session") {
			t.Errorf("missing kind tag in %v", (*seen)[0].body["tags"])
		}
	})

	// A fact is the absence of the tag, which is what keeps every record written
	// before the distinction existed readable as the fact it is.
	t.Run("a fact is tagged with nothing", func(t *testing.T) {
		c, seen := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"success":true,"content_hash":"h1"}`
		})
		_, err := c.Store(context.Background(), Record{
			Content: "the main branch is master", Scope: scope.Local, Project: projectA,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range bodyTags(t, (*seen)[0].body, "tags") {
			if strings.HasPrefix(tag, "shoulder-kind:") {
				t.Errorf("a fact must carry no kind tag, got %q", tag)
			}
		}
	})

	// The running note is rewritten every turn, so a supersede that dropped the
	// kind tag would promote it to a fact and put keyword soup in the digest.
	t.Run("supersede keeps the kind tag", func(t *testing.T) {
		c, seen := serve(t, func(string, map[string]any) (int, string) {
			return 200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Versioned update successful. New hash: abc123def4567890, parent hash: old."}]}}`
		})
		_, err := c.Supersede(context.Background(), "old", Record{
			Content: "keywords: deploy, rollback, migration", Kind: KindSession,
			Scope: scope.Local, Project: projectA,
		})
		if err != nil {
			t.Fatal(err)
		}
		args := (*seen)[0].body["params"].(map[string]any)["arguments"].(map[string]any)
		updates := args["updates"].(map[string]any)
		if !hasTag(bodyTags(t, updates, "tags"), "shoulder-kind:session") {
			t.Errorf("supersede dropped the kind tag: %v", updates["tags"])
		}
	})
}

func sessionTags(project string) []string {
	return append(localTags(project), "shoulder-kind:session")
}

// Nothing but this filter keeps a session's keywords out of a recall, a digest
// or the fact list, none of which have heard of Kind.
func TestReadsMatchTheKindExactly(t *testing.T) {
	searchPayload := results(
		hit("f", "the main branch is master", 0.9, "2026-08-30T10:00:00Z", localTags(projectA)...),
		hit("s", "keywords: branch, master", 0.8, "2026-08-30T11:00:00Z", sessionTags(projectA)...),
	)
	listPayload := `{"memories":[` + strings.Join([]string{
		listed("f", "the main branch is master", "2026-08-30T10:00:00Z", localTags(projectA)...),
		listed("s", "keywords: branch, master", "2026-08-30T11:00:00Z", sessionTags(projectA)...),
	}, ",") + `]}`

	t.Run("a query that never mentioned Kind gets facts", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, searchPayload })
		got, err := c.Search(context.Background(), Query{Text: "branch", Scope: scope.Local, Project: projectA})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "f" {
			t.Fatalf("got %+v; want only the fact", got)
		}

		c, _ = serve(t, func(string, map[string]any) (int, string) { return 200, listPayload })
		got, err = c.List(context.Background(), Query{Scope: scope.Local, Project: projectA})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "f" {
			t.Fatalf("got %+v; want only the fact", got)
		}
	})

	t.Run("asking for session records excludes the facts", func(t *testing.T) {
		c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, searchPayload })
		got, err := c.Search(context.Background(), Query{Text: "branch", Scope: scope.Local, Project: projectA, Kind: KindSession})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "s" || got[0].Kind != KindSession {
			t.Fatalf("got %+v; want only the session record", got)
		}
		if len(got[0].Tags) != 0 {
			t.Errorf("the kind tag leaked into Tags: %v", got[0].Tags)
		}

		c, seen := serve(t, func(string, map[string]any) (int, string) { return 200, listPayload })
		got, err = c.List(context.Background(), Query{Scope: scope.Local, Project: projectA, Kind: KindSession})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ID != "s" || got[0].Kind != KindSession {
			t.Fatalf("got %+v; want only the session record", got)
		}
		// The one direction the server can filter itself: a required tag.
		if !hasTag(bodyTags(t, (*seen)[0].body, "tags"), "shoulder-kind:session") {
			t.Errorf("the listing should ask the server for the kind it wants: %+v", (*seen)[0].body)
		}
	})
}

// The agent searches a second time with a lower floor than the search it was
// handed, so the floor has to mean something on the way back.
func TestSearchHonoursMinScore(t *testing.T) {
	payload := results(
		hit("near", "close enough", 0.62, "2026-08-30T10:00:00Z", localTags(projectA)...),
		hit("far", "barely related", 0.11, "2026-08-30T10:00:00Z", localTags(projectA)...),
	)

	c, _ := serve(t, func(string, map[string]any) (int, string) { return 200, payload })
	got, err := c.Search(context.Background(), Query{Text: "q", Scope: scope.Local, Project: projectA, MinScore: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "near" {
		t.Fatalf("got %+v; want only the hit above the floor", got)
	}

	c, _ = serve(t, func(string, map[string]any) (int, string) { return 200, payload })
	got, err = c.Search(context.Background(), Query{Text: "q", Scope: scope.Local, Project: projectA})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v; no floor must drop nothing", got)
	}
}

// A server that scores nothing would otherwise answer every floored search with
// nothing at all.
func TestMinScoreSparesUnscoredRecords(t *testing.T) {
	c, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"results":[{"memory":` +
			listed("h1", "unscored", "2026-08-30T10:00:00Z", localTags(projectA)...) + `}]}`
	})
	got, err := c.Search(context.Background(), Query{Text: "q", Scope: scope.Local, Project: projectA, MinScore: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Score != 0 {
		t.Fatalf("got %+v; a record the backend never scored must survive the floor", got)
	}
}

// sink is a Counter that keeps its tallies so a test can read what the
// connector reported about the shape of what came back.
type sink struct {
	mu sync.Mutex
	n  map[string]int
}

func newSink() *sink { return &sink{n: map[string]int{}} }

func (s *sink) Inc(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n[name]++
}

func (s *sink) get(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n[name]
}

// sessionPage serves a ranking whose first rows are all working notes, which is
// what a project accumulates: one per session, forever, worded out of that
// project's own turns, so they rank alongside its facts.
func sessionPage(notes int, facts ...string) func(string, map[string]any) (int, string) {
	return func(_ string, body map[string]any) (int, string) {
		n := int(body["n_results"].(float64))
		var rows []string
		for i := 0; i < notes && len(rows) < n; i++ {
			rows = append(rows, hit(fmt.Sprintf("note%d", i), fmt.Sprintf("session keywords %d", i), 0.9,
				"2026-08-30T10:00:00Z", append(localTags(projectA), "shoulder-kind:session")...))
		}
		for i, f := range facts {
			if len(rows) >= n {
				break
			}
			rows = append(rows, hit(fmt.Sprintf("fact%d", i), f, 0.5, "2026-08-30T10:00:00Z", localTags(projectA)...))
		}
		return 200, results(rows...)
	}
}

// The starve this exists to prevent: a fixed over-fetch of limit*4 reads a page
// that is entirely working notes, discards all of it, and answers nothing,
// which the caller cannot tell from a store that holds nothing.
func TestSearchWidensUntilFactsSurfaceAndCountsWhatItDiscarded(t *testing.T) {
	c, seen := serve(t, sessionPage(40, "the release rota lives in docs/rota.md"))
	counts := newSink()
	c.Metrics = counts

	got, err := c.Search(context.Background(), Query{
		Text: "release rota", Limit: 8, Scope: scope.Local, Project: projectA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "fact0" {
		t.Fatalf("the fact behind the notes must still be found, got %+v", got)
	}
	if len(*seen) < 2 {
		t.Fatalf("one page of notes must not be taken as the whole store; asked %d times", len(*seen))
	}
	first := (*seen)[0].body["n_results"].(float64)
	if second := (*seen)[1].body["n_results"].(float64); second <= first {
		t.Errorf("each round must ask for more, got %v then %v", first, second)
	}
	if n := counts.get("shoulder_memory_discarded_session_total"); n != 40 {
		t.Errorf("the discarded working notes must be visible, counted %d of 40", n)
	}
	if n := counts.get("shoulder_memory_discarded_fact_total"); n != 0 {
		t.Errorf("nothing of the kind that was asked for was discarded, counted %d", n)
	}
}

// A store with more working notes than the ceiling will read is a store that
// needs tidying, and the answer being short has to say so rather than pass for
// an honest empty one.
func TestSearchStopsAtTheCeilingAndSaysSo(t *testing.T) {
	c, seen := serve(t, sessionPage(1<<20))
	counts := newSink()
	c.Metrics = counts

	got, err := c.Search(context.Background(), Query{
		Text: "release rota", Limit: 8, Scope: scope.Local, Project: projectA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("there are no facts to find, got %+v", got)
	}
	if counts.get("shoulder_memory_search_ceiling_total") != 1 {
		t.Error("giving up short of the limit must be counted, or the starve is silent")
	}
	for _, c := range *seen {
		if n := c.body["n_results"].(float64); n > searchFetchCeiling {
			t.Fatalf("the widening must be bounded, asked for %v", n)
		}
	}
	if len(*seen) > 8 {
		t.Errorf("the widening must converge quickly, took %d rounds", len(*seen))
	}
}

func TestForgetDeletesByHashAndTreatsAMissingRecordAsDone(t *testing.T) {
	c, seen := serve(t, func(path string, _ map[string]any) (int, string) {
		return 200, `{"success":true}`
	})
	if err := c.Forget(context.Background(), "abc123def4567890", Query{Scope: scope.Local, Project: "/srv/app", Kind: KindSession}); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[0].path; got != "/api/memories/abc123def4567890" {
		t.Errorf("forget must name the record it deletes, asked %q", got)
	}

	// A janitor re-running after a crash asks again for what it already
	// removed, and that is the state it wanted.
	gone, _ := serve(t, func(string, map[string]any) (int, string) {
		return 404, `{"detail":"not found"}`
	})
	if err := gone.Forget(context.Background(), "abc123def4567890", Query{Scope: scope.Local, Project: "/srv/app", Kind: KindSession}); err != nil {
		t.Errorf("a record that is already absent is not a failure, got %v", err)
	}

	broken, _ := serve(t, func(string, map[string]any) (int, string) {
		return 500, `{"detail":"boom"}`
	})
	if err := broken.Forget(context.Background(), "abc123def4567890", Query{Scope: scope.Local, Project: "/srv/app", Kind: KindSession}); err == nil {
		t.Error("a backend that could not delete must say so")
	}
	if err := c.Forget(context.Background(), "", Query{Scope: scope.Local, Project: "/srv/app", Kind: KindSession}); !errors.Is(err, ErrForgetUnidentified) {
		t.Errorf("an empty id must be refused, got %v", err)
	}
}

func TestWritesCarryTheSessionMarkAndReadsStripItBackOff(t *testing.T) {
	c, seen := serve(t, func(string, map[string]any) (int, string) {
		return 200, `{"success":true,"content_hash":"h1"}`
	})
	if _, err := c.Store(context.Background(), Record{
		Content: "session keywords: parser", Kind: KindSession, Session: "s1",
		Scope: scope.Local, Project: projectA,
	}); err != nil {
		t.Fatal(err)
	}
	tags := bodyTags(t, (*seen)[0].body, "tags")
	if !hasTag(tags, "shoulder-session:s1") {
		t.Fatalf("the note must name the session that keeps it, got %v", tags)
	}

	read, _ := serve(t, func(string, map[string]any) (int, string) {
		return 200, results(hit("h1", "session keywords: parser", 0.9, "2026-08-30T10:00:00Z",
			append(localTags(projectA), "shoulder-kind:session", "shoulder-session:s1", "mine")...))
	})
	got, err := read.Search(context.Background(), Query{
		Text: "parser", Limit: 5, Scope: scope.Local, Project: projectA, Kind: KindSession,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the note back, got %+v", got)
	}
	if got[0].Session != "s1" {
		t.Errorf("the session mark must survive the read, got %q", got[0].Session)
	}
	for _, tag := range got[0].Tags {
		if strings.HasPrefix(tag, "shoulder-") {
			t.Errorf("the mark leaked into the user's tags: %v", got[0].Tags)
		}
	}
}
