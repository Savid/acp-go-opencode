//go:build windows

package opencodeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDirectoryFlushOutcome states what a failing directory-flush seam does
// to the write that published a file. Windows cannot flush a directory handle
// opened through os.Open, so the flush is a documented no-op there and never
// opens the directory at all: the seam is unreachable and the write succeeds.
func requireDirectoryFlushOutcome(t *testing.T, err error, _ string) {
	t.Helper()
	require.NoError(t, err)
}
