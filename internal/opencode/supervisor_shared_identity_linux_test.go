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
	"golang.org/x/sys/unix"
)

const sharedIdentityMismatchMessage = "supervisor identity disposition does not match the identity it runs as"

// TestVerifyLinuxTrustedSupervisorIdentityAcceptsTheIdentityItAlreadyRuns
// proves the identity rule reads two independent facts and refuses whenever
// they disagree. The supervisor descends from a trusted identity to the native
// one, so it must hold a higher identity first; when the native identity is the
// one it already runs as there is no descent to make and demanding root would
// refuse the only launch such a deployment can perform. Everything else is
// unmoved: an unprivileged supervisor pointed at a different identity, a root
// supervisor pointed at root, and a root supervisor pointed at itself all keep
// the refusal they always produced, byte for byte.
func TestVerifyLinuxTrustedSupervisorIdentityAcceptsTheIdentityItAlreadyRuns(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	withSharedIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux

	const (
		shared   = uint32(1000)
		distinct = uint32(65534)
	)

	processEffectiveUID = func() int { return int(shared) }
	supervisorTrustedEffectiveUID = func() int { return int(shared) }
	effectiveUIDSource = func() int { return int(shared) }

	require.NoError(t, verifyLinuxTrustedSupervisorIdentity(shared))
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(distinct),
		"OpenCode liveness supervisor requires a distinct trusted root identity",
		"an unprivileged supervisor pointed at another identity is still refused")

	supervisorTrustedEffectiveUID = func() int { return 0 }
	effectiveUIDSource = func() int { return 0 }
	processEffectiveUID = func() int { return 0 }

	require.NoError(t, verifyLinuxTrustedSupervisorIdentity(distinct))
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(0),
		"OpenCode liveness supervisor requires a distinct trusted root identity")

	effectiveUIDSource = func() int { return int(distinct) }
	require.EqualError(t, verifyLinuxTrustedSupervisorIdentity(distinct),
		"OpenCode liveness supervisor requires a distinct trusted root identity")
}

// TestLinuxSupervisorMarkerRootStaysInScratchUnderASharedIdentity proves where
// the containment proofs live for each arm. The namespace under /run is
// root-owned and root-created so the native identity cannot forge or scrub a
// proof about itself; a shared identity is that identity, so the namespace
// would protect nothing it does not already control and cannot be created
// without privilege anyway. The proofs stay in the adapter-owned scratch root
// the launch made for itself, and the isolated arm still opens the namespace.
func TestLinuxSupervisorMarkerRootStaysInScratchUnderASharedIdentity(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	scratch := t.TempDir()

	root, err := linuxSupervisorMarkerRoot(supervisorConfig{SharedIdentity: true, Scratch: scratch})
	require.NoError(t, err)
	require.Equal(t, scratch, root)

	effectiveUIDSource = func() int { return 1000 }
	_, err = linuxSupervisorMarkerRoot(supervisorConfig{Scratch: scratch})
	require.EqualError(t, err, "supervisor proof namespace requires a trusted root supervisor",
		"the isolated arm still demands the trusted namespace")
}

