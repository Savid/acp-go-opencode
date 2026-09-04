package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testPluginVersion       = "1.18.27"
	testPluginRelativeBin   = "../@opencode-ai/plugin/dist/index.js"
	testPluginExecutableBin = "node_modules/some-dep/bin.js"
)

// syncLogBuffer collects debug lines from goroutines the runtime spawns.
type syncLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(data)
}

func (b *syncLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func debugLogger(t *testing.T) (*slog.Logger, *syncLogBuffer) {
	t.Helper()

	buf := &syncLogBuffer{}

	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

func restorePluginSeedSeams(t *testing.T) {
	t.Helper()

	lstat, readFile, readDir := pluginSeedLstat, pluginSeedReadFile, pluginSeedReadDir
	readlink, symlink, mkdir := pluginSeedReadlink, pluginSeedSymlink, pluginSeedMkdir
	mkdirAll, mkdirTemp, open := pluginSeedMkdirAll, pluginSeedMkdirTemp, pluginSeedOpen
	openFile, writeFile, rename := pluginSeedOpenFile, pluginSeedWriteFile, pluginSeedRename
	removeAll, stat, now := pluginSeedRemoveAll, pluginSeedStat, pluginSeedNow
	marshal := openCodeMarshalIndent

	t.Cleanup(func() {
		pluginSeedLstat, pluginSeedReadFile, pluginSeedReadDir = lstat, readFile, readDir
		pluginSeedReadlink, pluginSeedSymlink, pluginSeedMkdir = readlink, symlink, mkdir
		pluginSeedMkdirAll, pluginSeedMkdirTemp, pluginSeedOpen = mkdirAll, mkdirTemp, open
		pluginSeedOpenFile, pluginSeedWriteFile, pluginSeedRename = openFile, writeFile, rename
		pluginSeedRemoveAll, pluginSeedStat, pluginSeedNow = removeAll, stat, now
		openCodeMarshalIndent = marshal
	})
}

// writePluginSeedTree lays down the npm state a completed OpenCode plugin
// install leaves behind: the manifest, the lock, the plugin package, a
// dependency with an executable, and npm's relative .bin symlink.
func writePluginSeedTree(t *testing.T, dir string, version string) {
	t.Helper()

	writeFile := func(rel string, content string, mode os.FileMode) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), mode))
		require.NoError(t, os.Chmod(path, mode))
	}

	writeFile(pluginSeedPackageFileName, `{"dependencies":{"@opencode-ai/plugin":"`+version+`"}}`, 0o644)
	writeFile(pluginSeedLockFileName, `{"lockfileVersion":3,"packages":{"":{"dependencies":{"@opencode-ai/plugin":"`+version+`"}},`+
		`"node_modules/@opencode-ai/plugin":{"version":"`+version+`"},"node_modules/some-dep":{"version":"1.0.0"}}}`, 0o644)
	writeFile("node_modules/@opencode-ai/plugin/package.json", `{"name":"@opencode-ai/plugin","version":"`+version+`"}`, 0o644)
	writeFile("node_modules/@opencode-ai/plugin/dist/index.js", "export {}\n", 0o644)
	writeFile(testPluginExecutableBin, "#!/usr/bin/env node\n", 0o755)
	writeFile("node_modules/some-dep/loose.js", "// group writable in the source tree\n", 0o664)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, pluginSeedModulesDirName, ".bin"), 0o755))
	require.NoError(t, os.Symlink(testPluginRelativeBin, filepath.Join(dir, pluginSeedModulesDirName, ".bin", "plugin")))
}

func writePluginSeedEntry(t *testing.T, cacheDir string, key string, version string, created time.Time) string {
	t.Helper()

	entry := filepath.Join(cacheDir, key)
	require.NoError(t, os.MkdirAll(entry, 0o700))
	writePluginSeedTree(t, entry, version)

	meta, err := json.Marshal(pluginSeedMeta{OpenCodeVersion: version, PluginVersion: version, CreatedAt: created})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(entry, pluginSeedMetaFileName), meta, 0o600))

	return entry
}

func requirePluginSeedTree(t *testing.T, dir string) {
	t.Helper()

	version, err := validatePluginSeedTree(dir)
	require.NoError(t, err)
	require.Equal(t, testPluginVersion, version)

	link, err := os.Readlink(filepath.Join(dir, pluginSeedModulesDirName, ".bin", "plugin"))
	require.NoError(t, err)
	require.Equal(t, filepath.FromSlash(testPluginRelativeBin), link,
		"npm's relative .bin link is recreated as a link")

	requireCopiedSeedModes(t,
		filepath.Join(dir, filepath.FromSlash(testPluginExecutableBin)),
		filepath.Join(dir, pluginSeedModulesDirName, "some-dep", "loose.js"))
}

func cacheEntryNames(t *testing.T, cacheDir string) []string {
	t.Helper()

	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

func writeExecutable(t *testing.T, dir string, name string, body string) string {
	t.Helper()

	path := filepath.Join(dir, testExecutableName(name))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))

	return path
}

