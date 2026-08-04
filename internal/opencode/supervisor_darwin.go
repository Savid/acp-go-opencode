//go:build darwin

package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const darwinGroupTermGrace = 500 * time.Millisecond

var (
	darwinFastExitWait      = supervisorQuiesceWindow
	darwinAbortWait         = supervisorQuiesceWindow
	darwinSupervisorGetpgid = syscall.Getpgid
	darwinSupervisorKill    = syscall.Kill
	darwinSupervisorNow     = time.Now
	darwinSupervisorSleep   = time.Sleep
)

type guardianContainment struct{}

type livenessContainment struct {
	generation *DarwinGeneration
	pgid       int
	process    *os.Process
	waiter     *supervisorWaiter

	cleanupOnce sync.Once
	cleanupErr  error
}

func (*livenessContainment) DescendantCount() (int, bool) { return 0, false }

func newGuardianContainment(config supervisorConfig) (*guardianContainment, error) {
	if !config.DarwinBestEffort {
		return nil, errors.New("OpenCode runtime containment is unsupported on darwin without explicit best-effort opt-in")
	}

	return &guardianContainment{}, nil
}

func (*guardianContainment) Name() string { return darwinBestEffortJobName }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(_ int, _ time.Duration) error {
	return errors.Join(
		ErrProcessContainmentIncomplete,
		errors.New("darwin liveness supervisor exited without publishing its captured-group cleanup result"),
	)
}

func openLivenessContainment(config supervisorConfig) (*livenessContainment, error) {
	if !config.DarwinBestEffort {
		return nil, errors.New("OpenCode runtime containment is unsupported on darwin without explicit best-effort opt-in")
	}

	if config.ScratchParent == "" || config.Scratch == "" {
		return nil, errors.New("darwin best-effort containment requires a scratch parent and generation root")
	}

	generation, err := NewDarwinGenerationRecord(config.ScratchParent, config.Scratch, config.LifecycleKind)
	if err != nil {
		return nil, err
	}

	return &livenessContainment{generation: generation}, nil
}

func (c *livenessContainment) Start(cmd *exec.Cmd) error {
	if c == nil || c.generation == nil {
		return errors.New("darwin best-effort containment generation is unavailable")
	}

	if err := c.generation.prepareCommand(cmd); err != nil {
		return errors.Join(err, c.generation.finish(true))
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = supervisorQuiesceWindow

	if err := cmd.Start(); err != nil {
		return errors.Join(err, c.generation.finish(true))
	}

	c.pgid = cmd.Process.Pid
	c.process = cmd.Process
	c.waiter = newSupervisorWaiter(cmd, true)

	pgid, err := darwinSupervisorGetpgid(cmd.Process.Pid)
	if errors.Is(err, syscall.ESRCH) {
		return c.handleFastExit(cmd.Process.Pid)
	}

	if err != nil {
		return c.abortUnvalidated(cmd, fmt.Errorf("inspect Darwin native root process group: %w", err))
	}

	if pgid != cmd.Process.Pid {
		return c.abortUnvalidated(cmd, fmt.Errorf("darwin native root joined unexpected process group %d", pgid))
	}

	if err := c.generation.started(cmd.Process.Pid, pgid); err != nil {
		cleanupErr := c.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)

		return errors.Join(err, cleanupErr)
	}

	c.waiter.start()

	return nil
}

func (c *livenessContainment) handleFastExit(pid int) error {
	probeErr := darwinSupervisorKill(-pid, 0)
	switch {
	case errors.Is(probeErr, syscall.ESRCH):
		c.waiter.start()

		ctx, cancel := context.WithTimeout(context.Background(), darwinFastExitWait)
		defer cancel()

		waitErr, completed := c.waiter.await(ctx)
		if !completed {
			return errors.Join(
				fmt.Errorf("%w: reap fast-exit Darwin native root: %v", ErrProcessContainmentIncomplete, waitErr),
				c.generation.finish(false),
			)
		}

		return errors.Join(
			errors.New("darwin native root exited before containment identity capture"),
			waitErr,
			c.generation.finish(true),
		)
	case probeErr == nil || errors.Is(probeErr, syscall.EPERM):
		cleanupErr := c.Quiesce(pid, supervisorQuiesceWindow)

		return errors.Join(errors.New("darwin native root exited before containment identity capture"), cleanupErr)
	default:
		return c.abortUnvalidated(nil, fmt.Errorf("probe expected Darwin process group %d: %w", pid, probeErr))
	}
}

