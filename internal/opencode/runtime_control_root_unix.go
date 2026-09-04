//go:build !windows

package opencode

import "io/fs"

// runtimeControlRootTrusted admits the control root only at mode 0700. The
// runtime's shared lock and lifecycle state live under it, so a directory
// another account may write is a directory another account may steer this
// runtime through.
func runtimeControlRootTrusted(info fs.FileInfo) bool {
	return info.Mode().Perm() == 0o700
}
