package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newServer() *server {
	s := &server{name: "test"}
	s.healthy.Store(true)
	return s
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestRootRepliesWithName(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := do(t, newServer().routes(), method, "/any/path")
		if rec.Code != http.StatusOK || rec.Body.String() != "test\n" {
			t.Errorf("%s: got %d %q, want 200 %q", method, rec.Code, rec.Body.String(), "test\n")
		}
		if got := rec.Header().Get("X-Backend"); got != "test" {
			t.Errorf("%s: X-Backend = %q, want %q", method, got, "test")
		}
	}
}

func TestErrorRate(t *testing.T) {
	s := newServer()
	s.errorRate = 1
	if rec := do(t, s.routes(), http.MethodGet, "/"); rec.Code != http.StatusInternalServerError {
		t.Errorf("error-rate 1: got %d, want 500", rec.Code)
	}
}

func TestHealthToggle(t *testing.T) {
	h := newServer().routes()

	steps := []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodPost, "/admin/health/down", http.StatusOK},
		{http.MethodGet, "/health", http.StatusServiceUnavailable},
		{http.MethodGet, "/", http.StatusOK}, // still serves traffic while unhealthy
		{http.MethodPost, "/admin/health/up", http.StatusOK},
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodPost, "/admin/health/sideways", http.StatusNotFound},
	}
	for _, s := range steps {
		if rec := do(t, h, s.method, s.target); rec.Code != s.want {
			t.Errorf("%s %s: got %d, want %d", s.method, s.target, rec.Code, s.want)
		}
	}
}

func TestStatus(t *testing.T) {
	h := newServer().routes()
	tests := map[string]int{
		"/status/200": http.StatusOK,
		"/status/502": http.StatusBadGateway,
		"/status/abc": http.StatusBadRequest,
		"/status/700": http.StatusBadRequest,
	}
	for target, want := range tests {
		if rec := do(t, h, http.MethodGet, target); rec.Code != want {
			t.Errorf("%s: got %d, want %d", target, rec.Code, want)
		}
	}
}

func TestSlow(t *testing.T) {
	h := newServer().routes()

	start := time.Now()
	rec := do(t, h, http.MethodGet, "/slow?d=50ms")
	if rec.Code != http.StatusOK || time.Since(start) < 50*time.Millisecond {
		t.Errorf("got %d after %s, want 200 after at least 50ms", rec.Code, time.Since(start))
	}
	if !strings.HasPrefix(rec.Body.String(), "test after 50ms") {
		t.Errorf("body = %q", rec.Body.String())
	}

	if rec := do(t, h, http.MethodGet, "/slow?d=banana"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad duration: got %d, want 400", rec.Code)
	}
}

func TestStream(t *testing.T) {
	// A real server, not a recorder, so the events really go over the network.
	srv := httptest.NewServer(newServer().routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/stream?every=10ms&n=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := "data: test 1\n\ndata: test 2\n\ndata: test 3\n\n"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	if rec := do(t, newServer().routes(), http.MethodGet, "/stream?every=0s"); rec.Code != http.StatusBadRequest {
		t.Errorf("zero interval: got %d, want 400", rec.Code)
	}
}

func TestUpgradeEchoes(t *testing.T) {
	srv := httptest.NewServer(newServer().routes())
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: test\r\nConnection: keep-alive, Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("got %d, want 101", resp.StatusCode)
	}

	for _, msg := range []string{"hello\n", "again\n"} {
		io.WriteString(conn, msg)
		got, err := br.ReadString('\n')
		if err != nil || got != msg {
			t.Fatalf("echo: got %q, %v; want %q", got, err, msg)
		}
	}
}

func TestUpgradeRequiresHeaders(t *testing.T) {
	if rec := do(t, newServer().routes(), http.MethodGet, "/ws"); rec.Code != http.StatusUpgradeRequired {
		t.Errorf("plain GET: got %d, want 426", rec.Code)
	}
}
