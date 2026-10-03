package balancer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

// pick only reads active, so these backends need no URL or proxy.
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

func TestLeastConnectionsAvoidsBusyBackend(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       urls(hangingBackend(t), namedBackend(t, "b")),
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

func weighted(weights ...int) []*backend {
	bs := make([]*backend, len(weights))
	for i, w := range weights {
		bs[i] = &backend{weight: w}
	}
	return bs
}

func pickIndexes(p picker, bs []*backend, n int) []int {
	got := make([]int, n)
	for i := range n {
		got[i] = indexOf(bs, p.pick(bs, nil))
	}
	return got
}

func TestWeightedRoundRobinIsSmooth(t *testing.T) {
	bs := weighted(5, 1, 1)
	// A A B A C A A, twice: the worked example from nginx's algorithm. A naive
	// version would give A A A A A B C, sending A its requests in a burst.
	want := []int{0, 0, 1, 0, 2, 0, 0, 0, 0, 1, 0, 2, 0, 0}
	if got := pickIndexes(&weightedRoundRobin{}, bs, len(want)); !slices.Equal(got, want) {
		t.Errorf("picks = %v, want %v", got, want)
	}
}

func TestWeightedRoundRobinProportions(t *testing.T) {
	bs := weighted(3, 1)
	counts := make([]int, len(bs))
	for _, i := range pickIndexes(&weightedRoundRobin{}, bs, 400) {
		counts[i]++
	}
	if counts[0] != 300 || counts[1] != 100 {
		t.Errorf("weights 3:1 over 400 picks gave %v, want [300 100]", counts)
	}
}

// TestWeightedRoundRobinSubset checks that a backend missing from the
// candidates (down, or already tried) simply sits out, and the rest share
// the traffic by their own weights.
func TestWeightedRoundRobinSubset(t *testing.T) {
	bs := weighted(5, 1, 1)
	wrr := &weightedRoundRobin{}
	pickIndexes(wrr, bs, 3) // build up some state with all three

	candidates := []*backend{bs[0], bs[2]}
	counts := map[*backend]int{}
	for range 60 {
		counts[wrr.pick(candidates, nil)]++
	}
	if counts[bs[1]] != 0 {
		t.Errorf("backend that wasn't a candidate was picked %d times", counts[bs[1]])
	}
	if counts[bs[0]] != 50 || counts[bs[2]] != 10 {
		t.Errorf("weights 5:1 over 60 picks gave %d:%d, want 50:10", counts[bs[0]], counts[bs[2]])
	}
}

// TestWeightedRoundRobinConcurrent runs picks from many goroutines. The
// mutex makes each pick one step, so the totals come out exact; without it,
// -race reports the map access and the totals drift.
func TestWeightedRoundRobinConcurrent(t *testing.T) {
	bs := weighted(5, 1, 1)
	wrr := &weightedRoundRobin{}

	var mu sync.Mutex
	counts := map[*backend]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 700 {
				b := wrr.pick(bs, nil)
				mu.Lock()
				counts[b]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	// 5600 picks is 800 full cycles of 7.
	if counts[bs[0]] != 4000 || counts[bs[1]] != 800 || counts[bs[2]] != 800 {
		t.Errorf("got %d/%d/%d, want 4000/800/800", counts[bs[0]], counts[bs[1]], counts[bs[2]])
	}
}

func TestWeightedRoundRobinThroughBalancer(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends: []Backend{
			{URL: namedBackend(t, "a"), Weight: 3},
			{URL: namedBackend(t, "b")}, // default weight 1
		},
		Algorithm: WeightedRoundRobin,
	})

	counts := map[string]int{}
	for range 8 {
		_, body := send(lb, http.MethodGet)
		counts[body]++
	}
	if counts["a"] != 6 || counts["b"] != 2 {
		t.Errorf("weights 3:1 over 8 requests gave a=%d b=%d, want 6 and 2", counts["a"], counts["b"])
	}
}

// loaded takes (active, weight) pairs.
func loaded(pairs ...[2]int) []*backend {
	bs := make([]*backend, len(pairs))
	for i, p := range pairs {
		bs[i] = &backend{weight: p[1]}
		bs[i].active.Store(int64(p[0]))
	}
	return bs
}

func TestWeightedLeastConnectionsUsesWeight(t *testing.T) {
	// A has more requests but three times the capacity: 3/3 = 1 per unit of
	// weight against B's 2/1 = 2. Plain least connections would pick B.
	bs := loaded([2]int{3, 3}, [2]int{2, 1})
	wlc := &weightedLeastConnections{}
	for i := range len(bs) { // every rotating start position
		if got := wlc.pick(bs, nil); got != bs[0] {
			t.Errorf("pick %d: got backend %d, want 0 (least loaded per unit of weight)", i, indexOf(bs, got))
		}
	}
}

func TestWeightedLeastConnectionsNoRounding(t *testing.T) {
	// Integer division would make both 0 (1/3 rounds down) and call it a tie.
	bs := loaded([2]int{1, 3}, [2]int{0, 1})
	wlc := &weightedLeastConnections{}
	for i := range len(bs) {
		if got := wlc.pick(bs, nil); got != bs[1] {
			t.Errorf("pick %d: got backend %d, want 1 (the idle one)", i, indexOf(bs, got))
		}
	}
}

func TestWeightedLeastConnectionsSpreadsTies(t *testing.T) {
	// 2/2 and 1/1 are equally loaded, so picks should alternate.
	bs := loaded([2]int{2, 2}, [2]int{1, 1})
	counts := make([]int, len(bs))
	for _, i := range pickIndexes(&weightedLeastConnections{}, bs, 6) {
		counts[i]++
	}
	if counts[0] != 3 || counts[1] != 3 {
		t.Errorf("equally loaded backends got %v picks, want 3 each", counts)
	}
}
