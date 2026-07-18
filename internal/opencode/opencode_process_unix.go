//go:build unix

package opencode

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var openCodeSyscallKill = syscall.Kill

func terminateOpenCodeProcess(process *os.Process, originalGroup int) error {
	return signalOpenCodeProcessGroup(process, originalGroup, syscall.SIGTERM)
}

func killOpenCodeProcess(process *os.Process, originalGroup int) error {
	return signalOpenCodeProcessGroup(process, originalGroup, syscall.SIGKILL)
}

func signalOpenCodeProcessGroup(process *os.Process, originalGroup int, signal syscall.Signal) error {
	if process == nil {
		return nil
	}

	if originalGroup <= 0 {
		return errors.New("captured OpenCode process group is unavailable")
	}

	if err := openCodeSyscallKill(-originalGroup, signal); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			directErr := process.Signal(signal)
			if errors.Is(directErr, os.ErrProcessDone) || errors.Is(directErr, syscall.ESRCH) {
				return nil
			}

			return directErr
		}

		return fmt.Errorf("signal captured OpenCode process group %d: %w", originalGroup, err)
	}

	return nil
}
