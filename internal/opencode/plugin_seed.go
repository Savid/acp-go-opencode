package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The plugin seed cache removes the cold-boot cost of the session carrier.
//
// Any plugin entry in the generated opencode.json makes OpenCode run an npm
// install of @opencode-ai/plugin into <XDG_CONFIG_HOME>/opencode before it
// answers its first directory-scoped request. On a fresh runtime root that
// install takes minutes, and without -home every launch gets a fresh root.
// OpenCode skips the install when node_modules is present and every dependency
// name in package.json appears in package-lock.json, so the adapter keeps one
// real copy of the tree OpenCode produced, keyed by native binary identity, and
// copies it into each new runtime root before launch.
//
// Entries are immutable: a harvest builds a temporary tree and renames it into
// place, and a restore copies out of an entry rather than linking to it, so a
// later npm run inside a runtime root can never write into the cache.
//
// A tree is harvested only from a priming launch: a runtime started for no
// purpose but the install, torn down as soon as the carrier handshake proves
// the install complete, and never handed to a session. That is what makes the
// harvested tree OpenCode's own work rather than anything a session's shell
// left behind, and it holds in managed execution too, where the launch goes
// through the same host authority and is read only after reclaim returns it.

const (
	pluginSeedPackageFileName = "package.json"
	pluginSeedLockFileName    = "package-lock.json"
	pluginSeedModulesDirName  = "node_modules"
	pluginSeedMetaFileName    = "meta.json"
	pluginSeedTempPrefix      = ".tmp-"
	pluginSeedPluginPackage   = "@opencode-ai/plugin"
	pluginSeedKeyVersion      = "acp-go-opencode-plugin-seed-v1"
	pluginSeedMaxEntries      = 4
	pluginSeedMaxAge          = 30 * 24 * time.Hour
	pluginSeedTempMaxAge      = 24 * time.Hour
	pluginSeedLooseModeBits   = 0o022
	pluginSeedParentDir       = ".."
	// pluginSeedPrimeRootPrefix names a priming launch's runtime root beneath
	// the scratch parent, in the family's sweepable directory-name shape.
	pluginSeedPrimeRootPrefix = "acp-go-opencode-plugin-prime-"
	// pluginSeedPrimeTimeout is the least readiness budget a priming launch
	// gets. Its whole purpose is the cold path, OpenCode's own npm install,
	// measured at two to four minutes on a warm npm cache, so the runtime's own
	// health budget is far too short for it.
	pluginSeedPrimeTimeout = 10 * time.Minute
)

var (
	// errPluginSeedMiss reports that no entry exists for the key.
	errPluginSeedMiss = errors.New("plugin seed entry is absent")
	// errPluginSeedTargetOccupied reports that the runtime config root already
	// carries npm state, which the seed must never overwrite.
	errPluginSeedTargetOccupied = errors.New("runtime config root already holds npm state")
	// errPluginSeedEntryExists reports that another harvest published the entry
	// first, which is the expected outcome of a lost race.
	errPluginSeedEntryExists = errors.New("plugin seed entry already exists")
	// errPluginSeedEntryRejected reports that the entry for the key failed
	// validation and was removed, leaving the key free for a fresh harvest.
	errPluginSeedEntryRejected = errors.New("plugin seed entry rejected")
)

// pluginSeedItems lists the npm state OpenCode produces, in copy order: the
// module tree first and package.json last, so a copy that dies part-way leaves
// a tree OpenCode treats as incomplete and reinstalls rather than one it
// mistakes for finished.
var pluginSeedItems = []string{pluginSeedModulesDirName, pluginSeedLockFileName, pluginSeedPackageFileName}

