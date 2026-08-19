package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// lifecycleSession builds a negotiated session with its stream open and its pump
// routing, which is the state every session reaches at the end of the establishing
// request.
func lifecycleSession(t *testing.T, options ...Option) (*session, *fakeOpenCodeClient, *recordingAgentClient) {
	t.Helper()

	agent := negotiatedAgent(t, options...)
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	require.NoError(t, current.establish(context.Background()))
	t.Cleanup(current.stopPump)

	return current, client, connection
}

// correlatedPrompt stamps both envelopes a negotiated prompt carries.
func correlatedPrompt(id acp.SessionId, nonce, text string) acp.PromptRequest {
	request := TextPromptRequest(id, nonce, text)
	request.Meta[lifecycle.MetaKey] = map[string]any{
		"version": 1,
		"submission": map[string]any{
			"submissionId": "submission-" + nonce,
			"clientNonce":  "client-" + nonce,
		},
	}

	return request
}

// TestStreamOpensWithASnapshotBeforeEveryOtherEnvelope proves the stream's first
// lifecycle-bearing notification is its snapshot, that it rides the one legal
// carrier, and that the whole emitted stream reduces under the family rules.
func TestStreamOpensWithASnapshotBeforeEveryOtherEnvelope(t *testing.T) {
	current, _, connection := lifecycleSession(t)

	events := connection.lifecycleEvents(t)
	require.Equal(t, []string{"lifecycle_snapshot"}, events)

	snapshot := connection.lifecycleEventsOfType(t, "lifecycle_snapshot")[0]
	foreground, ok := snapshot["foreground"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "idle", foreground["state"])
	require.NotContains(t, foreground, "turnId", "an idle snapshot names no turn")
	require.Empty(t, snapshot["actions"])
	require.Empty(t, snapshot["activities"])

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.NoError(t, err)

	require.Equal(t, []string{
		"lifecycle_snapshot", "prompt_accepted", "state_update", "state_update",
	}, connection.lifecycleEvents(t))

	state := requireLifecycleReduces(t, connection)
	require.Len(t, state.Turns, 1)
	require.True(t, state.Turns[0].Terminal)
	require.Equal(t, lifecycle.OutcomeSuccess, state.Turns[0].Outcome)
}

// TestAcceptanceFollowsNativeAdmissionAndPrecedesItsEvents proves the acceptance
// boundary: the frame is acknowledged by the native dispatcher first, acceptance
// is emitted next, and every event the frame caused is delivered after it.
func TestAcceptanceFollowsNativeAdmissionAndPrecedesItsEvents(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	// The harness publishes a transcript part before its POST is even answered,
	// which is the race the dispatch hold exists for.
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.stageAssistantMessage(id, "assistant-1")
		client.publishEvent(opencode.Event{
			Type:       opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{"id": "assistant-1", "sessionID": id, "role": "assistant"}}),
		})
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessagePartCreated,
			Properties: mustJSONValue(map[string]any{
				"id": "part-1", "sessionID": id, "messageID": "assistant-1", "type": "text", "text": "streamed",
			}),
		})
		client.publishSessionIdle(id)

		return opencode.NativeMessage{}, nil
	}

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.NoError(t, err)

	connection.mu.Lock()
	defer connection.mu.Unlock()

	accepted := -1
	chunk := -1

	for index, notification := range connection.updates {
		if envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any); ok {
			if event, _ := envelope["event"].(map[string]any); event["type"] == "prompt_accepted" {
				accepted = index
			}

			continue
		}

		if notification.Update.AgentMessageChunk != nil && chunk < 0 {
			chunk = index
		}
	}

	require.GreaterOrEqual(t, accepted, 0, "the stream carried no acceptance")
	require.GreaterOrEqual(t, chunk, 0, "the turn streamed no transcript")
	require.Less(t, accepted, chunk, "an event caused by the frame preceded its acceptance")
}

