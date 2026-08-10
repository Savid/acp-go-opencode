//go:build !unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
)

func currentProcessIdentity() (uint32, uint32, error) {
	uid, gid := os.Geteuid(), os.Getegid()
	if uid < 0 || gid < 0 {
		return 0, 0, errors.New("current process identity is unavailable")
	}

	return uint32(uid), uint32(gid), nil //nolint:gosec // Kernel IDs fit the process-isolation wire width.
}

// applyProcessCredential has nothing to apply here. Ordinary execution asks
// for no credential change anywhere, and an explicit policy has already been
// refused by the Linux-only platform gate before a command exists.
func applyProcessCredential(_ *exec.Cmd, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	return errors.New(errExplicitProcessIsolationPlatform)
}

func closeInheritedOnExec(*os.File) error {
	return errors.New("inherited descriptor hardening is unsupported on this platform")
}
