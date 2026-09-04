//go:build integration

package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// envTestPluginSeedDir names a plugin seed cache that outlives one package run.
// It is for local iteration only: a developer rerunning the native proofs can
// point it at a scratch directory such as .tmp/plugin-seed and pay for
// OpenCode's npm install once per binary rather than once per run. Nothing in
// the repository's make targets sets it, and it must never name the real user
// cache.
const envTestPluginSeedDir = "ACP_GO_OPENCODE_TEST_PLUGIN_SEED_DIR"

// The native proofs in this package share one plugin seed cache, primed by a
// single cold launch, so a package run pays for OpenCode's npm install once
// rather than once per runtime. TestMain removes the cache when the run ends
// unless the developer asked for a durable one.
var (
	sharedPluginSeedOnce     sync.Once
	sharedPluginSeedRoot     string
	sharedPluginSeedPath     string
	sharedPluginSeedErr      error
	sharedPluginSeedObserved *pluginSeedPrimeObservation
)

// pluginSeedPrimeObservation is what the priming pass saw of the managed
// launch shape, kept so a test can assert on it without paying a second cold
// install.
type pluginSeedPrimeObservation struct {
	mu                       sync.Mutex
	prepared                 []string
	reclaimed                []string
	cacheAtPrimeReclaim      int
	cacheAtRuntimePrepare    int
	restoredAtRuntimePrepare []string
	runtimePreparedAt        time.Time
	total                    time.Duration
	warm                     time.Duration
}

func isPluginSeedPrimeRoot(path string) bool {
	return strings.HasPrefix(filepath.Base(path), pluginSeedPrimeRootPrefix)
}

func countDirEntries(path string) int {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}

	return len(entries)
}

func TestMain(m *testing.M) {
	code := m.Run()

	if sharedPluginSeedRoot != "" {
		_ = os.RemoveAll(sharedPluginSeedRoot)
	}

	os.Exit(code)
}

// sharedPluginSeedDir returns the package's primed plugin seed cache, priming
// it on first use with a cold launch of the real OpenCode CLI.
func sharedPluginSeedDir(t *testing.T) string {
	t.Helper()

	executable := requireNativeCarrierIntegration(t)

	sharedPluginSeedOnce.Do(func() {
		sharedPluginSeedErr = primeSharedPluginSeed(t, executable)
	})

	require.NoError(t, sharedPluginSeedErr, "prime the shared plugin seed cache")

	return sharedPluginSeedPath
}

