//go:build linux

package homelock

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var linuxFstatfs = unix.Fstatfs

func validateLockFilesystem(file *os.File) error {
	var stat unix.Statfs_t
	if err := linuxFstatfs(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect filesystem containing runtime lock: %w", err)
	}

	if !linuxLocalLockFilesystem(stat.Type) {
		return fmt.Errorf("filesystem type %#x has no approved local lock semantics", stat.Type)
	}

	return nil
}

func linuxLocalLockFilesystem(fsType int64) bool {
	switch fsType {
	case unix.EXT4_SUPER_MAGIC,
		unix.XFS_SUPER_MAGIC,
		unix.BTRFS_SUPER_MAGIC,
		unix.F2FS_SUPER_MAGIC,
		unix.TMPFS_MAGIC:
		return true
	default:
		return false
	}
}