// TestRefusedDispatchCreatesNeitherSubmissionNorTurn proves a frame the native
// dispatcher refused emits no acceptance at all.
func TestRefusedDispatchCreatesNeitherSubmissionNorTurn(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	client.refusesDispatch(errors.New("native refused the frame"))

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.Error(t, err)

	require.Equal(t, []string{"lifecycle_snapshot"}, connection.lifecycleEvents(t))

	state := requireLifecycleReduces(t, connection)
	require.Empty(t, state.Turns)
}

// TestNativeIdleSettlesTheTurnWithoutPolling proves the native idle event is the
// completion authority: the turn settles on it, and nothing reads session status
// to decide.
func TestNativeIdleSettlesTheTurnWithoutPolling(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	release := make(chan struct{})
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.stageAssistantMessage(id, "assistant-1")

		go func() {
			<-release
			client.publishSessionIdle(id)
		}()

		return opencode.NativeMessage{}, nil
	}

	done := make(chan acp.PromptResponse, 1)

	go func() {
		response, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
		require.NoError(t, err)
		done <- response
	}()

	select {
	case <-done:
		t.Fatal("the turn settled before the native idle event")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	require.Equal(t, acp.StopReasonEndTurn, (<-done).StopReason)
	requireLifecycleOutcome(t, connection, lifecycle.OutcomeSuccess)
}

// TestNativeErrorBeforeIdleFailsTheTurnOnce proves a native failure published
// before the idle signal is the turn's outcome, and that the harness republishing
// it after the idle changes nothing.
func TestNativeErrorBeforeIdleFailsTheTurnOnce(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	nativeErr := providerNativeError("provider exploded", 429, "rate_limit_exceeded")
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.publishEvent(opencode.Event{
			Type:       opencode.EventSessionError,
			Properties: mustJSONValue(map[string]any{"sessionID": id, "error": nativeErr}),
		})
		client.publishSessionIdle(id)
		client.publishEvent(opencode.Event{
			Type:       opencode.EventSessionError,
			Properties: mustJSONValue(map[string]any{"sessionID": id, "error": nativeErr}),
		})

		return opencode.NativeMessage{}, nil
	}

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "provider exploded")

	requireLifecycleOutcome(t, connection, lifecycle.OutcomeFailed)

	idles := 0

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		if transition["state"] == "idle" {
			idles++

			require.NotContains(t, transition, "stopReason", "a failed outcome states no stop reason")
		}
	}

	require.Equal(t, 1, idles, "a turn ends exactly once")
	requireLifecycleReduces(t, connection)
}

// TestOutOfTurnNativeWorkOpensAnAgentOriginTurn proves work no prompt submitted is
// reported rather than dropped or refused: the session reports an agent-origin
// turn, streams its transcript, and settles it on the native idle event — all
// while no prompt is in flight.
func TestOutOfTurnNativeWorkOpensAnAgentOriginTurn(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	client.publishEvent(opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{"id": "assistant-out", "sessionID": native, "role": "assistant"}}),
	})
	client.publishEvent(opencode.Event{
		Type: opencode.EventMessagePartCreated,
		Properties: mustJSONValue(map[string]any{
			"id": "part-out", "sessionID": native, "messageID": "assistant-out", "type": "text", "text": "background",
		}),
	})
	client.publishSessionIdle(native)

	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "state_update")) >= 2
	}, "the session reported no agent-origin turn")

	transitions := connection.lifecycleEventsOfType(t, "state_update")
	require.Equal(t, "running", transitions[0]["state"])
	require.Equal(t, "activity", transitions[0]["cause"], "an agent-origin turn is activity-caused")
	require.Equal(t, "idle", transitions[len(transitions)-1]["state"])

	chunks := 0

	connection.mu.Lock()
	for _, notification := range connection.updates {
		if notification.Update.AgentMessageChunk != nil {
			chunks++

			require.Nil(t, notification.Meta, "an out-of-turn update carries no turn route")
		}
	}
	connection.mu.Unlock()

	require.Positive(t, chunks, "out-of-turn transcript was dropped")
	requireLifecycleReduces(t, connection)
}

