//go:build unix

package opencode

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestConfigureProcessCommandPlatform(t *testing.T) {
	cmd := exec.Command("cat")
	configureProcessCommandPlatform(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %#v, want Setpgid", cmd.SysProcAttr)
	}
}

func TestSignalProcessGroup(t *testing.T) {
	if err := signalProcessGroup(nil, syscall.SIGTERM); err != nil {
		t.Fatalf("nil command error = %v", err)
	}
	if err := signalProcessGroup(&exec.Cmd{}, syscall.SIGTERM); err != nil {
		t.Fatalf("nil process error = %v", err)
	}

	cmd := &exec.Cmd{Process: &os.Process{Pid: 123}}
	oldGetpgid := syscallGetpgid
	oldKill := syscallKill
	t.Cleanup(func() {
		syscallGetpgid = oldGetpgid
		syscallKill = oldKill
	})

	syscallGetpgid = func(int) (int, error) {
		return 0, syscall.ESRCH
	}
	if err := signalProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("getpgid ESRCH error = %v", err)
	}

	syscallGetpgid = func(int) (int, error) {
		return 0, errors.New("getpgid failed")
	}
	if err := signalProcessGroup(cmd, syscall.SIGTERM); err == nil ||
		!strings.Contains(err.Error(), "getpgid failed") {
		t.Fatalf("getpgid failure error = %v", err)
	}

	syscallGetpgid = func(int) (int, error) {
		return 77, nil
	}
	syscallKill = func(int, syscall.Signal) error {
		return syscall.ESRCH
	}
	if err := signalProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("kill ESRCH error = %v", err)
	}

	syscallKill = func(int, syscall.Signal) error {
		return errors.New("kill failed")
	}
	if err := signalProcessGroup(cmd, syscall.SIGTERM); err == nil ||
		!strings.Contains(err.Error(), "kill failed") {
		t.Fatalf("kill failure error = %v", err)
	}

	killed := false
	syscallKill = func(pid int, sig syscall.Signal) error {
		if pid != -77 || sig != syscall.SIGKILL {
			return fmt.Errorf("unexpected signal pid=%d sig=%v", pid, sig)
		}
		killed = true

		return nil
	}
	if err := killProcess(cmd); err != nil {
		t.Fatalf("killProcess returned error: %v", err)
	}
	if !killed {
		t.Fatal("killProcess did not call syscallKill")
	}

	syscallKill = func(pid int, sig syscall.Signal) error {
		if pid != -77 || sig != syscall.SIGTERM {
			return fmt.Errorf("unexpected signal pid=%d sig=%v", pid, sig)
		}

		return nil
	}
	if err := terminateProcess(cmd); err != nil {
		t.Fatalf("terminateProcess returned error: %v", err)
	}
}
