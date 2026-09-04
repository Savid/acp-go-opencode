//go:build !windows

package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// neutralizeBrowserShimWhereUnsupported leaves the seam alone. A real shim
// directory materialises here, so every broker test drives the shim the product
// actually builds.
func neutralizeBrowserShimWhereUnsupported(*testing.T) {}

func TestStartBrokerShadowsLaunchersAndKeepsControlBelowTraversableHome(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	restoreBrokerSeams(t)

	node := newFakeOpenCodeClient()

	var handed opencode.StartOptions

	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		handed = options

		return node, nil
	}

	created, err := broker.startBroker(context.Background())
	require.NoError(t, err)
	require.NotNil(t, handed.BrowserShim)
	require.Same(t, created.shim, handed.BrowserShim)
	require.Equal(t, filepath.Join(created.home, "control"), handed.ControlRoot)
	require.Nil(t, handed.StartProcess)
	require.Nil(t, handed.PrepareTree)
	require.Nil(t, handed.ReclaimTree)
	require.NotNil(t, handed.NativeEnvironment)
	require.Equal(t, agent.scratchParent(), filepath.Dir(handed.BrowserShim.Dir()))
	require.True(t, strings.HasPrefix(filepath.Base(handed.BrowserShim.Dir()), "acp-go-opencode-browser-shim-"))
	require.FileExists(t, filepath.Join(handed.BrowserShim.Dir(), "open"))

	created.destroy(context.Background())
	require.NoDirExists(t, handed.BrowserShim.Dir())
}

func TestDestroyReportsBrowserShimRemovalFailure(t *testing.T) {
	restoreBrokerSeams(t)

	shim, err := opencode.NewBrowserShim(t.TempDir())
	require.NoError(t, err)

	// The removal failure is injected, and the shim survives destruction.
	broker := &authBroker{
		home: t.TempDir(), shim: shim, client: newFakeOpenCodeClient(), log: slog.New(slog.DiscardHandler),
		removeShim: func() error { return errors.New("remove shim") },
	}
	broker.destroy(context.Background())

	require.DirExists(t, shim.Dir())
}
