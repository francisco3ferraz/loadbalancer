package balancer

import "time"

// DefaultRequestTimeout is the RequestTimeout used when none is set.
// It's exported so callers can size their own server timeouts above it.
const DefaultRequestTimeout = 10 * time.Second

const (
	defaultAttemptTimeout = 5 * time.Second
	defaultMaxFailures    = 3
	defaultHealthPath     = "/health"
)

// Algorithm names a way of choosing which backend gets each request.
type Algorithm string

const (
	// RoundRobin sends requests to each backend in turn.
	RoundRobin Algorithm = "round-robin"
	// LeastConnections sends each request to the backend with the fewest
	// requests in progress, which suits traffic where some requests are slow.
	LeastConnections Algorithm = "least-connections"
	// WeightedRoundRobin sends each backend a share of requests in proportion
	// to its Weight, spread evenly rather than in bursts.
	WeightedRoundRobin Algorithm = "weighted-round-robin"
	// WeightedLeastConnections sends each request to the backend with the
	// fewest requests in progress relative to its Weight, for backends of
	// different sizes where some requests are slow.
	WeightedLeastConnections Algorithm = "weighted-least-connections"
)

// Backend is one server to balance across.
type Backend struct {
	// URL is the server's address, such as "http://127.0.0.1:8080".
	URL string

	// Weight is the backend's relative capacity, used by WeightedRoundRobin
	// and WeightedLeastConnections: a backend with weight 3 gets three times
	// the requests of one with weight 1, or counts as equally loaded at three
	// times the requests in progress. Other algorithms ignore it. Default 1.
	Weight int
}

// Config configures a Balancer. Only Backends is required; any other field
// left at zero gets its default.
type Config struct {
	// Backends are the servers to balance across.
	Backends []Backend

	// Algorithm chooses which backend gets each request. Default RoundRobin.
	Algorithm Algorithm

	// AttemptTimeout is how long one backend has to start answering before
	// the attempt fails and, for safe methods, is retried elsewhere.
	// Default 5s.
	AttemptTimeout time.Duration

	// RequestTimeout is the total time a request may take, across all
	// retries. When it runs out the client gets a 504. Default 10s.
	RequestTimeout time.Duration

	// MaxFailures is how many failures in a row mark a backend down.
	// Connection errors mark it down immediately. Default 3.
	MaxFailures int

	// HealthPath is the path health checks request on every backend, such
	// as "/healthz". It's joined onto the backend's URL, so a backend at
	// http://host/api is checked at http://host/api/healthz. A backend is
	// healthy when it answers 200. Default "/health".
	HealthPath string
}

// withDefaults returns a copy of c with unset fields filled in.
func (c Config) withDefaults() Config {
	if c.AttemptTimeout == 0 {
		c.AttemptTimeout = defaultAttemptTimeout
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.MaxFailures == 0 {
		c.MaxFailures = defaultMaxFailures
	}
	if c.Algorithm == "" {
		c.Algorithm = RoundRobin
	}
	if c.HealthPath == "" {
		c.HealthPath = defaultHealthPath
	}

	return c
}
