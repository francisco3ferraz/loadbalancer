package balancer

import (
	"fmt"
	"net/http"
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
	default:
		return nil, fmt.Errorf("unknown algorithm %q", alg)
	}
}

// roundRobin takes the candidates in turn.
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