// TestOutOfTurnPermissionIsAnsweredAndSettledBeforeTheNextPrompt proves the
// out-of-turn permission defect is gone: a permission arriving between prompts is
// routed to the host, answered natively, reported terminal on the stream, and the
// next prompt runs normally.
func TestOutOfTurnPermissionIsAnsweredAndSettledBeforeTheNextPrompt(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	current.markPublishedToolCall("call-1")
	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "permission-1", "sessionID": native, "action": "edit",
			"resources": []string{"file.go"},
			"tool":      map[string]any{"callID": "call-1", "messageID": "assistant-out"},
		}),
	})

	requireEventually(t, func() bool { return connection.permissionRequestCount() == 1 }, "no permission reached the host")
	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "OpenCode was never answered")

	reply := client.permissionReply(0)
	require.Equal(t, "permission-1", reply.requestID)
	require.Equal(t, permissionReplyOnce, reply.reply)

	requireEventually(t, func() bool {
		actions := connection.lifecycleEventsOfType(t, "action_update")
		if len(actions) != 2 {
			return false
		}

		terminal, _ := actions[1]["action"].(map[string]any)

		return terminal["state"] == "accepted"
	}, "the action never terminalized")

	actions := connection.lifecycleEventsOfType(t, "action_update")
	first, ok := actions[0]["action"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "pending", first["state"])
	require.Equal(t, true, first["blocksForeground"])

	// The next prompt is admitted normally: nothing was left queued to reject it.
	client.publishSessionIdle(native)

	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the agent-origin turn never settled")

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.NoError(t, err)
	requireLifecycleReduces(t, connection)
}

// TestBlockingActionOrdersPendingBeforeItsForegroundAndTerminalBeforeRunning
// proves the action state machine's order: the host request is in flight before
// the action is announced, the announcement precedes the blocked foreground, and
// the cycle returns to running only after the action reports terminal.
func TestBlockingActionOrdersPendingBeforeItsForegroundAndTerminalBeforeRunning(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	started, release := connection.permissionStarted, connection.permissionRelease
	connection.mu.Unlock()

	current.markPublishedToolCall("call-1")
	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "permission-1", "sessionID": native, "action": "edit",
			"resources": []string{"file.go"},
			"tool":      map[string]any{"callID": "call-1"},
		}),
	})

	requireSignal(t, started)
	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "action_update")) == 1
	}, "the action was never announced")

	events := connection.lifecycleEvents(t)
	require.Equal(t, []string{"lifecycle_snapshot", "state_update", "action_update", "state_update"}, events,
		"pending must precede the foreground it blocks")

	transitions := connection.lifecycleEventsOfType(t, "state_update")
	require.Equal(t, "requires_action", transitions[1]["state"])

	close(release)

	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "state_update")) == 3
	}, "the cycle never resumed")

	require.Equal(t, []string{
		"lifecycle_snapshot", "state_update", "action_update", "state_update", "action_update", "state_update",
	}, connection.lifecycleEvents(t), "the terminal action must precede the transition that unblocks its cycle")

	requireLifecycleReduces(t, connection)
}

// TestNativelyResolvedActionTerminalizesWithoutTheHostAnswer proves the
// `*.replied` events are consumed: an action OpenCode resolved itself is reported
// terminal and stops blocking the foreground, rather than waiting forever on a
// host answer nothing needs any more.
func TestNativelyResolvedActionTerminalizesWithoutTheHostAnswer(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	started := connection.permissionStarted
	connection.mu.Unlock()

	current.markPublishedToolCall("call-1")
	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "permission-1", "sessionID": native, "action": "edit",
			"resources": []string{"file.go"},
			"tool":      map[string]any{"callID": "call-1"},
		}),
	})
	requireSignal(t, started)

	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Replied,
		Properties: mustJSONValue(map[string]any{
			"sessionID": native, "requestID": "permission-1", "reply": permissionReplyAlways,
		}),
	})

	requireEventually(t, func() bool {
		actions := connection.lifecycleEventsOfType(t, "action_update")
		if len(actions) != 2 {
			return false
		}

		terminal, _ := actions[1]["action"].(map[string]any)

		return terminal["state"] == "accepted"
	}, "a natively resolved action never terminalized")

	require.Zero(t, client.permissionReplyCount(), "an action OpenCode resolved was answered twice")
	requireLifecycleReduces(t, connection)
}

