package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

type backend struct {
	url   *url.URL
	proxy *httputil.ReverseProxy
	alive atomic.Bool
}

func main() {
	addrs := []string{"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082"}

	var backends []*backend
	for _, addr := range addrs {
		u, err := url.Parse(addr)
		if err != nil {
			log.Fatal(err)
		}

		b := &backend{url: u, proxy: httputil.NewSingleHostReverseProxy(u)}
		b.alive.Store(true)

		backends = append(backends, b)
	}

	var next atomic.Uint64
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		i := next.Add(1) - 1
		backends[i%uint64(len(backends))].proxy.ServeHTTP(w, r)
	})

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	go runHealthChecks(backends, client, 5*time.Second)
	log.Fatal(http.ListenAndServe(":8000", nil))
}

func runHealthChecks(backends []*backend, client *http.Client, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		for _, b := range backends {
			alive := isHealthy(b, client)
			if alive != b.alive.Load() {
				if alive {
					log.Printf("%s is up", b.url.Host)
				} else {
					log.Printf("%s is down", b.url.Host)
				}
			}
			b.alive.Store(alive)
		}
	}
}

func isHealthy(b *backend, client *http.Client) bool {
	resp, err := client.Get(b.url.JoinPath("health").String())
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}
