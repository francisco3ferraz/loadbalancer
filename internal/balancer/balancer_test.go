package balancer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
// every request in progress has finished. It reads the body first: the
// server only notices the client disconnecting, and cancels the context,
// once the body has been read.
func hangingBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// echoBackend starts a backend that accepts a protocol upgrade and then
// echoes back every byte it receives, like the fake backend's /ws.
func echoBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", r.Header.Get("Upgrade"))
		if brw.Flush() == nil {
			io.Copy(conn, brw)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// streamBackend starts a backend that sends n lines of the given content
// type, one every interval, flushing each so it leaves at once.
func streamBackend(t *testing.T, contentType string, n int, every time.Duration) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		rc := http.NewResponseController(w)
		for i := 1; i <= n; i++ {
			select {
			case <-time.After(every):
			case <-r.Context().Done():
				return
			}
			fmt.Fprintf(w, "data: %d\n\n", i)
			if rc.Flush() != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// urls turns backend URLs into Backends with the default weight.
func urls(addrs ...string) []Backend {
	bs := make([]Backend, len(addrs))
	for i, addr := range addrs {
		bs[i] = Backend{URL: addr}
	}
	return bs
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
		"bad url":     {Backends: urls("http://[::1")},
		"no scheme":   {Backends: urls("127.0.0.1:8080")},
		"health path without slash": {
			Backends:   urls("http://127.0.0.1:1"),
			HealthPath: "healthz",
		},
		"health path with query": {
			Backends:   urls("http://127.0.0.1:1"),
			HealthPath: "/health?full=1",
		},
		"negative weight": {
			Backends: []Backend{{URL: "http://127.0.0.1:1", Weight: -1}},
		},
		"negative timeout": {
			Backends:       urls("http://127.0.0.1:1"),
			RequestTimeout: -time.Second,
		},
		"bad algorithm": {
			Backends:  urls("http://127.0.0.1:1"),
			Algorithm: "fastest",
		},
	}
	for name, cfg := range tests {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New returned no error", name)
		}
	}
}

func TestNewAcceptsEveryAlgorithm(t *testing.T) {
	for _, alg := range []Algorithm{RoundRobin, LeastConnections, WeightedRoundRobin, WeightedLeastConnections} {
		if _, err := New(Config{Backends: urls("http://127.0.0.1:1"), Algorithm: alg}); err != nil {
			t.Errorf("%s: %v", alg, err)
		}
	}
}

func TestRoundRobin(t *testing.T) {
	lb := newBalancer(t, Config{Backends: urls(
		namedBackend(t, "a"), namedBackend(t, "b"), namedBackend(t, "c"),
	)})

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
	lb := newBalancer(t, Config{Backends: urls(
		namedBackend(t, "a"), deadBackend(t), namedBackend(t, "c"),
	)})

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
	lb := newBalancer(t, Config{Backends: urls(deadBackend(t), namedBackend(t, "b"))})

	if code, _ := send(lb, http.MethodPost); code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", code, http.StatusBadGateway)
	}
}

// TestRequestWithBodyIsNotRetried: the first attempt sends the body, so a
// retry would send it empty, and the next backend would fail and be blamed
// for it. The client gets a 502 and the second backend is left alone.
func TestRequestWithBodyIsNotRetried(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       urls(hangingBackend(t), namedBackend(t, "b")),
		AttemptTimeout: 50 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", strings.NewReader("hello")))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if second := lb.backends[1]; second.requests.Load() != 0 || second.failures.Load() != 0 {
		t.Errorf("second backend: requests = %d, failures = %d; want it untouched",
			second.requests.Load(), second.failures.Load())
	}
}

func TestAllBackendsDown(t *testing.T) {
	lb := newBalancer(t, Config{Backends: urls(deadBackend(t), deadBackend(t))})

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
		Backends:       urls(hangingBackend(t), namedBackend(t, "b")),
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
	lb := newBalancer(t, Config{Backends: urls(namedBackend(t, "a"))})
	lb.backends[0].failures.Store(2)

	send(lb, http.MethodGet)
	if got := lb.backends[0].failures.Load(); got != 0 {
		t.Errorf("failures = %d after a success, want 0", got)
	}
}

