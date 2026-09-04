//go:build windows

package opencode

import (
	"path/filepath"
	"strings"
)

// rootedPath reports whether value names a path from a filesystem root rather
// than from wherever the caller happens to be. Windows spells four such paths
// and filepath.IsAbs accepts only one of them: `C:\x` and `\\host\share\x` are
// absolute, while `\x` and `/x` are rooted on the current volume and `C:x` is
// rooted on that volume's own current directory. A confinement written as
// filepath.IsAbs alone would take the other three for ordinary relative names
// and admit them — so everything that is not purely relative is rooted here.
// The POSIX answer is unchanged; this only adds the refusals Windows needs to
// reach the same rule.
func rootedPath(value string) bool {
	return filepath.IsAbs(value) ||
		filepath.VolumeName(value) != "" ||
		strings.HasPrefix(value, `\`) ||
		strings.HasPrefix(value, "/")
}
