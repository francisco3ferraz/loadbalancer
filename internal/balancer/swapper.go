package balancer

import (
	"net/http"
	"sync/atomic"
)

// Swapper is an http.Handler that forwards each request to a Balancer that
// can be replaced at any time. Requests already in progress finish on the
// old one. It's safe for concurrent use.
type Swapper struct {
	current atomic.Pointer[Balancer]
}

// NewSwapper returns a Swapper that starts with lb.
func NewSwapper(lb *Balancer) *Swapper {
	s := &Swapper{}
	s.Swap(lb)
	return s
}

// Swap makes lb the Balancer for new requests and returns the previous one.
func (s *Swapper) Swap(lb *Balancer) *Balancer {
	// Otherwise the panic would come on the next request, far from the bug.
	if lb == nil {
		panic("balancer: Swap with nil Balancer")
	}
	return s.current.Swap(lb)
}

// Current returns the Balancer that new requests go to.
func (s *Swapper) Current() *Balancer {
	return s.current.Load()
}

func (s *Swapper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.current.Load().ServeHTTP(w, r)
}

// Stats returns the current Balancer's stats.
func (s *Swapper) Stats() Stats {
	return s.current.Load().Stats()
}
