package session

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func observe(r *Registry, sessionID string) {
	r.Observe(Event{SessionID: sessionID, Kind: KindTurnEnd, TS: time.Now()})
}

// The per-turn cap bounds one turn and nothing bounded the sum. A long session
// is hundreds of turns, and the note is written to the store and read back into
// every prompt after it, so an unbounded sum is a record and a prompt that grow
// all day.
func TestTheRunningNoteStopsGrowing(t *testing.T) {
	r := NewRegistry(10)
	observe(r, "s1")

	total := maxSessionKeywords + 50
	for i := 0; i < total; i++ {
		r.AddKeywords("s1", []string{fmt.Sprintf("k%d", i)})
	}

	got := r.Keywords("s1")
	if len(got) != maxSessionKeywords {
		t.Fatalf("the note kept %d keywords; it is joined into a record and a prompt on every turn", len(got))
	}
	// The note exists so a bare "do it" can be read against what just happened,
	// which is the recent end of the list rather than the start of the session.
	if want := fmt.Sprintf("k%d", total-1); got[len(got)-1] != want {
		t.Errorf("the newest keyword must survive, got %q want %q", got[len(got)-1], want)
	}
	if first := fmt.Sprintf("k%d", total-maxSessionKeywords); got[0] != first {
		t.Errorf("the window must slide, got %q want %q", got[0], first)
	}
}

func TestRepeatsDoNotAccumulate(t *testing.T) {
	r := NewRegistry(10)
	observe(r, "s1")
	for i := 0; i < 20; i++ {
		r.AddKeywords("s1", []string{"parser", "PARSER", "loader"})
	}
	if got := r.Keywords("s1"); len(got) != 2 {
		t.Fatalf("a session working on one file for an hour must not name it every turn: %v", got)
	}
}

// State is serialised whole into the diagnostic session listing. What belongs
// there is who the session is and whether it is alive; the content the daemon
// is reasoning over is the user's work rather than the daemon's health, and
// nothing should join that listing by being added to this struct.
func TestTheSessionListingPublishesIdentityAndNotContent(t *testing.T) {
	r := NewRegistry(10)
	r.Observe(Event{SessionID: "s1", Kind: KindTurnEnd, TS: time.Now(), CWD: "/srv/app", Harness: "claude-code"})
	r.AddKeywords("s1", []string{"parser", "loader"})
	r.SetKeywordRecord("s1", "/repo", "/repo", "mem_1", "parser, loader")

	listed := r.Sessions()
	if len(listed) != 1 {
		t.Fatalf("expected one session, got %d", len(listed))
	}
	raw, err := json.Marshal(listed[0])
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, leaked := range []string{"parser", "loader", "mem_1"} {
		if strings.Contains(out, leaked) {
			t.Errorf("the session's working note reached the listing: %s", out)
		}
	}
	for _, want := range []string{`"id":"s1"`, `"cwd":"/srv/app"`, `"turn":1`} {
		if !strings.Contains(out, want) {
			t.Errorf("the listing must still say who the session is; %s missing from %s", want, out)
		}
	}
}

// The project a note was filed under and the directory it was resolved from are
// one pair. CWD follows every event, so a session that moves between checkouts
// would otherwise be evicted with the project of the one its note is in and the
// path of the one it left, and a backend that keeps local facts with the code
// takes that pair as the truth about where the project is.
func TestAnEvictionPairsTheProjectWithTheDirectoryItWasResolvedFrom(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		out  func(*Registry) Evicted
	}{
		{"close", func(r *Registry) Evicted { gone, _ := r.CloseSession("s1"); return gone }},
		{"drain", func(r *Registry) Evicted { return r.Drain()[0] }},
		{"evict", func(r *Registry) Evicted { return r.Evict(time.Minute, now.Add(time.Hour))[0] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(10)
			r.Observe(Event{SessionID: "s1", Kind: KindUserPrompt, TS: now, CWD: "/repo/a"})
			r.SetKeywordRecord("s1", "a@deadbeef", "/repo/a", "mem_1", "kw")
			r.Observe(Event{SessionID: "s1", Kind: KindUserPrompt, TS: now, CWD: "/repo/b"})

			gone := tc.out(r)
			if gone.Project != "a@deadbeef" || gone.Dir != "/repo/a" {
				t.Fatalf("the session left project %q with directory %q; the two name different checkouts", gone.Project, gone.Dir)
			}
		})
	}
}
