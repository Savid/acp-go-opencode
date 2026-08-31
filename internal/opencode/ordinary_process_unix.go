//go:build unix

package opencode

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

func configureOrdinaryProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopOrdinaryProcess(_ context.Context, command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}

	err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

func containOrdinaryProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}

	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

func ordinaryProcessOutcome(command *exec.Cmd, _ error) ProcessOutcome {
	result := ProcessOutcome{ExitCode: -1}
	if command == nil || command.ProcessState == nil {
		return result
	}

	result.ExitCode = command.ProcessState.ExitCode()
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.Signal = int(status.Signal())
	}

	return result
}
