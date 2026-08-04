//go:build unix

package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

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

func verifySupervisorIdentity() error {
	uid, gid, err := expectedSupervisorIdentity()
	if err != nil {
		return err
	}

	actualUID, actualGID := os.Geteuid(), os.Getegid()
	if actualUID < 0 || actualGID < 0 || uint64(actualUID) != uint64(uid) || uint64(actualGID) != uint64(gid) {
		return fmt.Errorf("process isolation identity mismatch: got %d:%d, want %d:%d", actualUID, actualGID, uid, gid)
	}

	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("read process isolation supplementary groups: %w", err)
	}

	if len(groups) != 0 {
		return fmt.Errorf("process isolation supplementary groups are not empty: %v", groups)
	}

	return nil
}

func closeInheritedOnExec(file *os.File) error {
	if file == nil {
		return fmt.Errorf("inherited config descriptor is unavailable")
	}

	syscall.CloseOnExec(int(file.Fd()))

	return nil
}
