//go:build unix

package opencode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestOrdinaryProcessAwaitCancellation(t *testing.T) {
	process, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
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
	stopped, err := stopOrdinaryProcess(t.Context(), nil)
	require.NoError(t, err)
	require.False(t, stopped)
	require.NoError(t, containOrdinaryProcess(nil))
	require.Equal(t, ProcessOutcome{ExitCode: -1}, ordinaryProcessOutcome(nil, nil))

	finished := exec.Command("/bin/sh", "-c", "exit 0")
	require.NoError(t, finished.Run())
	stopped, err = stopOrdinaryProcess(t.Context(), finished)
	require.NoError(t, err)
	require.False(t, stopped)
	require.NoError(t, containOrdinaryProcess(finished))
}

func TestOrdinaryProcessReportsNaturalAndRevokedResults(t *testing.T) {
	natural, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "exit 7"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
	result, err := natural.Await(t.Context())
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.False(t, result.Revoked)

	revoked, err := startOrdinaryProcess(t.Context(), "/bin/sh", []string{"-c", "while :; do sleep 1; done"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	require.NoError(t, err)
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
