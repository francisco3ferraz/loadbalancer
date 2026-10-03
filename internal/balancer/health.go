package balancer

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

// RunHealthChecks checks every backend's health path each interval and
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

		lb.checkAll(ctx)
	}
}

// checkAll checks every backend once, all at the same time, and marks each
// up or down. It returns when every check has finished, so a round takes as
// long as the slowest check, and the next round can't start before then.
func (lb *Balancer) checkAll(ctx context.Context) {
	var wg sync.WaitGroup

	for _, b := range lb.backends {
		check := func() {
			alive := lb.isHealthy(ctx, b)

			// Swap, not Load then Store: a failing request may mark the
			// backend down in between, and the log would then be wrong.
			if b.alive.Swap(alive) != alive {
				if alive {
					b.failures.Store(0)
					log.Printf("%s is up", b.url.Host)
				} else {
					log.Printf("%s is down", b.url.Host)
				}
			}
		}

		wg.Go(check)
	}

	wg.Wait()
}

func (lb *Balancer) isHealthy(ctx context.Context, b *backend) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url.JoinPath(lb.healthPath).String(), nil)
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
