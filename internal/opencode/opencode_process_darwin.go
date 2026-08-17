//go:build darwin

package opencode

import (
	"os/exec"
	"syscall"
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	var credential *syscall.Credential
	if cmd.SysProcAttr != nil {
		credential = cmd.SysProcAttr.Credential
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: credential}
}
