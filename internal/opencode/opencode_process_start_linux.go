//go:build linux

package opencode

import "os/exec"

func startOpenCodeProcess(cmd *exec.Cmd) (*supervisorWaiter, error) {
	configureOpenCodeProcess(cmd)

	beginWait := make(chan struct{})
	waitDone, err := startCommandOnCreatorThread(cmd.Start, func() error {
		<-beginWait

		return openCodeWaitCommand(cmd)
	})
	if err != nil {
		return nil, err
	}

	return newSupervisorWaiterResult(waitDone, func() { close(beginWait) }, true), nil
}
