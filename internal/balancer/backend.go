package balancer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type backend struct {
	url      *url.URL
	proxy    *httputil.ReverseProxy
	alive    atomic.Bool
	failures atomic.Int32
	active   atomic.Int64
	requests atomic.Uint64

	maxFailures int32 // in a row; connection errors mark it down at once
	weight      int   // never changes, so it needs no synchronisation
}

// failedKey is the context key under which ServeHTTP stores a *error that
// handleError sets to the error when an attempt fails, so ServeHTTP can tell
// why it failed.
type failedKey struct{}

// retryKey is the context key under which ServeHTTP stores a *bool, set
// before each attempt, telling modifyResponse whether a 502 or 503 may be
// retried on another backend. When it can't, the response goes to the client.
type retryKey struct{}

// errRetryStatus is returned by modifyResponse to turn a 502 or 503 into a
// failed attempt, which ServeHTTP retries.
var errRetryStatus = errors.New("retryable status")

// streamKey is the context key under which ServeHTTP stores a func() that
// lifts the request deadline. modifyResponse calls it when the response is
// a stream. Upgrades have no deadline, so they store nothing.
type streamKey struct{}

func newBackend(u *url.URL, transport http.RoundTripper, maxFailures int32, weight int) *backend {
	b := &backend{
		url:         u,
		proxy:       &httputil.ReverseProxy{},
		maxFailures: maxFailures,
		weight:      weight,
	}
	b.alive.Store(true)
	b.proxy.Rewrite = b.rewrite
	b.proxy.Transport = transport
	b.proxy.ErrorHandler = b.handleError
	b.proxy.ModifyResponse = b.modifyResponse
	return b
}

// rewrite is the proxy's Rewrite hook. The proxy strips the client's
// forwarding headers before calling it, so the ones set here can be trusted
// by backends; appending to the client's would let it forge its address.
func (b *backend) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(b.url)
	pr.Out.Host = pr.In.Host // SetURL would send the backend's own host
	pr.SetXForwarded()
}

// handleError is the proxy's ErrorHandler. It writes nothing: it reports the
// failure to ServeHTTP, which decides whether to retry, and marks the backend
// down when the failure says it's unhealthy.
func (b *backend) handleError(w http.ResponseWriter, r *http.Request, err error) {
	if p, ok := r.Context().Value(failedKey{}).(*error); ok {
		*p = err
	}

	// When the request's context has ended, the backend isn't to blame.
	switch ctxErr := context.Cause(r.Context()); {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		log.Printf("%s: request deadline exceeded: %v", b.url.Host, err)
		return
	case ctxErr != nil:
		log.Printf("%s: client gave up: %v", b.url.Host, err)
		return
	}

	// The client sent too much, which isn't the backend's fault either.
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		log.Printf("%s: request body over %d bytes", b.url.Host, maxErr.Limit)
		return
	}

	// The backend answered, so it isn't counted as failing. Health checks
	// decide whether it's down.
	if errors.Is(err, errRetryStatus) {
		log.Printf("%s: %v, trying another backend", b.url.Host, err)
		return
	}

	log.Printf("%s failed: %v", b.url.Host, err)
	dead := false

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		dead = true
	} else {
		count := b.failures.Add(1)
		dead = count >= b.maxFailures
	}

	if dead && b.alive.CompareAndSwap(true, false) {
		log.Printf("%s marked down: %v", b.url.Host, err)
	}
}

// modifyResponse is the proxy's ModifyResponse hook, which runs when the
// backend's response headers arrive, before anything reaches the client.
// It rejects a 502 or 503 that may be retried, lifts the request deadline
// for server-sent events, which stay open for as long as the backend sends
// them, and tells the access log about protocol switches, which it can't see.
func (b *backend) modifyResponse(resp *http.Response) error {
	if resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable {
		if retry, ok := resp.Request.Context().Value(retryKey{}).(*bool); ok && *retry {
			// The proxy closes the body and calls handleError instead.
			return fmt.Errorf("%w %d", errRetryStatus, resp.StatusCode)
		}
	}

	if a := accessFrom(resp.Request.Context()); a != nil && resp.StatusCode == http.StatusSwitchingProtocols {
		a.upgraded = true
	}

	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" && err == nil {
		stop, ok := resp.Request.Context().Value(streamKey{}).(func())
		if ok {
			stop()
		}
	}

	return nil
}

// serve forwards one attempt to b, counting it in active for as long as it
// runs. The decrement is deferred so it also happens when the proxy panics,
// which it does when the client disconnects mid-response.
func (b *backend) serve(w http.ResponseWriter, r *http.Request) {
	b.requests.Add(1)

	b.active.Add(1)
	defer b.active.Add(-1)

	b.proxy.ServeHTTP(w, r)
}
