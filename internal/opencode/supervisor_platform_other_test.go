//go:build !linux

package opencode

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNonLinuxSupervisorPlatformAdoptsNoIdentityAuthority pins what the
// identity seams answer on a platform with no agent-identity registry. There is
// nothing to adopt, so adoption yields the inert lock rather than an error: a
// launch here is ordinary execution, which claims no authority to hand down and
// therefore has none to reject.
func TestNonLinuxSupervisorPlatformAdoptsNoIdentityAuthority(t *testing.T) {
	preserveSupervisorGlobals(t)
	configureSupervisorPlatform()

	lock, err := supervisorAdoptIdentityLock(65534)
	require.NoError(t, err)
	require.Nil(t, lock.InheritedFile())
	require.NoError(t, lock.Close())

	domain, err := supervisorAdoptAuthorityDomain(65534)
	require.NoError(t, err)
	require.Nil(t, domain.InheritedFile())
	require.NoError(t, domain.Close())
}
