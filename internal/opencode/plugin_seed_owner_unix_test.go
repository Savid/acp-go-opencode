//go:build !windows

package opencode

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ownerlessFileInfo carries no platform ownership record.
type ownerlessFileInfo struct{}

func (ownerlessFileInfo) Name() string       { return "ownerless" }
func (ownerlessFileInfo) Size() int64        { return 0 }
func (ownerlessFileInfo) Mode() fs.FileMode  { return 0o700 | fs.ModeDir }
func (ownerlessFileInfo) ModTime() time.Time { return time.Time{} }
func (ownerlessFileInfo) IsDir() bool        { return true }
func (ownerlessFileInfo) Sys() any           { return nil }

func TestPluginSeedOwnedByCaller(t *testing.T) {
	original := pluginSeedCurrentUID
	t.Cleanup(func() { pluginSeedCurrentUID = original })

	info, err := os.Lstat(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, pluginSeedOwnedByCaller(info))

	require.ErrorContains(t, pluginSeedOwnedByCaller(ownerlessFileInfo{}), "ownership is unavailable")

	pluginSeedCurrentUID = func() int { return os.Getuid() + 1 }
	require.ErrorContains(t, pluginSeedOwnedByCaller(info), "want the current user")
}

func TestCopyPluginSeedTreeRefusesSpecialFiles(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600))

	err := copyPluginSeedTree(t.Context(), source, filepath.Join(t.TempDir(), "copy"))
	require.ErrorContains(t, err, "unsupported file type")
}

func TestPluginSeedRestoreRefusesAnotherUsersEntry(t *testing.T) {
	original := pluginSeedCurrentUID
	t.Cleanup(func() { pluginSeedCurrentUID = original })

	logger, logs := debugLogger(t)
	cacheDir := t.TempDir()
	entry := writePluginSeedEntry(t, cacheDir, "key", testPluginVersion, time.Now())
	cache := newPluginSeedCache(cacheDir, logger)

	pluginSeedCurrentUID = func() int { return os.Getuid() + 1 }

	configDir := t.TempDir()
	require.False(t, cache.restore(t.Context(), "key", configDir))
	require.Contains(t, logs.String(), "want the current user")
	require.Empty(t, cacheEntryNames(t, configDir), "code another user could have written never reaches a runtime root")
	require.NoDirExists(t, entry)
	require.NoFileExists(t, filepath.Join(configDir, pluginSeedPackageFileName))
}

// TestValidatePluginSeedEntryRefusesAGroupWritableEntry states the half of the
// cache's guard that only a platform with POSIX modes can express. It lives
// beside pluginSeedModeLoose rather than in the entry table, because that table
// runs everywhere and Windows reports the group and world write bits set on
// every path it has.
func TestValidatePluginSeedEntryRefusesAGroupWritableEntry(t *testing.T) {
	entry := writePluginSeedEntry(t, t.TempDir(), "key", testPluginVersion, time.Now())
	require.NoError(t, os.Chmod(entry, 0o770))
	require.ErrorContains(t, validatePluginSeedEntry(entry), "writable by group or world")
}
