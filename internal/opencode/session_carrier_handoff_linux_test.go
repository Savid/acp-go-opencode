//go:build linux

package opencode

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionCarrierHandoffRequiresTraversableFixtureAncestry pins both sides
// of the carrier's two-principal filesystem boundary. A testing.T.TempDir root
// has a private ancestor and production must refuse it; the generated-runtime
// fixture lives directly beneath the suite's traversable temp root and reaches
// the real ownership handoff.
func TestSessionCarrierHandoffRequiresTraversableFixtureAncestry(t *testing.T) {
	nativeOwnershipRequireRoot(t)

	t.Run("private testing ancestor is refused", func(t *testing.T) {
		runtimeRoot := filepath.Join(t.TempDir(), "runtime")
		plugin, cleanup, err := materializeSessionCarrierPlugin(runtimeRoot, nativeOwnershipIsolation())
		require.ErrorContains(t, err, generatedTreeHandoffRefusal)
		require.Empty(t, plugin.URL)
		require.Nil(t, cleanup)
	})

	t.Run("generated runtime fixture is admitted", func(t *testing.T) {
		runtimeRoot := testGeneratedTempDir(t)
		plugin, cleanup, err := materializeSessionCarrierPlugin(runtimeRoot, nativeOwnershipIsolation())
		require.NoError(t, err)
		require.NotEmpty(t, plugin.URL)
		require.NotNil(t, cleanup)
		require.NoError(t, cleanup())
	})
}
