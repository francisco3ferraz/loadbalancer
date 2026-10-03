package balancer

// Stats is a snapshot of the balancer's state. It holds plain values, so it
// can be read, passed around, or encoded without any synchronisation.
type Stats struct {
	Backends []BackendStats `json:"backends"`
}

// BackendStats is a snapshot of one backend.
type BackendStats struct {
	URL      string `json:"url"`
	Alive    bool   `json:"alive"`
	Active   int64  `json:"active"`   // requests in progress
	Failures int32  `json:"failures"` // failures in a row
	Requests uint64 `json:"requests"` // attempts served in total
}

// Stats returns a snapshot of every backend. Each value is read atomically,
// but not all at the same instant, so numbers may be microseconds apart.
func (lb *Balancer) Stats() Stats {
	backends := make([]BackendStats, 0, len(lb.backends))
	for _, b := range lb.backends {
		backends = append(backends, BackendStats{
			URL:      b.url.String(),
			Alive:    b.alive.Load(),
			Active:   b.active.Load(),
			Failures: b.failures.Load(),
			Requests: b.requests.Load(),
		})
	}
	return Stats{Backends: backends}
}
