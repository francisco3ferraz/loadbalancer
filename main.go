package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

func main() {
	backends := []string{"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082"}

	parsed := make([]*url.URL, len(backends))
	for i, v := range backends {
		u, err := url.Parse(v)
		if err != nil {
			log.Fatal(err)
		}
		parsed[i] = u
	}

	proxies := make([]*httputil.ReverseProxy, len(parsed))
	for i, u := range parsed {
		proxies[i] = httputil.NewSingleHostReverseProxy(u)
	}

	var c atomic.Uint64
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := c.Add(1) - 1
		proxies[n%uint64(len(proxies))].ServeHTTP(w, r)
	})

	log.Fatal(http.ListenAndServe(":8000", nil))
}
