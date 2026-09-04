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
	require.NoError(t, ensureRuntimeControlRoot(root))
}

// TestEnsureRuntimeControlRootRefusesPathsThatAreNotADirectory covers what
// every platform judges the same way. What "this user alone may write" means
// is platform-specific and is pinned beside the platform's own predicate.
func TestEnsureRuntimeControlRootRefusesPathsThatAreNotADirectory(t *testing.T) {
	parent := t.TempDir()

	regular := filepath.Join(parent, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("state"), 0o600))
	require.Error(t, ensureRuntimeControlRoot(regular))
	require.Error(t, ensureRuntimeControlRoot(filepath.Join(parent, "missing", "control")))
}

func TestEnsureRuntimeControlRootReportsCreationAndInspectionFailures(t *testing.T) {
	originalLstat, originalMkdir := runtimeControlLstat, runtimeControlMkdir
	t.Cleanup(func() {
		runtimeControlLstat, runtimeControlMkdir = originalLstat, originalMkdir
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
}
