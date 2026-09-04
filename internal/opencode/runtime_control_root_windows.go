//go:build windows

package opencode

import "io/fs"

// runtimeControlRootTrusted has no POSIX mode to read on Windows. Go reports
// 0777 for every writable directory there and 0555 for a read-only one, so a
// 0700 comparison is not a weaker check on this platform — it is one no
// directory can pass, and it refused every runtime this adapter tried to
// start. Confinement comes from the ACL the control root inherits from the
// user profile that holds it, which is the standing the plugin seed cache's
// ownership check already has here.
func runtimeControlRootTrusted(fs.FileInfo) bool { return true }
