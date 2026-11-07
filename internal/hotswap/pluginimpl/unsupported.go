//go:build !(cgo && (linux || darwin || freebsd))

package pluginimpl

import (
	"errors"
	"net/http"
)

// Supported reports whether this build can load Go plugins.
const Supported = false

// Loaded is a plugin-backed implementation (unsupported in this build).
type Loaded struct {
	http.Handler
	Path, Version string
}

// ExportState is a no-op.
func (l *Loaded) ExportState() (any, error) { return nil, nil }

// ImportState is a no-op.
func (l *Loaded) ImportState(any) error { return nil }

// Open always fails: Go plugins need cgo on linux/darwin/freebsd.
func Open(path string) (*Loaded, error) {
	return nil, errors.New("go plugins are not supported in this build (need cgo on linux/darwin/freebsd)")
}
