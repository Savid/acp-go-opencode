//go:build !unix

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func forwardedSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func signalCode(sig os.Signal) int {
	if sig == os.Interrupt {
		return 130
	}
	if sig == syscall.SIGTERM {
		return 128 + int(syscall.SIGTERM)
	}

	return 1
}

func signalExitCode(*exec.ExitError) int {
	return 0
}
