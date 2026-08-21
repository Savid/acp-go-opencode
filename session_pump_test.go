package opencodeacp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

type panickingEventStreamClient struct{ *fakeOpenCodeClient }

func TestSessionPumpOwnershipHasNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	current.stopPump()
}

func TestReceivedAutonomousEventOwnsBeforeLaterPromptCanReserve(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	current, client, _ := lifecycleSession(t)
	current.mu.Lock()
	pump := current.pump
	current.mu.Unlock()
	require.NotNil(t, pump)

	received := make(chan struct{})
	releaseReceive := make(chan struct{})
	pauseRequested := make(chan struct{})
	var receiveOnce sync.Once
	var pauseOnce sync.Once
	pump.afterReceive = func() {
		receiveOnce.Do(func() { close(received) })
		<-releaseReceive
	}
	pump.beforePause = func() { pauseOnce.Do(func() { close(pauseRequested) }) }

	client.publishEvent(opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "assistant-a", "sessionID": current.idmap.NativeSessionID, "role": "assistant",
			"parentID": "user-a", "finish": "stop",
		}}),
	})
	requireSignal(t, received)

	posted := make(chan struct{})
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(posted)

		return opencode.NativeMessage{}, nil
	}

	promptDone := make(chan error, 1)
	go func() {
		_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, "prompt-b", "B"))
		promptDone <- err
	}()
	requireSignal(t, pauseRequested)
	select {
	case <-posted:
		t.Fatal("prompt B posted before the already-received A event fixed ownership")
	default:
	}

	close(releaseReceive)
	require.Error(t, <-promptDone)
	select {
	case <-posted:
		t.Fatal("prompt B posted after autonomous A had already claimed the session")
	default:
	}

	requireEventually(t, func() bool {
		cycle := current.currentCycle()

		return cycle != nil && cycle.origin == lifecycle.CauseActivity && cycle.assistantID == "assistant-a"
	}, "received A did not open its autonomous turn")
	client.publishSessionIdle(current.idmap.NativeSessionID)
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "autonomous A did not settle")
	current.stopPump()
	current.delivery.close()
}

func (c *panickingEventStreamClient) EventStream() <-chan opencode.EventStreamItem {
	panic("SECRET_SENTINEL")
}

func TestPumpPanicContainsExactGenerationWithoutPayloadDisclosure(t *testing.T) {
	current, base, _ := lifecycleSession(t)
	current.stopPump()
	client := &panickingEventStreamClient{fakeOpenCodeClient: base}
	var logs bytes.Buffer
	current.agent.log = slog.New(slog.NewTextHandler(&logs, nil))

	current.mu.Lock()
	current.client = client
	generation := current.runtimeGeneration
	current.mu.Unlock()
	current.agent.mu.Lock()
	current.agent.runtime = client
	current.agent.runtimeGeneration = generation
	current.agent.mu.Unlock()
	oldBinding := testIncarnation(current)
	binding := &nativeIncarnationBinding{
		client: client, generation: generation, stream: oldBinding.stream, registry: oldBinding.registry,
	}
	current.lifecycleMu.Lock()
	current.incarnation = binding
	current.lifecycleMu.Unlock()

	pump := &sessionPump{session: current, binding: binding,
		done: make(chan struct{}), released: make(chan struct{}, 1)}
	pump.run(context.Background())
	<-pump.done
	requireSignal(t, base.closeSignal)
	require.Error(t, current.lifecycleFailure())
	require.NotContains(t, logs.String(), "SECRET_SENTINEL")
}

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
	require.ErrorContains(t, blocked.markNativeTerminal(ctx), "commit refused")
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

// TestForeignAndMalformedNativeEventsChangeNothing proves the structural reading
// of the native event set: an event that names no session, decodes to no payload,
// or reports a status this adapter draws no boundary from leaves the session
// exactly as it was.
func TestForeignAndMalformedNativeEventsChangeNothing(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID
	idleDelivered := make(chan struct{})
	var idleOnce sync.Once
	connection.mu.Lock()
	connection.updateHook = func(notification acp.SessionNotification) {
		envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any)
		if !ok {
			return
		}
		event, _ := envelope["event"].(map[string]any)
		state, _ := event["state"].(string)
		if event["type"] == string(lifecycle.EventStateUpdate) && state == string(lifecycle.ForegroundIdle) {
			idleOnce.Do(func() { close(idleDelivered) })
		}
	}
	connection.mu.Unlock()

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
	requireSignal(t, idleDelivered)

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
	client.publishEvent(opencode.Event{Type: opencode.EventQuestionV2Asked, Properties: mustJSONValue(map[string]any{
		"sessionID": native, "questions": "malformed",
	})})

	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the broken cycle never ended")
	require.ErrorContains(t, current.cycleFailure(cycle), "invalid OpenCode question event")
	requireEventually(t, client.isClosed, "the producer of output the session could not report was not contained")
	require.Error(t, current.lifecycleFailure(), "the broken incarnation remained admissible")
	requireLifecycleReduces(t, connection)
}

// TestStreamTerminalBeforeAcceptanceFailsTheAwaitingDispatch proves loss of the
// exact incarnation releases a reserved POST even though no request-specific
// user-message evidence arrived.
func TestStreamTerminalBeforeAcceptanceFailsTheAwaitingDispatch(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)
	client.omitPromptEvidence = true
	client.dispatchMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.publishStreamTerminal(errors.New("SECRET_STREAM_FAILURE"))
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	assertTurnFailed(t, err, causeTransport, "")
	require.NotContains(t, err.Error(), "SECRET_STREAM_FAILURE")
	require.Nil(t, current.currentCycle(), "a frame that was never accepted opened a turn")
}

// TestPumpWithoutARuntimeBindingRoutesNothing proves the pump belongs to one
// runtime binding: a session with no binding starts none. A prompt is not
// dispatched in this state because native idle would have no ordered consumer.
func TestPumpWithoutARuntimeBindingRoutesNothing(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())
	current := testSession(t, agent, client)
	current.stopPump()

	current.lifecycleMu.Lock()
	bound := current.incarnation
	current.incarnation = nil
	current.lifecycleMu.Unlock()

	current.startPump()

	current.lifecycleMu.Lock()
	require.Nil(t, current.pump, "a session with no runtime binding started a pump")
	current.incarnation = bound
	current.lifecycleMu.Unlock()
}
