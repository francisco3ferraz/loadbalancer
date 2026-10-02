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
//	POST /admin/health/down   make /health fail; the backend keeps serving
//	POST /admin/health/up     make /health succeed again
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

// logRequests logs one line per request. X-Forwarded-For is included when
// present, which shows the real client behind the load balancer.
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
