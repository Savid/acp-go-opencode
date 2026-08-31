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

	restoreBrokerSeams(t)

	created, err := broker.startBroker(context.Background())
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(created.home), agent.scratchParent())
	require.True(t, strings.HasPrefix(filepath.Base(created.home), authBrokerPrefix))
	require.Equal(t, opencode.Client(node), created.client)
	require.DirExists(t, created.home)

	created.destroy(context.Background())
	require.NoDirExists(t, created.home)
	require.True(t, node.closed)
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

			WithScratchDir(file)(&agent.options)
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

// TestProviderAuthBrokerRunsOrdinaryWithoutAdapterPrivateEnvironment proves the
// login runtime is built as ordinary execution from the ambient snapshot the Agent captured once,
// with the adapter-private carriers and every adapter-managed OpenCode root
// already scrubbed. The broker is the second consumer of that snapshot, so a
// scrub that only happened at the session launch would leak here.
func TestProviderAuthBrokerRunsOrdinaryWithoutAdapterPrivateEnvironment(t *testing.T) {
	const (
		ambientCanary = "ACP_GO_OPENCODE_TEST_ACTUAL_AMBIENT"
		privateCanary = privateAdapterEnvPrefix + "SPOOF"
	)

	t.Setenv(privateCanary, "leaked")
	t.Setenv("OPENCODE_DB", "/leaked/opencode.db")
	t.Setenv("OPENCODE_CONFIG_DIR", "/leaked/config")
	t.Setenv(ambientCanary, "kept")

	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	restoreBrokerSeams(t)

	var handed opencode.StartOptions

	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		handed = options

		return newFakeOpenCodeClient(), nil
	}

	created, err := broker.startBroker(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { created.destroy(context.Background()) })

	require.Nil(t, handed.StartProcess, "the broker runtime uses the ordinary launcher")
	environment := handed.NativeEnvironment()
	require.NotContains(t, environment, privateCanary)
	require.NotContains(t, environment, strings.ToLower(privateCanary))
	require.Equal(t, "kept", environment[ambientCanary],
		"only the private namespace is dropped, not the whole prefix")

	// The managed OpenCode roots are dropped where the native environment is
	// assembled, so the snapshot may still carry them; what must never happen
	// is the broker inheriting a caller override of one.
	require.NotContains(t, handed.Env, "OPENCODE_DB")
	require.NotContains(t, handed.Env, "OPENCODE_CONFIG_DIR")
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
