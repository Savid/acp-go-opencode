//go:build !windows

package opencodeacp

import "errors"

// syncRestoreOwnershipDirectory flushes the control root so the rename that
// published the ownership registry survives a crash, not only its bytes.
func syncRestoreOwnershipDirectory(directory string) error {
	dir, err := restoreOpen(directory)
	if err != nil {
		return err
	}

	return errors.Join(restoreSync(dir), restoreClose(dir))
}
