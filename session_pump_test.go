package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"sort"
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
		current.lifecycleMu.Lock()
		defer current.lifecycleMu.Unlock()
		cycle := current.cycle

		return cycle != nil && cycle.origin == lifecycle.CauseActivity && cycle.assistantID == "assistant-a"
	}, "received A did not open its autonomous turn")
	client.publishSessionIdle(current.idmap.NativeSessionID)
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "autonomous A did not settle")
	current.stopPump()
	current.delivery.close()
}

// nativeMessageUpdated builds a `message.updated` payload in the shape OpenCode
// 1.18 publishes: the session named at the top level and again inside the message,
// a step's parent naming the user message that caused the turn, and a finish
// reason paired with a completion timestamp.
func nativeMessageUpdated(sessionID, id, role, parentID, finish string) opencode.Event {
	info := map[string]any{
		"id": id, "sessionID": sessionID, "role": role,
		"time": map[string]any{"created": 1786587079919},
	}
	if parentID != "" {
		info["parentID"] = parentID
	}

	if finish != "" {
		info["finish"] = finish
		info["time"] = map[string]any{"created": 1786587079919, "completed": 1786587081000}
	}

	return opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"sessionID": sessionID, "info": info}),
	}
}

// nativePartUpdated builds a `message.part.updated` payload. OpenCode addresses
// every part to the message it belongs to, which for the echo of a dispatched
// prompt is the user message itself.
func nativePartUpdated(sessionID, messageID, partID, partType string) opencode.Event {
	return opencode.Event{
		Type: opencode.EventMessagePartUpdated,
		Properties: mustJSONValue(map[string]any{"sessionID": sessionID, "part": map[string]any{
			"id": partID, "sessionID": sessionID, "messageID": messageID,
			"type": partType, "text": "step text",
		}}),
	}
}

// nativeSessionUpdated builds a `session.updated` payload. OpenCode publishes it
// repeatedly inside a turn as cost, tokens and title move, and this adapter has no
// structured reading for it.
func nativeSessionUpdated(sessionID string) opencode.Event {
	return opencode.Event{
		Type: "session.updated",
		Properties: mustJSONValue(map[string]any{"sessionID": sessionID, "info": map[string]any{
			"id": sessionID, "slug": "curious-garden", "projectID": "global",
			"title": "New session", "version": "1.18.18",
		}}),
	}
}

// TestPromptOwnsEveryStepOfOneNativeTurn walks the events one ordinary OpenCode
// turn publishes. The harness answers a single prompt with a new assistant message
// per step, echoes the prompt's own parts back, and punctuates the whole thing with
// session.updated. Every frame here belongs to the prompt that caused it or to
// nothing at all, and none of it is evidence against the runtime.
func TestPromptOwnsEveryStepOfOneNativeTurn(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	native := current.idmap.NativeSessionID
	ctx := withTurnRoute(context.Background(), "prompt")
	submission := lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client"}
	cycle, _, err := current.reservePromptCycle(ctx, "msg_user", submission)
	require.NoError(t, err)
	require.NoError(t, current.acceptPromptCycle(ctx, cycle, submission))

	for _, frame := range []struct {
		name      string
		event     opencode.Event
		ownership nativeEventOwnership
	}{
		{"dispatched user message", nativeMessageUpdated(native, "msg_user", roleUser, "", ""), nativeEventCycle},
		{"echoed prompt part", nativePartUpdated(native, "msg_user", "prt_user", "text"), nativeEventCycle},
		{"mid-turn session update", nativeSessionUpdated(native), nativeEventInert},
		{"first step", nativeMessageUpdated(native, "msg_step1", roleAssistant, "msg_user", "tool-calls"), nativeEventCycle},
		{"first step tool part", nativePartUpdated(native, "msg_step1", "prt_tool", "tool"), nativeEventCycle},
		{"second step", nativeMessageUpdated(native, "msg_step2", roleAssistant, "msg_user", "stop"), nativeEventCycle},
		{"second step text part", nativePartUpdated(native, "msg_step2", "prt_text", "text"), nativeEventCycle},
		{
			"idle",
			opencode.Event{Type: opencode.EventSessionIdle, Properties: mustJSONValue(map[string]any{"sessionID": native})},
			nativeEventCycle,
		},
	} {
		observation, ok := current.observeNativeEvent(binding, frame.event)
		require.True(t, ok, frame.name)
		require.Equal(t, frame.ownership, observation.ownership, frame.name)
		require.NoError(t, current.routeNativeEventForIncarnation(ctx, binding, frame.event, observation), frame.name)
	}

	require.Same(t, cycle, current.currentCycle())
	require.True(t, cycle.ownsAssistant("msg_step1"))
	require.Equal(t, "msg_step2", cycle.assistantID)
	require.True(t, cycle.idle)
	current.delivery.close()
}

