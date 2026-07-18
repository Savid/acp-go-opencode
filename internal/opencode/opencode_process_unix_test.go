//go:build unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestOpenCodeProcessSignalBranches(t *testing.T) {
	oldKill := openCodeSyscallKill
	oldSignal := openCodeProcessSignal
	t.Cleanup(func() {
		openCodeSyscallKill = oldKill
		openCodeProcessSignal = oldSignal
	})

	if err := terminateOpenCodeProcess(nil, 0); err != nil {
		t.Fatalf("terminate nil: %v", err)
	}
	if err := killOpenCodeProcess(nil, 0); err != nil {
		t.Fatalf("kill nil: %v", err)
	}
	command := exec.Command("/bin/true")
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	process := command.Process

	if err := signalOpenCodeProcessGroup(process, 0, syscall.SIGTERM); err == nil {
		t.Fatal("missing captured process group accepted")
	}
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := signalOpenCodeProcessGroup(process, 123, syscall.SIGTERM); err != nil {
		t.Fatalf("ESRCH kill: %v", err)
	}
	openCodeProcessSignal = func(*os.Process, syscall.Signal) error { return errors.New("direct signal failed") }
	if err := signalOpenCodeProcessGroup(process, 123, syscall.SIGTERM); err == nil || !strings.Contains(err.Error(), "direct signal failed") {
		t.Fatalf("unexpected direct signal result: %v", err)
	}
	openCodeProcessSignal = oldSignal
	openCodeSyscallKill = func(int, syscall.Signal) error { return errors.New("kill failed") }
	if err := signalOpenCodeProcessGroup(process, 123, syscall.SIGTERM); err == nil {
		t.Fatal("kill error ignored")
	}
	var signaledPID int
	openCodeSyscallKill = func(pid int, _ syscall.Signal) error {
		signaledPID = pid

		return nil
	}
	if err := signalOpenCodeProcessGroup(process, 123, syscall.SIGTERM); err != nil {
		t.Fatalf("signal success: %v", err)
	}
	if signaledPID != -123 {
		t.Fatalf("signaled process group = %d, want captured -123", signaledPID)
	}
	if err := killOpenCodeProcess(process, 123); err != nil {
		t.Fatalf("killOpenCodeProcess: %v", err)
	}
}
