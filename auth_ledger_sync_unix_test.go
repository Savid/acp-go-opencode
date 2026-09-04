//go:build !windows

package opencodeacp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSyncLedgerDirectoryUnix proves the POSIX flush both reaches a real
// directory and reports a root it cannot open, which is the branch the ledger
// write turns into a commit failure.
func TestSyncLedgerDirectoryUnix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syncLedgerDirectory(root))

	original := ledgerOpen
	t.Cleanup(func() { ledgerOpen = original })

	openErr := errors.New("open")
	ledgerOpen = func(string) (*os.File, error) { return nil, openErr }
	require.ErrorIs(t, syncLedgerDirectory(filepath.Join(root, "missing")), openErr)
}
