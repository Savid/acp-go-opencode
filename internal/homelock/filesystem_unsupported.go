//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package homelock

import (
	"fmt"
	"os"
	"runtime"
)

func validateLockFilesystem(*os.File) error {
	return fmt.Errorf("OpenCode writable-home filesystem validation is unsupported on %s", runtime.GOOS)
}