// TestSupervisorCommandStampsTheAuthorityItsIdentityAllows proves the parent
// records the one decision it made where both children can read it. The sealed
// config is the only thing the guardian and the liveness supervisor are handed,
// so the arm has to travel in it; recording it there also removes the last
// place a child could reach a different answer by accident. A shared launch
// stamps the arm, claims no capabilities and no standalone authority, and keeps
// its proofs in scratch; an isolated launch stamps exactly what it always did.
func TestSupervisorCommandStampsTheAuthorityItsIdentityAllows(t *testing.T) {
	t.Run("shared", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withSharedIdentitySeams(t)
		processIsolationGOOS = processIsolationLinux
		processEffectiveUID = func() int { return 1000 }

		scratch := t.TempDir()
		sealed := captureSupervisorConfig(t)

		cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
			NativePath: "/bin/true", Home: filepath.Join(scratch, "home"), Scratch: scratch,
			Isolation: sharedTestIsolation(),
		})
		require.NoError(t, err)
		require.NotNil(t, cmd)
		require.NoError(t, proof.closeInherited())
		require.Len(t, cmd.ExtraFiles, 1, "a shared identity hands down no capability descriptors")

		require.True(t, sealed().SharedIdentity)
		require.False(t, sealed().IdentityLock)
		require.False(t, sealed().AuthorityDomain)
		require.False(t, sealed().StandaloneAuthority)
		require.Empty(t, sealed().StandaloneOwnerID)
		require.Empty(t, sealed().StandaloneStateRoot)
		require.Equal(t, scratch, filepath.Dir(sealed().Completion))
	})

	t.Run("isolated", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withSharedIdentitySeams(t)
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
			NativePath: "/bin/true", Home: filepath.Join(scratch, "home"), Scratch: scratch,
			Isolation: isolation,
		})
		require.NoError(t, err)
		require.NotNil(t, cmd)
		require.NoError(t, proof.closeInherited())

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
// treat the stamp as a claim to check rather than an instruction to follow.
// Each of them re-derives the arm from the identity it is actually running as,
// and refuses in either direction: a shared stamp in a process that is not the
// native identity, and an isolated stamp in a process that is. A shared stamp
// carrying anything the arm never creates is refused for the same reason —
// nothing later would ever produce the authority it promises.
func TestRunSupervisorRefusesAConfigItsIdentityContradicts(t *testing.T) {
	withSharedIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }

	base := func() supervisorConfig {
		return supervisorConfig{
			NativePath: "/bin/true", Home: "/home/runner", Scratch: "/tmp/scratch",
			IsolationUID: 1000, IsolationGID: 1000, SharedIdentity: true,
		}
	}

	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, base())),
		`unknown internal mode "probe"`,
		"a stamp the running identity agrees with reaches the mode dispatch")

	foreign := base()
	foreign.IsolationUID = 65534
	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, foreign)), sharedIdentityMismatchMessage)

	unstamped := base()
	unstamped.SharedIdentity = false
	require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, unstamped)), sharedIdentityMismatchMessage)

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
			require.EqualError(t, runSupervisor("probe", encodeSupervisorConfig(t, config)),
				"shared supervisor identity disposition is invalid")
		})
	}
}

func encodeSupervisorConfig(t *testing.T, config supervisorConfig) io.Reader {
	t.Helper()
	encoded, err := json.Marshal(config)
	require.NoError(t, err)

	return bytes.NewReader(encoded)
}

// TestSharedIdentityGuardianTakesNoAgentAuthority proves the guardian claims
// nothing durable for an identity it already holds. The registry under
// /var/lib records who may enter an identity no live task occupies, and the
// guardian asking for it is such a task, so the claim could only ever fail; it
// also needs privilege the deployment never had. The guardian therefore takes
// an empty authority and never reaches the claim, and releasing that authority
// releases nothing.
func TestSharedIdentityGuardianTakesNoAgentAuthority(t *testing.T) {
	preserveSupervisorGlobals(t)

	supervisorVerifyTrustedIdentity = func(uint32) error {
		t.Fatal("a shared identity must not be checked against the trusted supervisor rule")

		return nil
	}
	supervisorAcquireIdentityAuthority = func(
		uint32, uint32, string, string, io.Reader,
	) (supervisorIdentityLock, supervisorIdentityLock, error) {
		t.Fatal("a shared identity must not claim the durable agent authority")

		return nil, nil, nil
	}

	identity, authority, err := acquireGuardianIdentityAuthority(supervisorConfig{
		IsolationUID: 1000, IsolationGID: 1000, SharedIdentity: true,
	}, strings.NewReader(""))
	require.NoError(t, err)
	require.Nil(t, identity.InheritedFile())
	require.Nil(t, authority.InheritedFile())
	require.NoError(t, identity.Close())
	require.NoError(t, authority.Close())
}

