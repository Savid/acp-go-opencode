//go:build windows

package opencode

import "io/fs"

// pluginSeedOwnedByCaller has no uid to compare on Windows. The cache lives
// under the calling user's local application data, whose ACL already confines
// writes to that user, so the completeness and plugin-version checks stand
// alone here.
func pluginSeedOwnedByCaller(fs.FileInfo) error {
	return nil
}

// pluginSeedModeLoose has no POSIX mode to read either. Go reports 0666 for
// every writable file on Windows and 0777 for every writable directory, so the
// group and world write bits are set on everything this adapter creates: read
// literally, the check refused every entry the cache could ever hold and no
// Windows runtime was ever restored from it. The same profile ACL that stands
// in for the ownership check stands in for this one.
func pluginSeedModeLoose(fs.FileInfo) bool { return false }
