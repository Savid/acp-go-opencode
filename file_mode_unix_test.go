//go:build !windows

package opencodeacp

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireOwnerOnlyMode asserts the POSIX permission bits a path was created
// with. They are the mechanism that keeps the path private here, so the test
// reads them exactly.
func requireOwnerOnlyMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm())
}