func (c *livenessContainment) abortUnvalidated(cmd *exec.Cmd, cause error) error {
	process := c.process
	if cmd != nil && cmd.Process != nil {
		process = cmd.Process
	}

	return errors.Join(
		fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, cause),
		terminateAndReapDarwinDirectChild(process, c.waiter, darwinAbortWait),
		c.generation.finish(false),
	)
}

func terminateAndReapDarwinDirectChild(process *os.Process, waiter *supervisorWaiter, timeout time.Duration) error {
	if waiter == nil {
		return errors.New("darwin direct-child waiter is unavailable")
	}

	if timeout <= 0 {
		return errors.New("positive Darwin direct-child cleanup deadline is required")
	}

	deadline := darwinSupervisorNow().Add(timeout)

	termDeadline := darwinSupervisorNow().Add(darwinGroupTermGrace)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	var actionErr error
	if process == nil {
		actionErr = errors.New("darwin direct-child process handle is unavailable")
	} else {
		actionErr = process.Signal(syscall.SIGTERM)
		if errors.Is(actionErr, os.ErrProcessDone) || errors.Is(actionErr, syscall.ESRCH) {
			actionErr = nil
		}
	}

	waiter.start()

	waitErr, completed := awaitDarwinWaiterUntil(waiter, termDeadline)
	if completed {
		return errors.Join(actionErr, waitErr)
	}

	if process != nil {
		killErr := process.Kill()
		if !errors.Is(killErr, os.ErrProcessDone) && !errors.Is(killErr, syscall.ESRCH) {
			actionErr = errors.Join(actionErr, killErr)
		}
	}

	waitErr, completed = awaitDarwinWaiterUntil(waiter, deadline)
	if !completed {
		waitErr = fmt.Errorf("direct child remained unreaped at the containment deadline: %w", waitErr)
	}

	return errors.Join(actionErr, waitErr)
}

