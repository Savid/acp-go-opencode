//go:build !darwin && !linux

package opencode

import "errors"

// processStartTime has no portable source on these platforms. Reporting the
// answer as unavailable is what keeps the reaper from signalling a process it
// cannot identify.
func processStartTime(int) (string, error) {
	return "", errors.ErrUnsupported
}