// TestPromptOwnsRequestsFromAnyOfItsSteps proves the correlation a native request
// carries is read against every step of the turn, not just its first, and that a
// request naming no tool at all still belongs to the only cycle open.
func TestPromptOwnsRequestsFromAnyOfItsSteps(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	native := current.idmap.NativeSessionID
	ctx := withTurnRoute(context.Background(), "prompt")
	submission := lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client"}
	cycle, _, err := current.reservePromptCycle(ctx, "msg_user", submission)
	require.NoError(t, err)
	require.NoError(t, current.acceptPromptCycle(ctx, cycle, submission))

	for _, event := range []opencode.Event{
		nativeMessageUpdated(native, "msg_user", roleUser, "", ""),
		nativeMessageUpdated(native, "msg_step1", roleAssistant, "msg_user", "tool-calls"),
		nativeMessageUpdated(native, "msg_step2", roleAssistant, "msg_user", ""),
	} {
		observation, ok := current.observeNativeEvent(binding, event)
		require.True(t, ok)
		require.Equal(t, nativeEventCycle, observation.ownership)
	}

	for _, request := range []struct {
		name  string
		event opencode.Event
	}{
		{"permission from the second step", opencode.Event{
			Type: opencode.EventPermissionV2Asked,
			Properties: mustJSONValue(map[string]any{
				"id": "per_1", "sessionID": native, "permission": "bash",
				"tool": map[string]any{"messageID": "msg_step2", "callID": "call_1"},
			}),
		}},
		{"question with no tool", opencode.Event{
			Type: opencode.EventQuestionV2Asked,
			Properties: mustJSONValue(map[string]any{
				"id": "que_1", "sessionID": native,
				"questions": []map[string]any{{"question": "which branch?"}},
			}),
		}},
	} {
		observation, ok := current.observeNativeEvent(binding, request.event)
		require.True(t, ok, request.name)
		require.Equal(t, nativeEventCycle, observation.ownership, request.name)
		require.Same(t, cycle, observation.cycle, request.name)
	}
}

// TestActionBeforeDispatchEvidenceIsDeclinedNotFatal proves an action this session
// cannot route is answered and dropped. The harness is never left waiting, and the
// runtime every peer session shares keeps running.
func TestActionBeforeDispatchEvidenceIsDeclinedNotFatal(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	native := current.idmap.NativeSessionID
	ctx := withTurnRoute(context.Background(), "prompt")
	submission := lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client"}
	cycle, _, err := current.reservePromptCycle(ctx, "msg_user", submission)
	require.NoError(t, err)
	require.NoError(t, current.acceptPromptCycle(ctx, cycle, submission))

	permission := opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "per_1", "sessionID": native, "permission": "bash",
			"tool": map[string]any{"messageID": "msg_earlier", "callID": "call_earlier"},
		}),
	}
	question := opencode.Event{
		Type: opencode.EventQuestionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "que_1", "sessionID": native,
			"questions": []map[string]any{{"question": "which branch?"}},
		}),
	}

	for _, event := range []opencode.Event{permission, question} {
		observation, ok := current.observeNativeEvent(binding, event)
		require.True(t, ok)
		require.Equal(t, nativeEventPromptBeforeEvidence, observation.ownership)
		require.NoError(t, current.routeNativeEventForIncarnation(ctx, binding, event, observation))
	}

	require.Equal(t, 1, client.permissionReplyCount())
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Equal(t, 1, client.questionRejectCount())
	require.True(t, current.incarnationIsCurrent(binding))
	require.Same(t, cycle, current.currentCycle())
}

