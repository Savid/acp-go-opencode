//go:build unix

package opencode

import (
	"context"
	"io"
	"os"
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
