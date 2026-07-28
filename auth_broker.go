package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

var (
	brokerReapHomes      = opencode.ReapAbandonedHomes
	brokerMkdirTemp      = os.MkdirTemp
	brokerCreateXDG      = opencode.CreateRuntimeXDGDirs
	brokerRemoveAll      = os.RemoveAll
	brokerNewBrowserShim = opencode.NewBrowserShim
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
	shim   *opencode.BrowserShim
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

	// A login leg the operator's browser can reach is an uncontrolled grant, not
	// noise: the native callback listens on this host. The shim shadows every
	// launcher the broker could exec, and a platform where it cannot refuses the
	// leg instead.
	shim, err := brokerNewBrowserShim(parent)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("neutralize provider auth broker browser launch: %w", err), brokerRemoveAll(home))
	}

	xdg, err := brokerCreateXDG(home)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create provider auth broker root: %w", err), shim.Remove(), brokerRemoveAll(home))
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
		LeaseDir:        home,
		BrowserShim:     shim,
		Env:             cloneStringMap(agent.options.Env),
		Pure:            agent.options.Pure,
		LogLevel:        agent.options.LogLevel,
		HealthTimeout:   agent.options.HealthCheckTimeout,
		Logger:          agent.log,
		ExistingXDG:     xdg,
		SkipVersionGate: true,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start provider auth broker: %w", err), shim.Remove(), brokerRemoveAll(home))
	}

	return &authBroker{home: home, shim: shim, client: client, log: agent.log}, nil
}

// authBrokerRemoveAttempts bounds how long destruction waits out descendants of
// the broker process that are still writing into the home. A plugin install
// running under the server keeps creating directories as removal walks them, so
// the first pass fails with a not-empty directory the caller never sees again.
const authBrokerRemoveAttempts = 5

// authBrokerRemoveBackoff is the pause between removal attempts.
var authBrokerRemoveBackoff = 100 * time.Millisecond

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

	if err := removeBrokerHome(b.home); err != nil {
		b.log.WarnContext(ctx, "remove provider auth broker home failed", loggableError(err))
	}

	// The shim outlives the process it shadows, so it goes last.
	if err := b.shim.Remove(); err != nil {
		b.log.WarnContext(ctx, "remove provider auth broker browser shim failed", loggableError(err))
	}
}

// removeBrokerHome removes the home, retrying while a descendant of the closed
// broker keeps repopulating it. Destruction is what guarantees a stale native
// approval lands in a store that no longer exists, so a home that survives is
// the one failure this leg cannot report as success on the first try.
func removeBrokerHome(home string) error {
	var err error

	for attempt := range authBrokerRemoveAttempts {
		if err = brokerRemoveAll(home); err == nil {
			return nil
		}

		if attempt < authBrokerRemoveAttempts-1 {
			time.Sleep(authBrokerRemoveBackoff)
		}
	}

	return err
}
