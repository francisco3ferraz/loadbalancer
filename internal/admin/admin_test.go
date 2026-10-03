package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

// fakeStats serves fixed stats, so the handler can be tested without a balancer.
type fakeStats balancer.Stats

func (f fakeStats) Stats() balancer.Stats { return balancer.Stats(f) }

// Every field is non-zero somewhere, so a field the handler failed to encode
// would show up as a difference instead of decoding to a matching zero.
var testStats = fakeStats{Backends: []balancer.BackendStats{
	{URL: "http://a:1", Alive: true, Active: 2, Failures: 0, Requests: 150, TotalFailures: 4},
	{URL: "http://b:2", Alive: false, Active: 0, Failures: 3, Requests: 7, TotalFailures: 1000000},
}}

func TestStats(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(testStats).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}

	var got balancer.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, rec.Body)
	}
	if want := balancer.Stats(testStats); !reflect.DeepEqual(got, want) {
		t.Errorf("decoded stats:\n got  %+v\n want %+v", got, want)
	}
}

// TestStatsJSONKeys pins the key names, which are the endpoint's public format.
// Decoding into balancer.Stats alone wouldn't notice a renamed key, because
// the same tags are used to encode and decode.
func TestStatsJSONKeys(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(testStats).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))

	var raw struct {
		Backends []map[string]any `json:"backends"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Backends) == 0 {
		t.Fatalf("no backends in %s", rec.Body)
	}
	for _, key := range []string{"url", "alive", "active", "failures", "requests", "total_failures"} {
		if _, ok := raw.Backends[0][key]; !ok {
			t.Errorf("key %q missing from %v", key, raw.Backends[0])
		}
	}
}

func TestStatsRejectsOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(testStats).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stats", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestPprof(t *testing.T) {
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		rec := httptest.NewRecorder()
		Handler(testStats).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
}
