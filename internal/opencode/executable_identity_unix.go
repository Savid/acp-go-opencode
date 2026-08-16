//go:build !windows

package opencode

import "syscall"

func processExecutableIdentity(path string) (uint64, uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, 0, err
	}

	return uint64(stat.Dev), stat.Ino, nil //nolint:unconvert // Stat_t.Dev is not uint64 on every Unix.
}
