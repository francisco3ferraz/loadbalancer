package balancer

import "time"

const (
	defaultAttemptTimeout = 5 * time.Second
	defaultRequestTimeout = 10 * time.Second
	defaultMaxFailures    = 3
)

// Algorithm names a way of choosing which backend gets each request.
type Algorithm string

const (
	// RoundRobin sends requests to each backend in turn.
	RoundRobin Algorithm = "round-robin"
	// LeastConnections sends each request to the backend with the fewest
	// requests in progress, which suits traffic where some requests are slow.
	LeastConnections Algorithm = "least-connections"
)

// Config configures a Balancer. Only Backends is required; any other field
// left at zero gets its default.
type Config struct {
	// Backends are the URLs of the servers to balance across, such as
	// "http://127.0.0.1:8080".
	Backends []string

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
}

// withDefaults returns a copy of c with unset fields filled in.
func (c Config) withDefaults() Config {
	if c.AttemptTimeout == 0 {
		c.AttemptTimeout = defaultAttemptTimeout
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.MaxFailures == 0 {
		c.MaxFailures = defaultMaxFailures
	}
	if c.Algorithm == "" {
		c.Algorithm = RoundRobin
	}

	return c
}