func TestPluginSeedKeyTracksBinaryIdentity(t *testing.T) {
	restorePluginSeedSeams(t)

	dir := t.TempDir()
	environment := []string{"PATH=" + dir}
	executable := writeExecutable(t, dir, "opencode", "#!/bin/sh\n")

	key, err := pluginSeedKey(executable, environment)
	require.NoError(t, err)
	require.Len(t, key, 64)

	again, err := pluginSeedKey(executable, environment)
	require.NoError(t, err)
	require.Equal(t, key, again, "the key is stable for an unchanged binary")

	byName, err := pluginSeedKey("opencode", environment)
	require.NoError(t, err)
	require.Equal(t, key, byName, "a bare name resolves through the child's PATH to the same binary")

	stamp := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(executable, stamp, stamp))
	touched, err := pluginSeedKey(executable, environment)
	require.NoError(t, err)
	require.NotEqual(t, key, touched, "a changed mtime changes the key")

	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	require.NoError(t, os.Chtimes(executable, stamp, stamp))
	grown, err := pluginSeedKey(executable, environment)
	require.NoError(t, err)
	require.NotEqual(t, touched, grown, "a changed size changes the key even at the same mtime")

	other := writeExecutable(t, t.TempDir(), "opencode", "#!/bin/sh\n")
	elsewhere, err := pluginSeedKey(other, environment)
	require.NoError(t, err)
	require.NotEqual(t, key, elsewhere, "the same bytes at another path are another binary")

	_, err = pluginSeedKey(filepath.Join(dir, "missing"), environment)
	require.ErrorContains(t, err, "resolve native executable")

	pluginSeedStat = func(string) (fs.FileInfo, error) { return nil, errors.New("stat refused") }
	_, err = pluginSeedKey(executable, environment)
	require.ErrorContains(t, err, "stat native executable")
}

func TestValidatePluginSeedTree(t *testing.T) {
	valid := t.TempDir()
	writePluginSeedTree(t, valid, testPluginVersion)

	version, err := validatePluginSeedTree(valid)
	require.NoError(t, err)
	require.Equal(t, testPluginVersion, version)

	lockWith := func(root string, plugin string) string {
		return `{"packages":{` + root + plugin + `}}`
	}

	writeAt := func(dir string, rel string, content string) error {
		return os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(content), 0o644)
	}

	cases := []struct {
		name    string
		mutate  func(dir string) error
		wantErr string
	}{
		{
			name:    "missing manifest",
			mutate:  func(dir string) error { return os.Remove(filepath.Join(dir, pluginSeedPackageFileName)) },
			wantErr: pluginSeedPackageFileName,
		},
		{
			name:    "malformed manifest",
			mutate:  func(dir string) error { return writeAt(dir, pluginSeedPackageFileName, "{") },
			wantErr: "decode",
		},
		{
			name: "manifest without the plugin",
			mutate: func(dir string) error {
				return writeAt(dir, pluginSeedPackageFileName, `{"dependencies":{"left-pad":"1"}}`)
			},
			wantErr: "does not depend on",
		},
		{
			name:    "missing lock",
			mutate:  func(dir string) error { return os.Remove(filepath.Join(dir, pluginSeedLockFileName)) },
			wantErr: pluginSeedLockFileName,
		},
		{
			name: "lock without a root entry",
			mutate: func(dir string) error {
				return writeAt(dir, pluginSeedLockFileName, lockWith(``, `"node_modules/@opencode-ai/plugin":{"version":"1"}`))
			},
			wantErr: "no root package entry",
		},
		{
			name: "lock root missing a manifest dependency",
			mutate: func(dir string) error {
				return writeAt(dir, pluginSeedLockFileName, lockWith(`"":{"dependencies":{}},`, `"node_modules/@opencode-ai/plugin":{"version":"1"}`))
			},
			wantErr: "does not lock dependency",
		},
		{
			name: "lock without the installed plugin",
			mutate: func(dir string) error {
				return writeAt(dir, pluginSeedLockFileName, lockWith(`"":{"dependencies":{"@opencode-ai/plugin":"1"}}`, ``))
			},
			wantErr: "does not record an installed",
		},
		{
			name: "missing installed package",
			mutate: func(dir string) error {
				return os.RemoveAll(filepath.Join(dir, pluginSeedModulesDirName, pluginSeedPluginPackage))
			},
			wantErr: filepath.FromSlash(pluginSeedPluginPackage),
		},
		{
			name: "installed package is another version",
			mutate: func(dir string) error {
				return writeAt(dir, "node_modules/@opencode-ai/plugin/package.json", `{"name":"@opencode-ai/plugin","version":"0.0.1"}`)
			},
			wantErr: "does not match locked",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePluginSeedTree(t, dir, testPluginVersion)
			require.NoError(t, tc.mutate(dir))

			_, err := validatePluginSeedTree(dir)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestValidatePluginSeedEntry(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(entry string) error
		wantErr string
	}{
		{
			name:    "valid",
			mutate:  func(string) error { return nil },
			wantErr: "",
		},
		{
			name: "entry is a link",
			mutate: func(entry string) error {
				return errors.Join(os.Rename(entry, entry+".real"), os.Symlink(entry+".real", entry))
			},
			wantErr: "is not a directory",
		},
		{
			name: "manifest is a directory",
			mutate: func(entry string) error {
				path := filepath.Join(entry, pluginSeedPackageFileName)

				return errors.Join(os.Remove(path), os.Mkdir(path, 0o700))
			},
			wantErr: "is not a regular file",
		},
		{
			name:    "modules missing",
			mutate:  func(entry string) error { return os.RemoveAll(filepath.Join(entry, pluginSeedModulesDirName)) },
			wantErr: pluginSeedModulesDirName,
		},
		{
			name:    "metadata missing",
			mutate:  func(entry string) error { return os.Remove(filepath.Join(entry, pluginSeedMetaFileName)) },
			wantErr: pluginSeedMetaFileName,
		},
		{
			name: "metadata malformed",
			mutate: func(entry string) error {
				return os.WriteFile(filepath.Join(entry, pluginSeedMetaFileName), []byte("{"), 0o600)
			},
			wantErr: "decode plugin seed metadata",
		},
		{
			name: "install incomplete",
			mutate: func(entry string) error {
				return os.RemoveAll(filepath.Join(entry, pluginSeedModulesDirName, pluginSeedPluginPackage))
			},
			wantErr: filepath.FromSlash(pluginSeedPluginPackage),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := writePluginSeedEntry(t, t.TempDir(), "key", testPluginVersion, time.Now())
			require.NoError(t, tc.mutate(entry))

			err := validatePluginSeedEntry(entry)
			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestPluginSeedRestore(t *testing.T) {
	t.Run("miss leaves the runtime untouched", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cache := newPluginSeedCache(filepath.Join(t.TempDir(), "seed"), logger)
		configDir := t.TempDir()

		require.False(t, cache.restore(t.Context(), "key", configDir))
		require.Contains(t, logs.String(), errPluginSeedMiss.Error())
		require.Empty(t, cacheEntryNames(t, configDir))
	})

	t.Run("occupied runtime is never overwritten", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		writePluginSeedEntry(t, cacheDir, "key", testPluginVersion, time.Now())
		cache := newPluginSeedCache(cacheDir, logger)

		configDir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(configDir, pluginSeedModulesDirName), 0o700))

		require.False(t, cache.restore(t.Context(), "key", configDir))
		require.Contains(t, logs.String(), errPluginSeedTargetOccupied.Error())
		require.Equal(t, []string{pluginSeedModulesDirName}, cacheEntryNames(t, configDir))
	})

	t.Run("unreadable entry path is reported", func(t *testing.T) {
		restorePluginSeedSeams(t)

		logger, logs := debugLogger(t)
		root := t.TempDir()
		cache := newPluginSeedCache(root, logger)

		// An entry that cannot be inspected is not an absent entry. The fault is
		// injected rather than staged on disk, because the two platforms report
		// a different errno for every staging a test could build.
		pluginSeedLstat = func(path string) (fs.FileInfo, error) {
			if strings.HasPrefix(path, root) {
				return nil, fmt.Errorf("stat %s: %w", path, fs.ErrPermission)
			}

			return os.Lstat(path)
		}

		require.False(t, cache.restore(t.Context(), "key", t.TempDir()))
		require.Contains(t, logs.String(), "inspect plugin seed entry")
	})

	t.Run("rejected entry is discarded", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		entry := writePluginSeedEntry(t, cacheDir, "key", testPluginVersion, time.Now())
		require.NoError(t, os.Remove(filepath.Join(entry, pluginSeedMetaFileName)))
		cache := newPluginSeedCache(cacheDir, logger)

		require.False(t, cache.restore(t.Context(), "key", t.TempDir()))
		require.Contains(t, logs.String(), "plugin seed entry rejected")
		require.NoDirExists(t, entry, "an entry that fails validation is removed so a harvest can replace it")
	})

	t.Run("hit copies a real tree", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		entry := writePluginSeedEntry(t, cacheDir, "key", testPluginVersion, time.Now())
		cache := newPluginSeedCache(cacheDir, logger)
		configDir := t.TempDir()

		require.True(t, cache.restore(t.Context(), "key", configDir))
		require.Contains(t, logs.String(), "plugin seed restored")
		requirePluginSeedTree(t, configDir)
		require.NoError(t, validatePluginSeedEntry(entry), "the entry is read, never changed")
		require.NoFileExists(t, filepath.Join(configDir, pluginSeedMetaFileName), "metadata stays in the cache")

		info, err := os.Lstat(filepath.Join(configDir, pluginSeedModulesDirName))
		require.NoError(t, err)
		require.Zero(t, info.Mode()&fs.ModeSymlink, "the restored tree is a copy, not a link into the cache")
	})

	t.Run("failed copy unwinds every item", func(t *testing.T) {
		restorePluginSeedSeams(t)

		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		writePluginSeedEntry(t, cacheDir, "key", testPluginVersion, time.Now())
		cache := newPluginSeedCache(cacheDir, logger)
		configDir := t.TempDir()

		openFile := pluginSeedOpenFile
		pluginSeedOpenFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
			if filepath.Base(name) == pluginSeedLockFileName {
				return nil, errors.New("disk full")
			}

			return openFile(name, flag, perm)
		}

		require.False(t, cache.restore(t.Context(), "key", configDir))
		require.Contains(t, logs.String(), "disk full")
		require.Empty(t, cacheEntryNames(t, configDir), "a partial restore would make OpenCode skip an install that never happened")
	})

	t.Run("cancellation stops the copy", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		writePluginSeedEntry(t, cacheDir, "key", testPluginVersion, time.Now())
		cache := newPluginSeedCache(cacheDir, logger)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		configDir := t.TempDir()
		require.False(t, cache.restore(ctx, "key", configDir))
		require.Contains(t, logs.String(), context.Canceled.Error())
		require.Empty(t, cacheEntryNames(t, configDir))
	})
}