// TestUnanswerableElicitationDeclinesInsteadOfBlockingForever proves the
// unsupported-form path terminalizes: a host that cannot be asked declines the
// question, OpenCode is answered, and the foreground is released.
func TestUnanswerableElicitationDeclinesInsteadOfBlockingForever(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	client.publishEvent(opencode.Event{
		Type: opencode.EventQuestionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "question-1", "sessionID": native,
			"questions": []map[string]any{{"question": "Continue?"}},
		}),
	})

	requireEventually(t, func() bool { return client.questionRejectCount() == 1 }, "OpenCode was left blocked")
	requireEventually(t, func() bool {
		actions := connection.lifecycleEventsOfType(t, "action_update")
		if len(actions) != 2 {
			return false
		}

		terminal, _ := actions[1]["action"].(map[string]any)

		return terminal["state"] == "declined"
	}, "the unanswerable action never terminalized")

	require.Empty(t, connection.elicitations)
	requireLifecycleReduces(t, connection)
}

// TestCancelTerminalizesBlockersBeforeTheEndingIdle proves cancellation
// arbitration: the pending action reports cancelled, OpenCode is answered, and the
// turn's ending idle follows both.
func TestCancelTerminalizesBlockersBeforeTheEndingIdle(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	started := connection.permissionStarted
	connection.mu.Unlock()

	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}

	dispatched := make(chan struct{})
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(dispatched)
		client.publishEvent(opencode.Event{
			Type: opencode.EventPermissionV2Asked,
			Properties: mustJSONValue(map[string]any{
				"id": "permission-1", "sessionID": id, "action": "edit",
				"resources": []string{"file.go"},
				"tool":      map[string]any{"callID": "call-1"},
			}),
		})

		return opencode.NativeMessage{}, nil
	}

	current.markPublishedToolCall("call-1")

	done := make(chan acp.PromptResponse, 1)

	go func() {
		response, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
		require.NoError(t, err)
		done <- response
	}()

	<-dispatched
	requireSignal(t, started)
	require.NoError(t, current.agent.Cancel(context.Background(), CancelRequest(current.id, internalSeamTurnNonce)))
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)

	require.Equal(t, []string{native}, client.abortedSessions())
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Equal(t, reasonCancelled, client.permissionReply(0).message)

	events := connection.lifecycleEvents(t)
	lastAction, lastIdle := -1, -1

	for index, event := range events {
		if event == "action_update" {
			lastAction = index
		}
	}

	transitions := connection.lifecycleEventsOfType(t, "state_update")
	require.Equal(t, "idle", transitions[len(transitions)-1]["state"])
	require.Equal(t, string(lifecycle.OutcomeCancelled), transitions[len(transitions)-1]["outcome"])

	for index := len(events) - 1; index >= 0; index-- {
		if events[index] == "state_update" {
			lastIdle = index

			break
		}
	}

	require.Less(t, lastAction, lastIdle, "a blocker terminalized after the cycle ended")
	requireLifecycleReduces(t, connection)
}

