//go:build unix

package opencode

import (
	"errors"
	"os/exec"
	"syscall"
)

var (
	openCodeSyscallGetpgid = syscall.Getpgid
	openCodeSyscallKill    = syscall.Kill
)

func terminateOpenCodeProcess(cmd *exec.Cmd) error {
	return signalOpenCodeProcessGroup(cmd, syscall.SIGTERM)
}

func killOpenCodeProcess(cmd *exec.Cmd) error {
	return signalOpenCodeProcessGroup(cmd, syscall.SIGKILL)
}

func signalOpenCodeProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pgid, err := openCodeSyscallGetpgid(cmd.Process.Pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}

		return err
	}

	if err := openCodeSyscallKill(-pgid, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}

		return err
	}

	return nil
}
