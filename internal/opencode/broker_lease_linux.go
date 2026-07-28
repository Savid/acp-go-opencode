//go:build linux

package opencode

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

var leaseProcReadFile = os.ReadFile

// processStartTime reads field 22 of /proc/<pid>/stat, the process's start time
// in clock ticks since boot. It is what separates the process a lease named
// from an unrelated one that reused its PID, which liveness alone cannot tell
// apart. The scan starts after the last `)` because the second field is the
// executable name and may itself contain spaces and parentheses.
func processStartTime(pid int) (string, error) {
	if pid <= 0 {
		return "", os.ErrNotExist
	}

	contents, err := leaseProcReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}

	stat := string(contents)

	closing := strings.LastIndex(stat, ")")
	if closing < 0 {
		return "", errors.New("malformed proc stat")
	}

	fields := strings.Fields(stat[closing+1:])

	// Fields 3 onward follow the name, so the start time is the twentieth of
	// them.
	const startTimeOffset = 19

	if len(fields) <= startTimeOffset {
		return "", errors.New("malformed proc stat")
	}

	return fields[startTimeOffset], nil
}
