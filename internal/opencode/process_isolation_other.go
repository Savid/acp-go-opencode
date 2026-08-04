//go:build !unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
)

func validateProcessIsolationPlatform() error {
	return errors.New("process isolation is unsupported on this platform")
}
func applyProcessCredential(*exec.Cmd, *ProcessIsolation) error {
	return errors.New("process isolation is unsupported on this platform")
}
func verifySupervisorIdentity() error {
	return errors.New("process isolation is unsupported on this platform")
}
func closeInheritedOnExec(*os.File) error {
	return errors.New("process isolation is unsupported on this platform")
}
