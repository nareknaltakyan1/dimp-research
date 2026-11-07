//go:build linux

package handoff

// syscall.SO_REUSEPORT is missing from the frozen syscall package on some
// Linux architectures (e.g. amd64), so define it here. 15 is correct for
// x86, arm and arm64 (it differs on mips/sparc, which we do not target).
const soReusePort = 0xf
