//go:build linux

package opencode

import (
	"errors"
	"os"
	"syscall"
)

func validateRuntimeControlRootOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		return errors.New("OpenCode runtime control root must be owned by the trusted identity")
	}

	return nil
}
