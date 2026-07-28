//go:build unix

package opencode

import (
	"errors"
	"syscall"
)

// signalLeasedProcessGroup signals the leased process's whole group, which is
// the boundary the native server sits inside. A group that is already gone is
// not a failure to report: the reaper's whole purpose is that it be gone.
func signalLeasedProcessGroup(pid int, force bool) error {
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}

	if err := openCodeSyscallKill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}
