//go:build windows

package opencodeacp

import (
	"path/filepath"
	"strings"
)

// localPathFromURIPath converts the path component of a file URI to a local
// path, or returns "" when it names none. A Windows volume is spelled after the
// URI's root slash — "file:///C:/shot.png" carries "/C:/shot.png" — so the slash
// comes off before the rest is read as a local path. Whatever is left must name
// a volume or a UNC share: a URI path that names neither is refused rather than
// silently rooted on whichever drive this process happens to sit on.
func localPathFromURIPath(uriPath string) string {
	local := filepath.FromSlash(strings.TrimPrefix(uriPath, "/"))
	if !filepath.IsAbs(local) {
		return ""
	}

	return local
}
