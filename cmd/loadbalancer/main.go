package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/francisco3ferraz/loadbalancer/internal/admin"
	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
	"github.com/francisco3ferraz/loadbalancer/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	lb, err := balancer.New(cfg.BalancerConfig())
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go lb.RunHealthChecks(ctx, cfg.HealthCheckInterval)

	// Requests in progress may take up to the request timeout, so the server's
	// write timeout and the shutdown drain both allow a little more than that.
	drainTimeout := cfg.EffectiveRequestTimeout() + 5*time.Second

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: lb,
		// Slow clients can't hold connections open by sending headers slowly.
		ReadHeaderTimeout: 5 * time.Second,
		// Longer than the balancer's request timeout, so its 504 can be written.
		WriteTimeout: drainTimeout,
		IdleTimeout:  60 * time.Second,
	}

	// The admin server is optional: nil means it's disabled.
	var adminSrv *http.Server
	if cfg.AdminListen != "" {
		adminSrv = &http.Server{
			Addr:              cfg.AdminListen,
			Handler:           admin.Handler(lb),
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	// One slot per server, so neither goroutine blocks if nobody reads.
	errc := make(chan error, 2)
	serve(srv, errc)
	log.Printf("listening on %s with %d backends", srv.Addr, len(cfg.Backends))
	if adminSrv != nil {
		serve(adminSrv, errc)
		log.Printf("admin on %s", adminSrv.Addr)
	}

	select {
	case err := <-errc:
		log.Fatal(err) // failed to start, e.g. port already in use
	case <-ctx.Done():
	}

	cancel() // a second Ctrl+C now kills the program immediately
	log.Print("shutting down")

	// ctx has already ended, so the drain needs a fresh context.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelShutdown()
	// Drain client requests first. The admin server stays up meanwhile, so
	// /stats shows the last requests finishing.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("admin shutdown: %v", err)
		}
	}
}

// serve runs srv in the background and reports why it stopped on errc.
func serve(srv *http.Server, errc chan<- error) {
	go func() { errc <- srv.ListenAndServe() }()
}
