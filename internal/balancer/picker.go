package balancer

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// picker chooses which backend a request tries next. candidates is never
// empty and holds only backends that are alive and not yet tried for this
// request, so a picker only has to choose, not check health or retries.
type picker interface {
	pick(candidates []*backend, r *http.Request) *backend
}

func newPicker(alg Algorithm) (picker, error) {
	switch alg {
	case RoundRobin:
		return &roundRobin{}, nil
	case LeastConnections:
		return &leastConnections{}, nil
	case WeightedRoundRobin:
		return &weightedRoundRobin{}, nil
	case WeightedLeastConnections:
		return &weightedLeastConnections{}, nil
	default:
		return nil, fmt.Errorf("unknown algorithm %q", alg)
	}
}

type roundRobin struct {
	counter atomic.Uint64
}

func (rr *roundRobin) pick(candidates []*backend, r *http.Request) *backend {
	counter := rr.counter.Add(1) - 1
	return candidates[counter%uint64(len(candidates))]
}

// leastConnections picks the candidate with the fewest requests in progress.
// The scan starts at a rotating position, so equally busy backends take turns
// instead of the first one always winning.
type leastConnections struct {
	counter atomic.Uint64
}

func (lc *leastConnections) pick(candidates []*backend, r *http.Request) *backend {
	n := uint64(len(candidates))
	start := lc.counter.Add(1) - 1

	best := candidates[start%n]
	least := best.active.Load()
	for i := uint64(1); i < n; i++ {
		c := candidates[(start+i)%n]
		if active := c.active.Load(); active < least {
			best, least = c, active
		}
	}

	return best
}

// weightedRoundRobin is nginx's smooth weighted round robin. On each pick it
// adds every candidate's weight to that candidate's current score, picks the
// highest score, and subtracts the candidates' total weight from the winner.
// Over a cycle each backend wins in proportion to its weight, and a winner
// drops far enough that the others get their turns in between: weights 5, 1,
// 1 give A A B A C A A rather than A A A A A B C.
type weightedRoundRobin struct {
	mu      sync.Mutex       // a pick updates several scores together, which atomics can't
	current map[*backend]int // guarded by mu
}

func (w *weightedRoundRobin) pick(candidates []*backend, r *http.Request) *backend {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.current == nil {
		w.current = make(map[*backend]int)
	}

	var best *backend
	total := 0
	for _, c := range candidates {
		w.current[c] += c.weight
		total += c.weight
		if best == nil || w.current[c] > w.current[best] {
			best = c
		}
	}
	w.current[best] -= total
	return best
}

// weightedLeastConnections picks the candidate with the fewest requests in
// progress per unit of weight, so a backend with weight 3 is as loaded at 3
// requests as one with weight 1 at 1. It compares by cross-multiplying,
// since integer division would round 1/3 down to 0. Like leastConnections,
// the scan starts at a rotating position so equally loaded backends take turns.
type weightedLeastConnections struct {
	counter atomic.Uint64
}

func (w *weightedLeastConnections) pick(candidates []*backend, r *http.Request) *backend {
	n := uint64(len(candidates))
	start := w.counter.Add(1) - 1

	best := candidates[start%n]
	least := best.active.Load()
	for i := uint64(1); i < n; i++ {
		c := candidates[(start+i)%n]
		// active/c.weight < least/best.weight, without the division.
		if active := c.active.Load(); active*int64(best.weight) < least*int64(c.weight) {
			best, least = c, active
		}
	}

	return best
}
