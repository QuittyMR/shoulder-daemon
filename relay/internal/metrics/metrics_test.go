package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestCountersAddUpAndZeroIsNotWritten(t *testing.T) {
	m := New()
	m.Inc("a_total")
	m.Inc("a_total")
	m.IncBy("b_total", 5)
	m.IncBy("never_total", 0)

	if got := m.Get("a_total"); got != 2 {
		t.Fatalf("a_total = %d, want 2", got)
	}
	if got := m.Get("b_total"); got != 5 {
		t.Fatalf("b_total = %d, want 5", got)
	}
	if got := m.Get("missing_total"); got != 0 {
		t.Fatalf("an unknown counter reads %d, want 0", got)
	}
	out := m.Render()
	if strings.Contains(out, "never_total") {
		t.Fatal("IncBy(0) created a series; a batch of nothing must leave no trace")
	}
	if !strings.Contains(out, "# TYPE a_total counter\na_total 2\n") {
		t.Fatalf("counter is not rendered as Prometheus text:\n%s", out)
	}
}

// Prometheus buckets are cumulative: an observation counts in every bucket
// whose bound it is under, and +Inf equals the total. A scraper that gets
// non-cumulative buckets computes nonsense quantiles without complaining.
func TestHookLatencyBucketsAreCumulative(t *testing.T) {
	m := New()
	m.ObserveHook("Stop", 700*time.Microsecond)
	m.ObserveHook("Stop", 3*time.Millisecond)
	m.ObserveHook("Stop", 2*time.Second)

	out := m.Render()
	for _, want := range []string{
		`shoulder_hook_latency_seconds_bucket{event="Stop",le="0.0005"} 0`,
		`shoulder_hook_latency_seconds_bucket{event="Stop",le="0.001"} 1`,
		`shoulder_hook_latency_seconds_bucket{event="Stop",le="0.005"} 2`,
		`shoulder_hook_latency_seconds_bucket{event="Stop",le="1"} 2`,
		`shoulder_hook_latency_seconds_bucket{event="Stop",le="+Inf"} 3`,
		`shoulder_hook_latency_seconds_count{event="Stop"} 3`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderIsOrderedSoScrapesAreStable(t *testing.T) {
	m := New()
	m.Inc("z_total")
	m.Inc("a_total")
	m.ObserveHook("UserPromptSubmit", time.Millisecond)
	m.ObserveHook("PreToolUse", time.Millisecond)

	out := m.Render()
	if strings.Index(out, `event="PreToolUse"`) > strings.Index(out, `event="UserPromptSubmit"`) {
		t.Fatal("histogram series are not sorted by event")
	}
	if strings.Index(out, "a_total 1") > strings.Index(out, "z_total 1") {
		t.Fatal("counters are not sorted by name")
	}
	if out != m.Render() {
		t.Fatal("two scrapes of the same state differ")
	}
}

func TestGaugesAreSampledAtScrapeTimeAndOnlyWhenWired(t *testing.T) {
	m := New()
	if out := m.Render(); strings.Contains(out, "gauge") {
		t.Fatalf("gauges rendered before anything was wired:\n%s", out)
	}
	depth := 3
	m.SetGauges(func() int { return depth }, func() int { return 1 }, func() int { return 7 })
	if out := m.Render(); !strings.Contains(out, "shoulder_queue_depth 3\n") || !strings.Contains(out, "shoulder_sessions 7\n") {
		t.Fatalf("gauges not rendered:\n%s", out)
	}
	depth = 9
	if out := m.Render(); !strings.Contains(out, "shoulder_queue_depth 9\n") {
		t.Fatal("a gauge is a snapshot at scrape time, not a value captured when wired")
	}
}

// The design rests on advisor time and hook time being separate series; one
// slow advisor call must never move the hook latency histogram.
func TestAdvisorLatencyIsItsOwnSeries(t *testing.T) {
	m := New()
	m.ObserveHook("Stop", time.Microsecond)
	m.ObserveAdvisor(5 * time.Second)

	out := m.Render()
	if !strings.Contains(out, `shoulder_hook_latency_seconds_bucket{event="advisor",le="+Inf"} 1`) {
		t.Fatalf("advisor observation missing:\n%s", out)
	}
	if !strings.Contains(out, `shoulder_hook_latency_seconds_bucket{event="Stop",le="0.0005"} 1`) {
		t.Fatalf("the advisor call leaked into the hook series:\n%s", out)
	}
}
