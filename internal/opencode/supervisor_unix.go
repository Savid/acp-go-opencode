//go:build freebsd || openbsd

package opencode

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

type guardianContainment struct{}

type livenessContainment struct{}

func newGuardianContainment(supervisorConfig) (*guardianContainment, error) {
	return nil, errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("proof-capable OpenCode runtime containment is unavailable on %s", runtime.GOOS))
}

func (*guardianContainment) Name() string { return "" }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceProcessGroup(nativePID, timeout)
}

func openLivenessContainment(supervisorConfig) (*livenessContainment, error) {
	return nil, errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("proof-capable OpenCode runtime containment is unavailable on %s", runtime.GOOS))
}

func (*livenessContainment) Start(cmd *exec.Cmd) error {
	configureOpenCodeProcess(cmd)

	return cmd.Start()
}

func (*livenessContainment) Wait() <-chan error {
	result := make(chan error, 1)
	result <- nil

	return result
}

func (*livenessContainment) Close() error { return nil }

func (*livenessContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceProcessGroup(nativePID, timeout)
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func startIndependentSupervisor(cmd *exec.Cmd) error {
	return cmd.Start()
}

func releaseIndependentSupervisorWaiter(cmd *exec.Cmd, waiter *supervisorWaiter) (int, error) {
	if cmd == nil || cmd.Process == nil || waiter == nil {
		return 0, errors.Join(ErrProcessContainmentIncomplete, errors.New("direct-child waiter is unavailable"))
	}

	waiter.start()

	return cmd.Process.Pid, nil
}

func querySupervisorProcessSnapshot(string) (int, bool) { return 0, false }

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
