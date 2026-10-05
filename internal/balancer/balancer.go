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
	"strings"
	"time"
)

// Balancer is an http.Handler that forwards each request to one of its backends.
type Balancer struct {
	backends       []*backend
	picker         picker
	client         *http.Client  // for health checks
	requestTimeout time.Duration // total time per request, across retries
	healthPath     string        // path requested by health checks
	maxBodySize    int64         // largest request body accepted, in bytes
	retryStatus    bool          // retry safe requests answered 502 or 503
}

// New returns a Balancer for cfg. It returns an error if cfg is invalid: no
// backends, a malformed backend URL, a negative setting or weight, or an
// unknown algorithm.
func New(cfg Config) (*Balancer, error) {
	cfg = cfg.withDefaults()

	if len(cfg.Backends) == 0 {
		return nil, errors.New("no backends given")
	}
	if cfg.AttemptTimeout < 0 || cfg.RequestTimeout < 0 || cfg.MaxFailures < 0 || cfg.MaxBodySize < 0 {
		return nil, errors.New("timeouts, max failures and max body size must not be negative")
	}
	// Only a path: a query or fragment would be escaped into the path by
	// JoinPath and silently check the wrong URL.
	if !strings.HasPrefix(cfg.HealthPath, "/") || strings.ContainsAny(cfg.HealthPath, "?#") {
		return nil, fmt.Errorf("health path %q: want a path starting with /, such as /healthz", cfg.HealthPath)
	}
	p, err := newPicker(cfg.Algorithm)
	if err != nil {
		return nil, err
	}

	// Shared by every proxy, so they share a connection pool.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = cfg.AttemptTimeout
	// The default keeps 2 idle connections per backend, so under load most
	// requests dialed a new one and closed it after, leaving sockets in
	// TIME_WAIT. 0 lifts the total limit, leaving the per-backend one.
	transport.MaxIdleConnsPerHost = 100
	transport.MaxIdleConns = 0
	buffers := newBufferPool()

	lb := &Balancer{
		client:         &http.Client{Timeout: 2 * time.Second},
		requestTimeout: cfg.RequestTimeout,
		healthPath:     cfg.HealthPath,
		maxBodySize:    cfg.MaxBodySize,
		retryStatus:    cfg.RetryUnavailable,
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
		lb.backends = append(lb.backends, newBackend(u, transport, buffers, int32(cfg.MaxFailures), weight))
	}
	return lb, nil
}

func (lb *Balancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A body that declares its size can be rejected before any backend is
	// picked. One that doesn't is cut off by MaxBytesReader once it passes
	// the limit, which reaches handleError as a *http.MaxBytesError.
	if r.ContentLength > lb.maxBodySize {
		bodyTooLarge(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, lb.maxBodySize)

	var failErr error
	var canRetry bool
	var sent time.Time
	ctx := context.WithValue(r.Context(), failedKey{}, &failErr)
	ctx = context.WithValue(ctx, sentKey{}, &sent)
	r = r.WithContext(context.WithValue(ctx, retryKey{}, &canRetry))

	if !isUpgrade(r) {
		// A timer instead of WithTimeout, so the deadline can be lifted when
		// the response turns out to be a stream. The cause tells the timer
		// apart from the client giving up, which also cancels the context.
		cancelCtx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)

		timer := time.AfterFunc(lb.requestTimeout, func() { cancel(context.DeadlineExceeded) })
		defer timer.Stop()

		r = r.WithContext(context.WithValue(cancelCtx, streamKey{}, func() {
			timer.Stop()
			// If w can't lift the WriteTimeout, the stream is cut at it.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}))
	}

	retryable := isRetryable(r)
	candidates := lb.aliveBackends()
	for len(candidates) > 0 {
		b := lb.picker.pick(candidates, r)
		candidates = slices.DeleteFunc(candidates, func(c *backend) bool { return c == b })
		if a := accessFrom(r.Context()); a != nil {
			a.backend = b.url.Host
			a.attempts++
		}

		failErr = nil
		canRetry = lb.retryStatus && retryable && len(candidates) > 0
		sent = time.Now()
		b.serve(w, r)

		if failErr == nil {
			b.failures.Store(0)
			return
		}

		var maxErr *http.MaxBytesError
		if errors.As(failErr, &maxErr) {
			bodyTooLarge(w)
			return
		}

		switch ctxErr := context.Cause(r.Context()); {
		case errors.Is(ctxErr, context.DeadlineExceeded):
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return
		case ctxErr != nil:
			return
		}

		if !retryable {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
	}

	if failErr != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	http.Error(w, "no backends available", http.StatusServiceUnavailable)
}

// bodyTooLarge replies 413, for a request body over the size limit.
func bodyTooLarge(w http.ResponseWriter) {
	http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
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

// isRetryable reports whether a failed attempt may be retried on another
// backend. The method must be safe to repeat, and the request must have no
// body: a body is read while it's sent, so a retry would send it empty, and
// the next backend would fail through no fault of its own.
func isRetryable(r *http.Request) bool {
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
	return safe && r.ContentLength == 0 // -1 means a body of unknown length
}

// isUpgrade reports whether r asks to switch protocols, such as to a
// WebSocket. Upgraded connections are long-lived, so they get no total
// deadline. Both headers are required, as ReverseProxy requires them: a
// Connection: upgrade alone would let any request escape the deadline.
func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") == "" {
		return false
	}

	headers := r.Header.Values("Connection")
	for _, header := range headers {
		connections := strings.SplitSeq(header, ",")
		for connection := range connections {
			connection = strings.TrimSpace(connection)
			if strings.EqualFold(connection, "Upgrade") {
				return true
			}
		}
	}

	return false
}
