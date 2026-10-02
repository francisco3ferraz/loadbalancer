package balancer

import (
	"context"
	"log"
	"net/http"
	"time"
)

// RunHealthChecks checks every backend's /health endpoint each interval and
// marks it up or down. It blocks until ctx is cancelled, so run it with go.
func (lb *Balancer) RunHealthChecks(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		for _, b := range lb.backends {
			alive := lb.isHealthy(ctx, b)
			if alive != b.alive.Load() {
				if alive {
					b.failures.Store(0)
					log.Printf("%s is up", b.url.Host)
				} else {
					log.Printf("%s is down", b.url.Host)
				}
			}
			b.alive.Store(alive)
		}
	}
}

func (lb *Balancer) isHealthy(ctx context.Context, b *backend) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url.JoinPath("health").String(), nil)
	if err != nil {
		return false
	}
	resp, err := lb.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}
