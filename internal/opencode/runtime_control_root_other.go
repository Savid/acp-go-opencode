//go:build !linux

package opencode

import "os"

func validateRuntimeControlRootOwner(os.FileInfo) error {
	return nil
}
