//go:build darwin

package opencode

import (
	"os/exec"
	"syscall"
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