var (
	pluginSeedLstat     = os.Lstat
	pluginSeedReadFile  = os.ReadFile
	pluginSeedReadDir   = os.ReadDir
	pluginSeedReadlink  = os.Readlink
	pluginSeedSymlink   = os.Symlink
	pluginSeedMkdir     = os.Mkdir
	pluginSeedMkdirAll  = os.MkdirAll
	pluginSeedMkdirTemp = os.MkdirTemp
	pluginSeedOpen      = os.Open
	pluginSeedOpenFile  = os.OpenFile
	pluginSeedWriteFile = os.WriteFile
	pluginSeedRename    = os.Rename
	pluginSeedRemoveAll = os.RemoveAll
	pluginSeedStat      = os.Stat
	pluginSeedNow       = time.Now
)

// pluginSeedMeta describes one cache entry. It is diagnostic: the entry key
// already binds the tree to a native binary.
type pluginSeedMeta struct {
	OpenCodeVersion string    `json:"opencodeVersion"`
	PluginVersion   string    `json:"pluginVersion"`
	CreatedAt       time.Time `json:"createdAt"`
}

// pluginSeedCache addresses one adapter-owned seed directory.
type pluginSeedCache struct {
	dir string
	log *slog.Logger
}

func newPluginSeedCache(dir string, log *slog.Logger) *pluginSeedCache {
	return &pluginSeedCache{dir: dir, log: log}
}

// pluginSeedKey names the entry for the binary the runtime will launch. The
// native version is only known after launch, but binary identity is known
// before it, and a binary upgrade changes the key on its own.
func pluginSeedKey(executable string, environment []string) (string, error) {
	resolved, err := resolveOrdinaryProcessExecutable(executable, environment)
	if err != nil {
		return "", fmt.Errorf("resolve native executable for plugin seed key: %w", err)
	}

	info, err := pluginSeedStat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat native executable for plugin seed key: %w", err)
	}

	digest := sha256.New()
	for _, part := range []string{pluginSeedKeyVersion, resolved, strconv.FormatInt(info.Size(), 10), strconv.FormatInt(info.ModTime().UnixNano(), 10)} {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}

// restore copies the entry for key into configDir. It reports whether the tree
// was restored; any other outcome is logged at debug and leaves configDir
// exactly as the cold path expects.
func (c *pluginSeedCache) restore(ctx context.Context, key string, configDir string) bool {
	return c.restoreOutcome(ctx, key, configDir) == nil
}

// restoreOutcome is restore reporting why a tree was not restored, so a caller
// can tell a miss a priming launch can fill from a root that already holds an
// install of its own.
func (c *pluginSeedCache) restoreOutcome(ctx context.Context, key string, configDir string) error {
	started := pluginSeedNow()

	if err := c.restoreEntry(ctx, key, configDir); err != nil {
		c.log.DebugContext(ctx, "plugin seed not restored", slog.String("key", key), slog.String("reason", err.Error()))

		return err
	}

	c.log.DebugContext(ctx, "plugin seed restored", slog.String("key", key), slog.Duration("elapsed", pluginSeedNow().Sub(started)))

	return nil
}

// pluginSeedPrimable reports whether a restore failure is one a priming launch
// can fill: the entry is absent, or it was rejected and removed. A root that
// already holds npm state of its own gains nothing from the cache, and a copy
// that failed part-way would fail the same way again.
func pluginSeedPrimable(err error) bool {
	return errors.Is(err, errPluginSeedMiss) || errors.Is(err, errPluginSeedEntryRejected)
}

// seedRuntimePlugins gives the runtime rooted at xdg a restored plugin install
// where the cache can supply one. A primable miss is filled first by a priming
// launch, and the restore is then attempted again, so the runtime itself only
// ever pays the install when priming was impossible.
func seedRuntimePlugins(ctx context.Context, options StartOptions, executable string, environment map[string]string, xdg XDGDirs) {
	cache := newPluginSeedCache(options.PluginSeedDir, options.Logger)

	key, err := pluginSeedKey(executable, envMapToSlice(environment))
	if err != nil {
		options.Logger.DebugContext(ctx, "plugin seed not restored", slog.String("reason", err.Error()))

		return
	}

	configDir := openCodeConfigDir(xdg)

	if restoreErr := cache.restoreOutcome(ctx, key, configDir); !pluginSeedPrimable(restoreErr) {
		return
	}

	if primeErr := cache.prime(ctx, options, key); primeErr != nil {
		options.Logger.DebugContext(ctx, "plugin seed not primed", slog.String("key", key), slog.String("reason", primeErr.Error()))
	}

	// A lost harvest race still leaves the entry another launch published, so
	// the second attempt is made whatever priming reported.
	cache.restore(ctx, key, configDir)
}

