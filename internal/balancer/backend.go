package balancer

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

// maxFailures is how many failures in a row (other than connection errors)
// mark a backend down.
const maxFailures = 3

type backend struct {
	url      *url.URL
	proxy    *httputil.ReverseProxy
	alive    atomic.Bool
	failures atomic.Int32
}

// failedKey is the context key under which ServeHTTP stores a *bool that
// handleError sets when an attempt fails.
type failedKey struct{}

func newBackend(u *url.URL, transport http.RoundTripper) *backend {
	b := &backend{url: u, proxy: httputil.NewSingleHostReverseProxy(u)}
	b.alive.Store(true)
	b.proxy.Transport = transport
	b.proxy.ErrorHandler = b.handleError
	return b
}

// handleError is the proxy's ErrorHandler. It writes nothing: it reports the
// failure to ServeHTTP, which decides whether to retry, and marks the backend
// down when the failure says it's unhealthy.
func (b *backend) handleError(w http.ResponseWriter, r *http.Request, err error) {
	if p, ok := r.Context().Value(failedKey{}).(*bool); ok {
		*p = true
	}

	// When the request's context has ended, the backend isn't to blame.
	switch ctxErr := r.Context().Err(); {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		log.Printf("%s: request deadline exceeded: %v", b.url.Host, err)
		return
	case ctxErr != nil:
		log.Printf("%s: client gave up: %v", b.url.Host, err)
		return
	}

	log.Printf("%s failed: %v", b.url.Host, err)
	dead := false
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		dead = true
	} else {
		count := b.failures.Add(1)
		dead = count >= maxFailures
	}

	if dead && b.alive.CompareAndSwap(true, false) {
		log.Printf("%s marked down: %v", b.url.Host, err)
	}
}
