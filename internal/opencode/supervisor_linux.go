//go:build linux

package opencode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const linuxSupervisorTaskRoot = "/proc/self/task"

type guardianContainment struct{}

type livenessContainment struct{}

var (
	supervisorLinuxPrctl           = unix.Prctl
	supervisorLinuxPIDFDOpen       = unix.PidfdOpen
	supervisorLinuxPIDFDSendSignal = unix.PidfdSendSignal
	supervisorLinuxPoll            = unix.Poll
	supervisorLinuxWait4           = unix.Wait4
	supervisorLinuxWaitid          = unix.Waitid
	supervisorLinuxReadDir         = os.ReadDir
	supervisorLinuxReadFile        = os.ReadFile
	supervisorLinuxClose           = unix.Close
)

func newGuardianContainment() (*guardianContainment, error) {
	if err := enableLinuxSubreaper(supervisorModeGuardian); err != nil {
		return nil, err
	}

	return &guardianContainment{}, nil
}

func (*guardianContainment) Name() string { return "" }

func (*guardianContainment) Close() error { return nil }

func (*guardianContainment) Quiesce(nativePID int, timeout time.Duration) error {
	return quiesceLinuxReaper(nativePID, 0, timeout)
}

func openLivenessContainment(string) (*livenessContainment, error) {
	if err := enableLinuxSubreaper(supervisorModeLiveness); err != nil {
		return nil, err
	}

	return &livenessContainment{}, nil
}

func (*livenessContainment) Start(cmd *exec.Cmd) error {
	configureOpenCodeProcess(cmd)

	return cmd.Start()
}

func (*livenessContainment) Close() error { return nil }

func (*livenessContainment) Quiesce(nativePID int, timeout time.Duration) error {
	// cmd.Wait owns the direct native root. This helper kills it through a
	// pidfd but leaves reaping that one PID to os/exec; every adopted descendant
	// is reaped here.
	return quiesceLinuxReaper(nativePID, nativePID, timeout)
}

func enableLinuxSubreaper(_ string) error {
	// This function is package-private and is reached in production only from
	// runGuardian/runLiveness, after supervisorBootstrap has replaced the
	// embedding command with a dedicated helper mode.
	if err := supervisorLinuxPrctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return errors.Join(ErrProcessTreeUnproven, fmt.Errorf("enable Linux child subreaper: %w", err))
	}

	fd, err := supervisorLinuxPIDFDOpen(os.Getpid(), 0)
	if err != nil {
		return errors.Join(ErrProcessTreeUnproven, fmt.Errorf("open Linux pidfd containment probe: %w", err))
	}

	if err := supervisorLinuxClose(fd); err != nil {
		return errors.Join(ErrProcessTreeUnproven, fmt.Errorf("close Linux pidfd containment probe: %w", err))
	}

	return nil
}

func configureIndependentSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateIndependentSupervisor(cmd *exec.Cmd) error {
	return signalOpenCodeProcessGroup(cmd, syscall.SIGKILL)
}

func querySupervisorProcessSnapshot(string) (int, bool) { return 0, false }

func quiesceLinuxReaper(nativePID int, waitOwnedPID int, timeout time.Duration) error {
	if nativePID <= 0 {
		return errors.New("native process group ID is required")
	}

	deadline := time.Now().Add(timeout)

	termDeadline := time.Now().Add(500 * time.Millisecond)
	if termDeadline.After(deadline) {
		termDeadline = deadline
	}

	_ = signalProcessGroup(nativePID, syscall.SIGTERM)
	for time.Now().Before(termDeadline) {
		_, err := signalLinuxReaperChildren(waitOwnedPID, unix.SIGTERM)
		if err != nil {
			return err
		}

		quiescent, err := linuxReaperNoChildren()
		if err != nil {
			return err
		}

		if quiescent {
			return nil
		}

		time.Sleep(10 * time.Millisecond)
	}

	_ = signalProcessGroup(nativePID, syscall.SIGKILL)

	for time.Now().Before(deadline) {
		_, err := signalLinuxReaperChildren(waitOwnedPID, unix.SIGKILL)
		if err != nil {
			return err
		}

		quiescent, err := linuxReaperNoChildren()
		if err != nil {
			return err
		}

		if quiescent {
			return nil
		}

		time.Sleep(10 * time.Millisecond)
	}

	return fmt.Errorf("native process tree rooted at %d did not become quiescent", nativePID)
}

