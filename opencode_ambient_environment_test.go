package opencodeacp

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCaptureAmbientEnvironmentDropsAdapterPrivateCarriers proves the snapshot
// every ordinary launch and every provider-auth broker is built from never
// carries adapter-private state. The scrub happens once, at capture, because
// the snapshot has several consumers and a key dropped at one of them is a key
// that survived at the others. Case is not a defence: an ambient environment is
// not case-normalized on every platform, and a variant spelling that survives
// is the same leak as the canonical one.
func TestCaptureAmbientEnvironmentDropsAdapterPrivateCarriers(t *testing.T) {
	const (
		privateCanary = privateAdapterEnvPrefix + "SPOOF"
		keptCanary    = "ACP_GO_OPENCODE_TEST_ACTUAL_AMBIENT"
	)

	t.Setenv(privateCanary, "leaked")
	t.Setenv(strings.ToLower(privateCanary), "leaked")
	t.Setenv(keptCanary, "kept")

	captured := captureAmbientEnvironment()

	require.NotContains(t, captured, privateCanary)
	require.NotContains(t, captured, strings.ToLower(privateCanary))
	require.Equal(t, "kept", captured[keptCanary],
		"only the private namespace is dropped")
	require.Equal(t, os.Getenv("PATH"), captured["PATH"])
}