func TestPluginSeedHarvest(t *testing.T) {
	newSource := func(t *testing.T) string {
		t.Helper()

		dir := t.TempDir()
		writePluginSeedTree(t, dir, testPluginVersion)

		return dir
	}

	t.Run("publishes a complete tree with metadata", func(t *testing.T) {
		restorePluginSeedSeams(t)

		created := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
		pluginSeedNow = func() time.Time { return created }

		logger, logs := debugLogger(t)
		cacheDir := filepath.Join(t.TempDir(), "cache", "plugin-seed")
		cache := newPluginSeedCache(cacheDir, logger)

		require.NoError(t, cache.harvest(t.Context(), "key", newSource(t), "1.18.27-native"))
		require.Contains(t, logs.String(), "plugin seed harvested")
		require.Equal(t, []string{"key"}, cacheEntryNames(t, cacheDir), "no staging tree survives a successful publish")

		entry := filepath.Join(cacheDir, "key")
		requirePluginSeedTree(t, entry)
		require.NoError(t, validatePluginSeedEntry(entry))

		requireOwnerOnlyMode(t, entry, 0o700)

		meta, err := readPluginSeedMeta(entry)
		require.NoError(t, err)
		require.Equal(t, pluginSeedMeta{OpenCodeVersion: "1.18.27-native", PluginVersion: testPluginVersion, CreatedAt: created}, meta)
	})

	t.Run("incomplete source is refused before the cache is touched", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cacheDir := filepath.Join(t.TempDir(), "seed")
		cache := newPluginSeedCache(cacheDir, logger)

		source := newSource(t)
		require.NoError(t, os.RemoveAll(filepath.Join(source, pluginSeedModulesDirName, pluginSeedPluginPackage)))

		require.ErrorContains(t, cache.harvest(t.Context(), "key", source, "1"), "not a complete plugin install")
		require.Contains(t, logs.String(), "not a complete plugin install")
		require.NoDirExists(t, cacheDir)
	})

	t.Run("existing entry is kept", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		entry := writePluginSeedEntry(t, cacheDir, "key", "0.0.1", time.Now())
		cache := newPluginSeedCache(cacheDir, logger)

		require.Error(t, cache.harvest(t.Context(), "key", newSource(t), "1"))
		require.Contains(t, logs.String(), errPluginSeedEntryExists.Error())
		require.Equal(t, []string{"key"}, cacheEntryNames(t, cacheDir))

		meta, err := readPluginSeedMeta(entry)
		require.NoError(t, err)
		require.Equal(t, "0.0.1", meta.PluginVersion, "an entry is never rewritten in place")
	})

	t.Run("cache directory cannot be created", func(t *testing.T) {
		logger, logs := debugLogger(t)
		file := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		cache := newPluginSeedCache(filepath.Join(file, "seed"), logger)

		require.Error(t, cache.harvest(t.Context(), "key", newSource(t), "1"))
		require.Contains(t, logs.String(), "create plugin seed cache")
	})

	t.Run("staging failures leave nothing behind", func(t *testing.T) {
		failures := []struct {
			name    string
			arrange func()
			wantErr string
		}{
			{
				name: "staging tree",
				arrange: func() {
					pluginSeedMkdirTemp = func(string, string) (string, error) { return "", errors.New("no temp") }
				},
				wantErr: "create plugin seed staging tree",
			},
			{
				name: "copy",
				arrange: func() {
					pluginSeedOpen = func(string) (*os.File, error) { return nil, errors.New("unreadable source") }
				},
				wantErr: "unreadable source",
			},
			{
				name: "metadata encoding",
				arrange: func() {
					openCodeMarshalIndent = func(any, string, string) ([]byte, error) { return nil, errors.New("no json") }
				},
				wantErr: "encode plugin seed metadata",
			},
			{
				name: "metadata write",
				arrange: func() {
					pluginSeedWriteFile = func(string, []byte, os.FileMode) error { return errors.New("no write") }
				},
				wantErr: "write plugin seed metadata",
			},
			{
				name: "publish",
				arrange: func() {
					pluginSeedRename = func(string, string) error { return errors.New("no rename") }
				},
				wantErr: "publish plugin seed entry",
			},
			{
				name: "lost race",
				arrange: func() {
					pluginSeedRename = func(string, string) error { return &os.LinkError{Op: "rename", Err: fs.ErrExist} }
				},
				wantErr: errPluginSeedEntryExists.Error(),
			},
		}

		for _, tc := range failures {
			t.Run(tc.name, func(t *testing.T) {
				restorePluginSeedSeams(t)
				tc.arrange()

				logger, logs := debugLogger(t)
				cacheDir := t.TempDir()
				cache := newPluginSeedCache(cacheDir, logger)

				require.Error(t, cache.harvest(t.Context(), "key", newSource(t), "1"))
				require.Contains(t, logs.String(), tc.wantErr)
				require.Empty(t, cacheEntryNames(t, cacheDir), "no entry and no staging tree remain")
			})
		}
	})

	t.Run("concurrent harvesters publish exactly one entry", func(t *testing.T) {
		logger, _ := debugLogger(t)
		cacheDir := t.TempDir()
		source := newSource(t)

		var wg sync.WaitGroup
		for range 6 {
			wg.Go(func() {
				_ = newPluginSeedCache(cacheDir, logger).harvest(t.Context(), "key", source, "1")
			})
		}

		wg.Wait()

		require.Equal(t, []string{"key"}, cacheEntryNames(t, cacheDir))
		require.NoError(t, validatePluginSeedEntry(filepath.Join(cacheDir, "key")))
	})
}

