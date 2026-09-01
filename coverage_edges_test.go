package opencodeacp

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

type cancelAfterFirstErrContext struct {
	context.Context //nolint:containedctx // Test context stages the gate's post-admission cancellation recheck.
	calls           atomic.Int32
	done            <-chan struct{}
}

func (c *cancelAfterFirstErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterFirstErrContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterFirstErrContext) Err() error {
	if c.calls.Add(1) > 1 {
		return context.Canceled
	}

	return nil
}

func TestRuntimeOptionAndScratchEdges(t *testing.T) {
	require.Error(t, validateRuntimeOptions(Options{Env: map[string]string{"home": "/reserved"}}))
	require.Error(t, validateRuntimeOptions(Options{Home: "relative"}))
	require.Error(t, validateRuntimeOptions(Options{hostAuthorityConfigured: true}))
	require.Error(t, validateDurableHomePath("/tmp/control\npath"))
	require.True(t, reservedOpenCodeEnvKey(privateAdapterEnvPrefix+"TOKEN"))
	require.True(t, adapterPrivateEnvKey(privateAdapterEnvPrefix+"TOKEN"))

	homeAgent := &Agent{options: Options{Home: "/durable/home"}}
	root, generated, err := homeAgent.newRuntimeRoot()
	require.NoError(t, err)
	require.Equal(t, "/durable/home", root)
	require.False(t, generated)

	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	broken := &Agent{options: Options{ScratchDir: filepath.Join(file, "child")}}
	_, _, err = broken.newRuntimeRoot()
	require.ErrorContains(t, err, "create scratch parent")
}

func TestRecoveryAndLifecycleAdmissionEdges(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ctx := &cancelAfterFirstErrContext{Context: t.Context(), done: done}
	var gate sessionRecoveryGate
	require.ErrorIs(t, gate.lock(ctx), context.Canceled)
	require.NoError(t, gate.lock(t.Context()), "cancelled post-admission check must return the permit")
	gate.unlock()

	agent := NewAgent()
	id := acp.SessionId("session")
	flight := &sessionLifecycleFlight{done: make(chan struct{})}
	fence := make(chan struct{})
	close(fence)
	agent.lifecycleFence = fence
	agent.lifecycleFlights = make(map[acp.SessionId]*sessionLifecycleFlight)
	agent.lifecycleFlights[id] = flight
	_, err := agent.acquireSessionLifecycle(t.Context(), id)
	require.Error(t, err)
	agent.releaseSessionLifecycle(id, &sessionLifecycleFlight{})

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = agent.LoadSession(cancelled, acp.LoadSessionRequest{SessionId: id})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAgentStoreAndActiveLoadMatchEdges(t *testing.T) {
	agent := NewAgent()
	id := acp.SessionId("deleted")
	agent.deleted[id] = struct{}{}
	require.Error(t, agent.storeStartedSession(&session{id: id}))

	active := &session{
		cwd: "/cwd", providerID: "provider", modelID: "model", mode: "build", permission: "ask",
		outputSchema: map[string]any{"type": "object"}, carrier: sessionCarrier{},
	}
	snapshot := active.snapshot()
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{Model: "other/model"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{Mode: "plan"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{PermissionSet: true, Permission: "allow"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{OutputSchema: map[string]any{"type": "array"}}, sessionCarrier{}))
}
