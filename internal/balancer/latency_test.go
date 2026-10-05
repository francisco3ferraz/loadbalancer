package balancer

import (
	"testing"
	"time"
)

func TestHistogram(t *testing.T) {
	var h histogram
	for _, d := range []time.Duration{
		0,
		time.Millisecond, // on a bound: counted in that bucket
		time.Millisecond + 1,
		7 * time.Millisecond,
		time.Minute, // over every bound
	} {
		h.observe(d)
	}

	got := h.snapshot()
	want := Latency{Counts: [len(LatencyBuckets)]uint64{2, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4}, Count: 5,
		Sum: 2*time.Millisecond + 1 + 7*time.Millisecond + time.Minute}
	if got != want {
		t.Errorf("snapshot:\n got  %+v\n want %+v", got, want)
	}
}

func TestLatencyBucketsAreSorted(t *testing.T) {
	for i := 1; i < len(LatencyBuckets); i++ {
		if LatencyBuckets[i] <= LatencyBuckets[i-1] {
			t.Errorf("bucket %d (%s) isn't above bucket %d (%s)", i, LatencyBuckets[i], i-1, LatencyBuckets[i-1])
		}
	}
}
