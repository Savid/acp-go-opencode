//go:build !linux

package opencodeacp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativePathOwnershipPortableBranches(t *testing.T) {
	require.NoError(t, handoffGeneratedNativeTree("ignored", nil))
	require.NoError(t, validateNativeOwnedDirectory("ignored", nil))
	sameIdentity := &ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	require.NoError(t, handoffGeneratedNativeTree("ignored", sameIdentity))
	require.NoError(t, validateNativeOwnedDirectory("ignored", sameIdentity))
	differentIdentity := &ProcessIsolation{UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid())}
	require.ErrorContains(t, handoffGeneratedNativeTree("ignored", differentIdentity), "ownership handoff is unsupported")
	require.ErrorContains(t, validateNativeOwnedDirectory("ignored", differentIdentity), "ownership validation is unsupported")
}
