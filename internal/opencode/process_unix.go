//go:build unix

package opencode

import (
	"errors"
	"os/exec"
	"syscall"
)

var (
	syscallGetpgid = syscall.Getpgid
	syscallKill    = syscall.Kill
)

func configureProcessCommandPlatform(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGTERM)
}

func killProcess(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGKILL)
}

func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pgid, err := syscallGetpgid(cmd.Process.Pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}

		return err
	}

	if err := syscallKill(-pgid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}

		return err
	}

	return nil
}
