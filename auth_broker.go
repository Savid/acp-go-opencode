package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

var (
	brokerMkdirTemp      = os.MkdirTemp
	brokerChmod          = os.Chmod
	brokerCreateXDG      = opencode.CreateRuntimeXDGDirs
	brokerRemoveAll      = os.RemoveAll
	brokerNewBrowserShim = opencode.NewBrowserShim
)

// authBrokerPrefix names every per-flow broker home under the scratch parent.
// The prefix keeps each ordinary provider-auth flow diagnosable and confined
// to the adapter's resolved scratch parent.
const authBrokerPrefix = "acp-go-opencode-auth-broker-"

// authBroker is one pending flow's short-lived native home: a dedicated
// opencode serve with its own XDG root, never the long-lived runtime server
// whose store is the durable credential root. It holds no durable credential —
// the single fenced install lands in the shared runtime root — and it is
// retired on every terminal transition of its flow. Cleanup retains its native
// handle, home, and browser shim until shutdown is proven.
type authBroker struct {
	mu           sync.Mutex
	sessionID    acp.SessionId
	shutdownDone bool
	destroyed    bool
	startErr     error
	home         string
	shim         *opencode.BrowserShim
	client       opencode.Client
	log          *slog.Logger

	// removeShim replaces shim deletion. Destruction must report a shim it
	// failed to delete, so the failure is injected per broker. A package-level
	// seam would race with the flow goroutines that call destroy.
	removeShim func() error
}

// removeBrowserShim deletes the shim through the broker's own seam.
func (b *authBroker) removeBrowserShim() error {
	if b.removeShim != nil {
		return b.removeShim()
	}

	return b.shim.Remove()
}

// startBroker creates the broker home under the adapter-supplied scratch
// parent and starts the broker server inside it.
func (p *providerAuth) startBroker(ctx context.Context, sessionID acp.SessionId) (*authBroker, error) {
	p.brokerMu.Lock()
	defer p.brokerMu.Unlock()

	p.mu.Lock()
	admitted := p.sessionAdmitted(sessionID)
	p.mu.Unlock()

	if !admitted {
		return nil, authSessionUnknown()
	}
	// Drain retired ownership before allocating another residence. A failed
	// cleanup cannot accumulate an unbounded sequence of abandoned launches.
	if err := p.cleanupBrokersLocked(ctx, "", false); err != nil {
		return nil, err
	}

	agent := p.agent

	parent, err := agent.ensureScratchParent()
	if err != nil {
		return nil, err
	}

	home, err := brokerMkdirTemp(parent, authBrokerPrefix)
	if err != nil {
		return nil, fmt.Errorf("create provider auth broker home: %w", err)
	}

	if chmodErr := brokerChmod(home, 0o711); chmodErr != nil {
		return nil, errors.Join(fmt.Errorf("protect provider auth broker home: %w", chmodErr), brokerRemoveAll(home))
	}

	// A login leg the operator's browser can reach is an uncontrolled grant, not
	// noise: the native callback listens on this host. The shim shadows every
	// launcher the broker could exec, and a platform where it cannot refuses the
	// leg instead.
	shim, err := brokerNewBrowserShim(parent)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("neutralize provider auth broker browser launch: %w", err), brokerRemoveAll(home))
	}

	nativeHome := filepath.Join(home, "native")

	xdg, err := brokerCreateXDG(nativeHome)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create provider auth broker root: %w", err), shim.Remove(), brokerRemoveAll(home))
	}

	factory := agent.options.clientFactory
	if factory == nil {
		factory = runtimeStartServer
	}

	// StartServer protects ControlRoot as 0700. Keep it below the broker home.
	controlRoot := filepath.Join(home, "control")

	broker := &authBroker{home: home, shim: shim, sessionID: sessionID, log: agent.log}

	if p.brokers == nil {
		p.brokers = make(map[*authBroker]bool)
	}

	p.brokers[broker] = false

	client, err := factory(ctx, opencode.StartOptions{
		Root:          nativeHome,
		ControlRoot:   controlRoot,
		ScratchParent: parent,
		NativeEnvironment: func() map[string]string {
			return cloneStringMap(agent.options.implicitEnvironment)
		},
		ExecutablePath:  agent.options.ExecutablePath,
		BrowserShim:     shim,
		Env:             cloneStringMap(agent.options.Env),
		Pure:            agent.options.Pure,
		LogLevel:        agent.options.LogLevel,
		HealthTimeout:   agent.options.HealthCheckTimeout,
		Logger:          agent.log,
		ExistingXDG:     xdg,
		SkipVersionGate: true,
	})

	broker.client = client
	if err != nil {
		// A failed start may have no retry handle. Preserve its residences when
		// containment was not proved; never infer safety from a nil client.
		if client == nil && (errors.Is(err, opencode.ErrProcessContainmentIncomplete) || errors.Is(err, ErrContainmentIncomplete)) {
			broker.startErr = err
		}

		p.brokers[broker] = true

		return nil, errors.Join(fmt.Errorf("start provider auth broker: %w", err), p.destroyBrokerLocked(ctx, broker))
	}

	return broker, nil
}

