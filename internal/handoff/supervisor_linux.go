//go:build linux

package handoff

import "syscall"

const prSetChildSubreaper = 36

func becomeSubreaper() error {
	_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	if e != 0 {
		return e
	}
	return nil
}
