package main

import (
	"log"
	"net/http"
)

func hello(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }

func version(w http.ResponseWriter, r *http.Request) { w.Write([]byte("v2")) }

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", hello)
	mux.HandleFunc("/version", version)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
