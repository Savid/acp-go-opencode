//go:build linux || darwin || freebsd || openbsd

package homelock

import (
	"os"

	"golang.org/x/sys/unix"
)

func platformLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func platformUnlock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