// TestSharedIdentitySupervisorContainsARealNativeLaunch is the whole arm with
// no seams in it. An unprivileged process runs the real supervisorCommand, the
// real guardian and liveness self-exec pair, the real readiness handshake and
// the real containment proof, and launches a native tree under the identity it
// already holds. The native process runs as that identity, a descendant that
// escapes into its own session and process group is still adopted and killed,
// the home claim is exclusive while the tree lives and returns after it exits,
// and the completion proof lands in the adapter-owned scratch root. Root cannot
// reach the arm at all, so it has nothing to run here.
func TestSharedIdentitySupervisorContainsARealNativeLaunch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the shared-identity arm is unreachable from the trusted root identity")
	}

	if os.Getegid() == 0 {
		t.Skip("the isolation policy requires a nonzero native group")
	}

	// Adopt the supervisor pair's orphans for the same reason the isolated
	// proof does: a reparented liveness supervisor's process group would
	// otherwise be resumed by POSIX before the test can observe containment.
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0))

	root, err := os.MkdirTemp("", "acp-go-opencode-shared-identity-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))

	rootPIDPath := filepath.Join(root, "root.pid")
	descPIDPath := filepath.Join(root, "desc.pid")
	identityPath := filepath.Join(root, "identity")
	readyPath := filepath.Join(root, "ready")
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "descendant" ]; then
  echo "$$" > "$DESC_PID_FILE"
  trap '' TERM
  while :; do sleep 1; done
fi
setsid "$0" descendant &
echo "$$" > "$ROOT_PID_FILE"
printf '%s %s\n' "$(id -u)" "$(id -g)" > "$IDENTITY_FILE"
touch "$READY_FILE"
trap '' TERM
while IFS= read -r line; do printf '%s\n' "$line"; done
`), 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	nativeEnv := append(os.Environ(),
		"ROOT_PID_FILE="+rootPIDPath,
		"DESC_PID_FILE="+descPIDPath,
		"IDENTITY_FILE="+identityPath,
		"READY_FILE="+readyPath,
	)

	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv:  nativeEnv,
		Home:       home,
		Scratch:    scratch,
		Isolation: &ProcessIsolation{
			UID: uid, GID: gid, BaseEnvironment: environmentMap(nativeEnv),
		},
	})
	require.NoError(t, err)
	require.Equal(t, scratch, filepath.Dir(proof.completion),
		"a shared identity keeps its containment proofs in its own scratch root")

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(supervisorTestBuffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	waiter, err := startOpenCodeProcess(cmd)
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())

	supervised := &supervisedNative{
		cmd: cmd, waiter: waiter, stdin: stdin, home: home,
		stderr: stderr, cancel: cancel, proof: proof, isolationUID: uid,
	}
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		killSupervisedGroup(supervised.rootPID)
		killSupervisedGroup(supervised.descPID)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	supervised.rootPID = waitPIDFile(t, rootPIDPath, stderr)
	supervised.descPID = waitPIDFile(t, descPIDPath, stderr)
	waitFile(t, readyPath)

	identity, err := os.ReadFile(identityPath)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%d %d", uid, gid), strings.TrimSpace(string(identity)),
		"the native process must run as the identity the supervisor already holds")

	descendantSession, descendantGroup := processSessionAndGroup(t, supervised.descPID)
	require.Equal(t, supervised.descPID, descendantSession, "descendant must escape into a new session")
	require.Equal(t, supervised.descPID, descendantGroup, "descendant must escape into a new process group")

	_, err = homelock.Acquire(home)
	require.Error(t, err, "second claimant must fail while the tree is live")

	require.NoError(t, stdin.Close())
	require.NoError(t, waitSupervisor(supervised, 20*time.Second), "supervisor stderr: %s", stderr.String())
	assertProcessGone(t, supervised.rootPID)
	assertProcessGone(t, supervised.descPID)
	assertHomeReacquires(t, home)
}
