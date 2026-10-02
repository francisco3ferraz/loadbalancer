package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

func main() {
	lb, err := balancer.New(balancer.Config{
		Backends: []string{"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082"},
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go lb.RunHealthChecks(ctx, 5*time.Second)

	srv := &http.Server{
		Addr:    ":8000",
		Handler: lb,
		// Slow clients can't hold connections open by sending headers slowly.
		ReadHeaderTimeout: 5 * time.Second,
		// Longer than the balancer's request timeout, so its 504 can be written.
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("listening on %s", srv.Addr)

	select {
	case err := <-errc:
		log.Fatal(err) // failed to start, e.g. port already in use
	case <-ctx.Done():
	}

	cancel() // a second Ctrl+C now kills the program immediately
	log.Print("shutting down")

	// ctx has already ended, so the drain needs a fresh context.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
