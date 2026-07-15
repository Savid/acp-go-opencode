//go:build windows

package homelock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func validateLockFilesystem(file *os.File) error {
	abs, err := filepath.Abs(file.Name())
	if err != nil {
		return fmt.Errorf("resolve runtime lock volume: %w", err)
	}

	volume := filepath.VolumeName(abs)
	if volume == "" {
		return fmt.Errorf("runtime lock has no volume")
	}

	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return fmt.Errorf("encode runtime lock volume: %w", err)
	}

	if driveType := windows.GetDriveType(root); driveType != windows.DRIVE_FIXED && driveType != windows.DRIVE_RAMDISK {
		return fmt.Errorf("drive type %d has no approved local lock semantics", driveType)
	}

	filesystem := make([]uint16, 32)
	if err := windows.GetVolumeInformationByHandle(
		windows.Handle(file.Fd()),
		nil,
		0,
		nil,
		nil,
		nil,
		&filesystem[0],
		uint32(len(filesystem)),
	); err != nil {
		return fmt.Errorf("inspect runtime lock volume: %w", err)
	}

	name := windows.UTF16ToString(filesystem)
	if !windowsLocalLockFilesystem(name) {
		return fmt.Errorf("filesystem %q has no approved local lock semantics", name)
	}

	return nil
}

func windowsLocalLockFilesystem(name string) bool {
	switch strings.ToUpper(name) {
	case "NTFS", "REFS":
		return true
	default:
		return false
	}
}
