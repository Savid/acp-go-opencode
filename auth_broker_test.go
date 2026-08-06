package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func restoreBrokerSeams(t *testing.T) {
	t.Helper()

	backoff := authBrokerRemoveBackoff
	authBrokerRemoveBackoff = time.Millisecond

	t.Cleanup(func() {
		brokerReapHomes = opencode.ReapAbandonedHomes
		brokerMkdirTemp = os.MkdirTemp
		brokerChmod = os.Chmod
		brokerCreateXDG = opencode.CreateRuntimeXDGDirs
		brokerRemoveAll = os.RemoveAll
		brokerNewBrowserShim = opencode.NewBrowserShim
		authBrokerRemoveBackoff = backoff
	})
}

type brokerRootLifecycleClient struct {
	*fakeOpenCodeClient
	runtimeRunning    bool
	scopeCloseCalls   int
	rootShutdownCalls int
	shutdownErr       error
}

func newBrokerRootLifecycleClient() *brokerRootLifecycleClient {
	return &brokerRootLifecycleClient{
		fakeOpenCodeClient: newFakeOpenCodeClient(),
		runtimeRunning:     true,
	}
}

func (c *brokerRootLifecycleClient) Close(context.Context) error {
	c.scopeCloseCalls++

	return nil
}

func (c *brokerRootLifecycleClient) Shutdown(context.Context) error {
	c.rootShutdownCalls++
	c.runtimeRunning = false

	return c.shutdownErr
}

func TestStartBrokerCreatesAPrefixedHomeUnderTheScratchParent(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	node := withBrokerFactory(t, agent)

	reaped := ""
	restoreBrokerSeams(t)

	brokerReapHomes = func(parent string, prefix string) error {
		reaped = parent + "|" + prefix

		return nil
	}

	created, err := broker.startBroker(context.Background())
	require.NoError(t, err)
	require.Equal(t, agent.options.ScratchDir+"|"+authBrokerPrefix, reaped)
	require.Equal(t, filepath.Dir(created.home), agent.options.ScratchDir)
	require.True(t, strings.HasPrefix(filepath.Base(created.home), authBrokerPrefix))
	require.Equal(t, opencode.Client(node), created.client)
	require.DirExists(t, created.home)

	created.destroy(context.Background())
	require.NoDirExists(t, created.home)
	require.True(t, node.closed)
}

func TestStartBrokerContinuesWhenReapFails(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	withBrokerFactory(t, agent)
	restoreBrokerSeams(t)

	brokerReapHomes = func(string, string) error { return errors.New("scan") }

	created, err := broker.startBroker(context.Background())
	require.NoError(t, err)

	created.destroy(context.Background())
}

func TestStartBrokerFailures(t *testing.T) {
	failure := errors.New("refused")

	cases := []struct {
		name  string
		setup func(t *testing.T, agent *Agent)
	}{
		{name: "scratch parent", setup: func(t *testing.T, agent *Agent) {
			t.Helper()

			file := filepath.Join(t.TempDir(), "not-a-dir")
			require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

			agent.options.ScratchDir = file
		}},
		{name: "home creation", setup: func(_ *testing.T, _ *Agent) {
			brokerMkdirTemp = func(string, string) (string, error) { return "", failure }
		}},
		{name: "home protection", setup: func(_ *testing.T, _ *Agent) {
			brokerChmod = func(string, os.FileMode) error { return failure }
		}},
		{name: "xdg creation", setup: func(_ *testing.T, _ *Agent) {
			brokerCreateXDG = func(string) (opencode.XDGDirs, error) { return opencode.XDGDirs{}, failure }
		}},
		{name: "browser shim", setup: func(_ *testing.T, _ *Agent) {
			brokerNewBrowserShim = func(string) (*opencode.BrowserShim, error) { return nil, failure }
		}},
		{name: "server start", setup: func(_ *testing.T, agent *Agent) {
			agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
				return nil, failure
			}
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthAgent(t)
			agent, broker, _, _ := harness.agent, harness.broker, harness.runtime, harness.session
			withBrokerFactory(t, agent)
			restoreBrokerSeams(t)
			testCase.setup(t, agent)

			created, err := broker.startBroker(context.Background())
			require.Error(t, err)
			require.Nil(t, created)
		})
	}
}

