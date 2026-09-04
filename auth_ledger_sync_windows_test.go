//go:build windows

package opencodeacp

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLedgerCommitNeverOpensTheLedgerRootOnWindows pins the documented Windows
// behaviour: FlushFileBuffers refuses a directory handle opened through os.Open,
// so the commit path never opens the ledger root at all and a fault planted on
// that open cannot reach a ledger write. The entry itself is still readable
// back, which is the durability the commit promises here.
func TestLedgerCommitNeverOpensTheLedgerRootOnWindows(t *testing.T) {
	ledger := newTestLedger(t)

	original := ledgerOpen
	t.Cleanup(func() { ledgerOpen = original })

	ledgerOpen = func(string) (*os.File, error) { return nil, errors.New("open") }

	record := authLedgerRecord{ProviderID: "xai"}
	require.NoError(t, syncLedgerDirectory(ledger.dir))
	require.NoError(t, ledger.write(record))

	stored, found, err := ledger.read("xai")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record, stored)
}
