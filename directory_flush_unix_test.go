//go:build !windows

package opencodeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDirectoryFlushOutcome states what a failing directory-flush seam does
// to the write that published a file. On POSIX the flush is real, so the failure
// reaches the caller.
func requireDirectoryFlushOutcome(t *testing.T, err error, contains string) {
	t.Helper()
	require.ErrorContains(t, err, contains)
}