func TestStartBrokerFallsBackToTheDefaultFactory(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	restoreBrokerSeams(t)

	agent.options.clientFactory = nil

	original := runtimeStartServer
	runtimeStartServer = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		return nil, errors.New("spawn")
	}

	t.Cleanup(func() { runtimeStartServer = original })

	created, err := broker.startBroker(context.Background())
	require.Error(t, err)
	require.Nil(t, created)
}

func TestDestroyShutsDownTheRootRuntimeBeforeRemovingItsHome(t *testing.T) {
	restoreBrokerSeams(t)

	home := t.TempDir()
	node := newBrokerRootLifecycleClient()
	broker := &authBroker{home: home, client: node, log: slog.New(slog.DiscardHandler)}
	broker.destroy(context.Background())

	require.False(t, node.runtimeRunning)
	require.Zero(t, node.scopeCloseCalls)
	require.Equal(t, 1, node.rootShutdownCalls)
	require.NoDirExists(t, home)
}

func TestDestroyReportsShutdownAndRemoveFailures(t *testing.T) {
	restoreBrokerSeams(t)

	node := newBrokerRootLifecycleClient()
	node.shutdownErr = errors.New("shutdown")

	brokerRemoveAll = func(string) error { return errors.New("remove") }

	broker := &authBroker{home: t.TempDir(), client: node, log: slog.New(slog.DiscardHandler)}
	broker.destroy(context.Background())

	require.False(t, node.runtimeRunning)
	require.Zero(t, node.scopeCloseCalls)
	require.Equal(t, 1, node.rootShutdownCalls)
}

// TestDestroyWaitsOutDescendantsStillWritingIntoTheHome pins the mechanism the
// whole containment argument rests on. Shutting down the broker can return
// before its descendants stop writing, so the first removal walks a tree that
// is still growing and fails with a not-empty directory; a home that survives
// that is a home a stale native approval can still complete into.
func TestDestroyWaitsOutDescendantsStillWritingIntoTheHome(t *testing.T) {
	restoreBrokerSeams(t)

	home := t.TempDir()
	attempts := 0

	brokerRemoveAll = func(path string) error {
		attempts++

		if attempts < 3 {
			return errors.New("directory not empty")
		}

		return os.RemoveAll(path)
	}

	broker := &authBroker{home: home, shim: nil, client: newFakeOpenCodeClient(), log: slog.New(slog.DiscardHandler)}
	broker.destroy(context.Background())

	require.Equal(t, 3, attempts)
	require.NoDirExists(t, home)
}

func TestDestroyToleratesANilBroker(t *testing.T) {
	var broker *authBroker

	broker.destroy(context.Background())
}

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
	require.Equal(t, created.home, handed.LeaseDir)
	require.Equal(t, agent.options.ScratchDir, filepath.Dir(handed.BrowserShim.Dir()))
	require.True(t, strings.HasPrefix(filepath.Base(handed.BrowserShim.Dir()), "acp-go-opencode-browser-shim-"))
	require.FileExists(t, filepath.Join(handed.BrowserShim.Dir(), "open"))

	created.destroy(context.Background())
	require.NoDirExists(t, handed.BrowserShim.Dir())
}

func TestDestroyReportsBrowserShimRemovalFailure(t *testing.T) {
	restoreBrokerSeams(t)

	shim, err := opencode.NewBrowserShim(t.TempDir())
	require.NoError(t, err)

	// A 0500 parent does not deny a privileged identity, so the removal failure
	// is injected instead of provoked, and the shim survives destruction.
	broker := &authBroker{
		home: t.TempDir(), shim: shim, client: newFakeOpenCodeClient(), log: slog.New(slog.DiscardHandler),
		removeShim: func() error { return errors.New("remove shim") },
	}
	broker.destroy(context.Background())

	require.DirExists(t, shim.Dir())
}

func TestStartBrokerReservesOneContainmentScratchRoot(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	restoreBrokerSeams(t)

	reserved := make(chan RuntimeResourceKind, 1)
	agent.options.RuntimeResourceHooks.ReserveScratchRoot = func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
		reserved <- kind

		return func() {}, nil
	}

	node := newFakeOpenCodeClient()
	agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		release, err := options.ReserveContainmentScratch(ctx)
		require.NoError(t, err)

		release()

		return node, nil
	}

	created, err := broker.startBroker(context.Background())
	require.NoError(t, err)
	require.Equal(t, RuntimeResourceDiscovery, <-reserved)

	created.destroy(context.Background())
}
