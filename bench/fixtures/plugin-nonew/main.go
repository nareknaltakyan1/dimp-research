// Package main is a benchmark fixture for E6 (failed upgrade) of s2-plugin: a
// well-formed Go plugin that lacks the required "New" symbol, so
// pluginimpl.Open fails after dlopen and before publication.
// Build: go build -buildmode=plugin -o v2-broken.so ./bench/fixtures/plugin-nonew
package main

// Version is the logical version this (broken) plugin would report.
var Version = "v2-broken"

// main is never run (plugins ignore it); it only lets `go build ./...` succeed.
func main() {}
