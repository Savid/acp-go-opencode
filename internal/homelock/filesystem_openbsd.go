//go:build openbsd

package homelock

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func validateLockFilesystem(file *os.File) error {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect filesystem containing runtime lock: %w", err)
	}

	nameBytes := make([]byte, 0, len(stat.F_fstypename))
	for _, value := range stat.F_fstypename {
		nameBytes = append(nameBytes, byte(value))
	}

	name := strings.TrimRight(string(nameBytes), "\x00")
	if stat.F_flags&unix.MNT_LOCAL == 0 || !openBSDLocalLockFilesystem(name) {
		return fmt.Errorf("filesystem %q has no approved local lock semantics", name)
	}

	return nil
}

func openBSDLocalLockFilesystem(name string) bool {
	switch name {
	case "ffs", "tmpfs":
		return true
	default:
		return false
	}
}