// linuxReaperNoChildren uses WNOWAIT so it never steals the direct native root
// from cmd.Wait. ECHILD is the only authoritative success result: a nil result
// means at least one live or waitable child still exists, even when a concurrent
// /proc children snapshot happened to be empty.
func linuxReaperNoChildren() (bool, error) {
	var info unix.Siginfo

	err := supervisorLinuxWaitid(
		unix.P_ALL,
		0,
		&info,
		unix.WEXITED|unix.WNOHANG|unix.WNOWAIT,
		nil,
	)
	switch {
	case errors.Is(err, unix.ECHILD):
		return true, nil
	case err == nil, errors.Is(err, unix.EINTR):
		return false, nil
	default:
		return false, errors.Join(ErrProcessTreeUnproven, fmt.Errorf("fence Linux child subreaper: %w", err))
	}
}

func signalLinuxReaperChildren(waitOwnedPID int, signal unix.Signal) (int, error) {
	children, err := linuxReaperChildren()
	if err != nil {
		return 0, err
	}

	active := 0

	for _, pid := range children {
		fd, err := supervisorLinuxPIDFDOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}

		if err != nil {
			return 0, fmt.Errorf("open pidfd for adopted native descendant %d: %w", pid, err)
		}

		signalErr := supervisorLinuxPIDFDSendSignal(fd, signal, nil, 0)
		if signalErr != nil && !errors.Is(signalErr, unix.ESRCH) {
			_ = supervisorLinuxClose(fd)

			return 0, fmt.Errorf("signal adopted native descendant %d: %w", pid, signalErr)
		}

		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // Linux file descriptors fit pollfd's signed 32-bit field.
		_, pollErr := supervisorLinuxPoll(poll, 0)
		closeErr := supervisorLinuxClose(fd)

		if pollErr != nil {
			return 0, fmt.Errorf("poll adopted native descendant %d: %w", pid, pollErr)
		}

		if closeErr != nil {
			return 0, fmt.Errorf("close pidfd for adopted native descendant %d: %w", pid, closeErr)
		}

		exited := poll[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
		if !exited {
			active++

			continue
		}

		if pid == waitOwnedPID {
			continue
		}

		var status unix.WaitStatus

		_, waitErr := supervisorLinuxWait4(pid, &status, syscall.WNOHANG, nil)
		if waitErr != nil && !errors.Is(waitErr, unix.ECHILD) {
			return 0, fmt.Errorf("reap adopted native descendant %d: %w", pid, waitErr)
		}
	}

	return active, nil
}

func linuxReaperChildren() ([]int, error) {
	tasks, err := supervisorLinuxReadDir(linuxSupervisorTaskRoot)
	if err != nil {
		return nil, fmt.Errorf("list Linux supervisor tasks: %w", err)
	}

	children := make([]int, 0)

	for _, task := range tasks {
		if !task.IsDir() {
			continue
		}

		raw, err := supervisorLinuxReadFile(filepath.Join(linuxSupervisorTaskRoot, task.Name(), "children"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read Linux supervisor children for task %s: %w", task.Name(), err)
		}

		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 {
				return nil, fmt.Errorf("parse Linux supervisor child PID %q", field)
			}

			children = append(children, pid)
		}
	}

	slices.Sort(children)

	return slices.Compact(children), nil
}

// These process-group helpers remain the graceful first strike. The subreaper
// inventory above is the actual proof boundary and catches setsid/setpgid
// escapees after they are adopted by the dedicated helper process.
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
