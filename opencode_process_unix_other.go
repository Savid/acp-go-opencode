//go:build unix && !linux && !freebsd && !darwin

package opencodeacp

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureOpenCodeProcess(cmd *exec.Cmd) {
	// These platforms have no Pdeathsig equivalent; parent-death cleanup is
	// best-effort via process-group signalling and stale-lease reaping.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func inspectOpenCodeProcess(int) (processIdentity, error) {
	return processIdentity{}, errors.ErrUnsupported
}
