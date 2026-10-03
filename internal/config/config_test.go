package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigFull(t *testing.T) {
	path := writeConfig(t, `
listen: ":9000"
algorithm: least-connections
backends:
  - http://a:1
  - http://b:2
health_check_interval: 2s
attempt_timeout: 1500ms
request_timeout: 1m
max_failures: 5
health_path: /healthz
max_body_size: 10MB
retry_unavailable: true
access_log: false
admin_listen: "127.0.0.1:9001"
`)
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen:              ":9000",
		Algorithm:           "least-connections",
		Backends:            []Backend{{URL: "http://a:1"}, {URL: "http://b:2"}},
		HealthCheckInterval: 2 * time.Second,
		AttemptTimeout:      1500 * time.Millisecond,
		RequestTimeout:      time.Minute,
		MaxFailures:         5,
		HealthPath:          "/healthz",
		MaxBodySize:         10 << 20,
		RetryUnavailable:    true,
		AccessLog:           new(false),
		AdminListen:         "127.0.0.1:9001",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	if hp := got.BalancerConfig().HealthPath; hp != "/healthz" {
		t.Errorf("balancer health path = %q, want %q", hp, "/healthz")
	}
	if got.AccessLogEnabled() {
		t.Error("access_log: false left the access log on")
	}
	if !got.BalancerConfig().RetryUnavailable {
		t.Error("retry_unavailable didn't reach the balancer config")
	}
	if size := got.BalancerConfig().MaxBodySize; size != 10<<20 {
		t.Errorf("balancer max body size = %d, want %d", size, 10<<20)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	got, err := Load(writeConfig(t, "backends: [http://a:1]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Listen != defaultListen || got.HealthCheckInterval != defaultHealthCheckInterval {
		t.Errorf("listen = %q, health_check_interval = %s; want %q, %s",
			got.Listen, got.HealthCheckInterval, defaultListen, defaultHealthCheckInterval)
	}
	if got.AdminListen != "" {
		t.Errorf("admin_listen = %q, want empty (admin server off)", got.AdminListen)
	}
	if !got.AccessLogEnabled() {
		t.Error("access log off by default, want on")
	}
	if got.EffectiveRequestTimeout() != 10*time.Second {
		t.Errorf("EffectiveRequestTimeout() = %s, want the balancer's default of 10s", got.EffectiveRequestTimeout())
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name, content, wantErr string
	}{
		{"unknown key", "backends: [http://a:1]\nalgoritm: round-robin\n", "algoritm"},
		{"bad duration", "backends: [http://a:1]\nrequest_timeout: 5 seconds\n", "line 2"},
		{"bare number duration", "backends: [http://a:1]\nrequest_timeout: 5\n", "line 2"},
		{"negative interval", "backends: [http://a:1]\nhealth_check_interval: -1s\n", "health_check_interval"},
		{"bad size", "backends: [http://a:1]\nmax_body_size: 10 megabytes\n", "line 2"},
		{"size as a list", "backends: [http://a:1]\nmax_body_size: [10MB]\n", "line 2"},
		{"empty file", "", "empty"},
		{"not yaml", "backends: [unclosed\n", "load config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want an error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	valid := map[string]int64{
		"0":            0,
		"1048576":      1 << 20,
		"512B":         512,
		"512KB":        512 << 10,
		"10MB":         10 << 20,
		"10mb":         10 << 20,
		"10 MB":        10 << 20,
		"2GB":          2 << 30,
		" 1KB ":        1 << 10,
		"8589934591GB": 8589934591 << 30, // the most GB an int64 can hold
	}
	for text, want := range valid {
		if got, err := parseByteSize(text); err != nil || got != want {
			t.Errorf("parseByteSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}

	for _, text := range []string{"", "MB", "-1MB", "1.5MB", "10TB", "ten", "10M", "1GBB", "8589934592GB"} {
		if got, err := parseByteSize(text); err == nil {
			t.Errorf("parseByteSize(%q) = %d, want an error", text, got)
		}
	}
}

func TestLoadConfigBackendForms(t *testing.T) {
	got, err := Load(writeConfig(t, `
backends:
  - http://a:1
  - url: http://b:2
    weight: 3
  - url: http://c:3
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Backend{
		{URL: "http://a:1"},
		{URL: "http://b:2", Weight: 3},
		{URL: "http://c:3"},
	}
	if !reflect.DeepEqual(got.Backends, want) {
		t.Errorf("backends = %+v, want %+v", got.Backends, want)
	}

	if w := got.BalancerConfig().Backends[1].Weight; w != 3 {
		t.Errorf("balancer weight = %d, want 3", w)
	}
}

func TestLoadConfigBackendErrors(t *testing.T) {
	tests := []struct {
		name, content, wantErr string
	}{
		{"typo in backend field", "backends:\n  - url: http://a:1\n    wieght: 3\n", `"wieght"`},
		{"weight not a number", "backends:\n  - url: http://a:1\n    weight: heavy\n", "line 3"},
		{"backend is a list", "backends:\n  - [http://a:1]\n", "must be a URL or an object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want an error mentioning %s", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if !os.IsNotExist(errors.Unwrap(err)) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}

// TestShippedConfig makes sure config.yaml at the repo root stays valid.
func TestShippedConfig(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Backends) == 0 {
		t.Error("config.yaml lists no backends")
	}
}
