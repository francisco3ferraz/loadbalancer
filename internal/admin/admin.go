// Package admin serves the load balancer's admin endpoints, such as /stats.
package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/pprof"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

// statsSource is what the admin endpoints need from the balancer.
type statsSource interface {
	Stats() balancer.Stats
}

// Handler returns the admin endpoints, reading stats from src. It exposes
// internal backend addresses and profiles, so serve it only where clients
// can't reach it, such as on a loopback address.
//
//	GET /stats         JSON snapshot of every backend (see balancer.Stats)
//	GET /debug/pprof/  runtime profiles for go tool pprof
func Handler(src statsSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Encode only fails when writing to the client fails. The response
		// has already started by then, so all that's left is to log it.
		if err := json.NewEncoder(w).Encode(src.Stats()); err != nil {
			log.Printf("admin: encode stats: %v", err)
		}
	})

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		
	})

	// Registered by hand: importing net/http/pprof for its side effect only
	// adds them to http.DefaultServeMux.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
}