// failingInfoEntry is a directory listing entry whose file information has
// vanished, which is what a concurrent removal looks like mid-collection.
type failingInfoEntry struct {
	fs.DirEntry
}

func (failingInfoEntry) Name() string               { return "vanished" }
func (failingInfoEntry) IsDir() bool                { return true }
func (failingInfoEntry) Type() fs.FileMode          { return fs.ModeDir }
func (failingInfoEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

func TestPluginSeedCollect(t *testing.T) {
	t.Run("unlistable cache is reported", func(t *testing.T) {
		logger, logs := debugLogger(t)
		cache := newPluginSeedCache(filepath.Join(t.TempDir(), "missing"), logger)

		cache.collect(t.Context(), "keep")
		require.Contains(t, logs.String(), "plugin seed cache not collected")
	})

	t.Run("retention", func(t *testing.T) {
		restorePluginSeedSeams(t)

		now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
		pluginSeedNow = func() time.Time { return now }

		logger, _ := debugLogger(t)
		cacheDir := t.TempDir()
		cache := newPluginSeedCache(cacheDir, logger)

		// Six entries newest-first: the budget keeps four, the protected key
		// survives regardless of age and rank, and the expired entry goes.
		for index, age := range []time.Duration{0, time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 5 * time.Hour} {
			writePluginSeedEntry(t, cacheDir, "entry-"+string(rune('a'+index)), testPluginVersion, now.Add(-age))
		}

		writePluginSeedEntry(t, cacheDir, "expired", testPluginVersion, now.Add(-pluginSeedMaxAge-time.Hour))
		writePluginSeedEntry(t, cacheDir, "protected", testPluginVersion, now.Add(-pluginSeedMaxAge-time.Hour))

		unreadable := writePluginSeedEntry(t, cacheDir, "unreadable-meta", testPluginVersion, now)
		require.NoError(t, os.WriteFile(filepath.Join(unreadable, pluginSeedMetaFileName), []byte("{"), 0o600))
		recent := now.Add(-30 * time.Minute)
		require.NoError(t, os.Chtimes(unreadable, recent, recent))

		stale := filepath.Join(cacheDir, pluginSeedTempPrefix+"dead")
		require.NoError(t, os.Mkdir(stale, 0o700))
		old := now.Add(-pluginSeedTempMaxAge - time.Hour)
		require.NoError(t, os.Chtimes(stale, old, old))

		live := filepath.Join(cacheDir, pluginSeedTempPrefix+"live")
		require.NoError(t, os.Mkdir(live, 0o700))
		require.NoError(t, os.Chtimes(live, now, now))

		require.NoError(t, os.WriteFile(filepath.Join(cacheDir, "stray-file"), nil, 0o600))

		cache.collect(t.Context(), "protected")

		require.ElementsMatch(t, []string{
			pluginSeedTempPrefix + "live", "stray-file", "protected",
			"entry-a", "unreadable-meta", "entry-b", "entry-c",
		}, cacheEntryNames(t, cacheDir))
	})

	t.Run("vanished entries and failed removals are tolerated", func(t *testing.T) {
		restorePluginSeedSeams(t)

		logger, logs := debugLogger(t)
		cacheDir := t.TempDir()
		cache := newPluginSeedCache(cacheDir, logger)
		writePluginSeedEntry(t, cacheDir, "expired", testPluginVersion, time.Now().Add(-pluginSeedMaxAge-time.Hour))

		readDir := pluginSeedReadDir
		pluginSeedReadDir = func(name string) ([]fs.DirEntry, error) {
			entries, err := readDir(name)

			return append(entries, failingInfoEntry{}), err
		}
		pluginSeedRemoveAll = func(string) error { return errors.New("busy") }

		cache.collect(t.Context(), "")
		require.Contains(t, logs.String(), "plugin seed entry not removed")
		require.Equal(t, []string{"expired"}, cacheEntryNames(t, cacheDir))
	})
}

func TestCopyPluginSeedTreeRefusesWhatItCannotReproduce(t *testing.T) {
	t.Run("absolute link", func(t *testing.T) {
		source := t.TempDir()
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(source, "link")))

		err := copyPluginSeedTree(t.Context(), source, filepath.Join(t.TempDir(), "copy"))
		require.ErrorContains(t, err, "symbolic link leaves the tree")
	})

	t.Run("escaping link", func(t *testing.T) {
		source := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(source, "bin"), 0o755))
		require.NoError(t, os.Symlink("../../outside", filepath.Join(source, "bin", "link")))

		err := copyPluginSeedTree(t.Context(), source, filepath.Join(t.TempDir(), "copy"))
		require.ErrorContains(t, err, "symbolic link leaves the tree")
	})

	t.Run("link to the tree root's parent", func(t *testing.T) {
		source := t.TempDir()
		require.NoError(t, os.Symlink("..", filepath.Join(source, "link")))

		err := copyPluginSeedTree(t.Context(), source, filepath.Join(t.TempDir(), "copy"))
		require.ErrorContains(t, err, "symbolic link leaves the tree")
	})

	t.Run("missing source", func(t *testing.T) {
		err := copyPluginSeedTree(t.Context(), filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "copy"))
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("seam failures", func(t *testing.T) {
		newSource := func(t *testing.T) string {
			t.Helper()

			source := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(source, "file"), []byte("x"), 0o644))
			require.NoError(t, os.Symlink("file", filepath.Join(source, "link")))

			return source
		}

		cases := []struct {
			name    string
			arrange func(source string)
			wantErr string
		}{
			{
				name: "lstat",
				arrange: func(string) {
					pluginSeedLstat = func(string) (fs.FileInfo, error) { return nil, errors.New("lstat refused") }
				},
				wantErr: "lstat refused",
			},
			{
				name: "mkdir",
				arrange: func(string) {
					pluginSeedMkdir = func(string, os.FileMode) error { return errors.New("mkdir refused") }
				},
				wantErr: "mkdir refused",
			},
			{
				name: "readlink",
				arrange: func(string) {
					pluginSeedReadlink = func(string) (string, error) { return "", errors.New("readlink refused") }
				},
				wantErr: "readlink refused",
			},
			{
				name: "symlink",
				arrange: func(string) {
					pluginSeedSymlink = func(string, string) error { return errors.New("symlink refused") }
				},
				wantErr: "symlink refused",
			},
			{
				name: "create",
				arrange: func(string) {
					pluginSeedOpenFile = func(string, int, os.FileMode) (*os.File, error) { return nil, errors.New("create refused") }
				},
				wantErr: "create refused",
			},
			{
				name: "read",
				arrange: func(source string) {
					// A directory handle satisfies Open and then fails the read.
					pluginSeedOpen = func(string) (*os.File, error) { return os.Open(source) }
				},
				wantErr: "copy",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				restorePluginSeedSeams(t)

				source := newSource(t)
				tc.arrange(source)

				err := copyPluginSeedTree(t.Context(), source, filepath.Join(t.TempDir(), "copy"))
				require.ErrorContains(t, err, tc.wantErr)
			})
		}
	})
}

