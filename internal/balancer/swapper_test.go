package balancer

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// sendTo makes one request through h and returns the status code and body.
func sendTo(h http.Handler) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Code, rec.Body.String()
}

func TestSwapper(t *testing.T) {
	oldLB := newBalancer(t, Config{Backends: urls(namedBackend(t, "old"))})
	newLB := newBalancer(t, Config{Backends: urls(namedBackend(t, "new"))})
	s := NewSwapper(oldLB)

	if _, body := sendTo(s); body != "old" {
		t.Fatalf("before swap: got %q, want %q", body, "old")
	}
	if prev := s.Swap(newLB); prev != oldLB {
		t.Errorf("Swap returned %p, want the old balancer %p", prev, oldLB)
	}
	if _, body := sendTo(s); body != "new" {
		t.Errorf("after swap: got %q, want %q", body, "new")
	}
	if s.Current() != newLB {
		t.Error("Current isn't the new balancer")
	}
}

func TestSwapperStatsFollowSwap(t *testing.T) {
	oldURL, newURL := namedBackend(t, "old"), namedBackend(t, "new")
	s := NewSwapper(newBalancer(t, Config{Backends: urls(oldURL)}))
	s.Swap(newBalancer(t, Config{Backends: urls(newURL)}))

	got := s.Stats().Backends
	if len(got) != 1 || got[0].URL != newURL {
		t.Errorf("got %+v, want only %s", got, newURL)
	}
}

func TestSwapperNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Swap(nil) didn't panic")
		}
	}()
	NewSwapper(nil)
}

// Requests sent while the balancer is swapped must all succeed. Run with
// -race to check the swap is safe.
func TestSwapperConcurrent(t *testing.T) {
	a := newBalancer(t, Config{Backends: urls(namedBackend(t, "a"))})
	b := newBalancer(t, Config{Backends: urls(namedBackend(t, "b"))})
	s := NewSwapper(a)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if code, body := sendTo(s); code != http.StatusOK || (body != "a" && body != "b") {
					t.Errorf("got %d %q, want 200 from a or b", code, body)
				}
			}
		})
	}
	for i := range 100 {
		if i%2 == 0 {
			s.Swap(b)
		} else {
			s.Swap(a)
		}
	}
	wg.Wait()
}
