package balancer

import (
	"net/http"
	"testing"
)

func TestStats(t *testing.T) {
	healthy, dead := namedBackend(t, "a"), deadBackend(t)
	lb := newBalancer(t, Config{Backends: []string{healthy, dead}})

	// Round robin starts at the healthy backend, then the dead one fails once
	// and is retried on the healthy one. That's 2 attempts on a, 1 on the dead.
	for range 2 {
		send(lb, http.MethodGet)
	}

	// The dead backend's Failures stays 0: a connection error marks a backend
	// down straight away without counting toward MaxFailures.
	got := lb.Stats().Backends
	want := []BackendStats{
		{URL: healthy, Alive: true, Active: 0, Failures: 0, Requests: 2},
		{URL: dead, Alive: false, Active: 0, Failures: 0, Requests: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d backends, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backend %d:\n got  %+v\n want %+v", i, got[i], want[i])
		}
	}
}
