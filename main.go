package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

const (
	maxFailure     = 3
	requestTimeout = 10 * time.Second
)

type backend struct {
	url     *url.URL
	proxy   *httputil.ReverseProxy
	alive   atomic.Bool
	failure atomic.Int32
}

type failedKey struct{}

func main() {
	addrs := []string{"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082"}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 5 * time.Second

	var backends []*backend
	for _, addr := range addrs {
		u, err := url.Parse(addr)
		if err != nil {
			log.Fatal(err)
		}

		b := &backend{url: u, proxy: httputil.NewSingleHostReverseProxy(u)}
		b.alive.Store(true)
		b.proxy.Transport = transport
		b.proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
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
				count := b.failure.Add(1)
				dead = count >= maxFailure
			}

			if dead && b.alive.CompareAndSwap(true, false) {
				log.Printf("%s marked down: %v", b.url.Host, err)
			}
		}

		backends = append(backends, b)
	}

	var next atomic.Uint64
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		start := next.Add(1) - 1
		failed := false

		r = r.WithContext(context.WithValue(r.Context(), failedKey{}, &failed))
		timeoutCtx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(timeoutCtx)

		for offset := range uint64(len(backends)) {
			b := backends[(start+offset)%uint64(len(backends))]
			if b.alive.Load() {
				failed = false
				b.proxy.ServeHTTP(w, r)

				if !failed {
					b.failure.Store(0)
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
		}

		if failed {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		http.Error(w, "no backends available", http.StatusServiceUnavailable)
	})

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	go runHealthChecks(backends, client, 5*time.Second)
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func runHealthChecks(backends []*backend, client *http.Client, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for _, b := range backends {
			alive := isHealthy(b, client)
			if alive != b.alive.Load() {
				if alive {
					b.failure.Store(0)
					log.Printf("%s is up", b.url.Host)
				} else {
					log.Printf("%s is down", b.url.Host)
				}
			}
			b.alive.Store(alive)
		}
	}
}

func isHealthy(b *backend, client *http.Client) bool {
	resp, err := client.Get(b.url.JoinPath("health").String())
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

func isRetryable(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
}
