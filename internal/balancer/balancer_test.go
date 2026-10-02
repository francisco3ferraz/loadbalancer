package balancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// namedBackend starts a backend that answers every request with its name.
func namedBackend(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// deadBackend returns the URL of a server that has already shut down, so
// connections to it are refused, like a crashed process.
func deadBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// hangingBackend starts a backend that never answers. It waits on the
// request's context rather than sleeping, because srv.Close blocks until
// every request in progress has finished.
func hangingBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newBalancer(t *testing.T, cfg Config) *Balancer {
	t.Helper()
	lb, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return lb
}

// send makes one request through lb and returns the status code and body.
func send(lb *Balancer, method string) (int, string) {
	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
	return rec.Code, rec.Body.String()
}

func TestNewErrors(t *testing.T) {
	tests := map[string]Config{
		"no backends": {},
		"bad url":     {Backends: []string{"http://[::1"}},
		"bad algorithm": {
			Backends:  []string{"http://127.0.0.1:1"},
			Algorithm: "fastest",
		},
	}
	for name, cfg := range tests {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New returned no error", name)
		}
	}
}

func TestRoundRobin(t *testing.T) {
	lb := newBalancer(t, Config{Backends: []string{
		namedBackend(t, "a"), namedBackend(t, "b"), namedBackend(t, "c"),
	}})

	counts := map[string]int{}
	for range 6 {
		code, body := send(lb, http.MethodGet)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want %d", code, http.StatusOK)
		}
		counts[body]++
	}
	for _, name := range []string{"a", "b", "c"} {
		if counts[name] != 2 {
			t.Errorf("backend %s got %d requests, want 2 (all: %v)", name, counts[name], counts)
		}
	}
}

func TestFailoverToHealthyBackend(t *testing.T) {
	lb := newBalancer(t, Config{Backends: []string{
		namedBackend(t, "a"), deadBackend(t), namedBackend(t, "c"),
	}})

	for i := range 6 {
		if code, body := send(lb, http.MethodGet); code != http.StatusOK {
			t.Errorf("request %d: status = %d (%q), want %d", i, code, body, http.StatusOK)
		}
	}
	if lb.backends[1].alive.Load() {
		t.Error("dead backend is still marked alive")
	}
}

func TestPostIsNotRetried(t *testing.T) {
	// Round robin starts at index 0, so the first request hits the dead backend.
	lb := newBalancer(t, Config{Backends: []string{deadBackend(t), namedBackend(t, "b")}})

	if code, _ := send(lb, http.MethodPost); code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", code, http.StatusBadGateway)
	}
}

func TestAllBackendsDown(t *testing.T) {
	lb := newBalancer(t, Config{Backends: []string{deadBackend(t), deadBackend(t)}})

	// The first request tries both backends and marks them down.
	if code, _ := send(lb, http.MethodGet); code != http.StatusBadGateway {
		t.Errorf("first request: status = %d, want %d", code, http.StatusBadGateway)
	}
	// Now none are alive, so nothing is tried.
	if code, _ := send(lb, http.MethodGet); code != http.StatusServiceUnavailable {
		t.Errorf("second request: status = %d, want %d", code, http.StatusServiceUnavailable)
	}
}

func TestTimeoutsMarkDownAfterMaxFailures(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       []string{hangingBackend(t), namedBackend(t, "b")},
		AttemptTimeout: 50 * time.Millisecond,
		MaxFailures:    3,
	})
	hanging := lb.backends[0]

	// Which requests reach the hanging backend depends on the picker, so this
	// checks the rule rather than an order: it stays alive while it has fewer
	// than MaxFailures failures, and goes down when it reaches them. Every
	// request still succeeds, because a timeout is retried on b.
	for i := 0; i < 10 && hanging.alive.Load(); i++ {
		if code, _ := send(lb, http.MethodGet); code != http.StatusOK {
			t.Errorf("request %d: status = %d, want %d", i, code, http.StatusOK)
		}
		failures, alive := hanging.failures.Load(), hanging.alive.Load()
		if alive != (failures < 3) {
			t.Fatalf("after request %d: alive = %v with %d failures, want alive only below 3", i, alive, failures)
		}
	}
	if hanging.alive.Load() {
		t.Errorf("hanging backend still alive after 10 requests (failures = %d)", hanging.failures.Load())
	}
}

func TestSuccessResetsFailures(t *testing.T) {
	lb := newBalancer(t, Config{Backends: []string{namedBackend(t, "a")}})
	lb.backends[0].failures.Store(2)

	send(lb, http.MethodGet)
	if got := lb.backends[0].failures.Load(); got != 0 {
		t.Errorf("failures = %d after a success, want 0", got)
	}
}

func TestRequestDeadline(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       []string{hangingBackend(t), hangingBackend(t)},
		AttemptTimeout: time.Second,
		RequestTimeout: 100 * time.Millisecond,
	})

	start := time.Now()
	code, _ := send(lb, http.MethodGet)
	elapsed := time.Since(start)

	if code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", code, http.StatusGatewayTimeout)
	}
	if elapsed < 100*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Errorf("took %s, want about 100ms", elapsed)
	}
	// Running out of budget is not the backend's fault.
	for i, b := range lb.backends {
		if b.failures.Load() != 0 || !b.alive.Load() {
			t.Errorf("backend %d: failures = %d, alive = %v; want 0, true", i, b.failures.Load(), b.alive.Load())
		}
	}
}

func TestClientGivingUpIsNotCounted(t *testing.T) {
	lb := newBalancer(t, Config{Backends: []string{hangingBackend(t)}})

	// A disconnecting client cancels the request's context. Use cancel, not a
	// timeout: a timeout ends with DeadlineExceeded, which is the balancer's
	// own budget running out, a different code path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	lb.ServeHTTP(httptest.NewRecorder(), req)

	b := lb.backends[0]
	if b.failures.Load() != 0 || !b.alive.Load() {
		t.Errorf("failures = %d, alive = %v; want 0, true", b.failures.Load(), b.alive.Load())
	}
}

func TestActiveCountsRequestsInProgress(t *testing.T) {
	lb := newBalancer(t, Config{Backends: []string{hangingBackend(t)}})
	b := lb.backends[0]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		lb.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil))
	}()

	waitFor(t, "active == 1 while the request is in progress", func() bool { return b.active.Load() == 1 })

	// Cut the request short: the count must still go back down.
	cancel()
	<-done
	if got := b.active.Load(); got != 0 {
		t.Errorf("active = %d after the request ended, want 0", got)
	}
}

func TestHealthChecks(t *testing.T) {
	healthy := make(chan bool, 1)
	healthy <- false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := <-healthy
		healthy <- ok
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	setHealthy := func(ok bool) { <-healthy; healthy <- ok }

	lb := newBalancer(t, Config{Backends: []string{srv.URL}})
	b := lb.backends[0]

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		lb.RunHealthChecks(ctx, 10*time.Millisecond)
		close(done)
	}()

	waitFor(t, "backend marked down", func() bool { return !b.alive.Load() })

	b.failures.Store(2)
	setHealthy(true)
	waitFor(t, "backend marked up", b.alive.Load)
	if got := b.failures.Load(); got != 0 {
		t.Errorf("failures = %d after recovery, want 0", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunHealthChecks did not return after its context was cancelled")
	}
}

// waitFor polls cond until it returns true, failing the test after a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
