//go:build unix

package opencode

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenCodeProcessSignalBranches(t *testing.T) {
	oldGetpgid := openCodeSyscallGetpgid
	oldKill := openCodeSyscallKill
	oldInspect := openCodeInspectProcess
	t.Cleanup(func() {
		openCodeSyscallGetpgid = oldGetpgid
		openCodeSyscallKill = oldKill
		openCodeInspectProcess = oldInspect
	})

	if err := terminateOpenCodeProcess(nil); err != nil {
		t.Fatalf("terminate nil: %v", err)
	}
	if err := killOpenCodeProcess(nil); err != nil {
		t.Fatalf("kill nil: %v", err)
	}
	cmd := &exec.Cmd{Process: &os.Process{Pid: 123}}

	openCodeSyscallGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("ESRCH getpgid: %v", err)
	}
	openCodeSyscallGetpgid = func(int) (int, error) { return 0, errors.New("getpgid failed") }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err == nil {
		t.Fatal("getpgid error ignored")
	}
	openCodeSyscallGetpgid = func(int) (int, error) { return 123, nil }
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("ESRCH kill: %v", err)
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return errors.New("kill failed") }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err == nil {
		t.Fatal("kill error ignored")
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return nil }
	if err := signalOpenCodeProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("signal success: %v", err)
	}
	if err := killOpenCodeProcess(cmd); err != nil {
		t.Fatalf("killOpenCodeProcess: %v", err)
	}

	if err := killProcessID(0); err != nil {
		t.Fatalf("kill zero pid: %v", err)
	}
	openCodeSyscallGetpgid = func(int) (int, error) { return 0, errors.New("no group") }
	openCodeSyscallKill = func(pid int, signal syscall.Signal) error {
		if pid != 123 || signal != syscall.SIGTERM {
			t.Fatalf("kill pid=%d signal=%v", pid, signal)
		}

		return nil
	}
	if err := killProcessID(123); err != nil {
		t.Fatalf("killProcessID fallback: %v", err)
	}
	openCodeSyscallGetpgid = func(int) (int, error) { return 123, nil }
	openCodeSyscallKill = func(int, syscall.Signal) error { return errors.New("kill failed") }
	if err := killProcessID(123); err == nil {
		t.Fatal("killProcessID error ignored")
	}

	root := t.TempDir()
	xdg, err := CreateXDGDirs(root, "lease-log")
	if err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(xdg.State, LeaseFileName)
	openCodeInspectProcess = func(int) (processIdentity, error) {
		return processIdentity{
			StartTime: "start",
			Cmdline:   []string{"opencode", "serve"},
			Env: map[string]string{
				"XDG_STATE_HOME":           xdg.State,
				"OPENCODE_SERVER_PASSWORD": "secret",
			},
		}, nil
	}
	openCodeSyscallGetpgid = func(int) (int, error) { return 123, nil }
	openCodeSyscallKill = func(int, syscall.Signal) error { return errors.New("kill failed") }
	if err := writeLease(xdg.State, serverLease{
		PID:              123,
		PasswordHash:     passwordHash("secret"),
		XDGRoot:          xdg.Root,
		ProcessStartTime: "start",
	}); err != nil {
		t.Fatal(err)
	}
	ReapLeaseFile(leasePath, slog.New(slog.DiscardHandler))
	if _, err := os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease after logged reap = %v", err)
	}
}
