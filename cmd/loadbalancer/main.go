package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/francisco3ferraz/loadbalancer/internal/admin"
	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
	"github.com/francisco3ferraz/loadbalancer/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once the first Ctrl+C has started the shutdown, stop catching signals,
	// so a second one kills the program instead of waiting for the drain.
	context.AfterFunc(ctx, stop)

	if err := run(ctx, *configPath, os.Stdout, nil); err != nil {
		log.Fatal(err)
	}
}

// run starts the load balancer, and the admin server if configured, and
// serves until ctx is cancelled. Then it shuts both down gracefully and
// returns. It returns an error if the config is invalid, a port can't be
// listened on, or a server fails.
//
// The access log goes to stdout. When ready isn't nil, it's called with the
// addresses actually listened on, which is how a test configured with port
// 0 finds out which ports it got. adminAddr is nil when there's no admin
// server.
func run(ctx context.Context, configPath string, stdout io.Writer, ready func(addr, adminAddr net.Addr)) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	lb, err := balancer.New(cfg.BalancerConfig())
	if err != nil {
		return fmt.Errorf("config %s: %w", configPath, err)
	}

	// Requests in progress may take up to the request timeout, so the server's
	// write timeout and the shutdown drain both allow a little more than that.
	drainTimeout := cfg.EffectiveRequestTimeout() + 5*time.Second

	// Access lines go to stdout and everything else to stderr, so the two
	// can be sent to different places, like nginx's access.log and error.log.
	var handler http.Handler = lb
	if cfg.AccessLogEnabled() {
		handler = balancer.AccessLog(lb, slog.New(slog.NewTextHandler(stdout, nil)))
	}

	srv := &http.Server{
		Handler: handler,
		// Slow clients can't hold connections open by sending headers slowly.
		ReadHeaderTimeout: 5 * time.Second,
		// Longer than the balancer's request timeout, so its 504 can be written.
		WriteTimeout: drainTimeout,
		IdleTimeout:  60 * time.Second,
	}
	// Listening before serving, rather than ListenAndServe, means a port
	// that's in use is reported here, and the real address is known even
	// when the config asks for port 0.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	// The admin server is optional: nil means it's disabled.
	var adminSrv *http.Server
	var adminLn net.Listener
	if cfg.AdminListen != "" {
		adminSrv = &http.Server{
			Handler:           admin.Handler(lb),
			ReadHeaderTimeout: 5 * time.Second,
		}
		if adminLn, err = net.Listen("tcp", cfg.AdminListen); err != nil {
			ln.Close()
			return fmt.Errorf("admin listen: %w", err)
		}
	}

	// Health checks get their own context, so they also stop when run
	// returns because a server failed rather than because ctx ended.
	healthCtx, stopHealth := context.WithCancel(ctx)
	var health sync.WaitGroup
	health.Go(func() { lb.RunHealthChecks(healthCtx, cfg.HealthCheckInterval) })
	defer func() {
		stopHealth()
		health.Wait() // so nothing is left running once run returns
	}()

	// One slot per server, so neither goroutine blocks if nobody reads.
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()
	log.Printf("listening on %s with %d backends", ln.Addr(), len(cfg.Backends))
	var adminAddr net.Addr
	if adminSrv != nil {
		go func() { errc <- adminSrv.Serve(adminLn) }()
		adminAddr = adminLn.Addr()
		log.Printf("admin on %s", adminAddr)
	}
	if ready != nil {
		ready(ln.Addr(), adminAddr)
	}

	var serveErr error
	select {
	case serveErr = <-errc:
	case <-ctx.Done():
	}
	log.Print("shutting down")

	// ctx may have ended already, so the drain needs a fresh context.
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

	if serveErr != nil {
		return fmt.Errorf("serve: %w", serveErr)
	}
	return nil
}