// primingStartOptions derives a priming launch from the options of the runtime
// that needs the cache filled. It keeps the binary, environment, seed files,
// version gate, and authority — so the tree it produces is the tree that
// runtime would have produced — under a scratch root of its own, with no
// browser shim, no cache to recurse into, and a readiness budget sized for the
// install rather than for a warm boot.
func primingStartOptions(options StartOptions, root string) StartOptions {
	options.Root = root
	options.ControlRoot = ""
	options.ExistingXDG = XDGDirs{}
	options.RemoveRoot = false
	options.BrowserShim = nil
	options.PluginSeedDir = ""
	options.HealthTimeout = max(options.HealthTimeout, pluginSeedPrimeTimeout)

	return options
}

// prime fills the entry for key with a priming launch. The launch registers
// the session carrier like any runtime, so OpenCode performs the install its
// first directory-scoped request triggers, and it is shut down as soon as that
// handshake completes. The tree is harvested only once shutdown has returned
// it — under host authority, only once reclaim has — and the root is removed
// afterwards. A launch whose tree stays with the host is left to host cleanup:
// its root sits under the scratch parent with the family prefix, which is what
// makes it a host sweep's to reclaim.
func (c *pluginSeedCache) prime(ctx context.Context, options StartOptions, key string) error {
	if options.ScratchParent == "" {
		return errors.New("priming launch requires a scratch parent")
	}

	root, err := pluginSeedMkdirTemp(options.ScratchParent, pluginSeedPrimeRootPrefix)
	if err != nil {
		return fmt.Errorf("create priming runtime root: %w", err)
	}

	started := pluginSeedNow()

	c.log.DebugContext(ctx, "plugin seed priming", slog.String("key", key), slog.String("root", root))

	server, startErr := startServer(ctx, primingStartOptions(options, root))
	if startErr != nil {
		if NativeCleanupRetained(startErr) {
			return fmt.Errorf("priming launch: %w", startErr)
		}

		return errors.Join(fmt.Errorf("priming launch: %w", startErr), removePrimingRoot(root))
	}

	// The shutdown is waited out on its own context, as a failed start's is:
	// a wait the caller could withdraw from would leave the shutdown running
	// while the tree bookkeeping below is read. Every step of it is bounded by
	// the containment timeout, so the wait is finite either way.
	shutdownErr := server.Shutdown(context.Background())
	if len(server.preparedTrees) > 0 {
		retainPreparedTrees(server.preparedTrees, options.RetainTree)
		server.preparedTrees = nil

		return retainNativeCleanup(errors.Join(errors.New("priming runtime tree was not returned"), shutdownErr))
	}

	harvestErr := c.harvest(ctx, key, openCodeConfigDir(RuntimeXDGDirs(root)), server.nativeVersion)

	if err := errors.Join(shutdownErr, harvestErr, removePrimingRoot(root)); err != nil {
		return err
	}

	c.log.DebugContext(ctx, "plugin seed primed", slog.String("key", key), slog.Duration("elapsed", pluginSeedNow().Sub(started)))

	return nil
}

func removePrimingRoot(root string) error {
	return errors.Join(pluginSeedRemoveAll(root), pluginSeedRemoveAll(ControlRootForXDG(root)))
}

