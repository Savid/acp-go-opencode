package opencode

import (
	"fmt"
	"os"
	"path/filepath"
)

var processWorkingDirectory = os.Getwd

// processExecutable is the one file a launch will run, frozen where it was
// resolved. Path is absolute, so nothing downstream can resolve the same string
// against a different working directory, and the recorded filesystem identity
// ties the file that was validated to the file the kernel finally execs.
type processExecutable struct {
	Path   string `json:"path"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func freezeProcessExecutable(path string) (processExecutable, error) {
	if !filepath.IsAbs(path) {
		return processExecutable{}, fmt.Errorf("executable path %q is not absolute", path)
	}

	device, inode, err := processExecutableIdentity(path)
	if err != nil {
		return processExecutable{}, fmt.Errorf("identify executable %q: %w", path, err)
	}

	return processExecutable{Path: path, Device: device, Inode: inode}, nil
}

// verify refuses a launch whose executable is no longer the file that was
// resolved. The supervised arm resolves in the adapter's working directory and
// execs from another process that has already moved to /, so the recorded
// identity is the only thing binding the validated file to the executed one.
func (executable processExecutable) verify() error {
	current, err := freezeProcessExecutable(executable.Path)
	if err != nil {
		return err
	}

	if current != executable {
		return fmt.Errorf("executable %q is no longer the file the launch resolved", executable.Path)
	}

	return nil
}
