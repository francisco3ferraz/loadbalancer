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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// writeConfig writes a config file to a temporary directory and returns its
// path. Both servers listen on port 0, so the system picks free ports.
func writeConfig(t *testing.T, backendURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, configFor(backendURL))
	return path
}

func configFor(backendURL string) string {
	return fmt.Sprintf(`
listen: "127.0.0.1:0"
admin_listen: "127.0.0.1:0"
health_check_interval: 50ms
backends:
  - %s
`, backendURL)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// program is a run call in progress.
type program struct {
	addr, adminAddr string
	stdout          *bytes.Buffer // read it only after done has delivered
	cancel          context.CancelFunc
	// Unbuffered, so a send returns only once any earlier reload is done.
	reload chan os.Signal
	done   chan error // run's result
}

// start calls run in the background and waits until it's listening.
func start(t *testing.T, configPath string) *program {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &program{stdout: &bytes.Buffer{}, cancel: cancel, reload: make(chan os.Signal), done: make(chan error, 1)}
	t.Cleanup(cancel)

	ready := make(chan struct{})
	go func() {
		p.done <- run(ctx, configPath, p.stdout, p.reload, func(addr, adminAddr net.Addr) {
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

// sendReload sends two reloads: the second is only taken once the first
// has finished, so the caller knows it's done.
func (p *program) sendReload(t *testing.T) {
	t.Helper()
	for range 2 {
		select {
		case p.reload <- syscall.SIGHUP:
		case <-time.After(5 * time.Second):
			t.Fatal("run didn't take the reload")
		}
	}
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

// TestRunDrainsOnShutdown checks that a request in progress when the
// context is cancelled still gets its response.
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

	if conn, err := net.Dial("tcp", p.addr); err == nil {
		conn.Close()
		t.Error("still accepting connections after run returned")
	}
}

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
			err := run(context.Background(), path, io.Discard, nil, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want an error mentioning %q", err, tt.wantErr)
			}
		})
	}

	if err := run(context.Background(), filepath.Join(t.TempDir(), "missing.yaml"), io.Discard, nil, nil); err == nil {
		t.Error("missing config file: run returned nil")
	}
}

// slowNamedBackend answers with its name after a delay, so requests are in
// progress during a reload.
func slowNamedBackend(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, name)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunReload(t *testing.T) {
	path := writeConfig(t, slowNamedBackend(t, "old"))
	p := start(t, path)

	// Under load, a transport sometimes dials a connection it never uses.
	// Shutdown waits up to 5s for such new connections, so they're closed
	// before stopping.
	transport := &http.Transport{}
	client := &http.Client{Transport: transport}
	var (
		failures atomic.Int64
		sawNew   atomic.Bool
		done     = make(chan struct{})
		clients  sync.WaitGroup
	)
	for range 8 {
		clients.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				resp, err := client.Get("http://" + p.addr + "/")
				if err != nil {
					t.Errorf("request failed: %v", err)
					failures.Add(1)
					continue
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				switch {
				case err != nil || resp.StatusCode != http.StatusOK:
					t.Errorf("got %d %q, %v", resp.StatusCode, body, err)
					failures.Add(1)
				case string(body) == "new":
					sawNew.Store(true)
				case string(body) != "old":
					t.Errorf("got body %q, want old or new", body)
				}
			}
		})
	}

	time.Sleep(50 * time.Millisecond) // let traffic build up on the old backend
	writeFile(t, path, configFor(slowNamedBackend(t, "new")))
	p.sendReload(t)

	if code, body := get(t, "http://"+p.addr+"/"); code != http.StatusOK || body != "new" {
		t.Errorf("after reload: got %d %q, want 200 %q", code, body, "new")
	}
	time.Sleep(50 * time.Millisecond)
	close(done)
	clients.Wait()
	transport.CloseIdleConnections()

	if n := failures.Load(); n > 0 {
		t.Errorf("%d requests failed during the reload", n)
	}
	if !sawNew.Load() {
		t.Error("no client request reached the new backend")
	}

	if _, body := get(t, "http://"+p.adminAddr+"/stats"); strings.Count(body, `"url"`) != 1 {
		t.Errorf("stats = %s, want only the new backend", body)
	}
	if err := p.stop(t); err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
}

func TestRunReloadRejected(t *testing.T) {
	tests := map[string]string{
		"invalid yaml":       "backends: [\n",
		"no backends":        "listen: \"127.0.0.1:0\"\nadmin_listen: \"127.0.0.1:0\"\nbackends: []\n",
		"listen changed":     "listen: \"127.0.0.1:1\"\nadmin_listen: \"127.0.0.1:0\"\nbackends: [http://127.0.0.1:1]\n",
		"admin changed":      "listen: \"127.0.0.1:0\"\nbackends: [http://127.0.0.1:1]\n",
		"access log changed": configFor("http://127.0.0.1:1") + "access_log: false\n",
		"longer timeout":     configFor("http://127.0.0.1:1") + "request_timeout: 1m\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := writeConfig(t, slowNamedBackend(t, "old"))
			p := start(t, path)

			writeFile(t, path, content)
			p.sendReload(t)

			if code, body := get(t, "http://"+p.addr+"/"); code != http.StatusOK || body != "old" {
				t.Errorf("after rejected reload: got %d %q, want 200 %q", code, body, "old")
			}
			if err := p.stop(t); err != nil {
				t.Errorf("run returned %v, want nil", err)
			}
		})
	}
}
