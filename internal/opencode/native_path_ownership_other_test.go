//go:build !linux

package opencode

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnsupportedPlatformOwnershipAndIdentityLockBranches(t *testing.T) {
	require.NoError(t, handoffGeneratedNativeTree("ignored", nil))
	require.NoError(t, handoffGeneratedNativeTree("ignored", &ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}))
	require.ErrorContains(t, handoffGeneratedNativeTree("ignored", &ProcessIsolation{UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid())}), "ownership handoff is unsupported")
	_, err := duplicateLinuxAgentIdentityLock(nil)
	require.ErrorContains(t, err, "only on Linux")
}
