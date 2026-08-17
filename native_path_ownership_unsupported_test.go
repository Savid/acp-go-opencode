//go:build !linux

package opencodeacp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNativePathOwnershipOnUnsupportedPlatforms proves a platform with no
// ownership model to consult never pretends to isolate. It admits an isolation
// that is already the caller's own identity, because nothing has to change
// hands for that to hold, and refuses any other identity outright rather than
// launching a runtime whose containment it cannot establish.
func TestNativePathOwnershipOnUnsupportedPlatforms(t *testing.T) {
	current := &ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	require.NoError(t, handoffGeneratedNativeTree("ignored", current))
	require.NoError(t, validateNativeOwnedDirectory("ignored", current))

	foreign := &ProcessIsolation{UID: current.UID + 1, GID: current.GID}
	require.ErrorContains(
		t, handoffGeneratedNativeTree("ignored", foreign), "ownership handoff is unsupported",
	)
	require.ErrorContains(
		t, validateNativeOwnedDirectory("ignored", foreign), "ownership validation is unsupported",
	)
}

// nativeOwnedHomeRefusal is why every non-Linux platform refuses a durable
// native home owned by someone other than the isolated identity: there is no
// ownership model to consult, so the home is refused outright.
const nativeOwnedHomeRefusal = "ownership validation is unsupported"
