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
	if isolation.UID == uint32(os.Geteuid()) && isolation.GID == uint32(os.Getegid()) {
		return nil
	}

	return fmt.Errorf("native path ownership handoff is unsupported on %s", runtime.GOOS)
}
