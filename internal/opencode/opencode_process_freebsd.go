//go:build freebsd

package opencode

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func inspectOpenCodeProcess(int) (processIdentity, error) {
	return processIdentity{}, errors.ErrUnsupported
}
