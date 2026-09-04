//go:build !windows

package homelock

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireOwnerOnlyMode asserts the POSIX permission bits a lock file was
// created with. They are the mechanism that keeps the lock private here.
func requireOwnerOnlyMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm())
}
