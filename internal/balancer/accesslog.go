package balancer

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// accessKey is the context key under which AccessLog stores an *access
// that ServeHTTP and the proxy fill in as the request is handled.
type accessKey struct{}

// access is what AccessLog can't see from outside the balancer.
type access struct {
	backend  string // host of the last backend tried, empty if none was
	attempts int    // backends tried, more than 1 when retried
	upgraded bool   // the backend switched protocols, as for a WebSocket
}

// accessFrom returns the request's *access, or nil when the request isn't
// being logged.
func accessFrom(ctx context.Context) *access {
	a, _ := ctx.Value(accessKey{}).(*access)
	return a
}

// AccessLog wraps a Balancer so that every request it handles is logged to
// logger once it's finished: method, path, status, response size, duration,
// client address, and the backend that served it.
//
// A status of 0 means no response was sent, because the client gave up
// first. Upgraded connections are logged as 101 when they close, so their
// duration is how long the connection stayed open.
func AccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		a := &access{}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), accessKey{}, a)))

		status := rec.status
		if status == 0 && a.upgraded {
			// The proxy writes the 101 straight to the hijacked connection,
			// so the recorder never sees it.
			status = http.StatusSwitchingProtocols
		}
		// The query is left out: it can carry tokens and other secrets.
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("client", r.RemoteAddr),
			slog.String("backend", a.backend),
			slog.Int("attempts", a.attempts),
		)
	})
}

// statusRecorder remembers the status code and body size a handler sent.
type statusRecorder struct {
	http.ResponseWriter
	status int   // first final (2xx-5xx) status written, 0 if none yet
	bytes  int64 // body bytes written
}

func (r *statusRecorder) WriteHeader(code int) {
	// The proxy passes on 1xx informational responses, such as 103 Early
	// Hints, before the real one; only the final status is the outcome.
	if r.status == 0 && code >= 200 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap returns the underlying ResponseWriter. http.ResponseController uses
// it to reach Flush, Hijack and SetWriteDeadline, which streams and
// WebSockets need, and which statusRecorder doesn't implement itself.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
