//go:build !windows

package opencodeacp

import (
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDirectoryFlushOutcome states what a failing directory-flush seam does
// to the write that published a file. On POSIX the flush is real, so the failure
// reaches the caller.
func requireDirectoryFlushOutcome(t *testing.T, err error, contains string) {
	t.Helper()
	require.ErrorContains(t, err, contains)
}

// requireOwnerOnlyMode asserts the POSIX permission bits a path was created
// with. They are the mechanism that keeps the path private here, so the test
// reads them exactly.
func requireOwnerOnlyMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm())
}