// authBrokerRemoveAttempts bounds retries of transient filesystem failures.
const authBrokerRemoveAttempts = 5

var authBrokerRemoveBackoff = 100 * time.Millisecond

// destroy retains both the native home and browser neutralizer until shutdown
// proves completion. Concurrent cleanup callers join through the broker lock.
func (b *authBroker) destroy(ctx context.Context) error {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.destroyed {
		return nil
	}

	if b.startErr != nil {
		return containmentFailure(b.startErr)
	}

	if !b.shutdownDone {
		if b.client != nil {
			if err := b.client.Shutdown(ctx); err != nil {
				return containmentFailure(err)
			}
		}

		b.shutdownDone = true
	}

	if err := removeBrokerHome(b.home); err != nil {
		return fmt.Errorf("remove provider auth broker home: %w", err)
	}
	// The neutralizer outlives every process it shadows.
	if err := b.removeBrowserShim(); err != nil {
		return fmt.Errorf("remove provider auth broker browser shim: %w", err)
	}

	b.destroyed = true

	return nil
}

// retireBroker transfers the exact handle from a flow to agent cleanup ownership.
func (p *providerAuth) retireBroker(ctx context.Context, broker *authBroker) error {
	if broker == nil {
		return nil
	}

	p.brokerMu.Lock()
	defer p.brokerMu.Unlock()

	if p.brokers == nil {
		p.brokers = make(map[*authBroker]bool)
	}

	p.brokers[broker] = true

	return p.destroyBrokerLocked(ctx, broker)
}

func (p *providerAuth) destroyBrokerLocked(ctx context.Context, broker *authBroker) error {
	if err := broker.destroy(ctx); err != nil {
		p.agent.log.WarnContext(ctx, "provider auth broker cleanup failed", loggableError(err))

		return err
	}

	delete(p.brokers, broker)

	return nil
}

// cleanupBrokersLocked retries retained cleanup. Closing selects active brokers
// too, including starts whose flow has not yet received the runtime handle.
func (p *providerAuth) cleanupBrokersLocked(ctx context.Context, sessionID acp.SessionId, closing bool) error {
	var result error

	for broker, retired := range p.brokers {
		if sessionID != "" && broker.sessionID != sessionID {
			continue
		}

		if closing {
			retired = true
			p.brokers[broker] = true
		}

		if retired {
			result = errors.Join(result, p.destroyBrokerLocked(ctx, broker))
		}
	}

	return result
}

func (p *providerAuth) retryBrokerCleanup(ctx context.Context, sessionID acp.SessionId) error {
	p.brokerMu.Lock()
	defer p.brokerMu.Unlock()

	return p.cleanupBrokersLocked(ctx, sessionID, false)
}

// removeBrokerHome retries transient filesystem failures after shutdown.
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
