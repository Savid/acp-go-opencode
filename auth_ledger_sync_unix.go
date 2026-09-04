//go:build !windows

package opencodeacp

import (
	"errors"
	"fmt"
)

// syncLedgerDirectory flushes the ledger root so the rename that published the
// entry survives a crash, not only the entry's own bytes.
func syncLedgerDirectory(path string) error {
	dir, err := ledgerOpen(path)
	if err != nil {
		return fmt.Errorf("open provider auth ledger root: %w", err)
	}

	return errors.Join(dir.Sync(), dir.Close())
}
