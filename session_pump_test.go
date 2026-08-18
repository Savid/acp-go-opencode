package opencodeacp

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// TestSettleAgentCycleSettlesTheRecordedOutcome proves an agent-origin cycle
// settles as failed when it carries a failure, and that a prefix that cannot be
// committed fences the stream and surfaces the commit error instead of settling.
func TestSettleAgentCycleSettlesTheRecordedOutcome(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	failed := &foregroundCycle{id: "cycle-1", failure: errors.New("native turn failed"), signal: make(chan struct{})}
	require.NoError(t, session.settleAgentCycle(ctx, failed))
	require.True(t, failed.settled)

	store := &errorSessionStore{err: errors.New("commit refused")}
	blocked := testSession(t, NewAgent(WithSessionStore(store)), newFakeOpenCodeClient())
	cycle := &foregroundCycle{id: "cycle-2", signal: make(chan struct{})}
	require.ErrorContains(t, blocked.settleAgentCycle(ctx, cycle), "commit refused")
	require.False(t, cycle.settled)

	// An agent-origin terminal whose settlement fails records the failure on the
	// cycle it could not settle.
	open := &foregroundCycle{id: "cycle-3", origin: lifecycle.CauseActivity, signal: make(chan struct{})}
	blocked.lifecycleMu.Lock()
	blocked.cycle = open
	blocked.lifecycleMu.Unlock()
	blocked.markNativeTerminal(ctx)
	require.ErrorContains(t, blocked.cycleFailure(open), "commit refused")
}

// TestNativeRepliedActionStateMapsEveryResolution proves each native resolution
// form maps to exactly one terminal action state, including the reply forms that
// carry no accept/decline distinction.
func TestNativeRepliedActionStateMapsEveryResolution(t *testing.T) {
	t.Parallel()

	require.Equal(t, lifecycle.ActionCancelled,
		nativeRepliedActionState(opencode.EventQuestionRejected, opencode.ActionRepliedEvent{}))
	require.Equal(t, lifecycle.ActionDeclined,
		nativeRepliedActionState(opencode.EventPermissionReplied, opencode.ActionRepliedEvent{}))
	require.Equal(t, lifecycle.ActionAccepted,
		nativeRepliedActionState(opencode.EventQuestionV2Replied, opencode.ActionRepliedEvent{}))
}

// TestReconcileNativeActionsWithoutARuntimeIsANoOp proves a session whose
// runtime binding is gone reconciles nothing rather than failing.
func TestReconcileNativeActionsWithoutARuntimeIsANoOp(t *testing.T) {
	t.Parallel()

	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	session.stopPump()
	session.mu.Lock()
	session.client = nil
	session.mu.Unlock()

	require.NoError(t, session.reconcileNativeActions(context.Background()))
}

// TestRecordNativeFailureAttributesToTheOpenCycle proves a pump-side failure is
// recorded against the open cycle, and that a cycle already carrying a failure
// keeps the first one.
func TestRecordNativeFailureAttributesToTheOpenCycle(t *testing.T) {
	t.Parallel()

	session := testSession(t, NewAgent(), newFakeOpenCodeClient())

	session.lifecycleMu.Lock()
	cycle := &foregroundCycle{id: "cycle-1"}
	session.cycle = cycle
	session.lifecycleMu.Unlock()

	session.recordNativeFailure(nil)
	require.NoError(t, session.cycleFailure(cycle))

	session.recordNativeFailure(errors.New("late event failed"))
	require.ErrorContains(t, session.cycleFailure(cycle), "late event failed")

	session.recordNativeFailure(errors.New("second failure"))
	require.ErrorContains(t, session.cycleFailure(cycle), "late event failed")
}

