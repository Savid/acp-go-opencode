package opencodeacp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPluginSeedDirResolution(t *testing.T) {
	original := runtimeUserCacheDir
	t.Cleanup(func() { runtimeUserCacheDir = original })

	cacheRoot := t.TempDir()
	runtimeUserCacheDir = func() (string, error) { return cacheRoot, nil }

	t.Run("default lives under the user cache directory", func(t *testing.T) {
		agent := NewAgent()
		require.Equal(t, filepath.Join(cacheRoot, defaultAgentName, pluginSeedDirName), agent.pluginSeedDir(context.Background()))
	})

	t.Run("explicit directory wins", func(t *testing.T) {
		agent := NewAgent(WithPluginSeedDir(absTestPath("srv", "seed")))
		require.Equal(t, absTestPath("srv", "seed"), agent.pluginSeedDir(context.Background()))
	})

	t.Run("disabled resolves to nothing", func(t *testing.T) {
		agent := NewAgent(WithPluginSeedDir(absTestPath("srv", "seed")), WithPluginSeed(false))
		require.Empty(t, agent.pluginSeedDir(context.Background()))

		reenabled := NewAgent(WithPluginSeed(false), WithPluginSeed(true))
		require.NotEmpty(t, reenabled.pluginSeedDir(context.Background()))
	})

	t.Run("pure runtimes install no plugin loader", func(t *testing.T) {
		agent := NewAgent(WithPluginSeedDir(absTestPath("srv", "seed")), WithOpenCodePure(true))
		require.Empty(t, agent.pluginSeedDir(context.Background()))
	})

	t.Run("unknown user cache directory disables seeding", func(t *testing.T) {
		runtimeUserCacheDir = func() (string, error) { return "", errors.New("no cache home") }
		t.Cleanup(func() { runtimeUserCacheDir = func() (string, error) { return cacheRoot, nil } })

		var logs bytes.Buffer

		agent := NewAgent(WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
		require.Empty(t, agent.pluginSeedDir(context.Background()))
		require.Contains(t, logs.String(), "no cache home")
	})
}
