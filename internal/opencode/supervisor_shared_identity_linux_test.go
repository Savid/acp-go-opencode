//go:build linux

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
)

// withOrdinaryIdentitySeams restores the seams ordinary execution is stamped
// from. The identity a launch runs as is a property of the account the suite
// was started under, which a test cannot change for itself, so every case names
// the identity it means explicitly instead of inheriting one.
func withOrdinaryIdentitySeams(t *testing.T) {
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

// TestVerifyLinuxTrustedSupervisorIdentityRequiresADistinctTrustedRoot proves
// the explicit policy's admission gate has no exception left. Every shape a
// caller could reach it with — an unprivileged supervisor, a root native
// identity, and a supervisor pointed at the identity it already holds — is
// refused with the same message. The one accepted shape is a root supervisor
// descending to a distinct nonzero identity.
func TestVerifyLinuxTrustedSupervisorIdentityRequiresADistinctTrustedRoot(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	withOrdinaryIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux

	const (
		unprivileged = uint32(1000)
		distinct     = uint32(65534)
	)

	const refusal = "OpenCode liveness supervisor requires a distinct trusted root identity"

	processEffectiveUID = func() int { return int(unprivileged) }
	supervisorTrustedEffectiveUID = func() int { return int(unprivileged) }
	effectiveUIDSource = func() int { return int(unprivileged) }

	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(unprivileged), refusal,
		"asking to run the agent under the adapter's own identity is a refusal, never a launch")
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(distinct), refusal)

	supervisorTrustedEffectiveUID = func() int { return 0 }
	effectiveUIDSource = func() int { return 0 }
	processEffectiveUID = func() int { return 0 }

	require.NoError(t, verifyLinuxTrustedSupervisorIdentity(distinct))
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(0), refusal)

	effectiveUIDSource = func() int { return int(distinct) }
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(distinct), refusal)
}

// TestLinuxSupervisorMarkerRootStaysInScratchForOrdinaryExecution proves where
// each arm keeps its containment proofs. The namespace under /run is root-owned
// and root-created so the isolated native identity cannot forge or scrub a
// proof about itself. Ordinary execution runs as the adapter's own identity, so
// the namespace would protect nothing it does not already control and an
// unprivileged launch could not create it at all; those proofs stay in the
// adapter-owned scratch root the launch made for itself.
func TestLinuxSupervisorMarkerRootStaysInScratchForOrdinaryExecution(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	scratch := t.TempDir()

	root, err := linuxSupervisorMarkerRoot(supervisorConfig{OrdinaryExecution: true, Scratch: scratch})
	require.NoError(t, err)
	require.Equal(t, scratch, root)

	effectiveUIDSource = func() int { return 1000 }
	_, err = linuxSupervisorMarkerRoot(supervisorConfig{Scratch: scratch})
	require.EqualError(t, err, "supervisor proof namespace requires a trusted root supervisor",
		"the explicit arm still demands the trusted namespace")
}

// TestSupervisorCommandStampsTheAuthorityItsIdentityAllows proves the parent
// records the one decision it made where both children can read it. The sealed
// config is the only thing the guardian and the liveness supervisor are handed,
// so the arm has to travel in it. An ordinary launch stamps OrdinaryExecution,
// carries the identity it already runs as, claims no capabilities and no
// standalone authority, and keeps its proofs in scratch; an explicit launch
// stamps the standalone authority and its trusted marker namespace.
func TestSupervisorCommandStampsTheAuthorityItsIdentityAllows(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withOrdinaryIdentitySeams(t)
		processIsolationGOOS = processIsolationLinux
		processEffectiveUID = func() int { return 1000 }
		processEffectiveGID = func() int { return 1000 }

		scratch := t.TempDir()
		sealed := captureSupervisorConfig(t)

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			NativeExecutable: testNativeExecutable(t, "/bin/true"), Home: filepath.Join(scratch, "home"), Scratch: scratch,
		})
		require.NoError(t, err)
		require.NotNil(t, cmd)
		require.NoError(t, proof.closeInherited())
		require.Len(t, cmd.ExtraFiles, 1, "ordinary execution hands down no capability descriptors")

		require.True(t, sealed().OrdinaryExecution)
		require.True(t, sealed().SharedIdentity)
		require.Equal(t, uint32(1000), sealed().IsolationUID)
		require.Equal(t, uint32(1000), sealed().IsolationGID)
		require.False(t, sealed().IdentityLock)
		require.False(t, sealed().AuthorityDomain)
		require.False(t, sealed().StandaloneAuthority)
		require.Empty(t, sealed().StandaloneOwnerID)
		require.Empty(t, sealed().StandaloneStateRoot)
		require.Equal(t, scratch, filepath.Dir(sealed().Completion))
		require.NoError(t, validateSupervisorIdentityDisposition(sealed()))
	})

	t.Run("ordinary root", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withOrdinaryIdentitySeams(t)
		processIsolationGOOS = processIsolationLinux
		processEffectiveUID = func() int { return 0 }
		processEffectiveGID = func() int { return 0 }

		scratch := t.TempDir()
		sealed := captureSupervisorConfig(t)

		_, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			NativeExecutable: testNativeExecutable(t, "/bin/true"), Home: filepath.Join(scratch, "home"), Scratch: scratch,
		})
		require.NoError(t, err)
		require.NoError(t, proof.closeInherited())

		require.True(t, sealed().OrdinaryExecution)
		require.Equal(t, uint32(0), sealed().IsolationUID)
		require.Equal(t, uint32(0), sealed().IsolationGID)
		require.Equal(t, scratch, filepath.Dir(sealed().Completion),
			"ordinary root still keeps its proofs out of the trusted authority namespace")
		require.NoError(t, validateSupervisorIdentityDisposition(sealed()))
	})

	t.Run("explicit", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withOrdinaryIdentitySeams(t)
		processIsolationGOOS = processIsolationLinux
		processEffectiveUID = func() int { return 1000 }

		scratch := t.TempDir()
		markers := t.TempDir()
		supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
		supervisorMarkerRoot = func(supervisorConfig) (string, error) { return markers, nil }
		sealed := captureSupervisorConfig(t)

		isolation := testProcessIsolation()
		isolation.UID, isolation.GID = 65534, 65534

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			NativeExecutable: testNativeExecutable(t, "/bin/true"), Home: filepath.Join(scratch, "home"), Scratch: scratch,
			Isolation: isolation,
		})
		require.NoError(t, err)
		require.NotNil(t, cmd)
		require.NoError(t, proof.closeInherited())

		require.False(t, sealed().OrdinaryExecution)
		require.False(t, sealed().SharedIdentity)
		require.True(t, sealed().StandaloneAuthority)
		require.Equal(t, isolation.StandaloneOwnerID, sealed().StandaloneOwnerID)
		require.Equal(t, markers, filepath.Dir(sealed().Completion))
	})
}

