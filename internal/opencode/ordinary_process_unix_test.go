//go:build unix

package opencode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOrdinaryResolverSkipsUnusableEarlierPATHCandidate(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(first, "opencode"), []byte("not executable"), 0o600))
	want := filepath.Join(second, "opencode")
	require.NoError(t, os.WriteFile(want, []byte("#!/bin/sh\nexit 0\n"), 0o700))

	resolved, err := resolveOrdinaryProcessExecutable("opencode", []string{"PATH=" + first + string(os.PathListSeparator) + second})
	require.NoError(t, err)
	require.Equal(t, want, resolved)
}

func TestOrdinaryResolverAndProcessFailureEdges(t *testing.T) {
	_, err := resolveOrdinaryProcessExecutable("", nil)
	require.ErrorContains(t, err, "empty")
	_, err = resolveOrdinaryProcessExecutable(filepath.Join(t.TempDir(), "missing"), nil)
	require.ErrorContains(t, err, "stat executable")
	_, err = resolveOrdinaryProcessExecutable("opencode", nil)
	require.ErrorContains(t, err, "PATH is empty")
	_, err = resolveOrdinaryProcessExecutable("opencode", []string{"PATH=" + t.TempDir()})
	require.ErrorIs(t, err, exec.ErrNotFound)

	invalid := filepath.Join(t.TempDir(), "invalid")
	require.NoError(t, os.WriteFile(invalid, []byte("not an executable format"), 0o700))
	_, err = startOrdinaryProcess(t.Context(), invalid, nil, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.ErrorContains(t, err, "start native process")
	_, err = startOrdinaryProcess(t.Context(), filepath.Join(t.TempDir(), "missing"), nil, nil, t.TempDir())
	require.ErrorContains(t, err, "stat executable")

	require.NoError(t, normalizeOrdinaryWaitError(nil))
	want := errors.New("wait failed")
	require.ErrorIs(t, normalizeOrdinaryWaitError(want), want)
	failed := exec.Command("/bin/sh", "-c", "exit 1")
	require.NoError(t, normalizeOrdinaryWaitError(failed.Run()))
}

// releaseOrdinaryProcessPipes drops the three parent ends the ordinary backend
// hands out. They are the caller's property for the process's whole life —
// nothing in exec closes them any more — so every started process is released
// here rather than leaking descriptors through the rest of the package run.
func releaseOrdinaryProcessPipes(t *testing.T, process ProcessHandle) {
	t.Helper()
	t.Cleanup(func() {
		_ = process.Input.Close()
		_ = process.Output.Close()
		_ = process.Errors.Close()
	})
}

func TestOrdinaryProcessAwaitCancellation(t *testing.T) {
	process, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	releaseOrdinaryProcessPipes(t, process)
	go func() {
		_, _ = io.Copy(io.Discard, process.Output)
		_, _ = io.Copy(io.Discard, process.Errors)
	}()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = process.Await(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, process.Stop(context.Background()))
	result, err := process.Await(t.Context())
	require.NoError(t, err)
	require.True(t, result.Revoked)
}

func TestOrdinaryProcessPlatformHelperEdges(t *testing.T) {
	stopped, err := stopOrdinaryProcess(t.Context(), nil, &ordinaryProcessGuard{})
	require.NoError(t, err)
	require.False(t, stopped)
	require.NoError(t, containOrdinaryProcess(nil, &ordinaryProcessGuard{}))
	require.Equal(t, ProcessOutcome{ExitCode: -1}, ordinaryProcessOutcome(nil, nil))

	finished := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, finished.Run())
	stopped, err = stopOrdinaryProcess(t.Context(), finished, &ordinaryProcessGuard{})
	require.NoError(t, err)
	require.False(t, stopped)
	require.NoError(t, containOrdinaryProcess(finished, &ordinaryProcessGuard{}))

	want := errors.New("contain failed")
	require.NoError(t, normalizeContainOrdinaryProcessError(nil))
	require.NoError(t, normalizeContainOrdinaryProcessError(syscall.ESRCH))
	require.ErrorIs(t, normalizeContainOrdinaryProcessError(want), want)
}

// recordProcessPipes replaces the pipe seam with one that remembers both ends
// of every pipe it hands out, so a refusal can be checked for descriptors left
// behind rather than only for its message.
func recordProcessPipes(t *testing.T, allow int, refusal error) *[]*os.File {
	t.Helper()

	original := newProcessPipe
	t.Cleanup(func() { newProcessPipe = original })

	claimed := &[]*os.File{}
	remaining := allow
	newProcessPipe = func() (*os.File, *os.File, error) {
		if remaining == 0 {
			return nil, nil, refusal
		}

		remaining--

		read, write, err := original()
		if err == nil {
			*claimed = append(*claimed, read, write)
		}

		return read, write, err
	}

	return claimed
}

func requireProcessPipesReleased(t *testing.T, claimed *[]*os.File, want int) {
	t.Helper()
	require.Len(t, *claimed, 2*want, "each claimed pipe has two ends")

	for _, file := range *claimed {
		require.ErrorIs(t, file.Close(), os.ErrClosed, "descriptor leaked into the refusal")
	}
}

