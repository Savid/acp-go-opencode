//go:build unix && !linux && !freebsd && !darwin

package opencode

import (
	"os/exec"
	"syscall"
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	// These platforms have no Pdeathsig equivalent. The supervisor pair owns
	// process-tree cleanup and this process group is its containment boundary.
	var credential *syscall.Credential
	if cmd.SysProcAttr != nil {
		credential = cmd.SysProcAttr.Credential
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: credential}
}
