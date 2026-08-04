//go:build linux

package opencode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func preservePlatformSupervisorGlobals(t *testing.T) {
	t.Helper()
	oldPrctl := supervisorLinuxPrctl
	oldNoNewPrivileges := supervisorLinuxNoNewPrivileges
	oldPIDFDOpen := supervisorLinuxPIDFDOpen
	oldPIDFDSendSignal := supervisorLinuxPIDFDSendSignal
	oldPoll := supervisorLinuxPoll
	oldWait4 := supervisorLinuxWait4
	oldWaitid := supervisorLinuxWaitid
	oldReadDir := supervisorLinuxReadDir
	oldReadFile := supervisorLinuxReadFile
	oldClose := supervisorLinuxClose
	t.Cleanup(func() {
		supervisorLinuxPrctl = oldPrctl
		supervisorLinuxNoNewPrivileges = oldNoNewPrivileges
		supervisorLinuxPIDFDOpen = oldPIDFDOpen
		supervisorLinuxPIDFDSendSignal = oldPIDFDSendSignal
		supervisorLinuxPoll = oldPoll
		supervisorLinuxWait4 = oldWait4
		supervisorLinuxWaitid = oldWaitid
		supervisorLinuxReadDir = oldReadDir
		supervisorLinuxReadFile = oldReadFile
		supervisorLinuxClose = oldClose
	})

	// Coverage tests invoke the private containment constructors in the test
	// host. Real supervisor integration tests execute fresh helper subprocesses
	// and therefore retain the production PR_SET_CHILD_SUBREAPER call.
	supervisorLinuxPrctl = func(int, uintptr, uintptr, uintptr, uintptr) error { return nil }
	supervisorLinuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
		return unix.ECHILD
	}
}

func TestLinuxContainmentCapabilityFailures(t *testing.T) {
	if _, err := releaseIndependentSupervisorWaiter((*exec.Cmd)(nil), nil); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("invalid waiter release = %v", err)
	}

	t.Run("prctl", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		supervisorLinuxPrctl = func(int, uintptr, uintptr, uintptr, uintptr) error { return errors.New("prctl failed") }
		_, err := newGuardianContainment(supervisorConfig{})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})

	t.Run("pidfd", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		supervisorLinuxPIDFDOpen = func(int, int) (int, error) { return -1, errors.New("pidfd failed") }
		_, err := openLivenessContainment(supervisorConfig{})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})

	t.Run("close probe", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		supervisorLinuxPIDFDOpen = func(int, int) (int, error) { return 7, nil }
		supervisorLinuxClose = func(int) error { return errors.New("close failed") }
		require.ErrorIs(t, enableLinuxSubreaper(supervisorModeGuardian), ErrProcessContainmentIncomplete)
	})
}

