//go:build unix

package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

var inheritedDescriptorFcntl = unix.FcntlInt

func validateProcessIsolationPlatform() error { return nil }

func applyProcessCredential(cmd *exec.Cmd, isolation *ProcessIsolation) error {
	if err := validateProcessIsolation(isolation); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: isolation.UID, Gid: isolation.GID, Groups: []uint32{}, NoSetGroups: false}

	return nil
}

func closeInheritedOnExec(file *os.File) error {
	if file == nil {
		return fmt.Errorf("inherited config descriptor is unavailable")
	}

	flags, err := inheritedDescriptorFcntl(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("read inherited descriptor flags: %w", err)
	}

	if _, err = inheritedDescriptorFcntl(file.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("protect inherited descriptor from native exec: %w", err)
	}

	return nil
}