// TestForeignAndMalformedNativeEventsChangeNothing proves the structural reading
// of the native event set: an event that names no session, decodes to no payload,
// or reports a status this adapter draws no boundary from leaves the session
// exactly as it was.
func TestForeignAndMalformedNativeEventsChangeNothing(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	for _, event := range []opencode.Event{
		// A status transition that is not busy opens no turn.
		{Type: opencode.EventSessionStatus, Properties: mustJSONValue(map[string]any{
			"sessionID": native, "status": map[string]any{"type": "idle"},
		})},
		// A native error carrying no error names no failure.
		{Type: opencode.EventSessionError, Properties: mustJSONValue(map[string]any{
			"sessionID": native, "error": nil,
		})},
		// A part event that decodes to no part belongs to no message.
		{Type: opencode.EventMessagePartCreated, Properties: mustJSONValue(map[string]any{"id": "part-x"})},
		// A resolution that names no session resolves no action here.
		{Type: opencode.EventPermissionV2Replied, Properties: mustJSONValue(map[string]any{
			"requestID": "permission-x", "reply": permissionReplyOnce,
		})},
	} {
		client.publishEvent(event)
	}

	// Work queued behind them proves the pump routed every one.
	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	requireEventually(t, func() bool { return current.currentCycle() != nil }, "the pump stalled")

	// A busy status inside the open cycle is that cycle continuing, not a second one.
	cycle := current.currentCycle()
	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	client.publishSessionIdle(native)
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the agent-origin turn never settled")

	require.Equal(t, []string{"lifecycle_snapshot", "state_update", "state_update"}, connection.lifecycleEvents(t))
	require.NoError(t, current.cycleFailure(cycle))
	requireLifecycleReduces(t, connection)
}

// TestUndecodableQuestionFailsTheOpenCycle proves a question event this adapter
// cannot read is a delivery failure rather than a silent skip: the ordered
// representation of the turn is broken, so the native work is interrupted and the
// cycle ends on that failure.
func TestUndecodableQuestionFailsTheOpenCycle(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	requireEventually(t, func() bool { return current.currentCycle() != nil }, "no agent-origin cycle opened")

	cycle := current.currentCycle()
	client.publishEvent(opencode.Event{Type: opencode.EventQuestionV2Asked, Properties: mustJSONValue(map[string]any{})})

	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the broken cycle never ended")
	require.ErrorContains(t, current.cycleFailure(cycle), "invalid OpenCode question event")
	require.Equal(t, []string{native}, client.abortedSessions(), "output the session cannot report was left running")
	requireLifecycleOutcome(t, connection, lifecycle.OutcomeFailed)
	requireLifecycleReduces(t, connection)
}

// TestNativeErrorBeforeAcceptanceFailsTheAwaitingDispatch proves an error the
// harness published for the run a pending frame started answers that frame: the
// route is released with the native failure rather than waiting out a run that
// already died.
func TestNativeErrorBeforeAcceptanceFailsTheAwaitingDispatch(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)

	current.mu.Lock()
	current.mcpRefreshPending = true
	current.mu.Unlock()

	native := current.idmap.NativeSessionID
	client.refreshMCPFunc = func(context.Context, []opencode.MCPServerConfig) error {
		// The MCP refresh runs after the turn opens and before the dispatch hold,
		// which is the one window a native event is routed against a live turn
		// that has accepted nothing yet.
		client.publishEvent(opencode.Event{
			Type: opencode.EventSessionError,
			Properties: mustJSONValue(map[string]any{
				"sessionID": native,
				"error":     providerNativeError("provider exploded", 429, "rate_limit_exceeded"),
			}),
		})

		for current.takePendingDispatchFailure() == nil {
			runtime.Gosched()
		}

		return nil
	}
	client.dispatchMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{}, ctx.Err()
	}

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "provider exploded")
	require.Nil(t, current.currentCycle(), "a frame that was never accepted opened a turn")
}

// TestPumpWithoutARuntimeBindingRoutesNothing proves the pump belongs to one
// runtime binding: a session with no binding starts none, and a dispatch that
// releases the event gate with no pump to wake completes anyway.
func TestPumpWithoutARuntimeBindingRoutesNothing(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	current := testSession(t, NewAgent(), client)
	current.stopPump()

	current.mu.Lock()
	bound := current.client
	current.client = nil
	current.mu.Unlock()

	current.startPump()

	current.mu.Lock()
	require.Nil(t, current.pump, "a session with no runtime binding started a pump")
	current.client = bound
	current.mu.Unlock()

	// The command route reports its own completion, so the turn settles with no
	// pump to route events and no pump to wake when the gate is released.
	client.dispatchCommand = func(_ context.Context, id string, _ opencode.CommandRequest) (opencode.NativeMessage, error) {
		client.stageAssistantMessage(id, "assistant-1")

		return opencode.NativeMessage{}, nil
	}

	response, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "/review"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}
