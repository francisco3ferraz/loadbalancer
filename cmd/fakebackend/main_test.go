package main

import (
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