// TestCancellingOneSessionLeavesAPeerRunning proves routine cancellation is
// session-scoped: the peer session keeps its runtime binding, its lifecycle
// stream, and its turn.
func TestCancellingOneSessionLeavesAPeerRunning(t *testing.T) {
	agent := negotiatedAgent(t)
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)

	clientA := newFakeOpenCodeClient()
	first := testSession(t, agent, clientA)
	clientB := newFakeOpenCodeClient()
	second := newSession(agent, "session-2", "/tmp/project", nil, testNativeSession("native-2"), clientB, sessionMeta{}, idmapRecord{
		SessionID: "session-2", NativeSessionID: "native-2", Format: SessionStoreFormat,
	})
	second.runtimeGeneration = first.runtimeGeneration
	clientB.ensureSyncAggregate("native-2")

	agent.mu.Lock()
	agent.sessions[first.id] = first
	agent.sessions[second.id] = second
	agent.mu.Unlock()

	require.NoError(t, first.establish(context.Background()))
	require.NoError(t, second.establish(context.Background()))

	t.Cleanup(first.stopPump)
	t.Cleanup(second.stopPump)

	firstStarted := make(chan struct{})
	clientA.hangsAfterDispatch(firstStarted)
	clientA.abortFunc = func(id string) error {
		clientA.publishSessionIdle(id)

		return nil
	}

	secondStarted := make(chan struct{})
	secondRelease := make(chan struct{})
	clientB.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(secondStarted)
		clientB.stageAssistantMessage(id, "assistant-b")

		go func() {
			<-secondRelease
			clientB.publishSessionIdle(id)
		}()

		return opencode.NativeMessage{}, nil
	}

	firstDone := make(chan acp.PromptResponse, 1)
	secondDone := make(chan acp.PromptResponse, 1)

	go func() {
		response, err := first.Prompt(context.Background(), correlatedPrompt(first.id, "turn-a", "hello"))
		require.NoError(t, err)
		firstDone <- response
	}()

	go func() {
		response, err := second.Prompt(context.Background(), correlatedPrompt(second.id, "turn-b", "hello"))
		require.NoError(t, err)
		secondDone <- response
	}()

	<-firstStarted
	<-secondStarted

	require.NoError(t, agent.Cancel(context.Background(), CancelRequest(first.id, "turn-a")))
	require.Equal(t, acp.StopReasonCancelled, (<-firstDone).StopReason)

	require.Empty(t, clientB.abortedSessions(), "cancelling one session interrupted a peer")
	require.NotNil(t, agent.runtime, "cancelling one session retired the shared runtime")

	second.mu.Lock()
	peerLost := second.runtimeLostCause
	second.mu.Unlock()
	require.Empty(t, peerLost, "cancelling one session detached a peer")

	close(secondRelease)
	require.Equal(t, acp.StopReasonEndTurn, (<-secondDone).StopReason)
}

// TestConcurrentSessionsPromptWithoutAGlobalGate proves two sessions run
// overlapping turns: one waiting on a permission does not hold the other's
// dispatch.
func TestConcurrentSessionsPromptWithoutAGlobalGate(t *testing.T) {
	agent := negotiatedAgent(t)
	agent.setAgentClient(newRecordingAgentClient())

	clientA := newFakeOpenCodeClient()
	first := testSession(t, agent, clientA)
	clientB := newFakeOpenCodeClient()
	second := newSession(agent, "session-2", "/tmp/project", nil, testNativeSession("native-2"), clientB, sessionMeta{}, idmapRecord{
		SessionID: "session-2", NativeSessionID: "native-2", Format: SessionStoreFormat,
	})
	second.runtimeGeneration = first.runtimeGeneration
	clientB.ensureSyncAggregate("native-2")

	require.NoError(t, first.establish(context.Background()))
	require.NoError(t, second.establish(context.Background()))
	t.Cleanup(first.stopPump)
	t.Cleanup(second.stopPump)

	held := make(chan struct{})
	firstStarted := make(chan struct{})
	clientA.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(firstStarted)
		clientA.stageAssistantMessage(id, "assistant-a")

		go func() {
			<-held
			clientA.publishSessionIdle(id)
		}()

		return opencode.NativeMessage{}, nil
	}

	firstDone := make(chan struct{})

	go func() {
		defer close(firstDone)

		_, err := first.Prompt(context.Background(), correlatedPrompt(first.id, "turn-a", "hello"))
		require.NoError(t, err)
	}()

	<-firstStarted

	_, err := second.Prompt(context.Background(), correlatedPrompt(second.id, "turn-b", "hello"))
	require.NoError(t, err, "a live turn on one session blocked another session's prompt")

	close(held)
	<-firstDone
}

// TestPersistenceFailureFencesInsteadOfEmittingIdle proves the durability
// boundary: when the native-safe prefix cannot be committed, the stream is fenced
// and no ending idle is emitted for the turn.
func TestPersistenceFailureFencesInsteadOfEmittingIdle(t *testing.T) {
	current, client, connection := lifecycleSession(t, WithSessionStore(&errorSessionStore{err: errors.New("store offline")}))
	client.syncEvents = []opencode.SyncEvent{terminalMessageEvent(current.idmap.NativeSessionID, 0, "assistant-1", "assistant", "stop", nil)}

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "store offline")

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		require.NotEqual(t, "idle", transition["state"], "an unbacked idle was emitted")
	}

	require.Error(t, current.lifecycleFailure(), "the stream was not fenced")
}

