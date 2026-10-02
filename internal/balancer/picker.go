package balancer

import (
	"net/http"
	"sync/atomic"
)

// picker chooses which backend a request tries next. candidates is never
// empty and holds only backends that are alive and not yet tried for this
// request, so a picker only has to choose, not check health or retries.
type picker interface {
	pick(candidates []*backend, r *http.Request) *backend
}

// roundRobin takes the candidates in turn.
type roundRobin struct {
	counter atomic.Uint64
}

func (rr *roundRobin) pick(candidates []*backend, r *http.Request) *backend {
	counter := rr.counter.Add(1) - 1
	return candidates[counter%uint64(len(candidates))]
}
