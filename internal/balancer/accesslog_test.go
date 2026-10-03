package balancer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer collects log output. It's locked because the handler writes from
// the server's goroutine while the test reads.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var lines []map[string]any
	for line := range strings.Lines(b.buf.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		lines = append(lines, m)
	}
	return lines
}

func loggedBalancer(lb *Balancer) (http.Handler, *logBuffer) {
	logs := &logBuffer{}
	return AccessLog(lb, slog.New(slog.NewJSONHandler(logs, nil))), logs
}

func host(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// JSON numbers decode as float64, so want uses float64 for them.
func checkFields(t *testing.T, got map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (line %v)", k, got[k], v, got)
		}
	}
}

func TestAccessLog(t *testing.T) {
	good := namedBackend(t, "good")
	tests := []struct {
		name     string
		backends []string
		want     map[string]any
	}{
		{"served", []string{good}, map[string]any{
			"status": 200.0, "bytes": 4.0, "backend": host(t, good), "attempts": 1.0,
		}},
		// Round robin tries the dead backend first, then retries.
		{"retried", []string{deadBackend(t), good}, map[string]any{
			"status": 200.0, "backend": host(t, good), "attempts": 2.0,
		}},
		{"backend failed", []string{deadBackend(t)}, map[string]any{
			"status": 502.0, "attempts": 1.0,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, logs := loggedBalancer(newBalancer(t, Config{Backends: urls(tt.backends...)}))
			req := httptest.NewRequest(http.MethodGet, "/some/path?token=secret", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			h.ServeHTTP(httptest.NewRecorder(), req)

			lines := logs.lines(t)
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want 1", len(lines))
			}
			line := lines[0]
			checkFields(t, line, map[string]any{
				"msg": "request", "method": "GET", "path": "/some/path", "client": "192.0.2.1:1234",
			})
			checkFields(t, line, tt.want)
			if _, ok := line["duration"]; !ok {
				t.Error("no duration logged")
			}
			if strings.Contains(line["path"].(string), "secret") {
				t.Error("query string was logged")
			}
		})
	}
}

func TestAccessLogNoBackends(t *testing.T) {
	lb := newBalancer(t, Config{Backends: urls(namedBackend(t, "a"))})
	lb.backends[0].alive.Store(false)
	h, logs := loggedBalancer(lb)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	checkFields(t, logs.lines(t)[0], map[string]any{"status": 503.0, "backend": "", "attempts": 0.0})
}

// TestAccessLogUpgrade checks that a WebSocket-style upgrade still works
// through the access log, which needs statusRecorder's Unwrap to reach
// Hijack, and that it's logged as 101 once the connection closes.
func TestAccessLogUpgrade(t *testing.T) {
	h, logs := loggedBalancer(newBalancer(t, Config{Backends: urls(echoBackend(t))}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: test\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v, %v", resp, err)
	}
	io.WriteString(conn, "ping\n")
	if got, err := br.ReadString('\n'); err != nil || got != "ping\n" {
		t.Fatalf("echo: got %q, %v", got, err)
	}
	conn.Close()

	// The line is written when the tunnel closes, just after the client hangs up.
	waitFor(t, "access log line", func() bool { return len(logs.lines(t)) == 1 })
	checkFields(t, logs.lines(t)[0], map[string]any{"status": 101.0, "path": "/ws"})
}

// TestAccessLogStream checks that event streams still pass through the
// access log, which needs Unwrap to reach Flush and SetWriteDeadline.
func TestAccessLogStream(t *testing.T) {
	const timeout = 100 * time.Millisecond
	h, logs := loggedBalancer(newBalancer(t, Config{
		Backends:       urls(streamBackend(t, "text/event-stream", 4, timeout/2)),
		RequestTimeout: timeout,
	}))
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = timeout
	srv.Start()
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || strings.Count(string(body), "data:") != 4 {
		t.Fatalf("got %q, %v; want 4 events", body, err)
	}

	waitFor(t, "access log line", func() bool { return len(logs.lines(t)) == 1 })
	checkFields(t, logs.lines(t)[0], map[string]any{"status": 200.0, "bytes": float64(len(body))})
}
