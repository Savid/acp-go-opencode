//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package opencode

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

type guardianContainment struct{}
type livenessContainment struct{}

func unsupportedContainment() error {
	return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("OpenCode runtime containment is unsupported on %s", runtime.GOOS))
}

func newGuardianContainment(supervisorConfig) (*guardianContainment, error) {
	return nil, unsupportedContainment()
}
func (*guardianContainment) Name() string { return "" }
func (*guardianContainment) Close() error { return nil }
func (*guardianContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}
func openLivenessContainment(supervisorConfig) (*livenessContainment, error) {
	return nil, unsupportedContainment()
}
func (*livenessContainment) Start(*exec.Cmd) error { return unsupportedContainment() }
func (*livenessContainment) Wait() <-chan error {
	result := make(chan error, 1)
	result <- unsupportedContainment()

	return result
}
func (*livenessContainment) Close() error { return nil }
func (*livenessContainment) Quiesce(int, time.Duration) error {
	return unsupportedContainment()
}
func configureIndependentSupervisor(*exec.Cmd) {}

func startIndependentSupervisor(cmd *exec.Cmd) error {
	return cmd.Start()
}
func releaseIndependentSupervisorWaiter(_ *exec.Cmd, waiter *supervisorWaiter) (int, error) {
	waiter.start()

	return 0, unsupportedContainment()
}
func querySupervisorProcessSnapshot(string) (int, bool) { return 0, false }
