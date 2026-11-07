//go:build !linux

package handoff

import "syscall"

// On BSD/darwin SO_REUSEPORT exists but does not load-balance like Linux.
const soReusePort = syscall.SO_REUSEPORT
