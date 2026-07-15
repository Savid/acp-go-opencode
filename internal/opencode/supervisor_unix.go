//go:build linux || darwin || freebsd || openbsd

package opencode

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

type guardianContainment struct{}

type livenessContainment struct{}

func newGuardianContainment() (*guardianContainment, error) {
	return &guardianContainment{}, nil
}

func (*guardianContainment) Name() string { return "" }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceProcessGroup(nativePID, timeout)
}

func openLivenessContainment(string) (*livenessContainment, error) {
	return &livenessContainment{}, nil
}

func (*livenessContainment) Start(cmd *exec.Cmd) error {
	configureOpenCodeProcess(cmd)

	return cmd.Start()
}

func (*livenessContainment) Close() error { return nil }

func (*livenessContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceProcessGroup(nativePID, timeout)
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateIndependentSupervisor(cmd *exec.Cmd) error {
	return signalOpenCodeProcessGroup(cmd, syscall.SIGKILL)
}

func quiesceProcessGroup(nativePID int, timeout time.Duration) error {
	if nativePID <= 0 {
		return errors.New("native process group ID is required")
	}

	deadline := time.Now().Add(timeout)
	_ = signalProcessGroup(nativePID, syscall.SIGTERM)

	termDeadline := time.Now().Add(500 * time.Millisecond)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	for time.Now().Before(termDeadline) {
		alive, err := processGroupAlive(nativePID)
		if err != nil {
			return err
		}

		if !alive {
			return nil
		}

		time.Sleep(10 * time.Millisecond)
	}

	_ = signalProcessGroup(nativePID, syscall.SIGKILL)
	for time.Now().Before(deadline) {
		alive, err := processGroupAlive(nativePID)
		if err != nil {
			return err
		}

		if !alive {
			return nil
		}

		time.Sleep(10 * time.Millisecond)
	}

	return fmt.Errorf("native process group %d did not become quiescent", nativePID)
}

func signalProcessGroup(nativePID int, signal syscall.Signal) error {
	err := openCodeSyscallKill(-nativePID, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}

func processGroupAlive(nativePID int) (bool, error) {
	err := openCodeSyscallKill(-nativePID, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, fmt.Errorf("probe native process group %d: %w", nativePID, err)
	}
}
