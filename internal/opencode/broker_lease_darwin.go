//go:build darwin

package opencode

import (
	"os"
	"strconv"
)

// processStartTime reports the kernel's own start time for a PID. It is what
// separates the process a lease named from an unrelated one that reused its
// PID, which liveness alone cannot tell apart.
func processStartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", os.ErrNotExist
	}

	identity, err := currentDarwinProcessIdentity(pid)
	if err != nil {
		return "", err
	}

	return strconv.FormatInt(identity.StartSec, 10) + "." + strconv.FormatInt(int64(identity.StartUsec), 10), nil
}
