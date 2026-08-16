//go:build linux || darwin || freebsd || openbsd

package opencode

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSupervisorRefusalReasonReachesTheGuardiansError proves the whole point of
// the refusal frame. Readiness travels the liveness supervisor's stderr, so a
// refusal that only printed a reason there had it forwarded to a stream nobody
// correlates while the guardian reported a bare pre-readiness EOF. Framing the
// refusal beside the readiness puts the reason in the error the caller keeps.
func TestSupervisorRefusalReasonReachesTheGuardiansError(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	preserveSupervisorGlobals(t)
	withNeutralSupervisorIdentityHooks(t)

	const reason = "adopt OpenCode agent identity lock: operation not permitted"

	root := t.TempDir()
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
	supervisorExecCommand = func(string, ...string) *exec.Cmd {
		return exec.Command( // #nosec G204 -- fixed fixture command.
			"/bin/sh", "-c",
			"printf '%s\\n' '"+supervisorRefusedPrefix+reason+"' >&2; exit 1",
		)
	}

	config := withTestSupervisorIdentity(supervisorConfig{
		NativeExecutable: testNativeExecutable(t, "/bin/sh"), NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	})

	err := runGuardian(config)
	require.ErrorContains(t, err, "failed before readiness")
	require.ErrorContains(t, err, "supervisor refused to start")
	require.ErrorContains(t, err, reason)
}

// TestSupervisorRefusalFrameIsWhatTheBootstrapPublishes pins the publishing half
// of the same contract: the one line a refusing supervisor writes is the frame,
// on the stream its reader is already parsing.
func TestSupervisorRefusalFrameIsWhatTheBootstrapPublishes(t *testing.T) {
	preserveSupervisorGlobals(t)

	var reported strings.Builder

	supervisorError = &reported
	supervisorExit = func(int) {}
	supervisorInheritedFile = func(uintptr, string) *os.File { return nil }
	t.Setenv(supervisorModeEnv, supervisorModeGuardian)

	supervisorBootstrap()

	require.True(t, strings.HasPrefix(reported.String(), supervisorRefusedPrefix), reported.String())

	ready, err := parseSupervisorReady(reported.String())
	require.Zero(t, ready.NativePID)
	require.ErrorContains(t, err, "supervisor refused to start")
	require.ErrorContains(t, err, "inherited config descriptor is unavailable")
}

// TestSupervisorWordlessDeathIsNotAClaimedRefusal pins the other half. The frame
// names a refusal that had something to say; a supervisor that dies without
// writing one has not said anything, and the reader must keep reporting that
// rather than dress it up as a named refusal.
func TestSupervisorWordlessDeathIsNotAClaimedRefusal(t *testing.T) {
	for name, line := range map[string]string{
		"empty":     "",
		"unrelated": "opencode: killed\n",
	} {
		t.Run(name, func(t *testing.T) {
			ready, err := parseSupervisorReady(line)
			require.Zero(t, ready.NativePID)
			require.ErrorContains(t, err, "invalid readiness frame")
			require.NotErrorIs(t, err, errors.ErrUnsupported)
			require.NotContains(t, err.Error(), "refused to start")
		})
	}
}
