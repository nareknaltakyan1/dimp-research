//go:build !linux

package handoff

import "errors"

func becomeSubreaper() error {
	return errors.New("child subreaper is Linux-only; grandchildren will not be reaped here")
}