func (c *pluginSeedCache) restoreEntry(ctx context.Context, key string, configDir string) error {
	for _, name := range pluginSeedItems {
		if _, err := pluginSeedLstat(filepath.Join(configDir, name)); err == nil {
			return fmt.Errorf("%w: %s", errPluginSeedTargetOccupied, name)
		}
	}

	entry := filepath.Join(c.dir, key)
	if _, err := pluginSeedLstat(entry); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errPluginSeedMiss
		}

		return fmt.Errorf("inspect plugin seed entry: %w", err)
	}

	if err := validatePluginSeedEntry(entry); err != nil {
		// A rejected entry never becomes valid on its own, and leaving it in
		// place would also block the harvest that could replace it.
		return fmt.Errorf("%w: %w", errPluginSeedEntryRejected, errors.Join(err, pluginSeedRemoveAll(entry)))
	}

	if err := copyPluginSeedItems(ctx, entry, configDir); err != nil {
		var unwind error
		for _, name := range pluginSeedItems {
			unwind = errors.Join(unwind, pluginSeedRemoveAll(filepath.Join(configDir, name)))
		}

		return fmt.Errorf("copy plugin seed into runtime: %w", errors.Join(err, unwind))
	}

	return nil
}

// harvest publishes the tree OpenCode produced under configDir as the entry
// for key. A tree is only harvested complete, and the entry appears in one
// rename so a concurrent reader never sees a partial one.
func (c *pluginSeedCache) harvest(ctx context.Context, key string, configDir string, nativeVersion string) error {
	started := pluginSeedNow()

	if err := c.harvestEntry(ctx, key, configDir, nativeVersion); err != nil {
		c.log.DebugContext(ctx, "plugin seed not harvested", slog.String("key", key), slog.String("reason", err.Error()))

		return err
	}

	c.log.DebugContext(ctx, "plugin seed harvested", slog.String("key", key), slog.Duration("elapsed", pluginSeedNow().Sub(started)))
	c.collect(ctx, key)

	return nil
}

func (c *pluginSeedCache) harvestEntry(ctx context.Context, key string, configDir string, nativeVersion string) error {
	pluginVersion, err := validatePluginSeedTree(configDir)
	if err != nil {
		return fmt.Errorf("runtime npm state is not a complete plugin install: %w", err)
	}

	entry := filepath.Join(c.dir, key)
	if _, statErr := pluginSeedLstat(entry); statErr == nil {
		return errPluginSeedEntryExists
	}

	if mkdirErr := pluginSeedMkdirAll(c.dir, 0o700); mkdirErr != nil {
		return fmt.Errorf("create plugin seed cache: %w", mkdirErr)
	}

	temp, tempErr := pluginSeedMkdirTemp(c.dir, pluginSeedTempPrefix+strconv.Itoa(os.Getpid())+"-")
	if tempErr != nil {
		return fmt.Errorf("create plugin seed staging tree: %w", tempErr)
	}

	if stageErr := c.stageEntry(ctx, configDir, temp, pluginSeedMeta{
		OpenCodeVersion: nativeVersion, PluginVersion: pluginVersion, CreatedAt: pluginSeedNow().UTC(),
	}); stageErr != nil {
		return errors.Join(stageErr, pluginSeedRemoveAll(temp))
	}

	if renameErr := pluginSeedRename(temp, entry); renameErr != nil {
		removeErr := pluginSeedRemoveAll(temp)
		if errors.Is(renameErr, fs.ErrExist) {
			return errors.Join(errPluginSeedEntryExists, removeErr)
		}

		return fmt.Errorf("publish plugin seed entry: %w", errors.Join(renameErr, removeErr))
	}

	return nil
}

func (c *pluginSeedCache) stageEntry(ctx context.Context, configDir string, temp string, meta pluginSeedMeta) error {
	if err := copyPluginSeedItems(ctx, configDir, temp); err != nil {
		return fmt.Errorf("stage plugin seed entry: %w", err)
	}

	data, err := openCodeMarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode plugin seed metadata: %w", err)
	}

	if err := pluginSeedWriteFile(filepath.Join(temp, pluginSeedMetaFileName), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write plugin seed metadata: %w", err)
	}

	return nil
}

