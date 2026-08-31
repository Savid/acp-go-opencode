//go:build windows

package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func configureOrdinaryProcess(*exec.Cmd) {}

func stopOrdinaryProcess(_ context.Context, command *exec.Cmd) (bool, error) {
	if command == nil || command.Process == nil {
		return false, nil
	}

	err := command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return false, nil
	}

	return err == nil, err
}

func containOrdinaryProcess(*exec.Cmd) error { return nil }

func ordinaryProcessOutcome(command *exec.Cmd, _ error) ProcessOutcome {
	result := ProcessOutcome{ExitCode: -1}
	if command != nil && command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}

	return result
}
