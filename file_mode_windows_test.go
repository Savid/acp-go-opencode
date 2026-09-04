//go:build windows

package opencodeacp

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireOwnerOnlyMode states what Windows has instead of the POSIX bits. There
// is no mode to narrow here: Go synthesises 0777 for every directory and 0666
// for every writable file whatever the ACL says, so a chmod to 0700 records
// nothing and the path stays private through the profile ACL it inherits. The
// synthesised value is the only true statement about the mode on this platform,
// and asserting it keeps a mode that did change from passing silently.
func requireOwnerOnlyMode(t *testing.T, path string, _ fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	want := fs.FileMode(0o666)
	if info.IsDir() {
		want = fs.FileMode(0o777)
	}

	require.Equal(t, want, info.Mode().Perm())
}
