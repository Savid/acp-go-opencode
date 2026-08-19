package opencodeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNativePathOwnershipWithoutProcessIsolationDoesNothing proves an adapter
// that was never given a process identity performs no ownership work at all.
// The entry point admits a path that does not exist, which can only be true if
// it returned before the first syscall, and a tree it is pointed at keeps the
// identity and mode it already had. This is the short circuit that keeps a
// deployment without isolation free of ownership refusals, and it is the only
// arm of this function that is platform-independent.
func TestNativePathOwnershipWithoutProcessIsolationDoesNothing(t *testing.T) {
	root := t.TempDir()
	absent := filepath.Join(root, "absent")

	require.NoError(t, validateNativeOwnedDirectory(absent, nil))
	require.NoFileExists(t, absent)

	leaf := filepath.Join(root, "leaf")
	require.NoError(t, os.WriteFile(leaf, []byte("x"), 0o600))

	before, err := os.Stat(leaf)
	require.NoError(t, err)

	require.NoError(t, validateNativeOwnedDirectory(root, nil))

	after, err := os.Stat(leaf)
	require.NoError(t, err)
	require.Equal(t, before.Mode(), after.Mode())
	require.Equal(t, before.Sys(), after.Sys())
}
