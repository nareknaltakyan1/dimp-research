//go:build cgo && (linux || darwin || freebsd)

// Package pluginimpl loads a business-logic implementation from a Go plugin
// (.so built with -buildmode=plugin) and adapts it to svc.Impl.
package pluginimpl

import (
	"fmt"
	"net/http"
	"plugin"
)

// Supported reports whether this build can load Go plugins.
const Supported = true

// Loaded is a plugin-backed implementation. It forwards the optional state
// hooks so hotswap's ExportState/ImportState hand-over works across plugins.
type Loaded struct {
	http.Handler
	Path    string
	Version string
	impl    any
}

type exporter interface{ ExportState() (any, error) }
type importer interface{ ImportState(any) error }

// ExportState forwards to the plugin if it implements it.
func (l *Loaded) ExportState() (any, error) {
	if e, ok := l.impl.(exporter); ok {
		return e.ExportState()
	}
	return nil, nil
}

// ImportState forwards to the plugin if it implements it.
func (l *Loaded) ImportState(s any) error {
	if i, ok := l.impl.(importer); ok {
		return i.ImportState(s)
	}
	return nil
}

// Open loads path (or returns the already-loaded plugin for the same file:
// Go caches plugins and can never unload them) and creates a fresh instance
// through its New() factory.
func Open(path string) (*Loaded, error) {
	p, err := plugin.Open(path)
	if err != nil {
		return nil, err
	}
	newSym, err := p.Lookup("New")
	if err != nil {
		return nil, err
	}
	factory, ok := newSym.(func() any)
	if !ok {
		return nil, fmt.Errorf("%s: symbol New has type %T, want func() any", path, newSym)
	}
	version := "unknown"
	if vs, err := p.Lookup("Version"); err == nil {
		if s, ok := vs.(*string); ok {
			version = *s
		}
	}
	impl := factory()
	h, ok := impl.(http.Handler)
	if !ok {
		return nil, fmt.Errorf("%s: New() returned %T which is not an http.Handler", path, impl)
	}
	return &Loaded{Handler: h, Path: path, Version: version, impl: impl}, nil
}