// TestRecoveryOpensANewIncarnationWithItsOwnSnapshot proves a recovered runtime
// gets a new stream identity opened by its own snapshot, and that the fenced
// incarnation's identity is never reused.
func TestRecoveryOpensANewIncarnationWithItsOwnSnapshot(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	firstStream := current.lifecycleStreamID()
	require.NotEmpty(t, firstStream)

	current.detachRuntime(current.runtimeGeneration, "shared OpenCode runtime exited")
	require.Error(t, current.lifecycleFailure())

	replacement := newFakeOpenCodeClient()
	replacement.xdg = client.xdg
	replacement.getSession = testNativeSession(current.idmap.NativeSessionID)
	replacement.ensureSyncAggregate(current.idmap.NativeSessionID)

	current.agent.mu.Lock()
	current.agent.runtime = replacement
	current.agent.runtimeGeneration++
	generation := current.agent.runtimeGeneration
	current.agent.mu.Unlock()

	installed, closed := current.installRecoveredRuntime(replacement, func() {}, current.idmap, generation)
	require.True(t, installed)
	require.False(t, closed)

	current.reopenLifecycleStream()
	require.NoError(t, current.establish(context.Background()))
	t.Cleanup(current.stopPump)

	secondStream := current.lifecycleStreamID()
	require.NotEqual(t, firstStream, secondStream, "a recovered incarnation reused a fenced stream identity")

	snapshots := connection.lifecycleEventsOfType(t, "lifecycle_snapshot")
	require.Len(t, snapshots, 2, "the recovered incarnation opened without a snapshot")

	streams := make([]string, 0, 2)
	for _, envelope := range connection.lifecycleEnvelopes(t) {
		streamID, ok := envelope["streamId"].(string)
		require.True(t, ok, "envelope carries no streamId")
		streams = append(streams, streamID)
	}

	require.Contains(t, streams, firstStream)
	require.Contains(t, streams, secondStream)
}

// TestStreamFailureLatchesRatherThanHidingAGap proves a delivery failure latches
// the stream: the sequence it consumed is never reused, and the session refuses to
// report a contiguous stream it cannot back.
func TestStreamFailureLatchesRatherThanHidingAGap(t *testing.T) {
	current, _, connection := lifecycleSession(t)

	connection.mu.Lock()
	connection.updateErr = errors.New("wire down")
	connection.mu.Unlock()

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "wire down")

	connection.mu.Lock()
	connection.updateErr = nil
	connection.mu.Unlock()

	require.Error(t, current.lifecycleFailure())
	require.Error(t, current.emitLifecycle(context.Background(), lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, "cycle-late", "turn-late", lifecycle.CauseSubmission,
	)), "a latched stream emitted again")
}

// TestUnnegotiatedConnectionEmitsNoEnvelopeAndStillAnswersPermissions proves the
// extension is the only thing the answer gates: a connection that negotiated
// nothing carries no envelope, and its permission flow is unchanged.
func TestUnnegotiatedConnectionEmitsNoEnvelopeAndStillAnswersPermissions(t *testing.T) {
	agent := NewAgent()
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)
	require.NoError(t, current.establish(context.Background()))
	t.Cleanup(current.stopPump)

	current.markPublishedToolCall("call-1")
	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "permission-1", "sessionID": current.idmap.NativeSessionID, "action": "edit",
			"resources": []string{"file.go"},
			"tool":      map[string]any{"callID": "call-1"},
		}),
	})

	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "OpenCode was never answered")
	require.Equal(t, permissionReplyOnce, client.permissionReply(0).reply)
	require.Empty(t, connection.lifecycleEnvelopes(t))
}