func TestOrdinaryProcessPipeAllocationEdges(t *testing.T) {
	unlimited := errors.New("unreachable refusal")

	t.Run("stdin refusal", func(t *testing.T) {
		claimed := recordProcessPipes(t, 3, unlimited)
		command := exec.Command("/bin/sh")
		command.Stdin = strings.NewReader("configured")
		pipes, err := ordinaryProcessPipes(command)
		require.ErrorContains(t, err, "open native stdin")
		require.Nil(t, pipes)
		requireProcessPipesReleased(t, claimed, 0)
	})

	t.Run("stdout refusal releases the stdin pipe", func(t *testing.T) {
		claimed := recordProcessPipes(t, 3, unlimited)
		command := exec.Command("/bin/sh")
		command.Stdout = io.Discard
		pipes, err := ordinaryProcessPipes(command)
		require.ErrorContains(t, err, "open native stdout")
		require.Nil(t, pipes)
		requireProcessPipesReleased(t, claimed, 1)
	})

	t.Run("stderr refusal releases the stdin and stdout pipes", func(t *testing.T) {
		claimed := recordProcessPipes(t, 3, unlimited)
		command := exec.Command("/bin/sh")
		command.Stderr = io.Discard
		pipes, err := ordinaryProcessPipes(command)
		require.ErrorContains(t, err, "open native stderr")
		require.Nil(t, pipes)
		requireProcessPipesReleased(t, claimed, 2)
	})

	t.Run("success hands the child ends to the command", func(t *testing.T) {
		command := exec.Command("/bin/sh")
		pipes, err := ordinaryProcessPipes(command)
		require.NoError(t, err)
		require.Same(t, pipes.childInput, command.Stdin)
		require.Same(t, pipes.childOutput, command.Stdout)
		require.Same(t, pipes.childErrors, command.Stderr)

		pipes.closeChildEnds()
		pipes.closeParentEnds()
	})
}

// TestOrdinaryProcessPipeExhaustionReleasesClaimedDescriptors covers each pipe
// this backend claims: a host that cannot hand out the next one refuses the
// start naming that stream, and every descriptor already claimed is released
// rather than leaked into the refusal.
func TestOrdinaryProcessPipeExhaustionReleasesClaimedDescriptors(t *testing.T) {
	want := errors.New("no descriptors left")

	for _, testCase := range []struct {
		stream string
		allow  int
	}{
		{stream: "stdin", allow: 0},
		{stream: "stdout", allow: 1},
		{stream: "stderr", allow: 2},
	} {
		t.Run(testCase.stream, func(t *testing.T) {
			claimed := recordProcessPipes(t, testCase.allow, want)

			handle, err := startOrdinaryProcess(t.Context(), "/bin/sh", nil,
				[]string{"PATH=/usr/bin:/bin"}, t.TempDir())
			require.ErrorIs(t, err, want)
			require.ErrorContains(t, err, "open native "+testCase.stream)
			require.False(t, handle.valid())
			requireProcessPipesReleased(t, claimed, testCase.allow)
		})
	}
}

// TestOrdinaryProcessDeliversTheTailWrittenBeforeExit pins pipe ownership: the
// wait is taken before either stream is drained, exactly as the settlement
// takes it while the drains are still reading, and the child's whole output
// must still arrive afterwards. A backend that hands its parent ends to
// exec.Cmd.Wait loses both payloads here every time.
func TestOrdinaryProcessDeliversTheTailWrittenBeforeExit(t *testing.T) {
	process, err := startOrdinaryProcess(t.Context(), "/bin/sh",
		[]string{"-c", "printf 'ordinary stdout'; printf 'ordinary stderr' >&2"},
		[]string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	releaseOrdinaryProcessPipes(t, process)
	require.NoError(t, process.Input.Close())

	result, err := process.Await(t.Context())
	require.NoError(t, err)
	require.Equal(t, ProcessOutcome{}, result)

	stdout, err := io.ReadAll(process.Output)
	require.NoError(t, err)
	require.Equal(t, "ordinary stdout", string(stdout))

	stderr, err := io.ReadAll(process.Errors)
	require.NoError(t, err)
	require.Equal(t, "ordinary stderr", string(stderr))
}

func TestOrdinaryProcessReportsNaturalAndRevokedResults(t *testing.T) {
	natural, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "exit 7"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	releaseOrdinaryProcessPipes(t, natural)
	result, err := natural.Await(t.Context())
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.False(t, result.Revoked)

	revoked, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	releaseOrdinaryProcessPipes(t, revoked)
	go func() {
		_, _ = io.Copy(io.Discard, revoked.Output)
		_, _ = io.Copy(io.Discard, revoked.Errors)
	}()
	require.NoError(t, revoked.Stop(context.Background()))
	waitCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err = revoked.Await(waitCtx)
	require.NoError(t, err)
	require.True(t, result.Revoked)
}

func TestOrdinaryProcessCancelledRevokeStillStartsTeardown(t *testing.T) {
	process, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	releaseOrdinaryProcessPipes(t, process)
	go func() {
		_, _ = io.Copy(io.Discard, process.Output)
		_, _ = io.Copy(io.Discard, process.Errors)
	}()

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, process.Stop(cancelled), context.Canceled)

	waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer waitCancel()
	result, err := process.Await(waitCtx)
	require.NoError(t, err)
	require.True(t, result.Revoked)
}

// TestStartOrdinaryProcessStopsAnUncontainableChild pins the refusal path: a
// child the platform cannot place under containment is killed rather than
// returned, so no uncontained native process ever reaches a caller.
func TestStartOrdinaryProcessStopsAnUncontainableChild(t *testing.T) {
	original := superviseOrdinary
	t.Cleanup(func() { superviseOrdinary = original })

	failure := errors.New("containment refused")
	var contained *exec.Cmd

	superviseOrdinary = func(command *exec.Cmd) (*ordinaryProcessGuard, error) {
		contained = command

		return nil, failure
	}

	handle, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "sleep 30"},
		[]string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.ErrorIs(t, err, failure)
	require.False(t, handle.valid())
	require.NotNil(t, contained)
	require.NotNil(t, contained.ProcessState, "the uncontainable child must be reaped, not merely killed")
	require.False(t, contained.ProcessState.Success(), "the uncontainable child must not survive the refusal")
}
