//go:build !linux

package opencodeacp

import (
	"fmt"
	"os"
	"runtime"
)

func handoffGeneratedNativeTreePlatform(_ string, uid uint32, gid uint32) error {
	if uid == uint32(os.Geteuid()) && //nolint:gosec // Effective Unix IDs fit the process-isolation wire width.
		gid == uint32(os.Getegid()) { //nolint:gosec // Effective Unix IDs fit the process-isolation wire width.
		return nil
	}

	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}

func validateNativeOwnedDirectoryPlatform(_ string, uid uint32, gid uint32) error {
	if uid == uint32(os.Geteuid()) && //nolint:gosec // Effective Unix IDs fit the process-isolation wire width.
		gid == uint32(os.Getegid()) { //nolint:gosec // Effective Unix IDs fit the process-isolation wire width.
		return nil
	}

	return fmt.Errorf("native path ownership validation is unsupported on %s", runtime.GOOS)
}