func TestLinuxReaperQuiescenceBranches(t *testing.T) {
	t.Run("invalid root", func(t *testing.T) {
		require.Error(t, quiesceLinuxReaper(-1, 0, time.Second))
	})

	t.Run("term reaches quiet", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		configureLinuxChildrenFixture(t, "")
		require.NoError(t, quiesceLinuxReaper(123, 0, 100*time.Millisecond))
	})

	t.Run("term inventory failure", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
		supervisorLinuxReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("term inventory failed") }
		require.ErrorContains(t, quiesceLinuxReaper(123, 0, time.Second), "term inventory failed")
	})

	t.Run("term kernel fence failure", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
		configureLinuxChildrenFixture(t, "")
		supervisorLinuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
			return errors.New("term waitid failed")
		}
		err := quiesceLinuxReaper(123, 0, time.Second)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "term waitid failed")
	})

	t.Run("kill reaches quiet", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
		configureLinuxChildrenFixture(t, "123")
		supervisorLinuxPIDFDOpen = func(int, int) (int, error) { return 7, nil }
		supervisorLinuxClose = func(int) error { return nil }
		lastSignal := unix.Signal(0)
		supervisorLinuxPIDFDSendSignal = func(_ int, signal unix.Signal, _ *unix.Siginfo, _ int) error {
			lastSignal = signal

			return nil
		}
		supervisorLinuxPoll = func(fds []unix.PollFd, _ int) (int, error) {
			if lastSignal == unix.SIGKILL {
				fds[0].Revents = unix.POLLIN
			}

			return 1, nil
		}
		supervisorLinuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
			if lastSignal == unix.SIGKILL {
				return unix.ECHILD
			}

			return nil
		}
		supervisorLinuxWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return 123, nil }
		require.NoError(t, quiesceLinuxReaper(123, 0, 750*time.Millisecond))
	})

	t.Run("kill signaling failure", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
		configureLinuxChildrenFixture(t, "123")
		supervisorLinuxPIDFDOpen = func(int, int) (int, error) { return 7, nil }
		supervisorLinuxClose = func(int) error { return nil }
		supervisorLinuxPIDFDSendSignal = func(_ int, signal unix.Signal, _ *unix.Siginfo, _ int) error {
			if signal == unix.SIGKILL {
				return errors.New("kill signal failed")
			}

			return nil
		}
		supervisorLinuxPoll = func([]unix.PollFd, int) (int, error) { return 0, nil }
		supervisorLinuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return nil }
		require.ErrorContains(t, quiesceLinuxReaper(123, 0, time.Second), "kill signal failed")
	})

	t.Run("kill kernel fence failure", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		lastGroupSignal := syscall.Signal(0)
		openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
			lastGroupSignal = signal

			return nil
		}
		configureLinuxChildrenFixture(t, "")
		supervisorLinuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error {
			if lastGroupSignal == syscall.SIGKILL {
				return errors.New("kill waitid failed")
			}

			return nil
		}
		err := quiesceLinuxReaper(123, 0, 750*time.Millisecond)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, "kill waitid failed")
	})

	t.Run("deadline", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		oldKill := openCodeSyscallKill
		t.Cleanup(func() { openCodeSyscallKill = oldKill })
		openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
		configureLinuxChildrenFixture(t, "123")
		supervisorLinuxPIDFDOpen = func(int, int) (int, error) { return 7, nil }
		supervisorLinuxClose = func(int) error { return nil }
		supervisorLinuxPIDFDSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error { return nil }
		supervisorLinuxPoll = func([]unix.PollFd, int) (int, error) { return 0, nil }
		supervisorLinuxWaitid = func(int, int, *unix.Siginfo, int, *unix.Rusage) error { return nil }
		require.ErrorContains(t, quiesceLinuxReaper(123, 0, 520*time.Millisecond), "did not become quiescent")
	})
}

