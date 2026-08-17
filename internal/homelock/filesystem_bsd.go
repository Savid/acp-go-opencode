//go:build darwin || freebsd

package homelock

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var bsdFstatfs = unix.Fstatfs

func validateLockFilesystem(file *os.File) error {
	var stat unix.Statfs_t
	if err := bsdFstatfs(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect filesystem containing runtime lock: %w", err)
	}

	name := strings.TrimRight(string(stat.Fstypename[:]), "\x00")
	if stat.Flags&unix.MNT_LOCAL == 0 || !bsdLocalLockFilesystem(name) {
		return fmt.Errorf("filesystem %q has no approved local lock semantics", name)
	}

	return nil
}

func bsdLocalLockFilesystem(name string) bool {
	switch name {
	case "apfs", "hfs", "ufs", "zfs", "tmpfs":
		return true
	default:
		return false
	}
}
