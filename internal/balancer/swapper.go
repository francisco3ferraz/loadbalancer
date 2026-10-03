package balancer

import (
	"net/http"
	"sync/atomic"
)

// Swapper is an http.Handler that forwards each request to the current
// Balancer, which can be replaced at any time, such as when the config is
// reloaded. It's safe for concurrent use.
//
// A request that started before a swap finishes on the old Balancer: it
// keeps the pointer it loaded, so nothing has to be drained.
type Swapper struct {
	current atomic.Pointer[Balancer]
}

// NewSwapper returns a Swapper that starts with lb, which must not be nil.
func NewSwapper(lb *Balancer) *Swapper {
	s := &Swapper{}
	s.Swap(lb)
	return s
}

// Swap makes lb the Balancer for new requests and returns the previous one,
// so the caller can stop its health checks. lb must not be nil.
func (s *Swapper) Swap(lb *Balancer) *Balancer {
	// A nil Balancer would only panic later, on the next request, far from
	// the mistake. Failing here points at the caller.
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

// Stats returns the current Balancer's stats. Counters start again from
// zero after a swap, as the new Balancer has its own.
func (s *Swapper) Stats() Stats {
	return s.current.Load().Stats()
}
