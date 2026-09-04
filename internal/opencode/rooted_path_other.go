//go:build !windows

package opencode

import "path/filepath"

// rootedPath reports whether value names a path from a filesystem root rather
// than from wherever the caller happens to be. On POSIX there is one such
// spelling and filepath.IsAbs is the whole answer.
func rootedPath(value string) bool {
	return filepath.IsAbs(value)
}
