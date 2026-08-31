//go:build windows

package opencode

import (
	"context"
	"os/exec"
)

func configureOrdinaryProcess(*exec.Cmd) {}

func stopOrdinaryProcess(_ context.Context, command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}

	return command.Process.Kill()
}

func containOrdinaryProcess(*exec.Cmd) error { return nil }

func ordinaryProcessOutcome(command *exec.Cmd, _ error) ProcessOutcome {
	result := ProcessOutcome{ExitCode: -1}
	if command != nil && command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}

	return result
}