// TestUnpublishedToolCallPermissionIsRefused proves the permission fence stays
// structural: a permission naming a tool call this session never published is
// rejected natively and never shown to a host.
func TestUnpublishedToolCallPermissionIsRefused(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "permission-1", "sessionID": current.idmap.NativeSessionID, "action": "edit",
			"resources": []string{"file.go"},
			"tool":      map[string]any{"callID": "never-published"},
		}),
	})

	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "the refusal never reached OpenCode")
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Zero(t, connection.permissionRequestCount())
	require.Empty(t, connection.lifecycleEventsOfType(t, "action_update"))
}

// TestForeignSessionEventsAreNeverRouted proves the structural session filter: a
// directory-scoped stream carrying another native session's events changes nothing
// here.
func TestForeignSessionEventsAreNeverRouted(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": "native-other"}),
	})
	client.publishEvent(opencode.Event{
		Type: opencode.EventMessagePartCreated,
		Properties: mustJSONValue(map[string]any{
			"id": "part-x", "sessionID": "native-other", "messageID": "assistant-x", "type": "text", "text": "foreign",
		}),
	})

	// A subsequent event for this session proves the pump processed both.
	client.publishSessionIdle(current.idmap.NativeSessionID)

	require.Equal(t, []string{"lifecycle_snapshot"}, connection.lifecycleEvents(t))
	require.Nil(t, current.currentCycle())
}

// TestCloseSettlesTheOpenTurnBeforeReleasingTheSession proves close interrupts the
// session's own work, waits for the native acknowledgement, commits, and fences —
// without retiring the shared runtime.
func TestCloseSettlesTheOpenTurnBeforeReleasingTheSession(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	started := make(chan struct{})
	client.hangsAfterDispatch(started)
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}

	done := make(chan acp.PromptResponse, 1)

	go func() {
		response, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
		require.NoError(t, err)
		done <- response
	}()

	<-started

	closeErr := make(chan error, 1)
	go func() { closeErr <- current.Close(context.Background()) }()

	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)
	require.NoError(t, <-closeErr)
	require.NotNil(t, current.agent.runtime, "close retired the shared runtime")
	require.True(t, lifecycleFenced(current), "close left the stream unfenced")
	requireLifecycleReduces(t, connection)
}

// requireEventually waits for a condition the pump reaches asynchronously.
func requireEventually(t *testing.T, condition func() bool, message string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal(message)
}

// TestHeldEventOverflowFailsClosed proves the dispatch hold never drops an event:
// a hold that outruns its bound fences the stream instead.
func TestHeldEventOverflowFailsClosed(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	current.stopPump()

	pump := &sessionPump{
		session: current, client: client, released: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	pump.held = make([]opencode.Event, heldEventCapacity)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	current.dispatchGate.Lock()
	client.publishEvent(opencode.Event{Type: opencode.EventSessionIdle, Properties: json.RawMessage(`{}`)})

	go pump.run(ctx)
	<-pump.done
	current.dispatchGate.Unlock()

	require.ErrorContains(t, current.lifecycleFailure(), "held OpenCode event queue exceeded")
}

// TestAnEventTheStreamsOwnRulesRefuseLatchesIt proves emission is validated
// against the same rules a consumer applies: an event naming a turn the stream
// never introduced consumes its sequence and latches the stream rather than
// reaching a host that would fail closed on it.
func TestAnEventTheStreamsOwnRulesRefuseLatchesIt(t *testing.T) {
	current, _, connection := lifecycleSession(t)

	before := connection.updateCount()
	require.Error(t, current.emitLifecycle(context.Background(),
		lifecycle.IdleEvent("cycle-9", "turn-9", string(acp.StopReasonEndTurn), lifecycle.OutcomeSuccess)))

	require.Equal(t, before, connection.updateCount(), "a refused event was delivered anyway")
	require.Error(t, current.lifecycleFailure())
}

// TestLifecycleDeliveryWithoutAConnectionFailsTheEstablishingRequest proves the
// stream has exactly one legal carrier: with no ACP connection to carry it, the
// session refuses to establish rather than opening a stream it can never deliver.
func TestLifecycleDeliveryWithoutAConnectionFailsTheEstablishingRequest(t *testing.T) {
	agent := negotiatedAgent(t)
	client := newFakeOpenCodeClient()
	client.ensureSyncAggregate("native-1")

	current := newSession(agent, "session-1", "/tmp/project", nil, testNativeSession("native-1"),
		client, sessionMeta{}, idmapRecord{
			SessionID: "session-1", NativeSessionID: "native-1", Format: SessionStoreFormat,
		})

	require.ErrorContains(t, current.establish(context.Background()), "no ACP connection")
	require.ErrorContains(t, current.lifecycleFailure(), "no ACP connection")
}

// TestAgentOriginCycleThatCannotBeAnnouncedIsNeverOpened proves the agent-origin
// turn and its opening transition are one step: a transition that could not be
// delivered leaves no cycle behind for later events to attach to.
func TestAgentOriginCycleThatCannotBeAnnouncedIsNeverOpened(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	connection.mu.Lock()
	connection.updateErr = errors.New("wire down")
	connection.mu.Unlock()

	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})

	requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "the transition failure was swallowed")
	require.Nil(t, current.currentCycle(), "an unannounced cycle was opened anyway")
}

