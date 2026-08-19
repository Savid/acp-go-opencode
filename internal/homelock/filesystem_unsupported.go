//go:build !linux && !darwin && !freebsd && !openbsd && !windows

package homelock

import (
	"fmt"
	"os"
	"runtime"
)

func validateLockFilesystem(*os.File) error {
	return fmt.Errorf("%w: filesystem validation on %s", ErrRuntimeLockUnsupported, runtime.GOOS)
}
