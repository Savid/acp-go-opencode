//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package homelock

import (
	"fmt"
	"os"
	"runtime"
)

func platformLock(*os.File) error {
	return fmt.Errorf("%w: %s", ErrRuntimeLockUnsupported, runtime.GOOS)
}

func platformUnlock(*os.File) error { return nil }