func TestPromptOwnsStatusAndTodoAfterExactDispatchEvidence(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	ctx := withTurnRoute(context.Background(), "prompt")
	submission := lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client"}
	cycle, _, err := current.reservePromptCycle(ctx, "prompt-message", submission)
	require.NoError(t, err)

	evidence := opencode.Event{Type: opencode.EventMessageUpdated, Properties: mustJSONValue(map[string]any{
		"info": map[string]any{
			"id": "prompt-message", "sessionID": current.idmap.NativeSessionID, "role": roleUser,
		},
	})}
	observation, ok := current.observeNativeEvent(binding, evidence)
	require.True(t, ok)
	require.Equal(t, nativeEventCycle, observation.ownership)
	require.Same(t, cycle, observation.cycle)
	require.NoError(t, current.acceptPromptCycle(ctx, cycle, submission))
	require.NoError(t, current.routeNativeEventForIncarnation(ctx, binding, evidence, observation))

	for _, event := range []opencode.Event{
		{
			Type: opencode.EventSessionStatus,
			Properties: mustJSONValue(map[string]any{
				"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
			}),
		},
		{
			Type: opencode.EventTodoUpdated,
			Properties: mustJSONValue(map[string]any{
				"sessionID": current.idmap.NativeSessionID,
				"todos":     []map[string]any{{"content": "finish", "status": "in_progress", "priority": "high"}},
			}),
		},
	} {
		observation, ok = current.observeNativeEvent(binding, event)
		require.True(t, ok)
		require.Equal(t, nativeEventCycle, observation.ownership)
		require.Same(t, cycle, observation.cycle)
		require.NoError(t, current.routeNativeEventForIncarnation(ctx, binding, event, observation))
	}

	require.True(t, cycle.runStarted)
	require.Same(t, cycle, current.currentCycle())
}

func TestAcceptedPromptIsolatesEventsBeforeExactDispatchEvidence(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	ctx := withTurnRoute(context.Background(), "prompt")
	submission := lifecycle.Submission{SubmissionID: "submission", ClientNonce: "client"}
	cycle, _, err := current.reservePromptCycle(ctx, "prompt-message", submission)
	require.NoError(t, err)
	require.NoError(t, current.acceptPromptCycle(ctx, cycle, submission))
	updatesBefore := connection.updateCount()

	for _, event := range []opencode.Event{
		{
			Type: opencode.EventSessionStatus,
			Properties: mustJSONValue(map[string]any{
				"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
			}),
		},
		{
			Type: opencode.EventTodoUpdated,
			Properties: mustJSONValue(map[string]any{
				"sessionID": current.idmap.NativeSessionID,
				"todos":     []map[string]any{{"content": "earlier", "status": "completed", "priority": "low"}},
			}),
		},
		{
			Type:       opencode.EventSessionIdle,
			Properties: mustJSONValue(map[string]any{"sessionID": current.idmap.NativeSessionID}),
		},
	} {
		observation, ok := current.observeNativeEvent(binding, event)
		require.True(t, ok)
		require.Equal(t, nativeEventPromptBeforeEvidence, observation.ownership)
		require.NoError(t, current.routeNativeEventForIncarnation(ctx, binding, event, observation))
	}

	require.Equal(t, updatesBefore, connection.updateCount())
	require.False(t, cycle.runStarted)
	require.False(t, cycle.idle)
	require.Same(t, cycle, current.currentCycle())
}

