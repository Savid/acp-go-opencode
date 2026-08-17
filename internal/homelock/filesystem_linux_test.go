//go:build linux

package homelock

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestLinuxFilesystemAdmission(t *testing.T) {
	require.True(t, linuxLocalLockFilesystem(unix.EXT4_SUPER_MAGIC))
	require.True(t, linuxLocalLockFilesystem(unix.XFS_SUPER_MAGIC))
	require.True(t, linuxLocalLockFilesystem(unix.BTRFS_SUPER_MAGIC))
	require.True(t, linuxLocalLockFilesystem(unix.F2FS_SUPER_MAGIC))
	require.True(t, linuxLocalLockFilesystem(unix.TMPFS_MAGIC))
	require.False(t, linuxLocalLockFilesystem(unix.OVERLAYFS_SUPER_MAGIC))

	original := linuxFstatfs
	t.Cleanup(func() { linuxFstatfs = original })
	linuxFstatfs = func(int, *unix.Statfs_t) error { return errors.New("statfs") }
	file, err := os.CreateTemp(t.TempDir(), "lock-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	require.ErrorContains(t, validateLockFilesystem(file), "statfs")

	linuxFstatfs = func(_ int, stat *unix.Statfs_t) error {
		stat.Type = unix.OVERLAYFS_SUPER_MAGIC

		return nil
	}
	require.ErrorContains(t, validateLockFilesystem(file), "approved local lock semantics")
}