func TestStartServerPluginSeedLifecycle(t *testing.T) {
	executable := fakeOpenCodeExecutable(t)
	seedDir := filepath.Join(t.TempDir(), "plugin-seed")

	start := func(t *testing.T, root string, logger *slog.Logger, mutate func(*StartOptions)) Client {
		t.Helper()

		options := StartOptions{
			Root: root, ExecutablePath: executable, PluginSeedDir: seedDir,
			HealthTimeout: 10 * time.Second, Logger: logger, SkipVersionGate: true,
		}

		if mutate != nil {
			mutate(&options)
		}

		client, err := StartServer(t.Context(), options)
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

		return client
	}

	// installOnLaunch stands in for the npm install OpenCode performs inside a
	// runtime root: the tree appears in the config root of every launch whose
	// root satisfies want, and the native process starts as usual.
	installOnLaunch := func(t *testing.T, want func(dir string) bool, observe func(dir string)) ProcessStarter {
		t.Helper()

		return func(ctx context.Context, executable string, arguments []string, environment []string, dir string) (ProcessHandle, error) {
			if observe != nil {
				observe(dir)
			}

			if want(dir) {
				writePluginSeedTree(t, openCodeConfigDir(RuntimeXDGDirs(dir)), testPluginVersion)
			}

			return startOrdinaryProcess(ctx, executable, arguments, environment, dir)
		}
	}

	isPrimeRoot := func(dir string) bool { return strings.HasPrefix(filepath.Base(dir), pluginSeedPrimeRootPrefix) }

	t.Run("pure runtime writes nothing", func(t *testing.T) {
		logger, _ := debugLogger(t)
		start(t, t.TempDir(), logger, func(options *StartOptions) { options.Pure = true })
		require.NoDirExists(t, seedDir)
	})

	t.Run("an install already in the root is left alone and never harvested", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()
		configDir := openCodeConfigDir(RuntimeXDGDirs(root))
		writePluginSeedTree(t, configDir, testPluginVersion)

		start(t, root, logger, func(options *StartOptions) { options.ScratchParent = t.TempDir() })

		require.Contains(t, logs.String(), errPluginSeedTargetOccupied.Error())
		require.NotContains(t, logs.String(), "plugin seed priming", "a root with npm state of its own gains nothing from the cache")
		require.NotContains(t, logs.String(), "plugin seed harvested", "a tree sessions may have touched is never the cache's source")
		require.NoDirExists(t, seedDir)
	})

	t.Run("cold boot without a scratch parent boots cold", func(t *testing.T) {
		logger, logs := debugLogger(t)

		start(t, t.TempDir(), logger, nil)

		require.Contains(t, logs.String(), errPluginSeedMiss.Error())
		require.Contains(t, logs.String(), "plugin seed not primed")
		require.Contains(t, logs.String(), "requires a scratch parent")
		require.NoDirExists(t, seedDir)
	})

	t.Run("cold boot primes through a throwaway launch and restores from it", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()
		scratch := t.TempDir()

		var launched []string

		var restoredBeforeLaunch []string

		start(t, root, logger, func(options *StartOptions) {
			options.ScratchParent = scratch
			options.StartProcess = installOnLaunch(t, isPrimeRoot, func(dir string) {
				launched = append(launched, dir)

				if dir == root {
					restoredBeforeLaunch = cacheEntryNames(t, openCodeConfigDir(RuntimeXDGDirs(root)))
				}
			})
		})

		require.Len(t, launched, 2, "one priming launch, then the runtime")
		require.True(t, isPrimeRoot(launched[0]), "the priming launch goes first: %s", launched[0])
		require.Equal(t, scratch, filepath.Dir(launched[0]), "the priming root is a scratch generation")
		require.Equal(t, root, launched[1])

		for _, message := range []string{"plugin seed priming", "plugin seed harvested", "plugin seed primed", "plugin seed restored"} {
			require.Contains(t, logs.String(), message)
		}

		require.Subset(t, restoredBeforeLaunch, pluginSeedItems, "the runtime launches from the restored tree")
		requirePluginSeedTree(t, openCodeConfigDir(RuntimeXDGDirs(root)))
		require.Len(t, cacheEntryNames(t, seedDir), 1)
		require.NoDirExists(t, launched[0], "the priming root is removed after the harvest")
		require.NoDirExists(t, ControlRootForXDG(launched[0]))
	})

	t.Run("warm boot restores before launch and harvests nothing", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()

		var launchedWith []string

		start(t, root, logger, func(options *StartOptions) {
			options.ScratchParent = t.TempDir()
			options.StartProcess = func(ctx context.Context, executable string, arguments []string, environment []string, dir string) (ProcessHandle, error) {
				launchedWith = cacheEntryNames(t, openCodeConfigDir(RuntimeXDGDirs(root)))

				return startOrdinaryProcess(ctx, executable, arguments, environment, dir)
			}
		})

		require.Contains(t, logs.String(), "plugin seed restored")
		require.NotContains(t, logs.String(), "plugin seed priming")
		require.NotContains(t, logs.String(), "plugin seed harvested")
		require.Subset(t, launchedWith, pluginSeedItems, "the tree is in place before the native process starts")
		requirePluginSeedTree(t, openCodeConfigDir(RuntimeXDGDirs(root)))
		require.Len(t, cacheEntryNames(t, seedDir), 1)
	})

	t.Run("managed warm boot restores before preparation and harvests nothing", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()

		var preparedWith []string

		start(t, root, logger, func(options *StartOptions) {
			options.ScratchParent = t.TempDir()
			options.PrepareTree = func(_ context.Context, path string) error {
				preparedWith = cacheEntryNames(t, openCodeConfigDir(RuntimeXDGDirs(path)))

				return nil
			}
			options.ReclaimTree = func(context.Context, string) error { return nil }
		})

		require.Contains(t, logs.String(), "plugin seed restored")
		require.Subset(t, preparedWith, pluginSeedItems, "the copy is the adapter's last write before the tree is handed over")
		require.NotContains(t, logs.String(), "plugin seed priming")
		require.NotContains(t, logs.String(), "plugin seed harvested")
		require.NotContains(t, logs.String(), "plugin seed not harvested")
	})

	t.Run("managed cold boot primes through the authority and harvests only after reclaim", func(t *testing.T) {
		logger, logs := debugLogger(t)
		managedSeed := filepath.Join(t.TempDir(), "managed-seed")
		root := t.TempDir()
		scratch := t.TempDir()

		var timeline []string

		var cacheAtReclaim, cacheAtRuntimePrepare, restoredBeforePrepare []string

		start(t, root, logger, func(options *StartOptions) {
			options.PluginSeedDir = managedSeed
			options.ScratchParent = scratch
			options.StartProcess = installOnLaunch(t, isPrimeRoot, nil)
			options.PrepareTree = func(_ context.Context, path string) error {
				timeline = append(timeline, "prepare:"+path)

				if path == root {
					cacheAtRuntimePrepare = cacheEntryNames(t, managedSeed)
					restoredBeforePrepare = cacheEntryNames(t, openCodeConfigDir(RuntimeXDGDirs(path)))
				}

				return nil
			}
			options.ReclaimTree = func(_ context.Context, path string) error {
				timeline = append(timeline, "reclaim:"+path)

				if isPrimeRoot(path) {
					cacheAtReclaim = cacheEntryNames(t, managedSeed)
				}

				return nil
			}
		})

		require.Len(t, timeline, 3, "the priming tree is prepared and reclaimed before the runtime tree is prepared: %v", timeline)
		require.True(t, strings.HasPrefix(timeline[0], "prepare:") && isPrimeRoot(strings.TrimPrefix(timeline[0], "prepare:")), timeline[0])
		require.True(t, strings.HasPrefix(timeline[1], "reclaim:") && isPrimeRoot(strings.TrimPrefix(timeline[1], "reclaim:")), timeline[1])
		require.Equal(t, "prepare:"+root, timeline[2])
		require.Empty(t, cacheAtReclaim, "nothing is read from the priming tree until reclaim has returned it")
		require.Len(t, cacheAtRuntimePrepare, 1, "the harvest lands between the reclaim and the runtime's preparation")
		require.Subset(t, restoredBeforePrepare, pluginSeedItems, "the runtime tree is handed over already restored")
		require.Contains(t, logs.String(), "plugin seed primed")
		require.NoDirExists(t, strings.TrimPrefix(timeline[0], "prepare:"))
	})

	t.Run("priming launch that cannot start leaves the runtime to boot cold", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()
		scratch := t.TempDir()
		refused := errors.New("launch refused")

		var primeRoot string

		start(t, root, logger, func(options *StartOptions) {
			options.PluginSeedDir = filepath.Join(t.TempDir(), "empty-seed")
			options.ScratchParent = scratch
			options.StartProcess = func(ctx context.Context, executable string, arguments []string, environment []string, dir string) (ProcessHandle, error) {
				if isPrimeRoot(dir) {
					primeRoot = dir

					return ProcessHandle{}, refused
				}

				return startOrdinaryProcess(ctx, executable, arguments, environment, dir)
			}
		})

		require.Contains(t, logs.String(), "plugin seed not primed")
		require.Contains(t, logs.String(), refused.Error())
		require.NotEmpty(t, primeRoot)
		require.NoDirExists(t, primeRoot, "a priming root this adapter still owns is removed")
		require.NoDirExists(t, ControlRootForXDG(primeRoot))
	})

	t.Run("priming launch whose tree the host keeps is left to host cleanup", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()
		scratch := t.TempDir()

		var primeRoot string

		start(t, root, logger, func(options *StartOptions) {
			options.PluginSeedDir = filepath.Join(t.TempDir(), "empty-seed")
			options.ScratchParent = scratch
			options.PrepareTree = func(_ context.Context, path string) error {
				if isPrimeRoot(path) {
					primeRoot = path

					return MarkPrepareOpaque(errors.New("prepare refused"))
				}

				return nil
			}
			options.ReclaimTree = func(context.Context, string) error { return nil }
		})

		require.Contains(t, logs.String(), "plugin seed not primed")
		require.Contains(t, logs.String(), "prepare refused")
		require.NotEmpty(t, primeRoot)
		require.DirExists(t, primeRoot, "an opaque prepare leaves the tree to host cleanup; nothing here may touch it")
	})

	t.Run("priming launch whose reclaim stays pending retains the tree", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()
		scratch := t.TempDir()

		var retained []string

		start(t, root, logger, func(options *StartOptions) {
			options.PluginSeedDir = filepath.Join(t.TempDir(), "empty-seed")
			options.ScratchParent = scratch
			options.StartProcess = installOnLaunch(t, isPrimeRoot, nil)
			options.PrepareTree = func(context.Context, string) error { return nil }
			options.ReclaimTree = func(_ context.Context, path string) error {
				if isPrimeRoot(path) {
					return MarkTreeReclaimPending(errors.New("still busy"))
				}

				return nil
			}
			options.RetainTree = func(path string, _ bool, _ func() error) { retained = append(retained, path) }
		})

		require.Contains(t, logs.String(), "priming runtime tree was not returned")
		require.Len(t, retained, 1)
		require.True(t, isPrimeRoot(retained[0]))
		require.DirExists(t, retained[0], "a tree the host still holds is never read or removed here")
		require.NotContains(t, logs.String(), "plugin seed harvested")
	})

	t.Run("priming launch that installs nothing publishes nothing", func(t *testing.T) {
		logger, logs := debugLogger(t)
		root := t.TempDir()
		scratch := t.TempDir()
		emptySeed := filepath.Join(t.TempDir(), "empty-seed")

		start(t, root, logger, func(options *StartOptions) {
			options.PluginSeedDir = emptySeed
			options.ScratchParent = scratch
		})

		require.Contains(t, logs.String(), "not a complete plugin install")
		require.Contains(t, logs.String(), "plugin seed not primed")
		require.NoDirExists(t, emptySeed)

		entries, err := os.ReadDir(scratch)
		require.NoError(t, err)
		require.Empty(t, entries, "the priming root and its control root are removed even when the harvest fails")
	})

	t.Run("priming root creation failure is reported", func(t *testing.T) {
		restorePluginSeedSeams(t)

		pluginSeedMkdirTemp = func(string, string) (string, error) { return "", errors.New("mkdirtemp refused") }

		logger, logs := debugLogger(t)

		start(t, t.TempDir(), logger, func(options *StartOptions) {
			options.PluginSeedDir = filepath.Join(t.TempDir(), "empty-seed")
			options.ScratchParent = t.TempDir()
		})

		require.Contains(t, logs.String(), "create priming runtime root")
		require.Contains(t, logs.String(), "mkdirtemp refused")
	})

	t.Run("priming root removal failure is reported after the harvest", func(t *testing.T) {
		restorePluginSeedSeams(t)

		pluginSeedRemoveAll = func(string) error { return errors.New("remove refused") }

		logger, logs := debugLogger(t)
		root := t.TempDir()
		seed := filepath.Join(t.TempDir(), "seed")

		start(t, root, logger, func(options *StartOptions) {
			options.PluginSeedDir = seed
			options.ScratchParent = t.TempDir()
			options.StartProcess = installOnLaunch(t, isPrimeRoot, nil)
		})

		require.Contains(t, logs.String(), "plugin seed harvested")
		require.Contains(t, logs.String(), "plugin seed not primed")
		require.Contains(t, logs.String(), "remove refused")
		require.Contains(t, logs.String(), "plugin seed restored", "the entry the launch published still serves the runtime")
		require.Len(t, cacheEntryNames(t, seed), 1)
	})

	t.Run("unresolvable executable skips seeding", func(t *testing.T) {
		logger, logs := debugLogger(t)
		want := errors.New("launch refused")

		_, err := StartServer(t.Context(), StartOptions{
			Root: t.TempDir(), ExecutablePath: filepath.Join(t.TempDir(), "missing"), PluginSeedDir: filepath.Join(t.TempDir(), "other-seed"),
			Logger: logger,
			StartProcess: func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
				return ProcessHandle{}, want
			},
		})
		require.ErrorIs(t, err, want)
		require.Contains(t, logs.String(), "resolve native executable for plugin seed key")
	})

	t.Run("unbuildable environment still fails after tree bookkeeping", func(t *testing.T) {
		logger, _ := debugLogger(t)

		_, err := StartServer(t.Context(), StartOptions{
			Root: t.TempDir(), ExecutablePath: executable, PluginSeedDir: seedDir, Logger: logger,
			Env: map[string]string{"BAD=KEY": "x"},
		})
		require.ErrorContains(t, err, "invalid environment entry")
	})
}