// collect removes entries older than the retention window, trims the cache to
// its entry budget newest-first, and clears staging trees a dead harvester
// left behind. The entry named by keep is never removed.
func (c *pluginSeedCache) collect(ctx context.Context, keep string) {
	entries, err := pluginSeedReadDir(c.dir)
	if err != nil {
		c.log.DebugContext(ctx, "plugin seed cache not collected", slog.String("reason", err.Error()))

		return
	}

	now := pluginSeedNow()

	type aged struct {
		name    string
		created time.Time
	}

	kept := make([]aged, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		path := filepath.Join(c.dir, entry.Name())

		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}

		if strings.HasPrefix(entry.Name(), pluginSeedTempPrefix) {
			if now.Sub(info.ModTime()) > pluginSeedTempMaxAge {
				c.remove(ctx, path)
			}

			continue
		}

		created := info.ModTime()
		if meta, metaErr := readPluginSeedMeta(path); metaErr == nil {
			created = meta.CreatedAt
		}

		kept = append(kept, aged{name: entry.Name(), created: created})
	}

	slices.SortFunc(kept, func(left, right aged) int { return right.created.Compare(left.created) })

	for index, entry := range kept {
		if entry.name == keep {
			continue
		}

		if index >= pluginSeedMaxEntries || now.Sub(entry.created) > pluginSeedMaxAge {
			c.remove(ctx, filepath.Join(c.dir, entry.name))
		}
	}
}

func (c *pluginSeedCache) remove(ctx context.Context, path string) {
	if err := pluginSeedRemoveAll(path); err != nil {
		c.log.DebugContext(ctx, "plugin seed entry not removed", slog.String("path", path), slog.String("reason", err.Error()))
	}
}

func readPluginSeedMeta(entry string) (pluginSeedMeta, error) {
	var meta pluginSeedMeta

	data, err := pluginSeedReadFile(filepath.Join(entry, pluginSeedMetaFileName))
	if err != nil {
		return meta, err
	}

	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("decode plugin seed metadata: %w", err)
	}

	return meta, nil
}

// validatePluginSeedEntry admits an entry only when the adapter could have
// written it and nothing else could have changed it since: owned by the
// calling user, closed to group and world writes, carrying metadata, and
// holding a complete plugin install.
func validatePluginSeedEntry(entry string) error {
	if err := requireGuardedPluginSeedPath(entry, true); err != nil {
		return err
	}

	for _, name := range pluginSeedItems {
		if err := requireGuardedPluginSeedPath(filepath.Join(entry, name), name == pluginSeedModulesDirName); err != nil {
			return err
		}
	}

	if _, err := readPluginSeedMeta(entry); err != nil {
		return err
	}

	_, err := validatePluginSeedTree(entry)

	return err
}

func requireGuardedPluginSeedPath(path string, wantDir bool) error {
	info, err := pluginSeedLstat(path)
	if err != nil {
		return err
	}

	switch {
	case wantDir && !info.IsDir():
		return fmt.Errorf("%s is not a directory", path)
	case !wantDir && !info.Mode().IsRegular():
		return fmt.Errorf("%s is not a regular file", path)
	case pluginSeedModeLoose(info):
		return fmt.Errorf("%s is writable by group or world", path)
	}

	if err := pluginSeedOwnedByCaller(info); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	return nil
}

