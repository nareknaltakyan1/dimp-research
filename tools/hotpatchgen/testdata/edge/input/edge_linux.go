//go:build linux

package edge

// LinuxOnly lives in a constrained file and is never transformed.
//
//dimp:patchable
func LinuxOnly() int { return 1 }