func TestObservedAutonomousBurstSharesOneCycleAndSettles(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	events := []opencode.Event{
		{
			Type: opencode.EventSessionStatus,
			Properties: mustJSONValue(map[string]any{
				"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
			}),
		},
		{
			Type: opencode.EventTodoUpdated,
			Properties: mustJSONValue(map[string]any{
				"sessionID": current.idmap.NativeSessionID,
				"todos":     []map[string]any{{"content": "background", "status": "in_progress", "priority": "medium"}},
			}),
		},
		{
			Type:       opencode.EventSessionIdle,
			Properties: mustJSONValue(map[string]any{"sessionID": current.idmap.NativeSessionID}),
		},
	}

	observations := make([]*nativeEventObservation, 0, len(events))
	for _, event := range events {
		observation, ok := current.observeNativeEvent(binding, event)
		require.True(t, ok)
		require.Equal(t, nativeEventAgent, observation.ownership)
		observations = append(observations, observation)
	}

	for index, event := range events {
		require.NoError(t, current.routeNativeEventForIncarnation(
			context.Background(), binding, event, observations[index],
		))
	}

	require.Nil(t, current.currentCycle())
	require.Equal(t, []string{
		"lifecycle_snapshot", "state_update", "state_update",
	}, connection.lifecycleEvents(t))
	requireLifecycleReduces(t, connection)
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

func TestCorrectionNativeCausalPureBranches(t *testing.T) {
	cycle := &foregroundCycle{
		origin: lifecycle.CauseSubmission, nativeMessageID: "user", assistantID: "assistant",
		blockers: map[string]struct{}{"action": {}},
	}

	for _, event := range []opencode.Event{
		{Type: opencode.EventMessageUpdated, Properties: json.RawMessage(`{"info":{"id":"other","role":"assistant","parentID":"other"}}`)},
		{Type: opencode.EventMessageUpdated, Properties: json.RawMessage(`{"info":{"id":"other","role":"assistant","parentID":"user"}}`)},
		{Type: opencode.EventPermissionV2Asked, Properties: json.RawMessage(`{`)},
		{Type: opencode.EventPermissionV2Asked, Properties: json.RawMessage(`{"tool":{"messageID":"assistant"}}`)},
		{Type: opencode.EventQuestionV2Asked, Properties: json.RawMessage(`{"tool":{"messageID":"assistant"}}`)},
		{Type: opencode.EventPermissionV2Replied, Properties: json.RawMessage(`{"requestID":"action"}`)},
		{Type: "other"},
	} {
		_ = nativeEventCausallyMatchesCycleLocked(event, cycle)
	}

	require.False(t, nativeEventCausallyMatchesCycleLocked(opencode.Event{}, nil))
	require.False(t, nativeEventCausallyMatchesCycleLocked(opencode.Event{}, &foregroundCycle{}))
}

func TestCorrectionPumpBarrierAndRoutingBranches(t *testing.T) {
	filledWake := &sessionPump{released: make(chan struct{}, 1)}
	filledWake.released <- struct{}{}
	filledWake.wake()

	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstCancel()
	rejectedPause := &sessionPump{hold: make(chan chan error), done: make(chan struct{})}
	require.ErrorIs(t, rejectedPause.pauseForDispatch(firstCtx), context.Canceled)

	stoppedDuring := &sessionPump{hold: make(chan chan error), done: make(chan struct{})}
	go func() {
		<-stoppedDuring.hold
		close(stoppedDuring.done)
	}()
	require.ErrorContains(t, stoppedDuring.pauseForDispatch(context.Background()), "during dispatch barrier")

	cancelDuring := &sessionPump{hold: make(chan chan error), done: make(chan struct{})}
	barrierTaken := make(chan struct{})
	go func() {
		<-cancelDuring.hold
		close(barrierTaken)
	}()
	duringCtx, duringCancel := context.WithCancel(context.Background())
	go func() {
		<-barrierTaken
		duringCancel()
	}()
	require.ErrorIs(t, cancelDuring.pauseForDispatch(duringCtx), context.Canceled)

	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	binding := testIncarnation(current)
	require.NotNil(t, binding)

	pausedCtx, pausedCancel := context.WithCancel(context.Background())
	paused := &sessionPump{
		session: current, binding: binding, hold: make(chan chan error), released: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	go paused.run(pausedCtx)
	ack := make(chan error, 1)
	paused.hold <- ack
	require.NoError(t, <-ack)
	pausedCancel()
	<-paused.done

	emptyClient := newFakeOpenCodeClient()
	emptyBinding := &nativeIncarnationBinding{client: emptyClient, registry: newActionRegistry()}
	emptySession := &session{idmap: idmapRecord{NativeSessionID: "native"}, incarnation: emptyBinding}
	emptyPump := &sessionPump{
		session: emptySession, binding: emptyBinding, hold: make(chan chan error), released: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	emptyClient.eventStream <- opencode.EventStreamItem{}
	emptyPump.run(context.Background())
	<-emptyPump.done

	staleBinding := &nativeIncarnationBinding{client: binding.client, registry: newActionRegistry()}
	(&session{}).routeNativeEvent(context.Background(), opencode.Event{})
	current.routeNativeEvent(context.Background(), opencode.Event{})
	require.NoError(t, current.routeNativeEventForIncarnation(context.Background(), staleBinding, opencode.Event{}))
	require.NoError(t, current.routeNativeEventForIncarnation(context.Background(), binding, opencode.Event{}))
	require.NoError(t, current.routeNativeEventForIncarnation(context.Background(), binding, opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": "foreign"}),
	}, &nativeEventObservation{binding: binding}))

	cycle := &foregroundCycle{blockers: map[string]struct{}{"action": {}}}
	require.True(t, nativeEventCausallyMatchesCycleLocked(opencode.Event{
		Type:       opencode.EventPermissionV2Replied,
		Properties: mustJSONValue(map[string]any{"requestID": "action", "sessionID": current.idmap.NativeSessionID}),
	}, &foregroundCycle{origin: lifecycle.CauseSubmission, blockers: cycle.blockers}))
}

func TestCorrectionObservedOwnershipAndNativeFailureBranches(t *testing.T) {
	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	current.stopPump()
	binding := testIncarnation(current)
	other := &nativeIncarnationBinding{registry: newActionRegistry()}

	current.lifecycleMu.Lock()
	require.Nil(t, func() *foregroundCycle {
		cycle, _ := current.observedCycleLocked(
			context.WithValue(context.Background(), nativeEventObservationKey{}, &nativeEventObservation{binding: other}), true)

		return cycle
	}())

	for _, observation := range []*nativeEventObservation{
		{binding: binding, ownership: nativeEventPromptBeforeEvidence},
		{binding: binding, ownership: nativeEventCycle, cycle: &foregroundCycle{}},
		{binding: binding, ownership: nativeEventAgent, cycle: &foregroundCycle{}},
		{binding: binding, ownership: nativeEventAgent},
		{binding: binding, ownership: nativeEventOwnership(99)},
	} {
		current.cycle = &foregroundCycle{}
		cycle, err := current.observedCycleLocked(
			context.WithValue(context.Background(), nativeEventObservationKey{}, observation), false)
		require.NoError(t, err)
		require.Nil(t, cycle)
	}
	current.cycle = nil
	current.lifecycleMu.Unlock()

	closedStream := lifecycle.NewStream("closed", lifecycle.Negotiated{Versions: []int{1}})
	closedStream.Close()
	current.lifecycleMu.Lock()
	current.incarnation = &nativeIncarnationBinding{client: binding.client, stream: closedStream, registry: newActionRegistry()}
	current.cycle = nil
	closedBinding := current.incarnation
	current.lifecycleMu.Unlock()

	agentObservation := &nativeEventObservation{binding: closedBinding, ownership: nativeEventAgent}
	agentCtx := context.WithValue(withNativeIncarnationBinding(context.Background(), closedBinding), nativeEventObservationKey{}, agentObservation)
	busy := opencode.Event{Type: opencode.EventSessionStatus, Properties: mustJSONValue(map[string]any{
		"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
	})}
	require.Error(t, current.applyNativeStatus(agentCtx, busy))
	require.Error(t, current.markCycleRunning(agentCtx))
	require.Error(t, current.applyNativeMessageInfo(agentCtx, opencode.Event{
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "assistant", "sessionID": current.idmap.NativeSessionID, "role": roleAssistant,
		}}),
	}))

	beforeEvidence := context.WithValue(context.Background(), nativeEventObservationKey{},
		&nativeEventObservation{ownership: nativeEventPromptBeforeEvidence})
	require.Error(t, current.applyNativePermissionAsked(beforeEvidence, opencode.Event{}))
	require.Error(t, current.applyNativeQuestionAsked(beforeEvidence, opencode.Event{}))
	require.NoError(t, current.markNativeTerminal(context.Background()))
	require.NoError(t, current.applyNativePart(context.Background(), opencode.Event{Properties: json.RawMessage(`{`)}))
	current.applyNativeActionReplied(context.Background(), opencode.Event{Properties: json.RawMessage(`{`)})

	current.lifecycleMu.Lock()
	current.incarnation = binding
	stepCycle := &foregroundCycle{}
	stepCycle.adoptAssistant(opencode.NativeMessageInfo{ID: "assistant-a"})
	current.cycle = stepCycle
	current.lifecycleMu.Unlock()
	stepCtx := context.WithValue(context.Background(), nativeEventObservationKey{},
		&nativeEventObservation{binding: binding, ownership: nativeEventCycle, cycle: stepCycle})
	require.NoError(t, current.applyNativeMessageInfo(stepCtx, opencode.Event{
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "assistant-b", "sessionID": current.idmap.NativeSessionID, "role": roleAssistant,
		}}),
	}))
	require.True(t, stepCycle.ownsAssistant("assistant-a"))
	require.Equal(t, "assistant-b", stepCycle.assistantID)

	current.lifecycleMu.Lock()
	current.cycle = &foregroundCycle{}
	cycle, err := current.observedCycleLocked(
		context.WithValue(context.Background(), nativeEventObservationKey{},
			&nativeEventObservation{binding: binding, ownership: nativeEventAgent}), true)
	current.lifecycleMu.Unlock()
	require.NoError(t, err)
	require.Nil(t, cycle)

	current.lifecycleMu.Lock()
	current.cycle = nil
	current.lifecycleMu.Unlock()
	nilCycleCtx := context.WithValue(context.Background(), nativeEventObservationKey{},
		&nativeEventObservation{binding: binding, ownership: nativeEventPromptBeforeEvidence})
	require.NoError(t, current.applyNativeStatus(nilCycleCtx, busy))
}

