//go:build windows

package homelock

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireOwnerOnlyMode states what Windows has instead of the POSIX bits. There
// is no mode to narrow here: Go synthesises 0666 for every writable file
// whatever the ACL says, so a lock opened 0600 reads back 0666 and stays
// private through the profile ACL it inherits. Asserting the synthesised value
// is the only true statement about the mode here, and it still catches a lock
// that turned read-only.
func requireOwnerOnlyMode(t *testing.T, path string, _ fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o666), info.Mode().Perm())
}
