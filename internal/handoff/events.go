// Package handoff implements process-level zero-downtime upgrades: listener
// inheritance, SCM_RIGHTS file-descriptor passing, a framed control protocol
// with a state envelope, a readiness handshake and rollback, plus the shared
// contract workload (see docs/CONTRACT.md) used by cmd/b0-*, cmd/s4-*, cmd/s5-*.
package handoff

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

var (
	eventMu  sync.Mutex
	eventOut io.Writer = os.Stderr
)

// SetEventOutput redirects the upgrade event log (tests only).
func SetEventOutput(w io.Writer) {
	eventMu.Lock()
	eventOut = w
	eventMu.Unlock()
}

// Emit writes one contract event as a JSON line to stderr. ts_ns and pid are
// filled in automatically.
func Emit(ev string, fields map[string]any) {
	m := map[string]any{"ev": ev, "ts_ns": time.Now().UnixNano(), "pid": os.Getpid()}
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	b = append(b, '\n')
	eventMu.Lock()
	_, _ = eventOut.Write(b)
	eventMu.Unlock()
}
