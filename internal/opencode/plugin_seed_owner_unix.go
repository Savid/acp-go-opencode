//go:build !windows

package opencode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

var pluginSeedCurrentUID = os.Getuid

// pluginSeedOwnedByCaller admits a cache path only when the calling user owns
// it. The cache holds code OpenCode will execute, so a path another user could
// have written is never copied into a runtime root.
func pluginSeedOwnedByCaller(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("file ownership is unavailable")
	}

	if owner := int(stat.Uid); owner != pluginSeedCurrentUID() {
		return fmt.Errorf("owned by uid %d, want the current user", owner)
	}

	return nil
}
