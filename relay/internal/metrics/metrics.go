// Package metrics is a dependency-free Prometheus text exposition, so the relay
// ships with no third-party modules and builds offline. It sits outside httpapi
// because both the hook path and the background pipeline count into it, and the
// hook package must not become a dependency of the pipeline.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var latencyBuckets = []float64{0.0005, 0.001, 0.002, 0.005, 0.010, 0.015, 0.025, 0.050, 0.100, 0.250, 1.0}

type histogram struct {
	counts []uint64
	sum    float64
	total  uint64
}

func (h *histogram) observe(d time.Duration) {
	if h.counts == nil {
		h.counts = make([]uint64, len(latencyBuckets))
	}
	s := d.Seconds()
	h.sum += s
	h.total++
	for i, b := range latencyBuckets {
		if s <= b {
			h.counts[i]++
		}
	}
}

// Metrics holds every counter and histogram the relay exposes.
type Metrics struct {
	mu        sync.Mutex
	hookLat   map[string]*histogram
	counters  map[string]uint64
	queueFn   func() int
	outboxFn  func() int
	sessionFn func() int
}

func New() *Metrics {
	return &Metrics{hookLat: map[string]*histogram{}, counters: map[string]uint64{}}
}

// SetGauges wires the point-in-time readings that are sampled at scrape time
// rather than counted as they happen.
func (m *Metrics) SetGauges(queue, outbox, sessions func() int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueFn, m.outboxFn, m.sessionFn = queue, outbox, sessions
}

// ObserveHook records how long one hook request spent inside its handler.
func (m *Metrics) ObserveHook(event string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hookLat[event]
	if !ok {
		h = &histogram{}
		m.hookLat[event] = h
	}
	h.observe(d)
}

// IncBy adds n at once, for a caller counting a batch it already has in hand.
func (m *Metrics) IncBy(name string, n uint64) {
	if n == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[name] += n
}

func (m *Metrics) Inc(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[name]++
}

func (m *Metrics) Get(name string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counters[name]
}

func (m *Metrics) Render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder
	b.WriteString("# HELP shoulder_hook_latency_seconds Time spent inside a hook request handler.\n")
	b.WriteString("# TYPE shoulder_hook_latency_seconds histogram\n")
	events := make([]string, 0, len(m.hookLat))
	for e := range m.hookLat {
		events = append(events, e)
	}
	sort.Strings(events)
	for _, e := range events {
		h := m.hookLat[e]
		for i, bound := range latencyBuckets {
			fmt.Fprintf(&b, "shoulder_hook_latency_seconds_bucket{event=%q,le=\"%g\"} %d\n", e, bound, h.counts[i])
		}
		fmt.Fprintf(&b, "shoulder_hook_latency_seconds_bucket{event=%q,le=\"+Inf\"} %d\n", e, h.total)
		fmt.Fprintf(&b, "shoulder_hook_latency_seconds_sum{event=%q} %g\n", e, h.sum)
		fmt.Fprintf(&b, "shoulder_hook_latency_seconds_count{event=%q} %d\n", e, h.total)
	}

	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "# TYPE %s counter\n%s %d\n", n, n, m.counters[n])
	}

	if m.queueFn != nil {
		fmt.Fprintf(&b, "# TYPE shoulder_queue_depth gauge\nshoulder_queue_depth %d\n", m.queueFn())
	}
	if m.outboxFn != nil {
		fmt.Fprintf(&b, "# TYPE shoulder_outbox_depth gauge\nshoulder_outbox_depth %d\n", m.outboxFn())
	}
	if m.sessionFn != nil {
		fmt.Fprintf(&b, "# TYPE shoulder_sessions gauge\nshoulder_sessions %d\n", m.sessionFn())
	}
	return b.String()
}

// ObserveAdvisor records how long an advisor call took. It is deliberately a
// separate series from the hook latency: the whole design rests on these two
// being unrelated.
func (m *Metrics) ObserveAdvisor(d time.Duration) { m.ObserveHook("advisor", d) }
