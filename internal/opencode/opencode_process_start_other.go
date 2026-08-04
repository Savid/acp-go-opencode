//go:build !linux

package opencode

import "os/exec"

func startOpenCodeProcess(cmd *exec.Cmd) (*supervisorWaiter, error) {
	configureOpenCodeProcess(cmd)

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return newSupervisorWaiterFunc(func() error { return openCodeWaitCommand(cmd) }, true), nil
}
