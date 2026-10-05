package balancer

import (
	"slices"
	"sync/atomic"
	"time"
)

// LatencyBuckets are the upper bounds of the latency histogram's buckets:
// from 1ms, under which a backend on the same network answers, to 10s, the
// default request timeout.
var LatencyBuckets = [...]time.Duration{
	1 * time.Millisecond,
	2500 * time.Microsecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2500 * time.Millisecond,
	5 * time.Second,
	10 * time.Second,
}

// Latency is a snapshot of a backend's latency histogram, in the shape
// Prometheus expects.
type Latency struct {
	// Counts[i] is how many responses took at most LatencyBuckets[i], so
	// each count includes the ones before it.
	Counts [len(LatencyBuckets)]uint64
	Count  uint64        // all responses, however long they took
	Sum    time.Duration // their total time
}

// histogram counts durations into LatencyBuckets. Recording one is two
// atomic adds, so it's safe for concurrent use and never blocks a request.
type histogram struct {
	// One per bucket, not cumulative, so recording touches only one. The
	// extra last one counts durations over every bound.
	counts [len(LatencyBuckets) + 1]atomic.Uint64
	sum    atomic.Int64 // nanoseconds
}

func (h *histogram) observe(d time.Duration) {
	// The first bound at least d: a bucket counts durations up to and
	// including its bound. Past the last bound, i is the extra slot.
	i, _ := slices.BinarySearch(LatencyBuckets[:], d)
	h.counts[i].Add(1)
	h.sum.Add(int64(d))
}

// snapshot returns the histogram with cumulative counts. Like Stats, its
// values aren't read at one instant, but the counts always agree with each
// other and with Count.
func (h *histogram) snapshot() Latency {
	var l Latency
	var total uint64
	for i := range h.counts {
		total += h.counts[i].Load()
		if i < len(l.Counts) {
			l.Counts[i] = total
		}
	}
	l.Count = total
	l.Sum = time.Duration(h.sum.Load())
	return l
}