func TestRequestDeadline(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       urls(hangingBackend(t), hangingBackend(t)),
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

func TestIsUpgrade(t *testing.T) {
	tests := []struct {
		name string
		h    http.Header
		want bool
	}{
		{"websocket", http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}}, true},
		{"token in a list", http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"websocket"}}, true},
		{"any case", http.Header{"Connection": {"UPGRADE"}, "Upgrade": {"websocket"}}, true},
		{"second header line", http.Header{"Connection": {"keep-alive", "upgrade"}, "Upgrade": {"websocket"}}, true},
		{"no Upgrade header", http.Header{"Connection": {"upgrade"}}, false},
		{"no Connection token", http.Header{"Connection": {"keep-alive"}, "Upgrade": {"websocket"}}, false},
		{"token as substring", http.Header{"Connection": {"upgrades"}, "Upgrade": {"websocket"}}, false},
		{"plain request", http.Header{}, false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header = tt.h
		if got := isUpgrade(r); got != tt.want {
			t.Errorf("%s: isUpgrade = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestUpgradeOutlivesRequestDeadline checks that an upgraded connection, such
// as a WebSocket, keeps working after the request deadline has passed.
func TestUpgradeOutlivesRequestDeadline(t *testing.T) {
	const timeout = 100 * time.Millisecond
	lb := newBalancer(t, Config{
		Backends:       urls(echoBackend(t)),
		RequestTimeout: timeout,
	})
	// A real server, not a recorder: upgrading needs a connection to hijack.
	srv := httptest.NewServer(lb)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: test\r\nConnection: keep-alive, Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	time.Sleep(3 * timeout)
	io.WriteString(conn, "ping\n")
	if got, err := br.ReadString('\n'); err != nil || got != "ping\n" {
		t.Errorf("echo after the deadline: got %q, %v; want %q", got, err, "ping\n")
	}
}

// TestConnectionUpgradeAloneKeepsDeadline checks that a request can't escape
// the deadline by claiming to upgrade without naming a protocol.
func TestConnectionUpgradeAloneKeepsDeadline(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends:       urls(hangingBackend(t)),
		AttemptTimeout: time.Second,
		RequestTimeout: 100 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Connection", "upgrade")
	rec := httptest.NewRecorder()
	lb.ServeHTTP(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusGatewayTimeout)
	}
}

// TestStreamOutlivesTimeouts checks that a server-sent event stream outlives
// both the request deadline and the server's WriteTimeout, while a response
// that's just as slow but isn't a stream is still cut off.
func TestStreamOutlivesTimeouts(t *testing.T) {
	const timeout = 100 * time.Millisecond
	const n = 6 // events, sent every timeout/2, so they last 3 timeouts
	tests := []struct {
		contentType string
		wantAll     bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"text/plain", false},
	}
	for _, tt := range tests {
		lb := newBalancer(t, Config{
			Backends:       urls(streamBackend(t, tt.contentType, n, timeout/2)),
			RequestTimeout: timeout,
		})
		srv := httptest.NewUnstartedServer(lb)
		srv.Config.WriteTimeout = timeout
		srv.Start()

		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", tt.contentType, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()

		got := strings.Count(string(body), "data:")
		if tt.wantAll && (err != nil || got != n) {
			t.Errorf("%s: got %d of %d events, err %v; want all of them", tt.contentType, got, n, err)
		}
		if !tt.wantAll && err == nil && got == n {
			t.Errorf("%s: got all %d events; want it cut off at the deadline", tt.contentType, n)
		}
	}
}

func TestClientGivingUpIsNotCounted(t *testing.T) {
	lb := newBalancer(t, Config{Backends: urls(hangingBackend(t))})

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
	lb := newBalancer(t, Config{Backends: urls(hangingBackend(t))})
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

	lb := newBalancer(t, Config{Backends: urls(srv.URL)})
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

// TestHealthChecksRunConcurrently checks that a round of health checks takes
// about one check timeout, not one per backend: three hanging backends must
// not make a round take three times as long.
func TestHealthChecksRunConcurrently(t *testing.T) {
	lb := newBalancer(t, Config{
		Backends: urls(hangingBackend(t), hangingBackend(t), hangingBackend(t)),
	})
	timeout := lb.client.Timeout

	start := time.Now()
	lb.checkAll(context.Background())
	elapsed := time.Since(start)

	if elapsed < timeout || elapsed > timeout+timeout/2 {
		t.Errorf("round took %s, want about %s (one check timeout)", elapsed, timeout)
	}
	for i, b := range lb.backends {
		if b.alive.Load() {
			t.Errorf("backend %d still alive after its check timed out", i)
		}
	}
}

// TestHealthPath checks that health checks request the configured path,
// joined onto the backend's URL (including any base path it has).
func TestHealthPath(t *testing.T) {
	tests := []struct {
		name, base, healthPath, want string
	}{
		{"default", "", "", "/health"},
		{"custom", "", "/healthz", "/healthz"},
		{"under a base path", "/api", "/ready", "/api/ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := make(chan string, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case paths <- r.URL.Path:
				default:
				}
			}))
			t.Cleanup(srv.Close)

			lb := newBalancer(t, Config{Backends: urls(srv.URL + tt.base), HealthPath: tt.healthPath})
			if !lb.isHealthy(context.Background(), lb.backends[0]) {
				t.Error("backend answering 200 reported unhealthy")
			}
			if got := <-paths; got != tt.want {
				t.Errorf("health check requested %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHealthPathMismatch is the problem the setting solves: a backend that
// only serves /healthz is marked down by checks on the default /health.
func TestHealthPathMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	wrong := newBalancer(t, Config{Backends: urls(srv.URL)})
	right := newBalancer(t, Config{Backends: urls(srv.URL), HealthPath: "/healthz"})
	if wrong.isHealthy(context.Background(), wrong.backends[0]) {
		t.Error("default /health: backend reported healthy, want unhealthy (it serves only /healthz)")
	}
	if !right.isHealthy(context.Background(), right.backends[0]) {
		t.Error("health_path /healthz: backend reported unhealthy, want healthy")
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
