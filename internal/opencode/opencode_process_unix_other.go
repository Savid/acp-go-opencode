//go:build unix && !linux && !freebsd && !darwin

package opencode

import (
	"os/exec"
	"syscall"
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	// These platforms have no Pdeathsig equivalent. The supervisor pair owns
	// process-tree cleanup and this process group is its containment boundary.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
