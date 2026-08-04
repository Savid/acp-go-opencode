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

func closeInheritedOnExec(file *os.File) error {
	if file == nil {
		return fmt.Errorf("inherited config descriptor is unavailable")
	}

	syscall.CloseOnExec(int(file.Fd()))

	return nil
}
