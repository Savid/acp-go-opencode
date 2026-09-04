//go:build windows

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureRuntimeControlRootAcceptsADirectoryWindowsModesCannotDescribe is
// the Windows half of the trust rule. Go reports 0777 for every writable
// directory on this platform, so a 0700 comparison refused every control root
// the adapter created and no runtime could start at all; a directory is
// admitted here and confinement is the profile ACL's job.
func TestEnsureRuntimeControlRootAcceptsADirectoryWindowsModesCannotDescribe(t *testing.T) {
	root := filepath.Join(t.TempDir(), "control")
	require.NoError(t, ensureRuntimeControlRoot(root))

	loose := filepath.Join(t.TempDir(), "loose")
	require.NoError(t, os.Mkdir(loose, 0o755))
	info, err := os.Lstat(loose)
	require.NoError(t, err)
	require.NotEqual(t, os.FileMode(0o700), info.Mode().Perm(),
		"the mode this platform reports is what the removed comparison read")
	require.NoError(t, ensureRuntimeControlRoot(loose))
}
