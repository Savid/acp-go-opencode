package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureRuntimeControlRootCreatesAndValidatesProtectedDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "control")
	require.NoError(t, ensureRuntimeControlRoot(root))
	info, err := os.Lstat(root)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	require.NoError(t, ensureRuntimeControlRoot(root))
}

func TestEnsureRuntimeControlRootRefusesRedirectsAndUnsafeExistingPaths(t *testing.T) {
	parent := t.TempDir()
	decoy := filepath.Join(parent, "decoy")
	require.NoError(t, os.Mkdir(decoy, 0o755))
	symlink := filepath.Join(parent, "symlink")
	require.NoError(t, os.Symlink(decoy, symlink))
	require.Error(t, ensureRuntimeControlRoot(symlink))
	info, err := os.Stat(decoy)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	unsafe := filepath.Join(parent, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0o755))
	require.Error(t, ensureRuntimeControlRoot(unsafe))
	info, err = os.Stat(unsafe)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	regular := filepath.Join(parent, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("state"), 0o600))
	require.Error(t, ensureRuntimeControlRoot(regular))
	require.Error(t, ensureRuntimeControlRoot(filepath.Join(parent, "missing", "control")))
}

func TestEnsureRuntimeControlRootReportsCreationAndInspectionFailures(t *testing.T) {
	originalLstat, originalMkdir, originalValidate := runtimeControlLstat, runtimeControlMkdir, runtimeControlValidateOwner
	t.Cleanup(func() {
		runtimeControlLstat, runtimeControlMkdir, runtimeControlValidateOwner = originalLstat, originalMkdir, originalValidate
	})
	want := errors.New("filesystem")
	runtimeControlLstat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	runtimeControlMkdir = func(string, os.FileMode) error { return want }
	require.ErrorIs(t, ensureRuntimeControlRoot("/control"), want)

	calls := 0
	runtimeControlLstat = func(string) (os.FileInfo, error) {
		calls++
		if calls == 1 {
			return nil, os.ErrNotExist
		}

		return nil, want
	}
	runtimeControlMkdir = func(string, os.FileMode) error { return nil }
	require.ErrorIs(t, ensureRuntimeControlRoot("/control"), want)

	runtimeControlLstat, runtimeControlMkdir = originalLstat, originalMkdir
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))
	runtimeControlValidateOwner = func(os.FileInfo) error { return want }
	require.ErrorIs(t, ensureRuntimeControlRoot(root), want)
}
