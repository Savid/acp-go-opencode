package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
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

func newBrokerRootLifecycleClient(t *testing.T) *brokerRootLifecycleClient {
	t.Helper()

	return &brokerRootLifecycleClient{
		fakeOpenCodeClient: newFakeOpenCodeClient(t),
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

	created, err := broker.startBroker(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, filepath.Dir(created.home), agent.scratchParent())
	require.True(t, strings.HasPrefix(filepath.Base(created.home), authBrokerPrefix))
	require.Equal(t, opencode.Client(node), created.client)
	require.DirExists(t, created.home)

	require.NoError(t, created.destroy(context.Background()))
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

			created, err := broker.startBroker(context.Background(), "")
			require.Error(t, err)
			require.Nil(t, created)
		})
	}
}

func TestStartBrokerFallsBackToTheDefaultFactory(t *testing.T) {
	harness := newAuthAgent(t)
	agent, broker := harness.agent, harness.broker
	restoreBrokerSeams(t)
	neutralizeBrowserShimWhereUnsupported(t)

	agent.options.clientFactory = nil

	original := runtimeStartServer
	runtimeStartServer = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		return nil, errors.New("spawn")
	}

	t.Cleanup(func() { runtimeStartServer = original })

	created, err := broker.startBroker(context.Background(), "")
	require.Error(t, err)
	require.Nil(t, created)
}

func TestDestroyShutsDownTheRootRuntimeBeforeRemovingItsHome(t *testing.T) {
	restoreBrokerSeams(t)

	home := t.TempDir()
	node := newBrokerRootLifecycleClient(t)
	broker := &authBroker{home: home, client: node, log: slog.New(slog.DiscardHandler)}
	require.NoError(t, broker.destroy(context.Background()))

	require.False(t, node.runtimeRunning)
	require.Zero(t, node.scopeCloseCalls)
	require.Equal(t, 1, node.rootShutdownCalls)
	require.NoDirExists(t, home)
}

func TestDestroyReportsShutdownAndRemoveFailures(t *testing.T) {
	restoreBrokerSeams(t)

	node := newBrokerRootLifecycleClient(t)
	node.shutdownErr = errors.New("shutdown")

	brokerRemoveAll = func(string) error { return errors.New("remove") }

	broker := &authBroker{home: t.TempDir(), client: node, log: slog.New(slog.DiscardHandler)}
	require.ErrorIs(t, broker.destroy(context.Background()), node.shutdownErr)
	require.DirExists(t, broker.home)
	node.shutdownErr = nil
	require.ErrorContains(t, broker.destroy(context.Background()), "remove")
	brokerRemoveAll = os.RemoveAll
	require.NoError(t, broker.destroy(context.Background()))

	require.False(t, node.runtimeRunning)
	require.Zero(t, node.scopeCloseCalls)
	require.Equal(t, 2, node.rootShutdownCalls)
}

// TestDestroyRetriesTransientRemovalFailure keeps failed filesystem cleanup
// owned after the process is already stopped.
func TestDestroyRetriesTransientRemovalFailure(t *testing.T) {
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

	broker := &authBroker{home: home, shim: nil, client: newFakeOpenCodeClient(t), log: slog.New(slog.DiscardHandler)}
	require.NoError(t, broker.destroy(context.Background()))

	require.Equal(t, 3, attempts)
	require.NoDirExists(t, home)
}

func TestDestroyToleratesANilBroker(t *testing.T) {
	var broker *authBroker

	require.NoError(t, broker.destroy(context.Background()))
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
	neutralizeBrowserShimWhereUnsupported(t)

	var handed opencode.StartOptions

	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		handed = options

		return newFakeOpenCodeClient(t), nil
	}

	created, err := broker.startBroker(context.Background(), "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, created.destroy(context.Background())) })

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

type retryAuthBrokerClient struct {
	*fakeOpenCodeClient
	shutdownMu    sync.Mutex
	shutdownErr   error
	shutdownCalls int
}

func (c *retryAuthBrokerClient) Shutdown(ctx context.Context) error {
	c.shutdownMu.Lock()
	defer c.shutdownMu.Unlock()
	c.shutdownCalls++
	if c.shutdownErr != nil {
		return c.shutdownErr
	}

	return c.fakeOpenCodeClient.Shutdown(ctx)
}

func (c *retryAuthBrokerClient) allowShutdown() {
	c.shutdownMu.Lock()
	defer c.shutdownMu.Unlock()
	c.shutdownErr = nil
}

func TestAuthBrokerContainmentSurvivesFlowRetirement(t *testing.T) {
	for _, operation := range []string{"cancel", "supersede", "expire", "session close", "agent close"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newAuthFixture(t)
			fixture.useCodeAuthorization()
			node := &retryAuthBrokerClient{fakeOpenCodeClient: fixture.brokerNode, shutdownErr: opencode.ErrProcessContainmentIncomplete}
			launches := 0
			fixture.agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
				launches++

				return node, nil
			}
			presentation := fixture.authorize(t, nil)
			flow := fixture.broker.byID[presentation.FlowID]
			broker := flow.broker
			// A directory stands in for the neutralizer on every platform. No browser
			// or native executable is run by this ownership proof.
			neutralizer := t.TempDir()
			broker.removeShim = func() error {
				if broker.shim != nil {
					require.NoError(t, broker.shim.Remove())
				}

				return os.RemoveAll(neutralizer)
			}
			marker := filepath.Join(broker.home, "native", "retained")
			require.NoError(t, os.WriteFile(marker, []byte("still owned"), 0o600))
			params := mustJSON(t, map[string]any{authFieldSessionID: string(flow.sessionID), authFieldProviderID: flow.providerID, authFieldFlowID: flow.id})
			switch operation {
			case "cancel":
				_, err := fixture.broker.cancel(context.Background(), params)
				requireAuthFailure(t, err, authCauseProcess)
			case "supersede":
				require.ErrorIs(t, fixture.broker.supersede(authFlowKey{sessionID: flow.sessionID, providerID: flow.providerID}, authReasonSuperseded), opencode.ErrProcessContainmentIncomplete)
			case "expire":
				fixture.broker.expire(flow)
			case "session close":
				require.ErrorIs(t, fixture.session.Close(context.Background()), opencode.ErrProcessContainmentIncomplete)
			case "agent close":
				require.ErrorIs(t, fixture.agent.Close(), opencode.ErrProcessContainmentIncomplete)
				require.True(t, fixture.session.closed)
			}
			require.FileExists(t, marker)
			require.DirExists(t, neutralizer)
			require.False(t, node.closed)
			fixture.broker.brokerMu.Lock()
			require.Contains(t, fixture.broker.brokers, broker)
			fixture.broker.brokerMu.Unlock()

			// No second residence may be launched while prior cleanup is unresolved.
			_, err := fixture.broker.startBroker(context.Background(), acp.SessionId("another-session"))
			require.Error(t, err)
			require.Equal(t, 1, launches)

			node.allowShutdown()
			if operation == "cancel" {
				_, err = fixture.broker.cancel(context.Background(), params)
				require.NoError(t, err)
			}
			require.NoError(t, fixture.agent.Close())
			require.NoError(t, fixture.agent.Close())
			require.NoDirExists(t, broker.home)
			require.NoDirExists(t, neutralizer)
			require.True(t, node.closed)
			require.Empty(t, fixture.broker.brokers)
		})
	}
}

