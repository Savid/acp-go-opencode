//go:build !windows

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// absTestPath builds a host-absolute path from POSIX-looking segments, so a
// test states "an absolute path" rather than a spelling only one platform
// accepts.
func absTestPath(segments ...string) string {
	return filepath.Join(append([]string{"/"}, segments...)...)
}

// testLocalPathFromURIPath converts the path component of a file URI to a local
// path. Every absolute URI path already spells an absolute local path here.
func testLocalPathFromURIPath(uriPath string) string {
	return filepath.FromSlash(uriPath)
}

// requireOwnerOnlyMode asserts the POSIX permission bits a path was created
// with. They are the mechanism that keeps the path private here.
func requireOwnerOnlyMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm())
}

// requireCopiedSeedModes states the POSIX bits a copied seed tree carries: the
// executable bit survives the copy, and the group and world write bits the
// source tree had are dropped.
func requireCopiedSeedModes(t *testing.T, executable, loose string) {
	t.Helper()

	info, err := os.Lstat(executable)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the executable bit survives the copy")

	looseInfo, err := os.Lstat(loose)
	require.NoError(t, err)
	require.Zero(t, looseInfo.Mode().Perm()&pluginSeedLooseModeBits, "group and world write bits are dropped")
}