// nativeEventSwitchTypes reads the opencode event constants that one function's
// own dispatch switch in session_pump.go names. A switch nested inside another
// statement — applyNativeEvent's ownership gate — is deliberately not one of them.
func nativeEventSwitchTypes(t *testing.T, function string) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "session_pump.go", nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	types := make([]string, 0, 24)
	found := false

	for _, declaration := range file.Decls {
		declared, ok := declaration.(*ast.FuncDecl)
		if !ok || declared.Name.Name != function {
			continue
		}

		for _, statement := range declared.Body.List {
			dispatch, isSwitch := statement.(*ast.SwitchStmt)
			if !isSwitch {
				continue
			}

			require.False(t, found, "%s declares more than one dispatch switch", function)
			found = true

			for _, entry := range dispatch.Body.List {
				clause, isClause := entry.(*ast.CaseClause)
				require.True(t, isClause, "%s dispatches on something other than cases", function)

				for _, expression := range clause.List {
					selector, isSelector := expression.(*ast.SelectorExpr)
					require.True(t, isSelector, "%s names a case that is not an opencode event constant", function)
					types = append(types, selector.Sel.Name)
				}
			}
		}
	}

	require.True(t, found, "%s declares no dispatch switch", function)
	sort.Strings(types)

	return types
}

