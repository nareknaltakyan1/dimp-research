package main

import (
	"log"
	"net/http"
	"time"
)

func hello(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }

// routes wires handlers; it runs once, from main.
func routes(mux *http.ServeMux) {
	mux.HandleFunc("/hello", hello)

}

func main() {
	mux := http.NewServeMux()
	routes(mux)
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
