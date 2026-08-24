//go:build linux

package opencode

import "syscall"

// processExecutableIdentity answers with the device and inode numbers that
// identify one executable file. On Linux both are already the width the identity
// pair is compared at.
func processExecutableIdentity(path string) (uint64, uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return 0, 0, err
	}

	return stat.Dev, stat.Ino, nil
}
