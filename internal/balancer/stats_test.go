package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	healthy, dead := namedBackend(t, "a"), deadBackend(t)
	lb := newBalancer(t, Config{Backends: urls(healthy, dead)})

	// Round robin starts at the healthy backend, then the dead one fails once
	// and is retried on the healthy one. That's 2 attempts on a, 1 on the dead.
	for range 2 {
		send(lb, http.MethodGet)
	}

	// The dead backend's Failures stays 0: a connection error marks a backend
	// down straight away without counting toward MaxFailures. It's still a
	// failure, so TotalFailures counts it.
	got := lb.Stats().Backends
	want := []BackendStats{
		{URL: healthy, Alive: true, Active: 0, Failures: 0, Requests: 2},
		{URL: dead, Alive: false, Active: 0, Failures: 0, Requests: 1, TotalFailures: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d backends, want %d", len(got), len(want))
	}
	// Timings vary, so latency is checked on its own: only the healthy
	// backend answered, twice.
	for i, wantCount := range []uint64{2, 0} {
		if n := got[i].Latency.Count; n != wantCount {
			t.Errorf("backend %d: latency count = %d, want %d", i, n, wantCount)
		}
		got[i].Latency = Latency{}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backend %d:\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}
}

func TestLatency(t *testing.T) {
	const delay = 30 * time.Millisecond
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusServiceUnavailable) // an answer, so it's timed
	}))
	defer slow.Close()
	lb := newBalancer(t, Config{Backends: urls(slow.URL)})

	for range 3 {
		send(lb, http.MethodGet)
	}

	l := lb.Stats().Backends[0].Latency
	if l.Count != 3 {
		t.Errorf("count = %d, want 3", l.Count)
	}
	if l.Sum < 3*delay {
		t.Errorf("sum = %s, want at least %s", l.Sum, 3*delay)
	}
	// No upper bound checked: a loaded CI machine can be slower than any.
	for i, bound := range LatencyBuckets {
		if bound < delay && l.Counts[i] != 0 {
			t.Errorf("%d responses counted at most %s, faster than the backend's %s", l.Counts[i], bound, delay)
		}
	}
}