// validatePluginSeedTree checks that dir holds the npm state OpenCode leaves
// after a completed install, mirroring the test OpenCode applies before it
// skips one: node_modules present and every package.json dependency named in
// the lock. It also requires the plugin package itself to be installed at the
// locked version, because an entry that merely passes OpenCode's test would
// make OpenCode skip an install whose result is missing.
func validatePluginSeedTree(dir string) (string, error) {
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}

	if err := readPluginSeedJSON(filepath.Join(dir, pluginSeedPackageFileName), &manifest); err != nil {
		return "", err
	}

	if _, ok := manifest.Dependencies[pluginSeedPluginPackage]; !ok {
		return "", fmt.Errorf("%s does not depend on %s", pluginSeedPackageFileName, pluginSeedPluginPackage)
	}

	var lock struct {
		Packages map[string]struct {
			Version      string            `json:"version"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"packages"`
	}

	if err := readPluginSeedJSON(filepath.Join(dir, pluginSeedLockFileName), &lock); err != nil {
		return "", err
	}

	root, ok := lock.Packages[""]
	if !ok {
		return "", fmt.Errorf("%s has no root package entry", pluginSeedLockFileName)
	}

	for name := range manifest.Dependencies {
		if _, present := root.Dependencies[name]; !present {
			return "", fmt.Errorf("%s does not lock dependency %s", pluginSeedLockFileName, name)
		}
	}

	locked, ok := lock.Packages[pluginSeedModulesDirName+"/"+pluginSeedPluginPackage]
	if !ok || locked.Version == "" {
		return "", fmt.Errorf("%s does not record an installed %s", pluginSeedLockFileName, pluginSeedPluginPackage)
	}

	var installed struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}

	if err := readPluginSeedJSON(filepath.Join(dir, pluginSeedModulesDirName, pluginSeedPluginPackage, pluginSeedPackageFileName), &installed); err != nil {
		return "", err
	}

	if installed.Name != pluginSeedPluginPackage || installed.Version != locked.Version {
		return "", fmt.Errorf("installed %s@%s does not match locked %s@%s", installed.Name, installed.Version, pluginSeedPluginPackage, locked.Version)
	}

	return installed.Version, nil
}

func readPluginSeedJSON(path string, value any) error {
	data, err := pluginSeedReadFile(path)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}

func copyPluginSeedItems(ctx context.Context, source string, target string) error {
	for _, name := range pluginSeedItems {
		if err := copyPluginSeedTree(ctx, filepath.Join(source, name), filepath.Join(target, name)); err != nil {
			return err
		}
	}

	return nil
}

// copyPluginSeedTree copies source, a file or directory, to target as a real
// copy: directories and regular files are recreated, relative symbolic links
// that stay inside the tree are recreated as links, and anything else is
// refused. Group and world write bits are dropped on the way.
func copyPluginSeedTree(ctx context.Context, source string, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		// The tree-relative path carries no leading separator, so a symbolic
		// link's target resolves against the tree rather than the filesystem
		// root when the escape check below cleans it.
		relative := strings.TrimPrefix(strings.TrimPrefix(path, source), string(filepath.Separator))
		destination := filepath.Join(target, relative)

		info, err := pluginSeedLstat(path)
		if err != nil {
			return err
		}

		perm := info.Mode().Perm() &^ pluginSeedLooseModeBits

		switch mode := info.Mode(); {
		case mode.IsDir():
			return pluginSeedMkdir(destination, perm|0o700)
		case mode&fs.ModeSymlink != 0:
			return copyPluginSeedLink(path, destination, relative)
		case mode.IsRegular():
			return copyPluginSeedFile(path, destination, perm|0o600)
		default:
			return fmt.Errorf("refusing to copy %s: unsupported file type %s", path, mode.Type())
		}
	})
}

func copyPluginSeedLink(path string, destination string, relative string) error {
	link, err := pluginSeedReadlink(path)
	if err != nil {
		return err
	}

	resolved := filepath.Clean(filepath.Join(filepath.Dir(relative), link))
	if filepath.IsAbs(link) || resolved == pluginSeedParentDir || strings.HasPrefix(resolved, pluginSeedParentDir+string(filepath.Separator)) {
		return fmt.Errorf("refusing to copy %s: symbolic link leaves the tree", path)
	}

	return pluginSeedSymlink(link, destination)
}

func copyPluginSeedFile(path string, destination string, perm fs.FileMode) error {
	in, err := pluginSeedOpen(path)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	out, err := pluginSeedOpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return fmt.Errorf("copy %s: %w", path, err)
	}

	return nil
}
