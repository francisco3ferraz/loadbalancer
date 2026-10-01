package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	if len(os.Args) <= 1 {
		log.Fatal("port argument is missing")
	}

	port := fmt.Sprintf(":%s", os.Args[1])

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s from %s", port, r.Method, r.URL.Path, r.RemoteAddr)
		w.Write([]byte(port))
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s from %s", port, r.Method, r.URL.Path, r.RemoteAddr)
		w.Write([]byte("ok"))
	})

	log.Fatal(http.ListenAndServe(port, nil))
}
