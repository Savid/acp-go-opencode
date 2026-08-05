//go:build !linux

package opencode

import (
	"fmt"
	"os"
	"runtime"
)

func handoffGeneratedNativeTree(_ string, isolation *ProcessIsolation) error {
	if isolation == nil {
		return nil
	}

	if isolation.UID == uint32(os.Geteuid()) && //nolint:gosec // Effective Unix IDs fit the process-isolation wire width.
		isolation.GID == uint32(os.Getegid()) { //nolint:gosec // Effective Unix IDs fit the process-isolation wire width.
		return nil
	}

	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}
