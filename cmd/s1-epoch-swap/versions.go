package main

import (
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/nareknaltakyan1/dimp-research/internal/hotswap/svc"
)

// counterImpl is the shared shape of v1 and v2: the counter is held through a
// pointer whose ownership is handed from the old to the new version by the
// registry's ExportState/ImportState hook.
type counterImpl struct {
	version string
	count   *atomic.Uint64
}

func (c *counterImpl) ExportState() (any, error) {
	if c.count == nil {
		return nil, errors.New("no state")
	}
	return c.count, nil
}

func (c *counterImpl) ImportState(s any) error {
	p, ok := s.(*atomic.Uint64)
	if !ok || p == nil {
		return errors.New("state: expected *atomic.Uint64")
	}
	c.count = p
	return nil
}

// v1 is the original business logic.
type v1 struct{ counterImpl }

func (h *v1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	svc.WriteAPIResponse(w, "v1", h.count.Add(1))
}

// v2 is the "patched" business logic: observably different version string
// and an extra response header.
type v2 struct{ counterImpl }

func (h *v2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Patched", "true")
	svc.WriteAPIResponse(w, "v2", h.count.Add(1))
}

// build returns a fresh, state-less implementation for a version name. Both
// versions are compiled into the binary (this is S1's main limitation; see
// S2/S3 for runtime-loaded code).
func build(name string) (svc.Impl, error) {
	switch name {
	case "v1":
		return &v1{counterImpl{version: "v1"}}, nil
	case "v2":
		return &v2{counterImpl{version: "v2"}}, nil
	}
	return nil, errors.New("unknown version " + name)
}
