package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gitlab.com/quittymr/shoulder-daemon/relay/internal/scope"
)

// Probes are written under a synthetic project so a live run cannot contaminate
// the user's real global preferences or any real checkout's memory.
const probeProject = "/shoulder-daemon/live-probe"

func probeQuery(text string, limit int) Query {
	return Query{Text: text, Limit: limit, Scope: scope.Local, Project: probeProject}
}

// TestLiveMCPMemory exercises the connector against a real mcp-memory-service.
// Set SHOULDER_MEMORY_URL and SHOULDER_MEMORY_KEY to run it.
func TestLiveMCPMemory(t *testing.T) {
	url := os.Getenv("SHOULDER_MEMORY_URL")
	if url == "" {
		t.Skip("set SHOULDER_MEMORY_URL")
	}
	c := NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stamp := time.Now().UnixNano()
	original := fmt.Sprintf("Probe %d: the widget calibration constant is %d millivolts.", stamp, stamp%9000+1000)
	id, err := c.Store(ctx, Record{
		Content:  original,
		Category: "constraint",
		Tags:     []string{"shoulder-daemon-probe"},
		Scope:    scope.Local,
		Project:  probeProject,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if id == "" {
		t.Fatal("store returned no content hash")
	}
	t.Logf("stored %s", id)

	got, err := c.Search(ctx, probeQuery(fmt.Sprintf("widget calibration constant probe %d", stamp), 5))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var found *Record
	for i := range got {
		if got[i].ID == id {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("stored fact not recalled; got %d results: %+v", len(got), got)
	}
	if found.Category != "constraint" {
		t.Errorf("category lost in round trip: %q", found.Category)
	}
	if found.Scope != scope.Local || found.Project != probeProject {
		t.Errorf("placement lost in round trip: scope=%q project=%q", found.Scope, found.Project)
	}
	for _, tag := range found.Tags {
		if strings.HasPrefix(tag, "shoulder-") {
			t.Errorf("placement tag %q leaked into Tags", tag)
		}
	}
	if found.Score <= 0 {
		t.Errorf("expected a similarity score, got %v", found.Score)
	}
	t.Logf("recalled score=%.3f category=%q tags=%v", found.Score, found.Category, found.Tags)

	newID, err := c.Supersede(ctx, id, Record{
		Content:  fmt.Sprintf("Probe %d: the widget calibration constant is %d microvolts, not millivolts.", stamp, stamp%9000+1000),
		Category: "constraint",
		Tags:     []string{"shoulder-daemon-probe"},
		Scope:    scope.Local,
		Project:  probeProject,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	t.Logf("superseded %s -> %s", id, newID)

	after, err := c.Search(ctx, probeQuery(fmt.Sprintf("widget calibration constant probe %d", stamp), 10))
	if err != nil {
		t.Fatalf("search after supersede: %v", err)
	}
	for _, r := range after {
		if r.ID == id {
			t.Errorf("superseded fact %s is still being returned by search", id)
		}
	}
	var sawReplacement bool
	for _, r := range after {
		if r.ID == newID {
			sawReplacement = true
		}
	}
	if !sawReplacement {
		t.Errorf("the replacement fact %s should still be recallable", newID)
	}
}

// TestLiveRefusedCorrectionRecovers reproduces the sequence that silently lost
// data: store a fact, then store its correction. The backend refuses the
// correction as a near-duplicate, so the caller must supersede the memory that
// blocked it or the stale fact is recalled forever.
func TestLiveRefusedCorrectionRecovers(t *testing.T) {
	url := os.Getenv("SHOULDER_MEMORY_URL")
	if url == "" {
		t.Skip("set SHOULDER_MEMORY_URL")
	}
	c := NewMCPMemory(url, os.Getenv("SHOULDER_MEMORY_KEY"), 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	n := time.Now().UnixNano()
	subject := fmt.Sprintf("The deploy target for service %d", n)
	query := fmt.Sprintf("deploy target for service %d", n)

	first, err := c.Store(ctx, Record{
		Content: subject + " is staging.", Category: "decision", Tags: []string{"gc-live"},
		Scope: scope.Local, Project: probeProject,
	})
	if err != nil {
		t.Fatalf("store original: %v", err)
	}

	correction := Record{
		Content: subject + " is production, not staging.", Category: "decision", Tags: []string{"gc-live"},
		Scope: scope.Local, Project: probeProject,
	}
	_, err = c.Store(ctx, correction)

	var sem *ErrDuplicateSemantic
	if !errors.As(err, &sem) {
		t.Fatalf("expected the backend to refuse the correction as a near-duplicate, got err=%v", err)
	}
	if sem.Collided != first {
		t.Fatalf("refusal named %q, expected the original %q", sem.Collided, first)
	}
	t.Logf("backend refused the correction, naming %s", sem.Collided[:12])

	newID, err := c.Supersede(ctx, sem.Collided, correction)
	if err != nil {
		t.Fatalf("recovery supersede: %v", err)
	}
	t.Logf("recovered by superseding %s -> %s", sem.Collided[:12], newID[:12])

	got, err := c.Search(ctx, probeQuery(query, 10))
	if err != nil {
		t.Fatal(err)
	}
	var sawStale, sawCorrection bool
	for _, r := range got {
		if r.ID == first {
			sawStale = true
		}
		if r.ID == newID {
			sawCorrection = true
		}
	}
	if sawStale {
		t.Error("the superseded fact is still being recalled")
	}
	if !sawCorrection {
		t.Errorf("the correction should be recalled; got %d results", len(got))
	}
}