// TestASecondForegroundCycleIsRefused proves the foreground is single-occupancy:
// a prompt arriving while agent-origin work already holds the cycle is refused
// rather than opening a second turn over the same foreground.
func TestASecondForegroundCycleIsRefused(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	started := make(chan struct{})
	client.hangsAfterDispatch(started)

	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	requireEventually(t, func() bool { return current.currentCycle() != nil }, "no agent-origin cycle opened")

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "already holds foreground cycle")

	requireSignal(t, started)
	require.NotNil(t, current.currentCycle(), "the refused prompt ended the agent-origin turn")

	client.publishSessionIdle(native)
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the agent-origin turn never settled")
	requireLifecycleReduces(t, connection)
}

// TestPanickingPermissionRequestFailsItsActionInstead proves a crashed outbound
// request answers OpenCode: the action reports failed and the harness is released
// rather than left blocked on a request whose goroutine died.
func TestPanickingPermissionRequestFailsItsActionInstead(t *testing.T) {
	agent := negotiatedAgent(t)
	connection := newRecordingAgentClient()
	agent.setAgentClient(&panickingPermissionClient{recordingAgentClient: connection})
	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)
	require.NoError(t, current.establish(context.Background()))
	t.Cleanup(current.stopPump)

	current.markPublishedToolCall("call-1")
	client.publishEvent(permissionAskedEvent(current.idmap.NativeSessionID, "permission-1", "call-1"))

	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "OpenCode was left blocked")
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)

	requireEventually(t, func() bool {
		actions := connection.lifecycleEventsOfType(t, "action_update")
		if len(actions) != 2 {
			return false
		}

		terminal, _ := actions[1]["action"].(map[string]any)

		return terminal["state"] == string(lifecycle.ActionFailed)
	}, "the crashed request never terminalized")
}

// panickingPermissionClient is a host whose permission request crashes.
type panickingPermissionClient struct {
	*recordingAgentClient
}

func (c *panickingPermissionClient) RequestPermission(
	context.Context,
	acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	panic("host permission handler exploded")
}

// TestACycleSettlesExactlyOnce proves the ending transition is emitted once,
// whichever path reaches the cycle first. Retirement is what makes it once: a
// cycle already retired says nothing further, so a second settlement emits no
// second idle and reports no error, because the turn it would be ending is over.
func TestACycleSettlesExactlyOnce(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	cycle := acceptTestTurn(t, current)

	require.NoError(t, current.settleCycle(
		context.Background(), cycle, lifecycle.OutcomeSuccess, string(acp.StopReasonEndTurn)))

	settled := len(connection.lifecycleEventsOfType(t, "state_update"))

	require.NoError(t, current.settleCycle(
		context.Background(), cycle, lifecycle.OutcomeCancelled, string(acp.StopReasonCancelled)))
	require.Len(t, connection.lifecycleEventsOfType(t, "state_update"), settled,
		"a retired cycle reported a second ending")
	require.NoError(t, current.settleCycle(
		context.Background(), nil, lifecycle.OutcomeSuccess, string(acp.StopReasonEndTurn)))
}
