package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func main() {
	backends := []string{"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082"}

	u, err := url.Parse(backends[0])
	if err != nil {
		log.Fatal(err)
	}

	p := httputil.NewSingleHostReverseProxy(u)

	log.Fatal(http.ListenAndServe(":8000", p))
}
