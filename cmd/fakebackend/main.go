// Command fakebackend is a small HTTP server for exercising the load balancer.
// It can be told to be slow, to fail, or to report itself unhealthy, so every
// failure mode the load balancer handles can be reproduced on demand.
//
// Usage:
//
//	go run ./cmd/fakebackend -port 8080
//	go run ./cmd/fakebackend -port 8081 -delay 200ms -error-rate 0.1
//
// Endpoints:
//
//	ANY  /                    replies with the backend name
//	GET  /health              200 when healthy, 503 when marked down
//	GET  /slow?d=3s           waits d before replying (default 5s)
//	GET  /status/{code}       replies with the given status code
//	GET  /stream?every=1s&n=5 sends a server-sent event every interval, n times
//	                          (default: every second until the client leaves)
//	GET  /ws                  accepts a Connection: Upgrade and echoes every
//	                          byte back; a plain upgrade, no WebSocket framing
//	POST /admin/health/down   make /health fail; the backend keeps serving
//	POST /admin/health/up     make /health succeed again
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type server struct {
	name      string
	delay     time.Duration
	errorRate float64
	healthy   atomic.Bool
}

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	name := flag.String("name", "", `name to reply with (default ":<port>")`)
	delay := flag.Duration("delay", 0, "latency added to every request on /")
	errorRate := flag.Float64("error-rate", 0, "fraction of requests on / that fail with 500, from 0 to 1")
	logHealth := flag.Bool("log-health", false, "also log /health requests")
	flag.Parse()

	if *errorRate < 0 || *errorRate > 1 {
		log.Fatalf("-error-rate must be between 0 and 1, got %v", *errorRate)
	}
	if *name == "" {
		*name = fmt.Sprintf(":%d", *port)
	}

	s := &server{name: *name, delay: *delay, errorRate: *errorRate}
	s.healthy.Store(true)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           logRequests(s.name, *logHealth, s.routes()),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("%s listening on %s", s.name, srv.Addr)

	select {
	case err := <-errc:
		log.Fatal(err)
	case <-ctx.Done():
	}

	// A second Ctrl+C now kills the process instead of waiting for the drain.
	stop()
	log.Printf("%s shutting down", s.name)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("%s shutdown: %v", s.name, err)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /slow", s.handleSlow)
	mux.HandleFunc("GET /status/{code}", s.handleStatus)
	mux.HandleFunc("GET /stream", s.handleStream)
	mux.HandleFunc("GET /ws", s.handleUpgrade)
	mux.HandleFunc("POST /admin/health/{state}", s.handleSetHealth)
	return mux
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if !sleep(r.Context(), s.delay) {
		return
	}
	if s.errorRate > 0 && rand.Float64() < s.errorRate {
		http.Error(w, "injected failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Backend", s.name)
	fmt.Fprintln(w, s.name)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.healthy.Load() {
		http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

func (s *server) handleSlow(w http.ResponseWriter, r *http.Request) {
	d := 5 * time.Second
	if v := r.URL.Query().Get("d"); v != "" {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d < 0 {
			http.Error(w, "d must be a duration like 500ms or 3s", http.StatusBadRequest)
			return
		}
	}
	if !sleep(r.Context(), d) {
		return
	}
	w.Header().Set("X-Backend", s.name)
	fmt.Fprintf(w, "%s after %s\n", s.name, d)
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	code, err := strconv.Atoi(r.PathValue("code"))
	if err != nil || code < 200 || code > 599 {
		http.Error(w, "code must be between 200 and 599", http.StatusBadRequest)
		return
	}
	w.Header().Set("X-Backend", s.name)
	w.WriteHeader(code)
	fmt.Fprintf(w, "%d %s\n", code, http.StatusText(code))
}

func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	every := time.Second
	if v := q.Get("every"); v != "" {
		var err error
		if every, err = time.ParseDuration(v); err != nil || every <= 0 {
			http.Error(w, "every must be a positive duration like 500ms or 1s", http.StatusBadRequest)
			return
		}
	}
	n := 0 // 0 means until the client leaves
	if v := q.Get("n"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 0 {
			http.Error(w, "n must be a non-negative number", http.StatusBadRequest)
			return
		}
	}

	// Unflushed, events would reach the client in one lump.
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Backend", s.name)

	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for i := 1; n == 0 || i <= n; i++ {
		select {
		case <-ticker.C:
		case <-r.Context().Done():
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s %d\n\n", s.name, i); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// handleUpgrade switches the connection to an echo protocol: after the 101
// response, every byte the client sends comes straight back. That's all a
// proxy sees of a WebSocket too, so it's enough to test one, without
// implementing WebSocket framing.
func (s *server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if !headerHasToken(r.Header, "Connection", "upgrade") || r.Header.Get("Upgrade") == "" {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "echo")
		http.Error(w, "want Connection: Upgrade and an Upgrade header", http.StatusUpgradeRequired)
		return
	}

	// After Hijack, the handler must write the 101 itself.
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\nX-Backend: %s\r\n\r\n", r.Header.Get("Upgrade"), s.name)
	if err := brw.Flush(); err != nil {
		return
	}
	// Read through brw, not conn: the server may already have buffered bytes
	// the client sent right after its request. Copy returns when the client
	// closes its side.
	_, _ = io.Copy(conn, brw)
}

// headerHasToken reports whether the comma-separated header contains token,
// ignoring case. Connection: keep-alive, Upgrade is a valid upgrade request.
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func (s *server) handleSetHealth(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("state") {
	case "up":
		s.healthy.Store(true)
	case "down":
		s.healthy.Store(false)
	default:
		http.Error(w, "state must be up or down", http.StatusNotFound)
		return
	}
	log.Printf("%s health set to %s", s.name, r.PathValue("state"))
	fmt.Fprintf(w, "%s health %s\n", s.name, r.PathValue("state"))
}

// sleep waits for d and reports whether it finished. It returns false early
// if the client gives up, so abandoned requests don't hold the server.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// statusRecorder remembers the status code a handler sent. It stays 0 when the
// handler wrote nothing, which happens when the client gave up first.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logRequests logs one line per request.
func logRequests(name string, logHealth bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if r.URL.Path == "/health" && !logHealth {
			return
		}
		status := "-"
		if rec.status != 0 {
			status = strconv.Itoa(rec.status)
		} else if r.Context().Err() == nil {
			status = "200"
		}
		line := fmt.Sprintf("%s %s %s %s %s from %s", name, r.Method, r.URL.RequestURI(), status, time.Since(start).Round(time.Microsecond), r.RemoteAddr)
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			line += " for " + xff
		}
		if r.Context().Err() != nil {
			line += " (client gave up)"
		}
		log.Print(line)
	})
}
