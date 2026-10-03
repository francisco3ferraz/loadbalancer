// Package balancer implements an HTTP load balancer: it spreads requests
// across backends using a pluggable algorithm, skips backends that are down,
// and retries safe requests on another backend when one fails.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Balancer is an http.Handler that forwards each request to one of its backends.
type Balancer struct {
	backends       []*backend
	picker         picker
	client         *http.Client  // for health checks
	requestTimeout time.Duration // total time per request, across retries
}

// New returns a Balancer for cfg. It returns an error if cfg is invalid: no
// backends, a malformed backend URL, a negative setting or weight, or an
// unknown algorithm.
func New(cfg Config) (*Balancer, error) {
	cfg = cfg.withDefaults()

	if len(cfg.Backends) == 0 {
		return nil, errors.New("no backends given")
	}
	if cfg.AttemptTimeout < 0 || cfg.RequestTimeout < 0 || cfg.MaxFailures < 0 {
		return nil, errors.New("timeouts and max failures must not be negative")
	}
	p, err := newPicker(cfg.Algorithm)
	if err != nil {
		return nil, err
	}

	// One transport shared by every proxy, so they share a connection pool.
	// Cloning keeps the default settings; only the timeouts change.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = cfg.AttemptTimeout

	lb := &Balancer{
		client:         &http.Client{Timeout: 2 * time.Second},
		requestTimeout: cfg.RequestTimeout,
		picker:         p,
	}
	for _, be := range cfg.Backends {
		u, err := url.Parse(be.URL)
		if err != nil {
			return nil, fmt.Errorf("parse backend %q: %w", be.URL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("backend %q: want a URL like http://host:port", be.URL)
		}
		weight := be.Weight
		switch {
		case weight == 0:
			weight = 1
		case weight < 0:
			return nil, fmt.Errorf("backend %q: weight must not be negative", be.URL)
		}
		lb.backends = append(lb.backends, newBackend(u, transport, int32(cfg.MaxFailures), weight))
	}
	return lb, nil
}

func (lb *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	failed := false

	r = r.WithContext(context.WithValue(r.Context(), failedKey{}, &failed))
	timeoutCtx, cancel := context.WithTimeout(r.Context(), lb.requestTimeout)
	defer cancel()
	r = r.WithContext(timeoutCtx)

	candidates := lb.aliveBackends()
	for len(candidates) > 0 {
		b := lb.picker.pick(candidates, r)
		candidates = slices.DeleteFunc(candidates, func(c *backend) bool { return c == b })

		failed = false
		b.serve(w, r)

		if !failed {
			b.failures.Store(0)
			return
		}

		switch ctxErr := r.Context().Err(); {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		case ctxErr != nil:
			return
		}

		if !isRetryable(r) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
	}

	if failed {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	http.Error(w, "no backends available", http.StatusServiceUnavailable)
}

// aliveBackends returns the backends currently marked alive, in a new slice
// that the caller may modify.
func (lb *Balancer) aliveBackends() []*backend {
	alive := make([]*backend, 0, len(lb.backends))
	for _, b := range lb.backends {
		if b.alive.Load() {
			alive = append(alive, b)
		}
	}
	return alive
}

func isRetryable(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
}
