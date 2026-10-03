package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a config file to a temporary directory and returns its
// path. Both servers listen on port 0, so the system picks free ports.
func writeConfig(t *testing.T, backendURL string) string {
	t.Helper()
	content := fmt.Sprintf(`
listen: "127.0.0.1:0"
admin_listen: "127.0.0.1:0"
health_check_interval: 50ms
backends:
  - %s
`, backendURL)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// program is a run call in progress.
type program struct {
	addr, adminAddr string
	stdout          *bytes.Buffer // read it only after done has delivered
	cancel          context.CancelFunc
	done            chan error // run's result
}

// start calls run in the background and waits until it's listening.
func start(t *testing.T, configPath string) *program {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &program{stdout: &bytes.Buffer{}, cancel: cancel, done: make(chan error, 1)}
	t.Cleanup(cancel)

	ready := make(chan struct{})
	go func() {
		p.done <- run(ctx, configPath, p.stdout, func(addr, adminAddr net.Addr) {
			p.addr, p.adminAddr = addr.String(), adminAddr.String()
			close(ready)
		})
	}()

	select {
	case <-ready:
	case err := <-p.done:
		t.Fatalf("run returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run didn't start listening")
	}
	return p
}

// stop cancels run's context, as Ctrl+C would, and returns what run returned.
func (p *program) stop(t *testing.T) error {
	t.Helper()
	p.cancel()
	select {
	case err := <-p.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("run didn't return after its context was cancelled")
		return nil
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// TestRun starts the whole program, sends a request through it, reads the
// admin server's stats, and shuts it down.
func TestRun(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello from the backend")
	}))
	defer backend.Close()

	p := start(t, writeConfig(t, backend.URL))

	if code, body := get(t, "http://"+p.addr+"/hi"); code != http.StatusOK || body != "hello from the backend" {
		t.Errorf("proxied request: got %d %q", code, body)
	}

	code, body := get(t, "http://"+p.adminAddr+"/stats")
	if code != http.StatusOK || !json.Valid([]byte(body)) || !strings.Contains(body, backend.Listener.Addr().String()) {
		t.Errorf("stats: got %d %q, want JSON listing the backend", code, body)
	}

	if err := p.stop(t); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
	if !strings.Contains(p.stdout.String(), "path=/hi status=200") {
		t.Errorf("access log = %q, want a line for the request", p.stdout.String())
	}
}

// TestRunDrainsOnShutdown checks the graceful shutdown: a request that's in
// progress when the context is cancelled still gets its response.
func TestRunDrainsOnShutdown(t *testing.T) {
	arrived := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "finished")
	}))
	defer backend.Close()

	p := start(t, writeConfig(t, backend.URL))

	// Not get: it calls t.Fatal, which only works on the test's goroutine.
	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + p.addr + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		got <- result{string(body), err}
	}()

	<-arrived // the request is now in progress
	if err := p.stop(t); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
	if r := <-got; r.err != nil || r.body != "finished" {
		t.Errorf("in-progress request: got %q, %v; want %q", r.body, r.err, "finished")
	}

	// And once run has returned, nothing is listening any more.
	if conn, err := net.Dial("tcp", p.addr); err == nil {
		conn.Close()
		t.Error("still accepting connections after run returned")
	}
}

// TestRunErrors checks that startup failures come back as errors, where
// main used to exit with log.Fatal.
func TestRunErrors(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	tests := []struct {
		name, content, wantErr string
	}{
		{"invalid config", "backends: []\n", "no backends"},
		{"port in use", fmt.Sprintf("listen: %q\nbackends: [http://127.0.0.1:1]\n", busy.Addr()), "listen"},
		{"admin port in use", fmt.Sprintf("listen: \"127.0.0.1:0\"\nadmin_listen: %q\nbackends: [http://127.0.0.1:1]\n", busy.Addr()), "admin listen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			err := run(context.Background(), path, io.Discard, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want an error mentioning %q", err, tt.wantErr)
			}
		})
	}

	if err := run(context.Background(), filepath.Join(t.TempDir(), "missing.yaml"), io.Discard, nil); err == nil {
		t.Error("missing config file: run returned nil")
	}
}
