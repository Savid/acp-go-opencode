package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func restoreBrokerSeams(t *testing.T) {
	t.Helper()

	t.Cleanup(func() {
		brokerReapHomes = opencode.ReapAbandonedHomes
		brokerMkdirTemp = os.MkdirTemp
		brokerCreateXDG = opencode.CreateRuntimeXDGDirs
		brokerRemoveAll = os.RemoveAll
	})
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
		{name: "xdg creation", setup: func(_ *testing.T, _ *Agent) {
			brokerCreateXDG = func(string) (opencode.XDGDirs, error) { return opencode.XDGDirs{}, failure }
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

func TestDestroyReportsCloseAndRemoveFailures(t *testing.T) {
	restoreBrokerSeams(t)

	node := newFakeOpenCodeClient()
	node.closeErr = errors.New("close")

	brokerRemoveAll = func(string) error { return errors.New("remove") }

	broker := &authBroker{home: t.TempDir(), client: node, log: slog.New(slog.DiscardHandler)}
	broker.destroy(context.Background())

	require.True(t, node.closed)
}

func TestDestroyToleratesANilBroker(t *testing.T) {
	var broker *authBroker

	broker.destroy(context.Background())
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
