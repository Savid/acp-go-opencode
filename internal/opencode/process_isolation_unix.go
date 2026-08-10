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

// Seams for the identity this process is actually running as. Ordinary
// execution is stamped from them, so a case can place the launch at root or at
// an unprivileged account without the account the suite happens to run under
// deciding for it. They are deliberately not the seams the trusted-root
// assertion reads.
var (
	processEffectiveUID = os.Geteuid
	processEffectiveGID = os.Getegid
)

func currentProcessIdentity() (uint32, uint32, error) {
	uid, gid := processEffectiveUID(), processEffectiveGID()
	if uid < 0 || gid < 0 {
		return 0, 0, fmt.Errorf("current process identity is unavailable")
	}

	return uint32(uid), uint32(gid), nil //nolint:gosec // Kernel IDs fit the process-isolation wire width.
}

func applyProcessCredential(cmd *exec.Cmd, isolation *ProcessIsolation) error {
	if err := validateProcessIsolation(isolation); err != nil {
		return err
	}

	if isolation == nil {
		return nil
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	// An explicit policy always drops to the configured credential with an
	// empty supplementary set. There is no arm that applies no credential:
	// that shape was the one a caller could ask for isolation and receive the
	// adapter's own identity instead.
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
