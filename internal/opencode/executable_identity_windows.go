//go:build windows

package opencode

import "golang.org/x/sys/windows"

// processExecutableIdentity answers with the volume serial number and the file
// index, which together name a file on Windows the way a device and an inode
// name one on Unix.
func processExecutableIdentity(path string) (uint64, uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}

	handle, err := windows.CreateFile(
		name,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return 0, 0, err
	}

	defer func() { _ = windows.CloseHandle(handle) }()

	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return 0, 0, err
	}

	return uint64(information.VolumeSerialNumber),
		uint64(information.FileIndexHigh)<<32 | uint64(information.FileIndexLow),
		nil
}