// TestEveryReadNativeEventTypeIsNamedDecoded binds the two hand-written type sets
// that decide a native event's fate. nativeEventDecoded settles ownership and
// applyNativeEvent settles the reading, so a type read without being named
// decoded is observed inert and read against no cycle — for a settle-shaped type,
// that silently discards the evidence that ends a turn.
func TestEveryReadNativeEventTypeIsNamedDecoded(t *testing.T) {
	t.Parallel()

	decoded := nativeEventSwitchTypes(t, "nativeEventDecoded")
	require.NotEmpty(t, decoded)
	require.Equal(t, decoded, nativeEventSwitchTypes(t, "applyNativeEvent"))
}

// TestUnidentifiedNativeStepsNameNoCycle proves cycle membership is decided by a
// native id and nothing else: a message carrying none is neither owned nor
// adopted, and a part frame this adapter cannot read addresses no cycle even
// while that cycle's dispatch is already proven.
func TestUnidentifiedNativeStepsNameNoCycle(t *testing.T) {
	t.Parallel()

	cycle := &foregroundCycle{
		origin: lifecycle.CauseSubmission, nativeMessageID: "msg_user", dispatchProven: true,
	}

	require.False(t, cycle.ownsAssistant(""))

	cycle.adoptAssistant(opencode.NativeMessageInfo{Finish: "stop"})
	require.Empty(t, cycle.assistantIDs)
	require.Empty(t, cycle.assistantID)
	require.False(t, cycle.assistantTerminal)

	require.False(t, nativeEventCausallyMatchesCycleLocked(opencode.Event{
		Type:       opencode.EventMessagePartUpdated,
		Properties: mustJSONValue(map[string]any{"sessionID": "native-1", "part": map[string]any{"id": "prt_text"}}),
	}, cycle))
}

// TestPumpDropsAnEventReceivedForARetiredBinding proves the pump drains for
// exactly one incarnation: an event this session's binding no longer speaks for
// is read off the stream and dropped rather than queued for a routing pass that
// would attribute it to whatever replaced that binding.
func TestPumpDropsAnEventReceivedForARetiredBinding(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	retired := &nativeIncarnationBinding{client: client, registry: newActionRegistry()}
	replaced := &session{idmap: idmapRecord{NativeSessionID: "native"}}
	pump := &sessionPump{
		session: replaced, binding: retired,
		hold: make(chan chan error), released: make(chan struct{}, 1), done: make(chan struct{}),
	}

	client.eventStream <- opencode.EventStreamItem{Event: &opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": "native"}),
	}}
	client.eventStream <- opencode.EventStreamItem{Terminal: errors.New("stream over")}

	pump.run(context.Background())
	<-pump.done

	require.Empty(t, pump.held, "an event for a retired binding was queued for routing")
}
