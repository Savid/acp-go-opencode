//go:build unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func forwardedSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}

func signalCode(sig os.Signal) int {
	if sys, ok := sig.(syscall.Signal); ok {
		return 128 + int(sys)
	}

	return 1
}

func signalExitCode(err *exec.ExitError) int {
	status, ok := err.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0
	}

	return 128 + int(status.Signal())
}
