package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

var (
	brokerReapHomes = opencode.ReapAbandonedHomes
	brokerMkdirTemp = os.MkdirTemp
	brokerCreateXDG = opencode.CreateRuntimeXDGDirs
	brokerRemoveAll = os.RemoveAll
)

// authBrokerPrefix names every per-flow broker home under the scratch parent.
// The prefix buys diagnosability; residence under the scratch parent is what
// makes the directory reclaimable by a host sweep.
const authBrokerPrefix = "acp-go-opencode-auth-broker-"

// authBroker is one pending flow's short-lived native home: a dedicated
// opencode serve with its own XDG root, never the long-lived runtime server
// whose store is the durable credential root. It holds no durable credential —
// the single fenced install lands in the shared runtime root — and it is
// destroyed on every terminal transition of its flow.
type authBroker struct {
	home   string
	client opencode.Client
	log    *slog.Logger
}

// startBroker creates the broker home under the adapter-supplied scratch
// parent, reaps any home a crashed predecessor left behind there, and starts
// the broker server inside it.
func (p *providerAuth) startBroker(ctx context.Context) (*authBroker, error) {
	agent := p.agent

	parent, err := ensureScratchParent(agent.options.ScratchDir)
	if err != nil {
		return nil, err
	}

	if reapErr := brokerReapHomes(parent, authBrokerPrefix); reapErr != nil {
		agent.log.DebugContext(ctx, "reap abandoned provider auth broker homes failed", loggableError(reapErr))
	}

	home, err := brokerMkdirTemp(parent, authBrokerPrefix)
	if err != nil {
		return nil, fmt.Errorf("create provider auth broker home: %w", err)
	}

	xdg, err := brokerCreateXDG(home)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create provider auth broker root: %w", err), brokerRemoveAll(home))
	}

	factory := agent.options.clientFactory
	if factory == nil {
		factory = runtimeStartServer
	}

	client, err := factory(ctx, opencode.StartOptions{
		Root:                     home,
		ScratchParent:            parent,
		ContainmentScratchParent: parent,
		DarwinBestEffort:         agent.containmentMode == RuntimeContainmentBestEffort,
		ReserveContainmentScratch: func(reservationCtx context.Context) (func(), error) {
			return acquireRuntimeResource(
				reservationCtx,
				agent.options.RuntimeResourceHooks.ReserveScratchRoot,
				RuntimeResourceDiscovery,
			)
		},
		ExecutablePath:  agent.options.ExecutablePath,
		Env:             cloneStringMap(agent.options.Env),
		Pure:            agent.options.Pure,
		LogLevel:        agent.options.LogLevel,
		HealthTimeout:   agent.options.HealthCheckTimeout,
		Logger:          agent.log,
		ExistingXDG:     xdg,
		SkipVersionGate: true,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start provider auth broker: %w", err), brokerRemoveAll(home))
	}

	return &authBroker{home: home, client: client, log: agent.log}, nil
}

// destroy terminates the broker process first and removes its directory after.
// Removing the directory alone would leave a live server holding credentials in
// memory and able to recreate its own path.
func (b *authBroker) destroy(ctx context.Context) {
	if b == nil {
		return
	}

	if err := b.client.Close(ctx); err != nil {
		b.log.WarnContext(ctx, "close provider auth broker failed", loggableError(err))
	}

	if err := brokerRemoveAll(b.home); err != nil {
		b.log.WarnContext(ctx, "remove provider auth broker home failed", loggableError(err))
	}
}
