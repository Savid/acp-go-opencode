package opencodeacp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

var runtimeUserCacheDir = os.UserCacheDir

// pluginSeedDirName is the directory beneath the adapter's user cache root that
// holds the OpenCode plugin install seeded into new runtime roots.
const pluginSeedDirName = "plugin-seed"

// pluginSeedDir resolves the plugin seed cache the shared runtime may restore
// from and populate. Empty disables seeding: when the operator turned it off,
// when OpenCode runs pure and installs no plugin loader, or when no user cache
// directory can be determined.
func (a *Agent) pluginSeedDir(ctx context.Context) string {
	if a.options.PluginSeedDisabled || a.options.Pure {
		return ""
	}

	if a.options.PluginSeedDir != "" {
		return a.options.PluginSeedDir
	}

	base, err := runtimeUserCacheDir()
	if err != nil {
		a.log.DebugContext(ctx, "plugin seed cache disabled", slog.String("reason", err.Error()))

		return ""
	}

	return filepath.Join(base, defaultAgentName, pluginSeedDirName)
}
