package opencodeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScratchParent(t *testing.T) {
	t.Parallel()

	require.Equal(t, os.TempDir(), scratchParent(""), "empty scratch dir must resolve to the system temp directory")
	require.Equal(t, absTestPath("custom", "scratch"), scratchParent(absTestPath("custom", "scratch")), "non-empty scratch dir must pass through unchanged")
}

func TestEnsureScratchParent(t *testing.T) {
	t.Parallel()

	t.Run("empty resolves to system temp", func(t *testing.T) {
		t.Parallel()

		parent, err := ensureScratchParent("")
		require.NoError(t, err)
		require.Equal(t, os.TempDir(), parent)
	})

	t.Run("missing nested dir created 0700", func(t *testing.T) {
		t.Parallel()

		nested := filepath.Join(t.TempDir(), "a", "b", "scratch")

		parent, err := ensureScratchParent(nested)
		require.NoError(t, err)
		require.Equal(t, nested, parent)

		info, statErr := os.Stat(nested)
		require.NoError(t, statErr)
		require.True(t, info.IsDir())
		requireOwnerOnlyMode(t, nested, 0o700)
	})

	t.Run("regular-file parent errors", func(t *testing.T) {
		t.Parallel()

		file := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

		_, err := ensureScratchParent(filepath.Join(file, "child"))
		require.Error(t, err)
	})
}