func primeSharedPluginSeed(t *testing.T, executable string) error {
	t.Helper()

	root, err := os.MkdirTemp("", "acp-go-opencode-plugin-seed-")
	if err != nil {
		return err
	}

	sharedPluginSeedRoot = root
	sharedPluginSeedPath = filepath.Join(root, "seed")

	if durable := os.Getenv(envTestPluginSeedDir); durable != "" {
		sharedPluginSeedPath = durable

		if entries, listErr := os.ReadDir(durable); listErr == nil && len(entries) > 0 {
			t.Logf("reusing the durable plugin seed cache at %s", durable)

			return nil
		}
	}

	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}

	// The cold path is OpenCode's own npm install, measured at two to four
	// minutes on a warm npm cache; the budget leaves room for a slow registry.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The prime runs in the managed launch shape — a tree authority that
	// prepares and reclaims every root — with the same identity standing in as
	// the authority, so what it proves is the adapter's ordering against the
	// real binary: the priming root is prepared and reclaimed, the harvest waits
	// for the reclaim, and the runtime root is handed over already restored.
	observed := &pluginSeedPrimeObservation{}
	runtimeRoot := filepath.Join(root, "prime-runtime")
	started := time.Now()

	runtime, err := StartServer(ctx, StartOptions{
		Root:           runtimeRoot,
		RemoveRoot:     true,
		ScratchParent:  root,
		ExecutablePath: executable,
		NativeEnvironment: func() map[string]string {
			return map[string]string{"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "HOME": home}
		},
		HealthTimeout: 60 * time.Second,
		PluginSeedDir: sharedPluginSeedPath,
		PrepareTree: func(_ context.Context, path string) error {
			observed.mu.Lock()
			defer observed.mu.Unlock()

			observed.prepared = append(observed.prepared, path)

			if path == runtimeRoot {
				observed.cacheAtRuntimePrepare = countDirEntries(sharedPluginSeedPath)
				observed.runtimePreparedAt = time.Now()

				entries, listErr := os.ReadDir(openCodeConfigDir(RuntimeXDGDirs(path)))
				if listErr != nil {
					return listErr
				}

				for _, entry := range entries {
					observed.restoredAtRuntimePrepare = append(observed.restoredAtRuntimePrepare, entry.Name())
				}
			}

			return nil
		},
		ReclaimTree: func(_ context.Context, path string) error {
			observed.mu.Lock()
			defer observed.mu.Unlock()

			observed.reclaimed = append(observed.reclaimed, path)

			if isPluginSeedPrimeRoot(path) {
				observed.cacheAtPrimeReclaim = countDirEntries(sharedPluginSeedPath)
			}

			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("cold managed launch to prime the plugin seed cache: %w", err)
	}

	observed.total = time.Since(started)
	observed.warm = time.Since(observed.runtimePreparedAt)
	sharedPluginSeedObserved = observed

	t.Logf("cold managed boot primed the plugin seed cache in %s; the runtime itself booted warm in %s",
		observed.total.Round(time.Millisecond), observed.warm.Round(time.Millisecond))

	if shutdownErr := runtime.Shutdown(context.Background()); shutdownErr != nil {
		return shutdownErr
	}

	entries, err := os.ReadDir(sharedPluginSeedPath)
	if err != nil {
		return fmt.Errorf("list the primed plugin seed cache: %w", err)
	}

	if len(entries) != 1 {
		return fmt.Errorf("primed plugin seed cache holds %d entries, want exactly the running binary's", len(entries))
	}

	return validatePluginSeedEntry(filepath.Join(sharedPluginSeedPath, entries[0].Name()))
}

// TestPluginSeedPrimesManagedColdBoot is the acceptance proof for the priming
// launch against the real OpenCode CLI in the managed launch shape: a cold
// cache is filled by a throwaway root the authority prepared and reclaimed,
// nothing is read from that root before reclaim returns it, and the runtime
// root is handed to the authority already carrying the restored install.
func TestPluginSeedPrimesManagedColdBoot(t *testing.T) {
	requireNativeCarrierIntegration(t)
	sharedPluginSeedDir(t)

	observed := sharedPluginSeedObserved
	if observed == nil {
		t.Skipf("the durable cache named by %s was reused, so no cold prime ran in this process", envTestPluginSeedDir)
	}

	require.Len(t, observed.prepared, 2, "one priming root, then the runtime root: %v", observed.prepared)
	require.True(t, isPluginSeedPrimeRoot(observed.prepared[0]), "the priming root is prepared first: %s", observed.prepared[0])
	require.Equal(t, sharedPluginSeedRoot, filepath.Dir(observed.prepared[0]), "the priming root is a scratch generation")
	require.False(t, isPluginSeedPrimeRoot(observed.prepared[1]))
	require.True(t, slices.Contains(observed.reclaimed, observed.prepared[0]), "the priming root is reclaimed: %v", observed.reclaimed)
	require.Zero(t, observed.cacheAtPrimeReclaim, "nothing is harvested before reclaim returns the priming tree")
	require.Equal(t, 1, observed.cacheAtRuntimePrepare, "the harvest lands before the runtime root is prepared")
	require.Subset(t, observed.restoredAtRuntimePrepare, pluginSeedItems, "the runtime root is handed over restored")
	require.NoDirExists(t, observed.prepared[0], "the priming root is removed after the harvest")
	require.NoDirExists(t, ControlRootForXDG(observed.prepared[0]))
	require.Less(t, observed.warm, 20*time.Second, "the runtime root skips the install the priming launch paid for")

	t.Logf("managed cold prime: total %s, runtime warm boot %s", observed.total.Round(time.Millisecond), observed.warm.Round(time.Millisecond))
}

// sharedPluginSeedEntries lists the cache entries a test may reason about: a
// durable developer cache can hold entries for other binaries, so only a cache
// this run created is known to hold exactly one.
func sharedPluginSeedEntries(t *testing.T, seedDir string) []os.DirEntry {
	t.Helper()

	entries, err := os.ReadDir(seedDir)
	require.NoError(t, err)

	if os.Getenv(envTestPluginSeedDir) == "" {
		require.Len(t, entries, 1)
	}

	require.NotEmpty(t, entries)

	return entries
}

func pluginSeedFileTimes(t *testing.T, configDir string) map[string]time.Time {
	t.Helper()

	times := make(map[string]time.Time, 4)

	for _, rel := range []string{
		pluginSeedPackageFileName,
		pluginSeedLockFileName,
		pluginSeedModulesDirName,
		filepath.Join(pluginSeedModulesDirName, pluginSeedPluginPackage, pluginSeedPackageFileName),
	} {
		info, err := os.Lstat(filepath.Join(configDir, rel))
		require.NoError(t, err)

		times[rel] = info.ModTime()
	}

	return times
}

// TestPluginSeedWarmsNativeBoot is the acceptance proof for the plugin seed
// cache against the real OpenCode CLI: a fresh runtime root restored from the
// cache reaches the carrier handshake in seconds, OpenCode leaves the restored
// npm state exactly as it found it, and nothing is harvested twice.
func TestPluginSeedWarmsNativeBoot(t *testing.T) {
	executable := requireNativeCarrierIntegration(t)
	seedDir := sharedPluginSeedDir(t)

	before := sharedPluginSeedEntries(t, seedDir)

	var meta pluginSeedMeta

	for _, entry := range before {
		candidate, err := readPluginSeedMeta(filepath.Join(seedDir, entry.Name()))
		require.NoError(t, err)

		if candidate.CreatedAt.After(meta.CreatedAt) {
			meta = candidate
		}
	}

	require.NotEmpty(t, meta.OpenCodeVersion)
	require.NotEmpty(t, meta.PluginVersion)

	root := t.TempDir()
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	runtimeRoot := filepath.Join(root, "runtime")
	configDir := openCodeConfigDir(RuntimeXDGDirs(runtimeRoot))

	var restored map[string]time.Time

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	started := time.Now()

	runtime, err := StartServer(ctx, StartOptions{
		Root:           runtimeRoot,
		ExecutablePath: executable,
		NativeEnvironment: func() map[string]string {
			return map[string]string{"PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "HOME": home}
		},
		HealthTimeout: 60 * time.Second,
		PluginSeedDir: seedDir,
		StartProcess: func(ctx context.Context, executable string, arguments []string, environment []string, dir string) (ProcessHandle, error) {
			// The restore is complete by the time the native process launches,
			// so this is the last moment the tree is provably the adapter's copy.
			restored = pluginSeedFileTimes(t, configDir)

			return startOrdinaryProcess(ctx, executable, arguments, environment, dir)
		},
	})

	elapsed := time.Since(started)

	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	t.Logf("warm native boot from the plugin seed cache completed in %s", elapsed.Round(time.Millisecond))

	require.Less(t, elapsed, 20*time.Second, "a restored root must skip OpenCode's npm install")
	require.Equal(t, restored, pluginSeedFileTimes(t, configDir), "OpenCode accepted the restored install without rewriting it")
	require.Equal(t, meta.OpenCodeVersion, runtime.NativeVersion(), "the cache entry belongs to the binary that just ran")

	after := sharedPluginSeedEntries(t, seedDir)
	require.Len(t, after, len(before), "a restored boot harvests nothing and leaves no staging tree")
}