func awaitDarwinWaiterUntil(waiter *supervisorWaiter, deadline time.Time) (error, bool) {
	remaining := deadline.Sub(darwinSupervisorNow())
	if remaining <= 0 {
		select {
		case <-waiter.done:
			return waiter.err, true
		default:
			return context.DeadlineExceeded, false
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()

	return waiter.await(ctx)
}

func (c *livenessContainment) Wait() <-chan error {
	return c.waiter.result()
}

func (c *livenessContainment) Close() error {
	if c == nil || c.generation == nil || c.waiter != nil {
		return nil
	}

	return c.generation.finish(true)
}

func (c *livenessContainment) Quiesce(_ int, timeout time.Duration) error {
	if c == nil || c.pgid <= 0 {
		return nil
	}

	c.cleanupOnce.Do(func() {
		c.cleanupErr = quiesceDarwinOriginalGroup(c.pgid, c.process, c.waiter, c.generation, timeout)
	})

	return c.cleanupErr
}

func quiesceDarwinOriginalGroup(
	pgid int,
	process *os.Process,
	waiter *supervisorWaiter,
	generation *DarwinGeneration,
	timeout time.Duration,
) error {
	return quiesceDarwinOriginalGroupWithSignal(pgid, process, waiter, generation, timeout, darwinSupervisorKill)
}

func quiesceDarwinOriginalGroupWithSignal(
	pgid int,
	process *os.Process,
	waiter *supervisorWaiter,
	generation *DarwinGeneration,
	timeout time.Duration,
	signalProcess func(int, syscall.Signal) error,
) error {
	if pgid <= 0 || timeout <= 0 {
		return finishDarwinGeneration(generation, fmt.Errorf("%w: positive Darwin quiescence boundary is required", ErrProcessContainmentIncomplete))
	}

	deadline := darwinSupervisorNow().Add(timeout)

	termDeadline := darwinSupervisorNow().Add(darwinGroupTermGrace)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	groupAbsent := false
	killed := false

	termErr := signalProcess(-pgid, syscall.SIGTERM)

	waiter.start()

	switch {
	case errors.Is(termErr, syscall.ESRCH):
		groupAbsent = true
	case termErr != nil && !errors.Is(termErr, syscall.EPERM):
		containmentErr := fmt.Errorf("%w: terminate original process group %d: %v", ErrProcessContainmentIncomplete, pgid, termErr)

		return finishDarwinGeneration(generation, errors.Join(containmentErr, reapDarwinDirectChild(process, waiter, deadline)))
	}

	for darwinSupervisorNow().Before(deadline) {
		if !groupAbsent {
			probeErr := signalProcess(-pgid, 0)
			switch {
			case errors.Is(probeErr, syscall.ESRCH):
				groupAbsent = true
			case probeErr != nil && !errors.Is(probeErr, syscall.EPERM):
				containmentErr := fmt.Errorf("%w: inspect original process group %d: %v", ErrProcessContainmentIncomplete, pgid, probeErr)

				return finishDarwinGeneration(generation, errors.Join(containmentErr, reapDarwinDirectChild(process, waiter, deadline)))
			}
		}

		reaped := waiter == nil
		if waiter != nil {
			select {
			case <-waiter.done:
				reaped = true
			default:
			}
		}

		if groupAbsent && reaped {
			return finishDarwinGeneration(generation, nil)
		}

		if !groupAbsent && !killed && !darwinSupervisorNow().Before(termDeadline) {
			killErr := signalProcess(-pgid, syscall.SIGKILL)
			switch {
			case errors.Is(killErr, syscall.ESRCH):
				groupAbsent = true
			case killErr != nil && !errors.Is(killErr, syscall.EPERM):
				containmentErr := fmt.Errorf("%w: kill original process group %d: %v", ErrProcessContainmentIncomplete, pgid, killErr)

				return finishDarwinGeneration(generation, errors.Join(containmentErr, reapDarwinDirectChild(process, waiter, deadline)))
			}

			killed = true
		}

		darwinSupervisorSleep(10 * time.Millisecond)
	}

	return finishDarwinGeneration(generation, fmt.Errorf("%w: direct child or original process group %d remained observable", ErrProcessContainmentIncomplete, pgid))
}

func reapDarwinDirectChild(process *os.Process, waiter *supervisorWaiter, deadline time.Time) error {
	if waiter == nil {
		return nil
	}

	waiter.start()

	var killErr error
	if process == nil {
		killErr = errors.New("darwin direct-child process handle is unavailable")
	} else {
		killErr = process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) || errors.Is(killErr, syscall.ESRCH) {
			killErr = nil
		}
	}

	_, completed := awaitDarwinWaiterUntil(waiter, deadline)
	if !completed {
		return errors.Join(killErr, errors.New("darwin direct child remained unreaped at the containment deadline"))
	}

	return killErr
}

func finishDarwinGeneration(generation *DarwinGeneration, cleanupErr error) error {
	return errors.Join(cleanupErr, generation.finish(cleanupErr == nil))
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func startIndependentSupervisor(cmd *exec.Cmd) error {
	return cmd.Start()
}

func releaseIndependentSupervisorWaiter(cmd *exec.Cmd, waiter *supervisorWaiter) (int, error) {
	if cmd == nil || cmd.Process == nil || waiter == nil {
		return 0, fmt.Errorf("%w: Darwin direct-child waiter is unavailable", ErrProcessContainmentIncomplete)
	}

	pid := cmd.Process.Pid

	pgid, err := darwinSupervisorGetpgid(pid)
	if errors.Is(err, syscall.ESRCH) {
		probeErr := darwinSupervisorKill(-pid, 0)

		if probeErr == nil || errors.Is(probeErr, syscall.EPERM) {
			return 0, errors.Join(
				errors.New("darwin direct child exited before process-group identity capture"),
				quiesceDarwinOriginalGroup(pid, cmd.Process, waiter, nil, darwinFastExitWait),
			)
		}

		if !errors.Is(probeErr, syscall.ESRCH) {
			return 0, abortIndependentSupervisor(cmd.Process, waiter, fmt.Errorf("probe expected Darwin process group %d: %w", pid, probeErr))
		}

		waiter.start()

		ctx, cancel := context.WithTimeout(context.Background(), darwinFastExitWait)
		defer cancel()

		waitErr, completed := waiter.await(ctx)
		if !completed {
			return 0, fmt.Errorf("%w: reap fast-exit Darwin direct child: %v", ErrProcessContainmentIncomplete, waitErr)
		}

		return 0, errors.Join(errors.New("darwin direct child exited before process-group identity capture"), waitErr)
	}

	if err != nil {
		return 0, abortIndependentSupervisor(cmd.Process, waiter, fmt.Errorf("inspect Darwin direct-child process group: %w", err))
	}

	if pgid != pid {
		return 0, abortIndependentSupervisor(cmd.Process, waiter, fmt.Errorf("darwin direct child joined unexpected process group %d", pgid))
	}

	waiter.start()

	return pgid, nil
}

func abortIndependentSupervisor(process *os.Process, waiter *supervisorWaiter, cause error) error {
	return errors.Join(
		fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, cause),
		terminateAndReapDarwinDirectChild(process, waiter, darwinAbortWait),
	)
}

func querySupervisorProcessSnapshot(string) (int, bool) { return 0, false }

func quiesceProcessGroup(nativePID int, timeout time.Duration) error {
	return quiesceDarwinOriginalGroupWithSignal(nativePID, nil, nil, nil, timeout, openCodeSyscallKill)
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