// captureSupervisorConfig swaps the config write for one that keeps what was
// encoded and still hands back a descriptor the launch can inherit, so a case
// can read the stamp the parent sealed without starting a supervisor.
func captureSupervisorConfig(t *testing.T) func() supervisorConfig {
	t.Helper()

	var captured supervisorConfig

	supervisorWriteConfig = func(_ string, config supervisorConfig) (*os.File, error) {
		captured = config

		return os.CreateTemp(t.TempDir(), "sealed")
	}

	return func() supervisorConfig { return captured }
}

// TestRunSupervisorRefusesAConfigItsIdentityContradicts proves both children
// treat the stamp as a claim to check rather than an instruction to follow. An
// ordinary stamp naming an identity this process does not run as is refused, an
// ordinary stamp carrying any authority the arm never creates is refused, and
// an explicit stamp claiming a shared identity is refused because the hardened
// backend has no such launch to make.
func TestRunSupervisorRefusesAConfigItsIdentityContradicts(t *testing.T) {
	withOrdinaryIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }
	processEffectiveGID = func() int { return 1000 }

	const ordinaryRefusal = "OpenCode ordinary supervisor identity disposition is invalid"

	base := func() supervisorConfig {
		return supervisorConfig{
			NativeExecutable: testNativeExecutable(t, "/bin/true"), Home: "/home/runner", Scratch: "/tmp/scratch",
			IsolationUID: 1000, IsolationGID: 1000,
			SharedIdentity: true, OrdinaryExecution: true,
		}
	}

	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, base())),
		`unknown internal mode "probe"`,
		"a stamp the running identity agrees with reaches the mode dispatch")

	foreign := base()
	foreign.IsolationUID = 65534
	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, foreign)), ordinaryRefusal)

	unstamped := base()
	unstamped.SharedIdentity = false
	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, unstamped)), ordinaryRefusal)

	for name, corrupt := range map[string]func(*supervisorConfig){
		"capabilities": func(config *supervisorConfig) {
			config.IdentityLock, config.AuthorityDomain = true, true
		},
		"standalone authority": func(config *supervisorConfig) { config.StandaloneAuthority = true },
		"owner id":             func(config *supervisorConfig) { config.StandaloneOwnerID = "test-owner" },
		"state root":           func(config *supervisorConfig) { config.StandaloneStateRoot = testStandaloneStateRootPath },
	} {
		t.Run(name, func(t *testing.T) {
			config := base()
			corrupt(&config)
			require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, config)), ordinaryRefusal)
		})
	}

	explicitShared := base()
	explicitShared.OrdinaryExecution = false
	explicitShared.IsolationUID, explicitShared.IsolationGID = 65534, 65534
	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, explicitShared)),
		"explicit supervisor identity disposition cannot claim a shared identity")
}

func encodeSupervisorConfig(t *testing.T, config supervisorConfig) io.Reader {
	t.Helper()
	encoded, err := json.Marshal(config)
	require.NoError(t, err)

	return bytes.NewReader(encoded)
}

