//go:build !linux

package opencode

import (
	"errors"
	"os"
)

func duplicateLinuxAgentIdentityLock(*os.File) (*os.File, error) {
	return nil, errors.New("pre-acquired agent identity locks are supported only on Linux")
}
