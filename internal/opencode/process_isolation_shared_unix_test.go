//go:build unix

package opencode

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// withSharedIdentitySeams restores the seams the shared-identity arm is
// selected through. The arm is a property of the account the supervisor runs
// under, which a test cannot change for itself, so every case names both
// identities explicitly instead of inheriting whichever one the suite was
// started with.
func withSharedIdentitySeams(t *testing.T) {
	t.Helper()
	platform := processIsolationGOOS
	uid := processEffectiveUID
	gid := processEffectiveGID
	t.Cleanup(func() {
		processIsolationGOOS = platform
		processEffectiveUID = uid
		processEffectiveGID = gid
	})
}

// sharedTestIsolation is the canonical shared-identity shape: the identity the
// supervisor already runs as, no capabilities, and no standalone owner fields.
func sharedTestIsolation() *ProcessIsolation {
	return &ProcessIsolation{
		UID: 1000, GID: 1000,
		BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
	}
}

// TestSharedProcessIdentityNamesOnlyTheSupervisorsOwnLinuxIdentity pins which
// launches the shared arm claims. It claims exactly one: a Linux launch whose
// native identity is the nonzero identity this process already holds. A
// different identity still has a boundary to cross, root holds the trusted
// identity the isolated arm descends from and can never name the native uid the
// policy requires to be nonzero, and the Darwin backend states its own boundary
// and is not this arm's to redescribe. The group is deliberately not consulted:
// the mode decision is about privilege between the two ends of the launch, and
// that is an identity question.
func TestSharedProcessIdentityNamesOnlyTheSupervisorsOwnLinuxIdentity(t *testing.T) {
	withSharedIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }

	require.False(t, sharedProcessIdentity(nil))
	require.True(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 1000}))
	require.True(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 2000}),
		"the mode decision reads the identity, not the group it runs in")
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1001, GID: 1000}))

	processEffectiveUID = func() int { return 0 }
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 0, GID: 0}),
		"a trusted root supervisor must never enter the shared arm")
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 1000}))

	processEffectiveUID = func() int { return 1000 }
	processIsolationGOOS = "darwin"
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000, GID: 1000}))
}

// TestSharedIdentityIsolationCarriesNoStandaloneOwnerFields proves which
// disposition the policy accepts for an identity the supervisor already holds.
// The standalone record binds an identity to an owner by proving no live task
// runs as it, and the supervisor asking for the record is such a task, so owner
// fields promise something this arm can never write. The canonical shape is
// therefore empty, and a populated one is refused with the remedy rather than
// accepted and abandoned later. An identity the supervisor does not hold keeps
// the original requirement word for word.
func TestSharedIdentityIsolationCarriesNoStandaloneOwnerFields(t *testing.T) {
	withSharedIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }

	require.NoError(t, validateProcessIsolation(sharedTestIsolation()))

	owned := sharedTestIsolation()
	owned.StandaloneOwnerID = "test-owner"
	require.EqualError(t, validateProcessIsolation(owned),
		"standalone owner fields describe an identity the supervisor already holds; "+
			sharedIdentitySupervisorRemedy)

	rooted := sharedTestIsolation()
	rooted.StandaloneStateRoot = testStandaloneStateRootPath
	require.EqualError(t, validateProcessIsolation(rooted),
		"standalone owner fields describe an identity the supervisor already holds; "+
			sharedIdentitySupervisorRemedy)

	isolated := sharedTestIsolation()
	isolated.UID = 65534
	require.EqualError(t, validateProcessIsolation(isolated),
		"standalone owner id must be 1..256 canonical ASCII bytes")
}

// TestSharedIdentityCredentialRequestsNoIdentityChange proves the launch asks
// the kernel for nothing it cannot have. An unprivileged process cannot
// re-enter its own identity, and the supplementary groups on the request belong
// to the account the supervisor was started under rather than to a boundary it
// is crossing, so the honest instruction is no credential at all. A native
// group the supervisor is not in is still refused, because emitting nothing
// while the request names another group would silently run the agent in the
// wrong one. The isolated arm keeps the exact credential it always emitted.
func TestSharedIdentityCredentialRequestsNoIdentityChange(t *testing.T) {
	withSharedIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }
	processEffectiveGID = func() int { return 1000 }

	cmd := exec.Command("/usr/bin/true")
	require.NoError(t, applyProcessCredential(cmd, sharedTestIsolation()))
	require.Nil(t, cmd.SysProcAttr.Credential)

	processEffectiveGID = func() int { return 1001 }
	require.EqualError(t, applyProcessCredential(exec.Command("/usr/bin/true"), sharedTestIsolation()),
		"native group 1000 cannot be entered from group 1001; "+sharedIdentitySupervisorRemedy)

	processEffectiveGID = func() int { return -1 }
	require.EqualError(t, applyProcessCredential(exec.Command("/usr/bin/true"), sharedTestIsolation()),
		"native group 1000 cannot be entered from group -1; "+sharedIdentitySupervisorRemedy)

	processEffectiveGID = func() int { return 1000 }

	isolated := sharedTestIsolation()
	isolated.UID, isolated.GID = 65534, 65534
	isolated.StandaloneOwnerID = "test-owner"
	isolated.StandaloneStateRoot = testStandaloneStateRootPath
	cmd = exec.Command("/usr/bin/true")
	require.NoError(t, applyProcessCredential(cmd, isolated))
	require.Equal(t, &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}, NoSetGroups: false},
		cmd.SysProcAttr.Credential)
}
