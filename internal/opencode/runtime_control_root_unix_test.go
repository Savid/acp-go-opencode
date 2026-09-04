//go:build !windows

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureRuntimeControlRootRequiresModeSevenHundred pins what a trusted
// control root means where the platform has POSIX modes: exactly 0700, on the
// path itself rather than on what a symbolic link points at.
func TestEnsureRuntimeControlRootRequiresModeSevenHundred(t *testing.T) {
	root := filepath.Join(t.TempDir(), "control")
	require.NoError(t, ensureRuntimeControlRoot(root))
	info, err := os.Lstat(root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	parent := t.TempDir()
	decoy := filepath.Join(parent, "decoy")
	require.NoError(t, os.Mkdir(decoy, 0o755))
	symlink := filepath.Join(parent, "symlink")
	require.NoError(t, os.Symlink(decoy, symlink))
	require.Error(t, ensureRuntimeControlRoot(symlink))
	info, err = os.Stat(decoy)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	unsafe := filepath.Join(parent, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0o755))
	require.Error(t, ensureRuntimeControlRoot(unsafe))
	info, err = os.Stat(unsafe)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}
