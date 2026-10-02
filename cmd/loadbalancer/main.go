package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/francisco3ferraz/loadbalancer/internal/balancer"
)

func main() {
	lb, err := balancer.New([]string{"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082"})
	if err != nil {
		log.Fatal(err)
	}

	go lb.RunHealthChecks(context.Background(), 5*time.Second)
	log.Fatal(http.ListenAndServe(":8000", lb))
}
