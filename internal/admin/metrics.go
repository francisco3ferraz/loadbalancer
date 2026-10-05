package admin

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

// metric is one per-backend series on /metrics.
type metric struct {
	name, help, typ string
	value           func(balancer.BackendStats) float64
}

var metrics = []metric{
	{"loadbalancer_backend_up", "Whether the backend is marked alive (1) or down (0).", "gauge",
		func(s balancer.BackendStats) float64 {
			if s.Alive {
				return 1
			}
			return 0
		}},
	{"loadbalancer_backend_active_requests", "Requests in progress on the backend.", "gauge",
		func(s balancer.BackendStats) float64 { return float64(s.Active) }},
	{"loadbalancer_backend_requests_total", "Attempts sent to the backend.", "counter",
		func(s balancer.BackendStats) float64 { return float64(s.Requests) }},
	{"loadbalancer_backend_consecutive_failures", "Failures in a row; reset by a success.", "gauge",
		func(s balancer.BackendStats) float64 { return float64(s.Failures) }},
	{"loadbalancer_backend_failures_total", "Failed attempts the backend was blamed for.", "counter",
		func(s balancer.BackendStats) float64 { return float64(s.TotalFailures) }},
}

// labelEscaper escapes a label value as the text format requires.
// strconv.Quote won't do: it writes escapes like \x00 and é, which
// Prometheus doesn't decode.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

const latencyName = "loadbalancer_backend_latency_seconds"

// writeMetrics writes stats in Prometheus' text format. Each metric's
// samples must be together, so metrics are the outer loop.
func writeMetrics(w io.Writer, stats balancer.Stats) {
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
		for _, b := range stats.Backends {
			fmt.Fprintf(w, "%s{backend=\"%s\"} %s\n", m.name, labelEscaper.Replace(b.URL), formatFloat(m.value(b)))
		}
	}

	// A histogram is one metric written as several series: a cumulative
	// _bucket per upper bound (le), ending with +Inf, then _sum and _count.
	fmt.Fprintf(w, "# HELP %s Time from sending a request to the backend until its response headers arrived.\n", latencyName)
	fmt.Fprintf(w, "# TYPE %s histogram\n", latencyName)
	for _, b := range stats.Backends {
		label := labelEscaper.Replace(b.URL)
		for i, bound := range balancer.LatencyBuckets {
			fmt.Fprintf(w, "%s_bucket{backend=\"%s\",le=\"%s\"} %d\n", latencyName, label, seconds(bound), b.Latency.Counts[i])
		}
		fmt.Fprintf(w, "%s_bucket{backend=\"%s\",le=\"+Inf\"} %d\n", latencyName, label, b.Latency.Count)
		fmt.Fprintf(w, "%s_sum{backend=\"%s\"} %s\n", latencyName, label, seconds(b.Latency.Sum))
		fmt.Fprintf(w, "%s_count{backend=\"%s\"} %d\n", latencyName, label, b.Latency.Count)
	}
}

// formatFloat formats a sample value. 'f' rather than %g, which writes
// 1e+06 for a million.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// seconds formats d in seconds, Prometheus' unit for time.
func seconds(d time.Duration) string {
	return formatFloat(d.Seconds())
}
