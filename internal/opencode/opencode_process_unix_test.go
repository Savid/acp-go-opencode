//go:build unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestOpenCodeProcessSignalBranches(t *testing.T) {
	oldGetpgid := openCodeSyscallGetpgid
	oldKill := openCodeSyscallKill
	t.Cleanup(func() {
		openCodeSyscallGetpgid = oldGetpgid
		openCodeSyscallKill = oldKill
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
}
