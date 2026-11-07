// Package main is the v2 plugin for S2 (patched business logic (adds X-Patched header)).
// Build with: go build -buildmode=plugin -o v2.so ./cmd/s2-plugin/plugins/v2
//
// The plugin deliberately depends on the standard library only: any non-std
// package shared with the host (or with another plugin) must be byte-for-byte
// the same build, otherwise plugin.Open fails with "plugin was built with a
// different version of package". The host talks to it through method sets
// (ServeHTTP, ExportState, ImportState) and the exported symbols New/Version.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// Version is the logical version string; override with
// -ldflags "-X main.Version=..." when building generations.
var Version = "v2"

type handler struct {
	count *atomic.Uint64
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.count.Add(1)
	w.Header().Set("X-Patched", "true")
	w.Header().Set("X-Version", Version)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"version\":%q,\"pid\":%d,\"count\":%d,\"ts_ns\":%d}\n", Version, os.Getpid(), n, time.Now().UnixNano())
}

// ExportState hands ownership of the shared counter to the successor.
func (h *handler) ExportState() (any, error) {
	if h.count == nil {
		return nil, errors.New("v2: no state")
	}
	return h.count, nil
}

// ImportState adopts the predecessor's (or the host's) counter.
func (h *handler) ImportState(s any) error {
	c, ok := s.(*atomic.Uint64)
	if !ok || c == nil {
		return fmt.Errorf("v2: unexpected state type %T", s)
	}
	h.count = c
	return nil
}

// New is the factory symbol looked up by the host.
func New() any { return &handler{} }

func main() {} // unused in plugin mode; keeps "go build ./..." happy