func TestLinuxReaperNoChildrenBranches(t *testing.T) {
	for _, test := range []struct {
		name      string
		waitErr   error
		quiescent bool
		wantErr   bool
	}{
		{name: "kernel empty", waitErr: unix.ECHILD, quiescent: true},
		{name: "live or waitable"},
		{name: "interrupted", waitErr: unix.EINTR},
		{name: "failure", waitErr: errors.New("waitid failed"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			preservePlatformSupervisorGlobals(t)
			supervisorLinuxWaitid = func(idType int, id int, _ *unix.Siginfo, options int, _ *unix.Rusage) error {
				require.Equal(t, unix.P_ALL, idType)
				require.Zero(t, id)
				require.Equal(t, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, options)

				return test.waitErr
			}
			quiescent, err := linuxReaperNoChildren()
			require.Equal(t, test.quiescent, quiescent)
			if test.wantErr {
				require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSignalLinuxReaperChildrenBranches(t *testing.T) {
	t.Run("inventory", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		supervisorLinuxReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("inventory failed") }
		_, err := signalLinuxReaperChildren(0, unix.SIGTERM)
		require.ErrorContains(t, err, "inventory failed")
	})

	for _, tc := range []struct {
		name      string
		openErr   error
		signalErr error
		pollErr   error
		closeErr  error
		waitErr   error
		waitOwned bool
		exited    bool
		wantErr   string
		wantCount int
	}{
		{name: "vanished", openErr: unix.ESRCH},
		{name: "open", openErr: errors.New("open failed"), wantErr: "open pidfd"},
		{name: "signal", signalErr: errors.New("signal failed"), wantErr: "signal adopted"},
		{name: "poll", pollErr: errors.New("poll failed"), wantErr: "poll adopted"},
		{name: "close", closeErr: errors.New("close failed"), wantErr: "close pidfd"},
		{name: "active", wantCount: 1},
		{name: "wait owned", waitOwned: true, exited: true},
		{name: "wait ECHILD", exited: true, waitErr: unix.ECHILD},
		{name: "wait error", exited: true, waitErr: errors.New("wait failed"), wantErr: "reap adopted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preservePlatformSupervisorGlobals(t)
			configureLinuxChildrenFixture(t, "123")
			supervisorLinuxPIDFDOpen = func(int, int) (int, error) { return 7, tc.openErr }
			supervisorLinuxPIDFDSendSignal = func(int, unix.Signal, *unix.Siginfo, int) error { return tc.signalErr }
			supervisorLinuxPoll = func(fds []unix.PollFd, _ int) (int, error) {
				if tc.exited {
					fds[0].Revents = unix.POLLHUP
				}

				return 1, tc.pollErr
			}
			supervisorLinuxClose = func(int) error { return tc.closeErr }
			supervisorLinuxWait4 = func(int, *unix.WaitStatus, int, *unix.Rusage) (int, error) { return 123, tc.waitErr }
			waitOwned := 0
			if tc.waitOwned {
				waitOwned = 123
			}
			count, err := signalLinuxReaperChildren(waitOwned, unix.SIGTERM)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.wantCount, count)
			}
		})
	}
}

func TestLinuxReaperInventoryBranches(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		supervisorLinuxReadDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("list failed") }
		_, err := linuxReaperChildren()
		require.ErrorContains(t, err, "list Linux supervisor tasks")
	})

	t.Run("skip files and vanished tasks", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "file"), nil, 0o600))
		require.NoError(t, os.Mkdir(filepath.Join(root, "task"), 0o700))
		supervisorLinuxReadDir = func(string) ([]os.DirEntry, error) { return os.ReadDir(root) }
		supervisorLinuxReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
		children, err := linuxReaperChildren()
		require.NoError(t, err)
		require.Empty(t, children)
	})

	t.Run("read", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		configureLinuxChildrenFixture(t, "")
		supervisorLinuxReadFile = func(string) ([]byte, error) { return nil, errors.New("read failed") }
		_, err := linuxReaperChildren()
		require.ErrorContains(t, err, "read Linux supervisor children")
	})

	for _, raw := range []string{"bad", "0"} {
		t.Run("parse "+raw, func(t *testing.T) {
			preservePlatformSupervisorGlobals(t)
			configureLinuxChildrenFixture(t, raw)
			_, err := linuxReaperChildren()
			require.ErrorContains(t, err, "parse Linux supervisor child PID")
		})
	}

	t.Run("sort and compact", func(t *testing.T) {
		preservePlatformSupervisorGlobals(t)
		configureLinuxChildrenFixture(t, "3 1 3 2")
		children, err := linuxReaperChildren()
		require.NoError(t, err)
		require.Equal(t, []int{1, 2, 3}, children)
	})
}

func configureLinuxChildrenFixture(t *testing.T, children string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "task"), 0o700))
	supervisorLinuxReadDir = func(string) ([]os.DirEntry, error) { return os.ReadDir(root) }
	supervisorLinuxReadFile = func(string) ([]byte, error) { return []byte(children), nil }
}
