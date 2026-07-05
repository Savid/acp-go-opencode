//go:build unix

package opencodeacp

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

func killProcessID(pid int) error {
	if pid <= 0 {
		return nil
	}
	pgid, err := openCodeSyscallGetpgid(pid)
	if err == nil {
		pid = -pgid
	}
	if err := openCodeSyscallKill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
