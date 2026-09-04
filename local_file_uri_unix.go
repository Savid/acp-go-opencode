//go:build !windows

package opencodeacp

import (
	"path/filepath"
	"strings"
)

// localPathFromURIPath converts the path component of a file URI to a local
// path, or returns "" when it names none. Every absolute URI path already
// spells an absolute local path here.
func localPathFromURIPath(uriPath string) string {
	if !strings.HasPrefix(uriPath, "/") {
		return ""
	}

	return filepath.FromSlash(uriPath)
}
