//go:build unix

package opencodeacp

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenCodeProcessUnixSignalFaultBranches(t *testing.T) {
	getpgid := openCodeSyscallGetpgid
	kill := openCodeSyscallKill
	t.Cleanup(func() {
		openCodeSyscallGetpgid = getpgid
		openCodeSyscallKill = kill
	})

	if err := terminateOpenCodeProcess(nil); err != nil {
		t.Fatalf("terminate nil: %v", err)
	}
	if err := killOpenCodeProcess(&exec.Cmd{}); err != nil {
		t.Fatalf("kill without process: %v", err)
	}

	cmd := &exec.Cmd{Process: &os.Process{Pid: 1234}}
	openCodeSyscallGetpgid = func(int) (int, error) {
		return 0, syscall.ESRCH
	}
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("ESRCH getpgid: %v", err)
	}
	openCodeSyscallGetpgid = func(int) (int, error) {
		return 0, syscall.EPERM
	}
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("getpgid error = %v", err)
	}

	openCodeSyscallGetpgid = func(int) (int, error) { return 22, nil }
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("ESRCH kill: %v", err)
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.EPERM }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill error = %v", err)
	}
	var gotPID int
	var gotSignal syscall.Signal
	openCodeSyscallKill = func(pid int, signal syscall.Signal) error {
		gotPID = pid
		gotSignal = signal
		return nil
	}
	if err := killOpenCodeProcess(cmd); err != nil {
		t.Fatalf("kill process group: %v", err)
	}
	if gotPID != -22 || gotSignal != syscall.SIGKILL {
		t.Fatalf("kill pid/signal = %d/%v", gotPID, gotSignal)
	}

	if err := killProcessID(0); err != nil {
		t.Fatalf("kill pid 0: %v", err)
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := killProcessID(1234); err != nil {
		t.Fatalf("kill pid ESRCH: %v", err)
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.EPERM }
	if err := killProcessID(1234); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill pid error = %v", err)
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
	if err := killProcessID(1234); err != nil {
		t.Fatalf("kill pid success = %v", err)
	}
}

func TestReapStaleLeasesSignalsProcessID(t *testing.T) {
	getpgid := openCodeSyscallGetpgid
	kill := openCodeSyscallKill
	t.Cleanup(func() {
		openCodeSyscallGetpgid = getpgid
		openCodeSyscallKill = kill
	})

	root := t.TempDir()
	stateDir := filepath.Join(root, "session", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeLease(stateDir, serverLease{PID: 1234}); err != nil {
		t.Fatal(err)
	}
	openCodeSyscallKill = func(int, syscall.Signal) error {
		return syscall.EPERM
	}
	if err := reapStaleLeases(root, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("reap stale lease: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, leaseFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease after reap err = %v", err)
	}
}
