package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newBackends returns n backends with the given numbers of requests in
// progress. pick only reads active, so they need no URL or proxy.
func newBackends(active ...int64) []*backend {
	bs := make([]*backend, len(active))
	for i, n := range active {
		bs[i] = &backend{}
		bs[i].active.Store(n)
	}
	return bs
}

func TestRoundRobinPick(t *testing.T) {
	bs := newBackends(0, 0, 0)
	rr := &roundRobin{}
	for i := range 6 {
		if got := rr.pick(bs, nil); got != bs[i%3] {
			t.Errorf("pick %d: got backend %d, want %d", i, indexOf(bs, got), i%3)
		}
	}
}

func TestLeastConnectionsPicksLeastBusy(t *testing.T) {
	bs := newBackends(2, 1, 3)
	lc := &leastConnections{}
	// Several picks, so every rotating start position is covered.
	for i := range 3 {
		if got := lc.pick(bs, nil); got != bs[1] {
			t.Errorf("pick %d: got backend %d, want 1 (the least busy)", i, indexOf(bs, got))
		}
	}
}

func TestLeastConnectionsSpreadsTies(t *testing.T) {
	bs := newBackends(0, 0, 0)
	lc := &leastConnections{}
	counts := make([]int, len(bs))
	for range 6 {
		counts[indexOf(bs, lc.pick(bs, nil))]++
	}
	for _, n := range counts {
		if n != 2 {
			t.Fatalf("equally idle backends got %v picks, want 2 each", counts)
		}
	}
}

// TestLeastConnectionsAvoidsBusyBackend checks the picker end to end: while
// one backend is busy with a slow request, new requests go to the other.
func TestLeastConnectionsAvoidsBusyBackend(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       []string{hangingBackend(t), namedBackend(t, "b")},
		Algorithm:      LeastConnections,
		AttemptTimeout: 5 * time.Second,
	})
	busy := lb.backends[0]

	// Both backends are idle, and the tie-break starts at index 0, so this
	// request goes to the hanging backend and stays there.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		lb.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))
	}()
	waitFor(t, "the slow request to reach the hanging backend", func() bool { return busy.active.Load() == 1 })

	for i := range 4 {
		if code, body := send(lb, http.MethodGet); code != http.StatusOK || body != "b" {
			t.Errorf("request %d: got %d %q, want 200 from the idle backend b", i, code, body)
		}
	}

	cancel()
	<-done
}

func indexOf(bs []*backend, b *backend) int {
	for i, c := range bs {
		if c == b {
			return i
		}
	}
	return -1
}
