// Package obs provides a minimal, dependency-free metrics registry for the
// Prometheus text exposition format (Phase 3.3 observability). Counters are
// process-local; gauges for task-run occupancy are computed at scrape time
// by the /metrics handler from the adapter TaskStats snapshot.
package obs

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
)

type counter struct {
	v atomic.Int64
}

var (
	mu       sync.Mutex
	counters = map[string]*counter{}
)

// CounterInc increments a named monotonic counter by one, registering it on
// first use. Names should follow the clawsynapse_<subject>_<verb>_total
// convention.
func CounterInc(name string) { CounterAdd(name, 1) }

// CounterAdd adds n to a named monotonic counter.
func CounterAdd(name string, n int64) {
	mu.Lock()
	c, ok := counters[name]
	if !ok {
		c = &counter{}
		counters[name] = c
	}
	mu.Unlock()
	c.v.Add(n)
}

// CounterSnapshot returns a copy of all registered counter values (tests,
// /v1/health/detailed).
func CounterSnapshot() map[string]int64 {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]int64, len(counters))
	for name, c := range counters {
		out[name] = c.v.Load()
	}
	return out
}

// WritePrometheus writes all registered counters in the Prometheus text
// exposition format, sorted by name for stable scrape output.
func WritePrometheus(w io.Writer) {
	mu.Lock()
	names := make([]string, 0, len(counters))
	vals := make(map[string]int64, len(counters))
	for name, c := range counters {
		names = append(names, name)
		vals[name] = c.v.Load()
	}
	mu.Unlock()

	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintln(w, "# HELP", name, "clawsynapse counter")
		fmt.Fprintln(w, "# TYPE", name, "counter")
		fmt.Fprintln(w, name, vals[name])
	}
}
