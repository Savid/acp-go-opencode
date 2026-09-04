//go:build windows

package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// absTestPath builds a host-absolute path from POSIX-looking segments, so a
// test states "an absolute path" rather than a spelling only one platform
// accepts.
func absTestPath(segments ...string) string {
	return filepath.Join(append([]string{`C:\`}, segments...)...)
}

// testLocalPathFromURIPath converts the path component of a file URI to a local
// path. A Windows volume is spelled after the URI's root slash —
// "file:///C:/carrier.mjs" carries "/C:/carrier.mjs" — so the slash comes off
// before the rest is read as a local path.
func testLocalPathFromURIPath(uriPath string) string {
	return filepath.FromSlash(strings.TrimPrefix(uriPath, "/"))
}

// requireOwnerOnlyMode states what Windows has instead of the POSIX bits. There
// is no mode to narrow here: Go synthesises 0777 for every directory and 0666
// for every writable file whatever the ACL says, so a file created 0600 reads
// back 0666 and the path stays private through the profile ACL it inherits.
// Asserting the synthesised value is the only true statement about the mode
// here, and it still catches a file that turned read-only.
func requireOwnerOnlyMode(t *testing.T, path string, _ os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	want := os.FileMode(0o666)
	if info.IsDir() {
		want = os.FileMode(0o777)
	}

	require.Equal(t, want, info.Mode().Perm())
}

// requireCopiedSeedModes states what survives a seed copy on a platform with no
// POSIX modes to carry. Windows reports every writable file as 0666 whether or
// not it is executable and whoever may reach it, so there is no executable bit
// to preserve and no group or world write bit to drop; what the copy owes here
// is that both files arrived.
func requireCopiedSeedModes(t *testing.T, executable, loose string) {
	t.Helper()

	require.FileExists(t, executable, "the executable arrived in the copy")
	require.FileExists(t, loose, "the loose file arrived in the copy")
}
