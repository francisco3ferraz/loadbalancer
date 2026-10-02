// Package balancer implements an HTTP load balancer: it spreads requests
// across backends with round robin, skips backends that are down, and
// retries safe requests on another backend when one fails.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// requestTimeout is the total time a request may take, across all retries.
const requestTimeout = 10 * time.Second

// Balancer is an http.Handler that forwards each request to one of its backends.
type Balancer struct {
	backends []*backend
	next     atomic.Uint64
	client   *http.Client // for health checks
}

// New returns a Balancer for the given backend URLs, such as
// "http://127.0.0.1:8080".
func New(addrs []string) (*Balancer, error) {
	if len(addrs) == 0 {
		return nil, errors.New("no backends given")
	}

	// One transport shared by every proxy, so they share a connection pool.
	// Cloning keeps the default settings; only the timeouts change.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 5 * time.Second

	lb := &Balancer{client: &http.Client{Timeout: 2 * time.Second}}
	for _, addr := range addrs {
		u, err := url.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("parse backend %q: %w", addr, err)
		}
		lb.backends = append(lb.backends, newBackend(u, transport))
	}
	return lb, nil
}

func (lb *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	failed := false

	r = r.WithContext(context.WithValue(r.Context(), failedKey{}, &failed))
	timeoutCtx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	r = r.WithContext(timeoutCtx)

	for _, b := range lb.rotation() {
		if !b.alive.Load() {
			continue
		}

		failed = false
		b.proxy.ServeHTTP(w, r)

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

// rotation returns the backends in the order this request should try them:
// round robin, starting one further along on each request.
func (lb *Balancer) rotation() []*backend {
	n := uint64(len(lb.backends))
	start := lb.next.Add(1) - 1
	order := make([]*backend, 0, n)
	for offset := range n {
		order = append(order, lb.backends[(start+offset)%n])
	}
	return order
}

func isRetryable(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
}
