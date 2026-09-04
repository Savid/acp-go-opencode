//go:build unix

package opencode

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// ordinaryProcessGuard carries no state on POSIX: the process group set before
// the child starts already contains every descendant, and outlives its leader,
// so there is no handle for teardown to own. Windows has no such group and its
// guard owns a job object instead.
type ordinaryProcessGuard struct{}

func configureOrdinaryProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func superviseOrdinaryProcess(*exec.Cmd) (*ordinaryProcessGuard, error) {
	return &ordinaryProcessGuard{}, nil
}

func stopOrdinaryProcess(_ context.Context, command *exec.Cmd, _ *ordinaryProcessGuard) (bool, error) {
	if command == nil || command.Process == nil {
		return false, nil
	}

	err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}

	return err == nil, err
}

func containOrdinaryProcess(command *exec.Cmd, _ *ordinaryProcessGuard) error {
	if command == nil || command.Process == nil {
		return nil
	}

	return normalizeContainOrdinaryProcessError(syscall.Kill(-command.Process.Pid, syscall.SIGKILL))
}

func normalizeContainOrdinaryProcessError(err error) error {
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
