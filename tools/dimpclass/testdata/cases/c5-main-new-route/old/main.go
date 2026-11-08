package main

import (
	"log"
	"net/http"
)

func hello(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", hello)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
