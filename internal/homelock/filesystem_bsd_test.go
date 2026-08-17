//go:build darwin || freebsd

package homelock

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestBSDFilesystemAdmission(t *testing.T) {
	for _, name := range []string{"apfs", "hfs", "ufs", "zfs", "tmpfs"} {
		require.True(t, bsdLocalLockFilesystem(name))
	}
	require.False(t, bsdLocalLockFilesystem("nfs"))

	original := bsdFstatfs
	t.Cleanup(func() { bsdFstatfs = original })
	file, err := os.CreateTemp(t.TempDir(), "lock-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })

	bsdFstatfs = func(int, *unix.Statfs_t) error { return errors.New("statfs") }
	require.ErrorContains(t, validateLockFilesystem(file), "statfs")

	bsdFstatfs = func(_ int, stat *unix.Statfs_t) error {
		stat.Flags = unix.MNT_LOCAL

		return nil
	}
	require.ErrorContains(t, validateLockFilesystem(file), "approved local lock semantics")
}
