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
func sharedNativeIdentity(uint32) bool             { return false }
func sharedProcessIdentity(*ProcessIsolation) bool { return false }
func applyProcessCredential(*exec.Cmd, *ProcessIsolation) error {
	return errors.New("process isolation is unsupported on this platform")
}
func closeInheritedOnExec(*os.File) error {
	return errors.New("process isolation is unsupported on this platform")
}