func TestAuthBrokerSessionCleanupPreservesPeer(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()
	own := fixture.authorize(t, nil)
	peerNode := &retryAuthBrokerClient{fakeOpenCodeClient: newFakeOpenCodeClient(t), shutdownErr: opencode.ErrProcessContainmentIncomplete}
	fixture.agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) { return peerNode, nil }
	peer, err := fixture.broker.startBroker(context.Background(), "peer")
	require.NoError(t, err)
	require.ErrorIs(t, fixture.broker.retireBroker(context.Background(), peer), opencode.ErrProcessContainmentIncomplete)
	require.NoError(t, fixture.broker.closeSession(context.Background(), fixture.session.id))
	require.NotContains(t, fixture.broker.byID, own.FlowID)
	require.DirExists(t, peer.home)
	require.False(t, peerNode.closed)
	peerNode.allowShutdown()
	require.NoError(t, fixture.agent.Close())
}

func TestBrokerDestroyRetainsPathsOnCancelledShutdown(t *testing.T) {
	node := &retryAuthBrokerClient{fakeOpenCodeClient: newFakeOpenCodeClient(t), shutdownErr: context.Canceled}
	broker := &authBroker{home: t.TempDir(), client: node}
	require.ErrorIs(t, broker.destroy(context.Background()), context.Canceled)
	require.DirExists(t, broker.home)
	node.allowShutdown()
	require.NoError(t, broker.destroy(context.Background()))
	require.NoError(t, broker.destroy(context.Background()))
	require.Equal(t, 2, node.shutdownCalls)
}

func TestBrokerStartWithUnprovenContainmentRetainsResidences(t *testing.T) {
	harness := newAuthAgent(t)
	neutralizeBrowserShimWhereUnsupported(t)
	var options opencode.StartOptions
	harness.agent.options.clientFactory = func(_ context.Context, opts opencode.StartOptions) (opencode.Client, error) {
		options = opts

		return nil, errors.Join(errors.New("startup failed"), opencode.ErrProcessContainmentIncomplete)
	}
	_, err := harness.broker.startBroker(context.Background(), harness.session.id)
	require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
	require.DirExists(t, options.Root)
	if options.BrowserShim != nil {
		require.DirExists(t, options.BrowserShim.Dir())
	}
	require.ErrorIs(t, harness.agent.Close(), opencode.ErrProcessContainmentIncomplete)
	require.DirExists(t, options.Root)
}

func TestSessionCloseOwnsBrokerBeforeFlowReceivesIt(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseStart := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStart)
	fixture.agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		close(entered)
		<-release

		return fixture.brokerNode, nil
	}
	params := fixture.authorizeParams(t, nil)
	result := make(chan error, 1)
	go func() {
		_, err := fixture.broker.authorize(context.Background(), params)
		result <- err
	}()
	<-entered
	fixture.broker.mu.Lock()
	flow := fixture.broker.flows[authFlowKey{sessionID: fixture.session.id, providerID: "xai"}]
	fixture.broker.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- fixture.broker.closeSession(context.Background(), fixture.session.id) }()
	<-flow.disarm
	select {
	case err := <-closed:
		t.Fatalf("close escaped in-flight broker ownership: %v", err)
	default:
	}
	releaseStart()
	require.NoError(t, <-closed)
	requireAuthFailure(t, <-result, authCauseFlowCancelled)
	require.True(t, fixture.brokerNode.closed)
	require.Empty(t, fixture.broker.brokers)
}
