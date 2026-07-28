//go:build !windows

package opencode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	browserShimMkdirTemp = os.MkdirTemp
	browserShimWriteFile = os.WriteFile
)

// NewBrowserShim materialises the shim beneath parent. os.MkdirTemp creates the
// directory 0700, and every launcher name becomes an executable no-op, so a
// harness that execs one directly reaches this directory before any real
// browser launcher on PATH.
func NewBrowserShim(parent string) (*BrowserShim, error) {
	dir, err := browserShimMkdirTemp(parent, browserShimPrefix)
	if err != nil {
		return nil, fmt.Errorf("create browser shim directory: %w", err)
	}

	for _, name := range browserLauncherNames {
		if writeErr := browserShimWriteFile(filepath.Join(dir, name), browserShimScript, 0o700); writeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("write browser shim %s: %w", name, writeErr),
				os.RemoveAll(dir),
			)
		}
	}

	return &BrowserShim{dir: dir}, nil
}
