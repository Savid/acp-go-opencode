//go:build linux

package opencode

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

func TestProviderCreatorLinuxRuntimeStartRetainsCreatorThreadThroughWait(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done")
	waiter, err := startOpenCodeProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	for range 512 {
		runtime.Gosched()
		if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
			t.Fatalf("runtime child exited while its creator-thread Wait was active: %v", err)
		}
	}

	waiter.start()
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := <-waiter.result(); err == nil {
		t.Fatal("killed runtime child returned a successful wait result")
	}
}
