//go:build linux

package opencode

import "os/exec"

func startOpenCodeProcess(cmd *exec.Cmd) (*supervisorWaiter, error) {
	configureOpenCodeProcess(cmd)

	// Resolve the wait seam here rather than inside the creator thread. The
	// thread blocks until the caller releases it, which can be long after the
	// launch returned, and reading a package-level seam that late races with
	// anyone restoring it.
	waitCommand := openCodeWaitCommand

	beginWait := make(chan struct{})
	waitDone, err := startCommandOnCreatorThread(cmd.Start, func() error {
		<-beginWait

		return waitCommand(cmd)
	})
	if err != nil {
		return nil, err
	}

	return newSupervisorWaiterResult(waitDone, func() { close(beginWait) }, true), nil
}
