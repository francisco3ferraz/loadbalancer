package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

// TestMetrics pins the whole output, which is what Prometheus parses.
func TestMetrics(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(testStats).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got, want := rec.Header().Get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	want := `# HELP loadbalancer_backend_up Whether the backend is marked alive (1) or down (0).
# TYPE loadbalancer_backend_up gauge
loadbalancer_backend_up{backend="http://a:1"} 1
loadbalancer_backend_up{backend="http://b:2"} 0
# HELP loadbalancer_backend_active_requests Requests in progress on the backend.
# TYPE loadbalancer_backend_active_requests gauge
loadbalancer_backend_active_requests{backend="http://a:1"} 2
loadbalancer_backend_active_requests{backend="http://b:2"} 0
# HELP loadbalancer_backend_requests_total Attempts sent to the backend.
# TYPE loadbalancer_backend_requests_total counter
loadbalancer_backend_requests_total{backend="http://a:1"} 150
loadbalancer_backend_requests_total{backend="http://b:2"} 7
# HELP loadbalancer_backend_consecutive_failures Failures in a row; reset by a success.
# TYPE loadbalancer_backend_consecutive_failures gauge
loadbalancer_backend_consecutive_failures{backend="http://a:1"} 0
loadbalancer_backend_consecutive_failures{backend="http://b:2"} 3
# HELP loadbalancer_backend_failures_total Failed attempts the backend was blamed for.
# TYPE loadbalancer_backend_failures_total counter
loadbalancer_backend_failures_total{backend="http://a:1"} 4
loadbalancer_backend_failures_total{backend="http://b:2"} 1000000
`
	if got := rec.Body.String(); got != want {
		t.Errorf("body:\n%s\nwant:\n%s", got, want)
	}
}

func TestMetricsEscapesLabels(t *testing.T) {
	var buf bytes.Buffer
	writeMetrics(&buf, balancer.Stats{Backends: []balancer.BackendStats{{URL: "a\\b\"c\nd"}}})

	want := `loadbalancer_backend_up{backend="a\\b\"c\nd"} 0` + "\n"
	if !bytes.Contains(buf.Bytes(), []byte(want)) {
		t.Errorf("output doesn't contain %q:\n%s", want, buf.String())
	}
}

func TestMetricsRejectsOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(testStats).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