// TestOrdinaryGuardianTakesNoAgentAuthority proves ordinary execution claims
// nothing durable and asks nothing of the trusted-root rule. The registry under
// /var/lib records who may enter an identity no live task occupies, and this
// launch enters the one it already holds, so the claim could only ever fail; it
// also needs privilege the deployment never had. The guardian therefore takes
// an empty authority, and releasing that authority releases nothing.
func TestOrdinaryGuardianTakesNoAgentAuthority(t *testing.T) {
	preserveSupervisorGlobals(t)

	supervisorVerifyTrustedIdentity = func(uint32) error {
		t.Fatal("ordinary execution must not be checked against the trusted supervisor rule")

		return nil
	}
	supervisorAcquireIdentityAuthority = func(
		uint32, uint32, string, string, io.Reader,
	) (supervisorIdentityLock, supervisorIdentityLock, error) {
		t.Fatal("ordinary execution must not claim the durable agent authority")

		return nil, nil, nil
	}

	identity, authority, err := acquireGuardianIdentityAuthority(supervisorConfig{
		IsolationUID: 1000, IsolationGID: 1000, SharedIdentity: true, OrdinaryExecution: true,
	}, strings.NewReader(""))
	require.NoError(t, err)
	require.Nil(t, identity.InheritedFile())
	require.Nil(t, authority.InheritedFile())
	require.NoError(t, identity.Close())
	require.NoError(t, authority.Close())
}

// TestProcessIsolationOmissionAllowsOrdinaryUser is the omitted-policy launch
// with no seams in it, executed from an unprivileged account. See
// runOrdinaryOmissionLaunch for what it proves.
func TestProcessIsolationOmissionAllowsOrdinaryUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this case covers the non-root half of ordinary execution")
	}

	runOrdinaryOmissionLaunch(t)
}

// TestProcessIsolationOmissionAllowsRoot is the same executed launch from the
// root account. Omission is not a privilege question: root runs the identical
// ordinary path, drops no credential, and claims no authority.
func TestProcessIsolationOmissionAllowsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("this case covers the root half of ordinary execution")
	}

	runOrdinaryOmissionLaunch(t)
}

// runOrdinaryOmissionLaunch executes the real supervisorCommand, the real
// guardian and liveness self-exec pair, the real readiness handshake and the
// real containment proof with no ProcessIsolation at all. It proves the launch
// needs no privilege setup: the native process runs as this account, the
// command carries no credential request, nothing reads the trusted authority
// namespace, the adapter-private carriers never reach the child, the writable
// home stays exclusive while the tree lives and is claimable again after it
// exits, and the completion proof lands in the adapter-owned scratch root.
func runOrdinaryOmissionLaunch(t *testing.T) {
	t.Helper()

	// The ambient environment carries the private carriers a leak would show
	// up in, so the launch is asked to build its own native environment from it
	// exactly as an ordinary agent does.
	t.Setenv(privateAdapterEnvPrefix+"SPOOF", "leaked")
	t.Setenv(DarwinRuntimeIDEnv, "leaked")
	t.Setenv("OPENCODE_DB", "/leaked/opencode.db")

	supervised := startOrdinarySupervisedNative(t)

	require.Equal(t, filepath.Join(supervised.root, "scratch"), filepath.Dir(supervised.proof.completion),
		"ordinary execution keeps its containment proofs in its own scratch root")
	require.NoDirExists(t, filepath.Join(linuxAgentIdentityNamespace, "domain.lock"),
		"ordinary execution never bootstraps the trusted authority namespace")

	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid()) // Kernel IDs fit the wire width.

	identity, err := os.ReadFile(supervised.identityPath)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimSpace(string(identity)), fmt.Sprintf("%d %d", uid, gid)),
		"the native process must run as the identity the adapter already holds, groups untouched")

	childEnvironment, err := os.ReadFile(supervised.environmentPath)
	require.NoError(t, err)
	require.NotContains(t, string(childEnvironment), privateAdapterEnvPrefix)
	require.NotContains(t, string(childEnvironment), DarwinRuntimeIDEnv)
	require.NotContains(t, string(childEnvironment), "OPENCODE_DB=")

	descendantSession, descendantGroup := processSessionAndGroup(t, supervised.descPID)
	require.Equal(t, supervised.descPID, descendantSession, "descendant must escape into a new session")
	require.Equal(t, supervised.descPID, descendantGroup, "descendant must escape into a new process group")

	_, err = homelock.Acquire(supervised.home)
	require.Error(t, err, "second claimant must fail while the tree is live")

	require.NoError(t, supervised.stdin.Close())
	require.NoError(t, waitSupervisor(supervised, 20*time.Second), "supervisor stderr: %s", supervised.stderr.String())
	assertProcessGone(t, supervised.rootPID)
	assertProcessGone(t, supervised.descPID)
	assertHomeReacquires(t, supervised.home)
}
