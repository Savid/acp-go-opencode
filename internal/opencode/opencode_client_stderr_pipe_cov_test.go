package opencode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStartServerAbortsWhenTheRuntimeStderrPipeCannotBeOpened proves startup
// refuses to spawn the runtime it cannot read stderr from. Stderr is where the
// runtime reports why it failed to come up, and it is also the stream the
// supervisor's own diagnostics ride on, so a process started without it would
// run unobserved until the readiness poll timed out with nothing to report. The
// command the seam hands back already has Stderr claimed, which is the one shape
// StderrPipe refuses; the assertion is that nothing was spawned, proven by a
// marker the command would have written had it ever run.
func TestStartServerAbortsWhenTheRuntimeStderrPipeCannotBeOpened(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)

	marker := filepath.Join(t.TempDir(), "spawned")
	openCodeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		command := exec.Command("/bin/sh", "-c", "printf spawned >\"$1\"", "sh", marker)
		command.Stderr = os.Stderr

		return command
	}

	client, err := StartServer(context.Background(), StartOptions{
		Root:             testGeneratedTempDir(t),
		ExecutablePath:   "/usr/bin/true",
		skipSupervisor:   true,
		ProcessIsolation: testProcessIsolation(),
	})
	require.ErrorContains(t, err, "exec: Stderr already set")
	require.Nil(t, client)

	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "startup spawned a runtime it could not read stderr from")
}
