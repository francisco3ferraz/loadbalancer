package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
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

	// The heap stays small (tens of MB under load), so trading memory for
	// fewer collections is cheap: 400 gave 14% more requests/sec than the
	// default 100 in benchmarks. Setting GOGC still overrides it.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A second Ctrl+C kills the program instead of waiting for the drain.
	context.AfterFunc(ctx, stop)

	// Unhandled, SIGHUP kills the process. signal.Notify drops signals it
	// can't deliver at once, hence the buffer.
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)

	if err := run(ctx, *configPath, os.Stdout, reload, nil); err != nil {
		log.Fatal(err)
	}
}

// run serves the load balancer, and the admin server if configured, until
// ctx is cancelled, then shuts both down gracefully.
//
// Each value received on reload rereads the config and swaps in a new
// balancer. A config that can't be applied is logged and ignored. reload
// may be nil.
//
// ready, if not nil, gets the addresses actually listened on, so a test
// configured with port 0 can find its ports. adminAddr is nil when there's
// no admin server.
func run(ctx context.Context, configPath string, stdout io.Writer, reload <-chan os.Signal, ready func(addr, adminAddr net.Addr)) error {
	gen, err := build(configPath)
	if err != nil {
		return err
	}
	cfg, lb := gen.cfg, gen.lb

	// Room for a request that uses its whole timeout to still be answered.
	drainTimeout := cfg.EffectiveRequestTimeout() + 5*time.Second

	swapper := balancer.NewSwapper(lb)

	// Access lines go to stdout and everything else to stderr, like nginx's
	// access.log and error.log.
	var handler http.Handler = swapper
	if cfg.AccessLogEnabled() {
		handler = balancer.AccessLog(swapper, slog.New(slog.NewTextHandler(stdout, nil)))
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      drainTimeout,
		IdleTimeout:       60 * time.Second,
	}
	// Through GetCertificate rather than files given to ServeTLS, so a
	// reload can replace the certificate. ServeTLS still sets up HTTP/2.
	var certs *certStore
	if gen.cert != nil {
		certs = &certStore{}
		certs.current.Store(gen.cert)
		srv.TLSConfig = &tls.Config{GetCertificate: certs.getCertificate}
	}
	// Not ListenAndServe: a port in use is reported here, and the real
	// address is known even for port 0.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	var adminSrv *http.Server
	var adminLn net.Listener
	if cfg.AdminListen != "" {
		adminSrv = &http.Server{
			Handler:           admin.Handler(swapper),
			ReadHeaderTimeout: 5 * time.Second,
		}
		if adminLn, err = net.Listen("tcp", cfg.AdminListen); err != nil {
			ln.Close()
			return fmt.Errorf("admin listen: %w", err)
		}
	}

	stopHealth := startHealthChecks(ctx, lb, cfg.HealthCheckInterval)
	defer func() { stopHealth() }() // the stop current at return, not this one

	// One slot per server, so neither blocks if nobody reads.
	errc := make(chan error, 2)
	scheme := "http"
	if certs != nil {
		scheme = "https"
		go func() { errc <- srv.ServeTLS(ln, "", "") }()
	} else {
		go func() { errc <- srv.Serve(ln) }()
	}
	log.Printf("listening on %s://%s with %d backends", scheme, ln.Addr(), len(cfg.Backends))
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
serving:
	for {
		select {
		case serveErr = <-errc:
			break serving
		case <-ctx.Done():
			break serving
		case <-reload:
			// Against the startup config, which the servers were built from.
			next, err := loadReload(configPath, cfg)
			if err != nil {
				log.Printf("reload: %v; keeping the current config", err)
				continue
			}
			// New checks start before the old stop, so there's no gap.
			stopNext := startHealthChecks(ctx, next.lb, next.cfg.HealthCheckInterval)
			swapper.Swap(next.lb)
			if certs != nil {
				certs.current.Store(next.cert)
			}
			stopHealth()
			stopHealth = stopNext
			log.Printf("reloaded %s: %d backends", configPath, len(next.cfg.Backends))
		}
	}
	log.Print("shutting down")

	// ctx may have ended already, so the drain needs a fresh context.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), drainTimeout)
	defer cancelShutdown()
	// The admin server stays up during the drain, so /stats shows the last
	// requests finishing.
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

// startHealthChecks runs lb's health checks in the background. The returned
// stop ends them and waits until they have.
func startHealthChecks(ctx context.Context, lb *balancer.Balancer, interval time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { lb.RunHealthChecks(ctx, interval) })
	return func() {
		cancel()
		wg.Wait()
	}
}

// generation is what one reading of the config file builds.
type generation struct {
	cfg  config.Config
	lb   *balancer.Balancer
	cert *tls.Certificate // nil without TLS
}

// build reads the config at path and builds everything it describes, so a
// bad file, balancer setting or certificate is found before any of it is
// used.
func build(path string) (generation, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return generation{}, err
	}
	lb, err := balancer.New(cfg.BalancerConfig())
	if err != nil {
		return generation{}, fmt.Errorf("config %s: %w", path, err)
	}
	gen := generation{cfg: cfg, lb: lb}
	if cfg.TLSEnabled() {
		if gen.cert, err = loadCert(cfg.TLSCert, cfg.TLSKey); err != nil {
			return generation{}, err
		}
	}
	return gen, nil
}

// loadReload rereads the config at path and builds a new generation from
// it, provided it changes nothing that needs a restart.
func loadReload(path string, startup config.Config) (generation, error) {
	gen, err := build(path)
	if err != nil {
		return generation{}, err
	}
	if err := checkReloadable(startup, gen.cfg); err != nil {
		return generation{}, err
	}
	return gen, nil
}

// checkReloadable returns an error if next changes a setting that only a
// restart can apply. The whole reload is rejected, rather than the rest
// applied, so the running config always matches the file.
func checkReloadable(startup, next config.Config) error {
	switch {
	case next.Listen != startup.Listen:
		return errors.New("listen can't change without a restart")
	case next.AdminListen != startup.AdminListen:
		return errors.New("admin_listen can't change without a restart")
	case next.AccessLogEnabled() != startup.AccessLogEnabled():
		return errors.New("access_log can't change without a restart")
	// The certificate can change, but not whether the port speaks HTTP or
	// HTTPS: that's decided once, when serving starts.
	case next.TLSEnabled() != startup.TLSEnabled():
		return errors.New("tls_cert and tls_key can't be added or removed without a restart")
	// The write timeout was sized from the startup value; a longer request
	// timeout would have its 504s cut off.
	case next.EffectiveRequestTimeout() > startup.EffectiveRequestTimeout():
		return errors.New("request_timeout can't increase without a restart")
	}
	return nil
}
