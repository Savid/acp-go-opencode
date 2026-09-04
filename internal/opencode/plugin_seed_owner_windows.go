//go:build windows

package opencode

import "io/fs"

// pluginSeedOwnedByCaller has no uid to compare on Windows. The cache lives
// under the calling user's local application data, whose ACL already confines
// writes to that user, so the mode and completeness checks stand alone here.
func pluginSeedOwnedByCaller(fs.FileInfo) error {
	return nil
}
