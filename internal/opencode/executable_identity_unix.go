//go:build !windows && !linux

package opencode

import "syscall"

// processExecutableIdentity answers with the device and inode numbers that
// identify one executable file. The device number is an opaque identity token
// rather than an arithmetic quantity, and Stat_t.Dev is narrower and signed on
// these platforms, so widening it here is what makes the pair comparable at all.
func processExecutableIdentity(path string) (uint64, uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, 0, err
	}

	return uint64(stat.Dev), stat.Ino, nil //nolint:gosec // See above.
}