func TestPluginSeedPrimable(t *testing.T) {
	require.True(t, pluginSeedPrimable(errPluginSeedMiss))
	require.True(t, pluginSeedPrimable(fmt.Errorf("%w: bad", errPluginSeedEntryRejected)))
	require.False(t, pluginSeedPrimable(errPluginSeedTargetOccupied))
	require.False(t, pluginSeedPrimable(errors.New("copy failed")))
	require.False(t, pluginSeedPrimable(nil))
}

func TestPrimingStartOptions(t *testing.T) {
	shim := &BrowserShim{}
	base := StartOptions{
		Root: "/durable/home", ControlRoot: "/durable/home.control", ExistingXDG: RuntimeXDGDirs("/durable/home"),
		RemoveRoot: true, BrowserShim: shim, PluginSeedDir: "/cache", HealthTimeout: time.Second,
		ScratchParent: "/scratch", ExecutablePath: "opencode", SeedFiles: map[string]string{"opencode.json": "{}"},
		MinVersion: "1.0.0", Pure: false, LogLevel: "debug",
	}

	derived := primingStartOptions(base, "/scratch/acp-go-opencode-plugin-prime-1")

	require.Equal(t, "/scratch/acp-go-opencode-plugin-prime-1", derived.Root)
	require.Empty(t, derived.ControlRoot)
	require.Equal(t, XDGDirs{}, derived.ExistingXDG)
	require.False(t, derived.RemoveRoot)
	require.Nil(t, derived.BrowserShim)
	require.Empty(t, derived.PluginSeedDir, "a priming launch never primes again")
	require.Equal(t, pluginSeedPrimeTimeout, derived.HealthTimeout)
	require.Equal(t, base.SeedFiles, derived.SeedFiles)
	require.Equal(t, base.MinVersion, derived.MinVersion)
	require.Equal(t, base.ExecutablePath, derived.ExecutablePath)

	longer := primingStartOptions(StartOptions{HealthTimeout: time.Hour}, "/scratch/x")
	require.Equal(t, time.Hour, longer.HealthTimeout, "an operator budget above the floor is kept")
}

func TestPluginSeedTempPrefixSortsBeforeKeys(t *testing.T) {
	// Keys are hex digests; the staging prefix must never collide with one.
	require.True(t, strings.HasPrefix(pluginSeedTempPrefix, "."))
}
