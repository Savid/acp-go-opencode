package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
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

func fillEmitterTurnQuota(t *testing.T, current *session) {
	t.Helper()
	current.stopPump()
	stream := testIncarnation(current).stream
	require.NotNil(t, stream)
	for index := 1; index <= lifecycle.EmitterTurnLimit; index++ {
		turnID := fmt.Sprintf("turn-%d", index)
		cycleID := fmt.Sprintf("quota-cycle-%d", index)
		_, err := stream.Emit(lifecycle.AcceptedEvent(lifecycle.Submission{
			SubmissionID: fmt.Sprintf("quota-submission-%d", index), ClientNonce: "quota-client",
		}, turnID))
		require.NoError(t, err)
		_, err = stream.Emit(lifecycle.TransitionEvent(
			lifecycle.ForegroundRunning, cycleID, turnID, lifecycle.CauseSubmission))
		require.NoError(t, err)
		_, err = stream.Emit(lifecycle.IdleEvent(
			cycleID, turnID, string(acp.StopReasonEndTurn), lifecycle.OutcomeSuccess))
		require.NoError(t, err)
	}
	current.lifecycleMu.Lock()
	current.turnCounter = lifecycle.EmitterTurnLimit
	current.cycleCounter = lifecycle.EmitterTurnLimit
	current.lifecycleMu.Unlock()
}

func requireExactGenerationContained(t *testing.T, current *session, client *fakeOpenCodeClient, generation uint64) {
	t.Helper()
	requireSignal(t, client.closeSignal)
	require.Nil(t, testIncarnation(current), "contained generation %d remained published", generation)
	current.agent.mu.Lock()
	require.Nil(t, current.agent.runtime)
	current.agent.mu.Unlock()
	current.mu.Lock()
	require.Zero(t, current.runtimeGeneration)
	current.mu.Unlock()
}

func TestEmitterTurnBoundContainsExactGenerationForPromptAndAgentOrigins(t *testing.T) {
	for _, origin := range []string{"prompt", "agent"} {
		t.Run(origin, func(t *testing.T) {
			current, client, _ := lifecycleSession(t)
			generation := current.runtimeGeneration
			fillEmitterTurnQuota(t, current)

			if origin == "prompt" {
				var dispatches atomic.Int64
				client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
					dispatches.Add(1)

					return opencode.NativeMessage{}, nil
				}
				_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, "quota-overflow", "hello"))
				assertTurnFailed(t, err, causeTransport, "")
				require.ErrorIs(t, current.lifecycleFailure(), lifecycle.ErrEmitterTurnLimit)
				require.Zero(t, dispatches.Load())
			} else {
				current.routeNativeEvent(context.Background(), opencode.Event{
					Type: opencode.EventSessionStatus,
					Properties: mustJSONValue(map[string]any{
						"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
					}),
				})
				require.ErrorIs(t, current.lifecycleFailure(), lifecycle.ErrEmitterTurnLimit)
			}

			requireExactGenerationContained(t, current, client, generation)
		})
	}
}

func TestEmitterActionBoundContainsExactGenerationBeforeHostRegistration(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	current.stopPump()
	generation := current.runtimeGeneration

	current.lifecycleMu.Lock()
	cycle, err := current.openAgentCycleLocked(context.Background())
	require.NoError(t, err)
	stream := current.incarnation.stream
	for index := range lifecycle.EmitterActionLimit {
		actionID := fmt.Sprintf("quota-action-%d", index)
		_, err = stream.Emit(lifecycle.ActionEvent(lifecycle.PendingAction(
			actionID, lifecycle.ActionPermission, lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: cycle.turnID}, true)))
		require.NoError(t, err)
		_, err = stream.Emit(lifecycle.ActionEvent(lifecycle.ResolvedAction(actionID, lifecycle.ActionAccepted)))
		require.NoError(t, err)
	}
	current.lifecycleMu.Unlock()

	err = current.routeNativePermission(context.Background(), opencode.PermissionRequest{
		ID: "quota-action-overflow", SessionID: current.idmap.NativeSessionID,
	})
	require.ErrorIs(t, err, lifecycle.ErrEmitterActionLimit)
	require.Zero(t, connection.permissionRequestCount())
	require.Equal(t, 1, client.permissionReplyCount(), "overflow left the exact native request waiting")
	require.Equal(t, "quota-action-overflow", client.permissionReply(0).requestID)
	requireExactGenerationContained(t, current, client, generation)
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
	accepted := make(chan struct{})
	assistantObserved := make(chan struct{})
	postRelease := make(chan struct{})
	postReturned := make(chan struct{})
	connection.mu.Lock()
	connection.updateHook = func(notification acp.SessionNotification) {
		envelope, _ := notification.Meta[lifecycle.MetaKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "prompt_accepted" {
			signalTestHook(accepted)
		}
	}
	connection.mu.Unlock()
	current.mu.Lock()
	current.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
		if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "assistant-1" {
			signalTestHook(assistantObserved)
		}
	}
	current.mu.Unlock()

	// The harness publishes a transcript part before its POST is even answered,
	// which is the race the dispatch hold exists for.
	client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.publishPromptEvidence(id, request.MessageID)
		client.stageAssistantMessage(id, "assistant-1")
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-1", "sessionID": id, "role": "assistant",
				"parentID": request.MessageID, "finish": "stop",
			}}),
		})
		<-assistantObserved
		<-postRelease
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessagePartCreated,
			Properties: mustJSONValue(map[string]any{
				"id": "part-1", "sessionID": id, "messageID": "assistant-1", "type": "text", "text": "streamed",
			}),
		})
		client.publishSessionIdle(id)
		close(postReturned)

		return opencode.NativeMessage{}, nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
		done <- err
	}()
	requireSignal(t, accepted)
	select {
	case <-postReturned:
		t.Fatal("native POST returned before prompt admission")
	default:
	}
	close(postRelease)
	require.NoError(t, <-done)

	connection.mu.Lock()
	defer connection.mu.Unlock()

	acceptedIndex := -1
	chunk := -1

	for index, notification := range connection.updates {
		if envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any); ok {
			if event, _ := envelope["event"].(map[string]any); event["type"] == "prompt_accepted" {
				acceptedIndex = index
			}

			continue
		}

		if notification.Update.AgentMessageChunk != nil && chunk < 0 {
			chunk = index
		}
	}

	require.GreaterOrEqual(t, acceptedIndex, 0, "the stream carried no acceptance")
	require.GreaterOrEqual(t, chunk, 0, "the turn streamed no transcript")
	require.Less(t, acceptedIndex, chunk, "an event caused by the frame preceded its acceptance")
	for _, notification := range connection.updates {
		if notification.Update.AgentMessageChunk == nil {
			continue
		}
		route, _ := notification.Meta[routeEnvelopeKey].(map[string]any)
		require.Equal(t, internalSeamTurnNonce, route[routeFieldTurnNonce])
	}
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
	assistantObserved := make(chan struct{})
	current.mu.Lock()
	current.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
		if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "assistant-1" {
			signalTestHook(assistantObserved)
		}
	}
	current.mu.Unlock()
	client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.stageAssistantMessage(id, "assistant-1")
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-1", "sessionID": id, "role": "assistant",
				"parentID": request.MessageID, "finish": "stop",
			}}),
		})
		<-assistantObserved

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
	client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-1", "sessionID": id, "role": "assistant",
				"parentID": request.MessageID, "finish": "error",
			}}),
		})
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
	assertTurnFailed(t, err, causeProvider, "")
	require.NotContains(t, err.Error(), "provider exploded")

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

// TestHostRequestRegistrationPrecedesActionAnnouncement proves the request-first
// linearization for both action families with explicit gates. While registration
// is held, no request or action is visible. Releasing it installs the exact host
// request before beginAction is allowed to publish the pending action.
func TestHostRequestRegistrationPrecedesActionAnnouncement(t *testing.T) {
	t.Run("permission", func(t *testing.T) {
		current, client, connection := lifecycleSession(t)
		current.markPublishedToolCall("call-1")

		registrationStarted := make(chan struct{})
		registrationRelease := make(chan struct{})
		answerStarted := make(chan struct{})
		answerRelease := make(chan struct{})
		connection.mu.Lock()
		connection.permissionRegistrationStarted = registrationStarted
		connection.permissionRegistrationRelease = registrationRelease
		connection.permissionStarted = answerStarted
		connection.permissionRelease = answerRelease
		connection.mu.Unlock()

		done := make(chan error, 1)
		go func() {
			done <- current.routeNativePermission(context.Background(), opencode.PermissionRequest{
				ID: "permission-register", SessionID: current.idmap.NativeSessionID,
				Tool: opencode.PermissionTool{CallID: "call-1"},
			})
		}()

		requireSignal(t, registrationStarted)
		require.Zero(t, connection.permissionRequestCount())
		require.Empty(t, connection.lifecycleEventsOfType(t, "action_update"))

		close(registrationRelease)
		requireSignal(t, answerStarted)
		require.NoError(t, <-done)
		require.Equal(t, 1, connection.permissionRequestCount())
		require.Len(t, connection.lifecycleEventsOfType(t, "action_update"), 1)

		current.cancelActions(context.Background())
		close(answerRelease)
		require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	})

	t.Run("elicitation", func(t *testing.T) {
		current, client, connection := lifecycleSession(t)
		current.agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{
			Form: &acp.ElicitationFormCapabilities{},
		}

		registrationStarted := make(chan struct{})
		registrationRelease := make(chan struct{})
		answerStarted := make(chan struct{})
		answerRelease := make(chan struct{})
		connection.mu.Lock()
		connection.elicitationRegistrationStarted = registrationStarted
		connection.elicitationRegistrationRelease = registrationRelease
		connection.elicitationStarted = answerStarted
		connection.elicitationRelease = answerRelease
		connection.mu.Unlock()

		done := make(chan error, 1)
		go func() {
			done <- current.routeNativeQuestion(context.Background(), opencode.QuestionRequest{
				ID: "question-register", SessionID: current.idmap.NativeSessionID,
				Questions: []opencode.QuestionInfo{{Question: "Continue?"}},
			})
		}()

		requireSignal(t, registrationStarted)
		connection.mu.Lock()
		require.Empty(t, connection.elicitations)
		connection.mu.Unlock()
		require.Empty(t, connection.lifecycleEventsOfType(t, "action_update"))

		close(registrationRelease)
		requireSignal(t, answerStarted)
		require.NoError(t, <-done)
		connection.mu.Lock()
		require.Len(t, connection.elicitations, 1)
		connection.mu.Unlock()
		require.Len(t, connection.lifecycleEventsOfType(t, "action_update"), 1)

		current.cancelActions(context.Background())
		close(answerRelease)
		require.Equal(t, 1, client.questionRejectCount())
	})
}

// TestActionRegistrationCancellationAndCloseDoNotLeak proves teardown can win
// while the JSON-RPC registration itself is blocked. Both paths cancel the exact
// request, answer OpenCode once, and publish no action that was never registered.
func TestActionRegistrationCancellationAndCloseDoNotLeak(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		name := "cancel"
		if closeSession {
			name = "close"
		}

		t.Run(name, func(t *testing.T) {
			current, client, connection := lifecycleSession(t)
			current.markPublishedToolCall("call-1")

			registrationStarted := make(chan struct{})
			registrationRelease := make(chan struct{})
			connection.mu.Lock()
			connection.permissionRegistrationStarted = registrationStarted
			connection.permissionRegistrationRelease = registrationRelease
			connection.mu.Unlock()

			if closeSession {
				client.abortFunc = func(id string) error {
					client.publishSessionIdle(id)

					return nil
				}
			}

			routed := make(chan error, 1)
			go func() {
				routed <- current.routeNativePermission(context.Background(), opencode.PermissionRequest{
					ID: "permission-teardown", SessionID: current.idmap.NativeSessionID,
					Tool: opencode.PermissionTool{CallID: "call-1"},
				})
			}()

			requireSignal(t, registrationStarted)
			if closeSession {
				require.NoError(t, current.closeSession(context.Background(), false))
			} else {
				require.NoError(t, current.cancelTurn(context.Background()))
			}
			require.NoError(t, <-routed)
			require.Empty(t, connection.lifecycleEventsOfType(t, "action_update"))
			require.Zero(t, connection.permissionRequestCount())
			require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
			require.Equal(t, reasonCancelled, client.permissionReply(0).message)
		})
	}
}

// TestActionRegistrationFailureContainsWithoutAnnouncement proves a registration
// error is a stream-breaking delivery gap, not a permission decline. The native
// request is rejected, the exact producer is contained, and no pending action is
// fabricated on the lifecycle stream.
func TestActionRegistrationFailureContainsWithoutAnnouncement(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	current.markPublishedToolCall("call-1")
	connection.mu.Lock()
	connection.permissionRegistrationErr = errors.New("permission write failed")
	connection.mu.Unlock()

	client.publishEvent(permissionAskedEvent(current.idmap.NativeSessionID, "permission-register-failed", "call-1"))
	requireSignal(t, client.closeSignal)
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Empty(t, connection.lifecycleEventsOfType(t, "action_update"))
	require.Zero(t, connection.permissionRequestCount())
	require.Error(t, current.lifecycleFailure())
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

// TestUnanswerableElicitationDeclinesInsteadOfBlockingForever proves the hard
// request-first rule: a host that cannot register a request is answered natively
// without inventing a lifecycle action that was never answerable.
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
		return len(connection.lifecycleEventsOfType(t, "action_update")) == 2
	}, "synthetic decline never terminalized its lifecycle action")
	actions := connection.lifecycleEventsOfType(t, "action_update")
	require.Len(t, actions, 2)
	pending, _ := actions[0]["action"].(map[string]any)
	terminal, _ := actions[1]["action"].(map[string]any)
	require.Equal(t, "pending", pending["state"])
	require.Equal(t, "declined", terminal["state"])
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
	assistantObserved := make(chan struct{})
	current.mu.Lock()
	current.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
		if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "assistant-1" {
			signalTestHook(assistantObserved)
		}
	}
	current.mu.Unlock()
	client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(dispatched)
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-1", "sessionID": id, "role": "assistant", "parentID": request.MessageID,
			}}),
		})
		<-assistantObserved
		client.publishEvent(opencode.Event{
			Type: opencode.EventPermissionV2Asked,
			Properties: mustJSONValue(map[string]any{
				"id": "permission-1", "sessionID": id, "action": "edit",
				"resources": []string{"file.go"},
				"tool":      map[string]any{"callID": "call-1", "messageID": "assistant-1"},
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
	second.stampIncarnationGeneration(clientB, first.runtimeGeneration)
	clientB.ensureSyncAggregate("native-2")
	agent.mu.Lock()
	agent.sessions[second.id] = second
	agent.mu.Unlock()
	second.markEstablishmentResponseWritten()

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
	clientB.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(secondStarted)
		clientB.stageAssistantMessage(id, "assistant-b")
		clientB.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-b", "sessionID": id, "role": "assistant",
				"parentID": request.MessageID, "finish": "stop",
			}}),
		})

		go func() {
			<-secondRelease
			clientB.publishSessionIdle(id)
		}()

		return opencode.NativeMessage{}, nil
	}

	type promptResult struct {
		response acp.PromptResponse
		err      error
	}
	firstDone := make(chan promptResult, 1)
	secondDone := make(chan promptResult, 1)

	go func() {
		response, err := first.Prompt(context.Background(), correlatedPrompt(first.id, "turn-a", "hello"))
		firstDone <- promptResult{response: response, err: err}
	}()

	go func() {
		response, err := second.Prompt(context.Background(), correlatedPrompt(second.id, "turn-b", "hello"))
		secondDone <- promptResult{response: response, err: err}
	}()

	<-firstStarted
	<-secondStarted

	require.NoError(t, agent.Cancel(context.Background(), CancelRequest(first.id, "turn-a")))
	firstResult := <-firstDone
	if firstResult.err != nil {
		t.Fatalf("cancelled prompt failed: %v; lifecycle=%v; runtime=%v; aborts=%#v",
			firstResult.err, first.lifecycleFailure(), first.runtimeFailure(), clientA.abortedSessions())
	}
	require.Equal(t, acp.StopReasonCancelled, firstResult.response.StopReason)

	require.Empty(t, clientB.abortedSessions(), "cancelling one session interrupted a peer")
	require.NotNil(t, agent.runtime, "cancelling one session retired the shared runtime")

	second.mu.Lock()
	peerLost := second.runtimeLostCause
	second.mu.Unlock()
	require.Empty(t, peerLost, "cancelling one session detached a peer")

	close(secondRelease)
	secondResult := <-secondDone
	if secondResult.err != nil {
		clientB.mu.Lock()
		messages := append([]opencode.NativeMessage(nil), clientB.messages...)
		clientB.mu.Unlock()
		t.Fatalf("peer prompt failed: %v; lifecycle=%v; runtime=%v; messages=%#v",
			secondResult.err, second.lifecycleFailure(), second.runtimeFailure(), messages)
	}
	require.Equal(t, acp.StopReasonEndTurn, secondResult.response.StopReason)
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
	second.stampIncarnationGeneration(clientB, first.runtimeGeneration)
	clientB.ensureSyncAggregate("native-2")
	agent.mu.Lock()
	agent.sessions[second.id] = second
	agent.mu.Unlock()
	second.markEstablishmentResponseWritten()

	require.NoError(t, first.establish(context.Background()))
	require.NoError(t, second.establish(context.Background()))
	t.Cleanup(first.stopPump)
	t.Cleanup(second.stopPump)

	held := make(chan struct{})
	firstStarted := make(chan struct{})
	clientA.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(firstStarted)
		clientA.stageAssistantMessage(id, "assistant-a")
		clientA.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-a", "sessionID": id, "role": "assistant",
				"parentID": request.MessageID, "finish": "stop",
			}}),
		})

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
	assertTurnFailed(t, err, causeTransport, "")
	require.NotContains(t, err.Error(), "wire down")

	connection.mu.Lock()
	connection.updateErr = nil
	connection.mu.Unlock()

	require.Error(t, current.lifecycleFailure())
	require.Error(t, current.emitLifecycle(context.Background(), lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, "cycle-late", "turn-late", lifecycle.CauseSubmission,
	)), "a latched stream emitted again")
}

func TestUnnegotiatedConnectionRefusesActions(t *testing.T) {
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
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Zero(t, connection.permissionRequestCount())
	require.Empty(t, connection.lifecycleEnvelopes(t))
	requireEventually(t, client.isClosed, "unnegotiated action did not contain its native incarnation")
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
		session: current, binding: testIncarnation(current), released: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	pump.held = make([]queuedNativeEvent, heldEventCapacity)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	current.dispatchGate.Lock()
	client.publishEvent(opencode.Event{Type: opencode.EventSessionIdle, Properties: json.RawMessage(`{"sessionID":"native-1"}`)})

	go pump.run(ctx)
	<-pump.done
	current.dispatchGate.Unlock()

	require.Error(t, current.lifecycleFailure())
}

func TestForeignEventsDoNotConsumeHeldCapacity(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	current.stopPump()

	ctx := withTurnRoute(context.Background(), "nonce-held")
	cycle, _, err := current.reservePromptCycle(ctx, "native-held", lifecycle.Submission{
		SubmissionID: "submission-held", ClientNonce: "nonce-held",
	})
	require.NoError(t, err)

	pump := &sessionPump{
		session: current, binding: testIncarnation(current), released: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	pumpCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	current.dispatchGate.Lock()
	go pump.run(pumpCtx)
	for range heldEventCapacity + 1 {
		client.publishEvent(opencode.Event{Type: opencode.EventSessionIdle,
			Properties: json.RawMessage(`{"sessionID":"native-other"}`)})
	}
	client.publishPromptEvidence(current.idmap.NativeSessionID, "native-held")
	<-cycle.dispatchEvidence
	require.NoError(t, current.acceptPromptCycle(ctx, cycle,
		lifecycle.Submission{SubmissionID: "submission-held", ClientNonce: "nonce-held"}))
	current.dispatchGate.Unlock()
	pump.wake()
	client.publishEvent(opencode.Event{Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "assistant-held", "sessionID": current.idmap.NativeSessionID, "role": "assistant",
			"parentID": "native-held", "finish": "stop",
		}})})
	client.publishSessionIdle(current.idmap.NativeSessionID)
	<-cycle.signal
	cancel()
	<-pump.done

	require.NoError(t, current.lifecycleFailure())
}

// TestStaleStreamTerminalCannotFenceAFreshGeneration proves terminal identity is
// the exact immutable binding pointer. A delayed marker from a retired binding is a
// no-op after replacement; the current binding's own marker still fences it.
func TestStaleStreamTerminalCannotFenceAFreshGeneration(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	oldGeneration := current.runtimeGeneration
	oldBinding := testIncarnation(current)

	replacement := newFakeOpenCodeClient()
	replacement.ensureSyncAggregate(current.idmap.NativeSessionID)
	current.agent.mu.Lock()
	current.agent.runtime = replacement
	current.agent.runtimeGeneration = oldGeneration + 1
	current.agent.mu.Unlock()
	current.mu.Lock()
	current.client = replacement
	current.runtimeGeneration = oldGeneration + 1
	current.runtimeLostCause = ""
	current.mu.Unlock()
	current.reopenLifecycleStream()
	replacementBinding := testIncarnation(current)

	current.handleStreamError(oldBinding, errors.New("stale stream ended"))
	require.True(t, current.incarnationIsCurrent(replacementBinding))
	require.NoError(t, current.lifecycleFailure())
	current.agent.mu.Lock()
	require.Same(t, replacement, current.agent.runtime)
	current.agent.mu.Unlock()

	current.handleStreamError(replacementBinding, errors.New("current stream ended"))
	require.ErrorContains(t, current.lifecycleFailure(), "native event stream failed")
	require.NotContains(t, current.lifecycleFailure().Error(), "current stream ended")
	current.agent.mu.Lock()
	require.Nil(t, current.agent.runtime)
	current.agent.mu.Unlock()
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

	require.ErrorContains(t, current.establish(context.Background()), "authoritative session update delivery failed")
	require.ErrorContains(t, current.lifecycleFailure(), "lifecycle delivery failed")
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

	// The containment that answers the undeliverable transition retires the cycle
	// after latching the failure, so the retirement is what this waits on. Reading
	// the cycle pointer under the lifecycle mutex — never formatting the struct
	// behind it — keeps the wait off the fields that containment still writes.
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "an unannounced cycle was opened anyway")
}

// TestASecondForegroundCycleIsRefused proves the foreground is single-occupancy:
// a prompt arriving while agent-origin work already holds the cycle is refused
// rather than opening a second turn over the same foreground.
func TestASecondForegroundCycleIsRefused(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	var dispatches atomic.Int64
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatches.Add(1)

		return opencode.NativeMessage{}, nil
	}

	current.routeNativeEvent(context.Background(), opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	require.NotNil(t, current.currentCycle(), "no agent-origin cycle opened")

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, errValueBackpressure)

	require.Zero(t, dispatches.Load(), "the losing prompt reached native dispatch")
	require.Zero(t, client.abortCount(), "the losing prompt acquired cancellation authority")
	require.NotNil(t, current.currentCycle(), "the refused prompt ended the agent-origin turn")

	current.routeNativeEvent(context.Background(), opencode.Event{
		Type: opencode.EventSessionIdle, Properties: mustJSONValue(map[string]any{"sessionID": native}),
	})
	require.Nil(t, current.currentCycle(), "the agent-origin turn never settled")
	requireLifecycleReduces(t, connection)
}

func TestEitherConcurrentPromptWinnerLeavesTheLoserWithoutPostOrAbortAuthority(t *testing.T) {
	for _, winner := range []string{"first", "second"} {
		t.Run(winner, func(t *testing.T) {
			current, client, _ := lifecycleSession(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			assistantObserved := make(chan struct{})
			current.mu.Lock()
			current.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
				if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "winner-assistant" {
					signalTestHook(assistantObserved)
				}
			}
			current.mu.Unlock()
			var posts atomic.Int64
			client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
				posts.Add(1)
				close(entered)
				<-release
				client.stageAssistantMessage(id, "winner-assistant")
				client.publishEvent(opencode.Event{
					Type: opencode.EventMessageUpdated,
					Properties: mustJSONValue(map[string]any{"info": map[string]any{
						"id": "winner-assistant", "sessionID": id, "role": "assistant",
						"parentID": request.MessageID, "finish": "stop",
					}}),
				})

				return opencode.NativeMessage{}, nil
			}

			winnerDone := make(chan error, 1)
			go func() {
				_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, winner+"-winner", "winner"))
				winnerDone <- err
			}()
			<-entered

			loserStarted := make(chan struct{})
			loserDone := make(chan error, 1)
			go func() {
				close(loserStarted)
				_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, winner+"-loser", "loser"))
				loserDone <- err
			}()
			<-loserStarted
			close(release)

			require.ErrorContains(t, <-loserDone, errValueBackpressure)
			require.EqualValues(t, 1, posts.Load())
			require.Zero(t, client.abortCount())

			requireSignal(t, assistantObserved)
			client.publishSessionIdle(current.idmap.NativeSessionID)
			require.NoError(t, <-winnerDone)
		})
	}
}

func TestObservationBeforePromptImmutablyWinsAgentOwnership(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)

	event := opencode.Event{
		Type: opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{
			"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
		}),
	}
	observation, ok := current.observeNativeEvent(binding, event)
	require.True(t, ok)
	require.Equal(t, nativeEventAgent, observation.ownership)

	var dispatches atomic.Int64
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatches.Add(1)

		return opencode.NativeMessage{}, nil
	}
	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, "losing-prompt", "hello"))
	require.ErrorContains(t, err, errValueBackpressure)
	require.Zero(t, dispatches.Load())
	require.Zero(t, client.abortCount())

	require.NoError(t, current.routeNativeEventForIncarnation(
		context.Background(), binding, event, observation,
	))
	require.NotNil(t, current.currentCycle())
}

func TestReplacementClearsOnlyTheRetiredBindingsPendingObservations(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	oldBinding := testIncarnation(current)
	event := opencode.Event{
		Type: opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{
			"sessionID": current.idmap.NativeSessionID, "status": map[string]any{"type": "busy"},
		}),
	}

	observation, ok := current.observeNativeEvent(oldBinding, event)
	require.True(t, ok)
	require.Equal(t, nativeEventAgent, observation.ownership)

	current.reopenLifecycleStream()
	newBinding := testIncarnation(current)
	require.NotSame(t, oldBinding, newBinding)

	ctx := withTurnRoute(context.Background(), "replacement-prompt")
	cycle, _, err := current.reservePromptCycle(ctx, "replacement-message", lifecycle.Submission{
		SubmissionID: "replacement-submission", ClientNonce: "replacement-client",
	})
	require.NoError(t, err)
	require.NotNil(t, cycle)

	require.NoError(t, current.routeNativeEventForIncarnation(
		context.Background(), oldBinding, event, observation,
	))
	require.Same(t, cycle, current.currentCycle())
}

func TestOldIdleAndMessageCannotAcceptOrFinishReservedPrompt(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	ctx := withTurnRoute(context.Background(), "new-prompt")
	submission := lifecycle.Submission{SubmissionID: "new-submission", ClientNonce: "new-client"}
	cycle, _, err := current.reservePromptCycle(ctx, "new-native-user", submission)
	require.NoError(t, err)

	oldMessage := opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "old-assistant", "sessionID": current.idmap.NativeSessionID, "role": "assistant",
		}}),
	}
	oldIdle := opencode.Event{Type: opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": current.idmap.NativeSessionID})}
	for _, event := range []opencode.Event{oldMessage, oldIdle} {
		observation, ok := current.observeNativeEvent(binding, event)
		require.True(t, ok)
		require.Equal(t, nativeEventPromptBeforeEvidence, observation.ownership)
		require.NoError(t, current.routeNativeEventForIncarnation(
			context.Background(), binding, event, observation,
		))
	}
	select {
	case <-cycle.dispatchEvidence:
		t.Fatal("old same-session event accepted the new prompt")
	default:
	}
	select {
	case <-cycle.signal:
		t.Fatal("old idle finished the new prompt")
	default:
	}

	evidence := opencode.Event{Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "new-native-user", "sessionID": current.idmap.NativeSessionID, "role": roleUser,
		}})}
	observation, ok := current.observeNativeEvent(binding, evidence)
	require.True(t, ok)
	<-cycle.dispatchEvidence
	require.NoError(t, current.acceptPromptCycle(ctx, cycle, submission))
	require.NoError(t, current.routeNativeEventForIncarnation(
		context.Background(), binding, evidence, observation,
	))
	select {
	case <-cycle.signal:
		t.Fatal("pre-accept idle became terminal after acceptance")
	default:
	}

	observation, ok = current.observeNativeEvent(binding, oldIdle)
	require.True(t, ok)
	require.NoError(t, current.routeNativeEventForIncarnation(
		context.Background(), binding, oldIdle, observation,
	))
	select {
	case <-cycle.signal:
		t.Fatal("replayed idle finished the accepted prompt without its assistant")
	default:
	}

	assistant := opencode.Event{Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "new-assistant", "sessionID": current.idmap.NativeSessionID, "role": "assistant",
			"parentID": "new-native-user", "finish": "stop",
		}})}
	observation, ok = current.observeNativeEvent(binding, assistant)
	require.True(t, ok)
	require.NoError(t, current.routeNativeEventForIncarnation(
		context.Background(), binding, assistant, observation,
	))
	select {
	case <-cycle.signal:
		t.Fatal("assistant terminalized the prompt before native idle")
	default:
	}

	observation, ok = current.observeNativeEvent(binding, oldIdle)
	require.True(t, ok)
	require.NoError(t, current.routeNativeEventForIncarnation(
		context.Background(), binding, oldIdle, observation,
	))
	<-cycle.signal
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

func (c *panickingPermissionClient) BeginRequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
	_ hostRequestKey,
) registeredPermissionRequest {
	registered := make(chan error, 1)
	answered := make(chan permissionRequestResult, 1)
	registered <- nil

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				answered <- permissionRequestResult{err: errors.New("host permission handler exploded")}
			}
		}()

		response, err := c.RequestPermission(ctx, request)
		answered <- permissionRequestResult{response: response, err: err}
	}()

	return registeredPermissionRequest{registered: registered, answered: answered}
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
		context.Background(), cycle, lifecycle.OutcomeSuccess, string(acp.StopReasonEndTurn)).err)

	settled := len(connection.lifecycleEventsOfType(t, "state_update"))

	require.NoError(t, current.settleCycle(
		context.Background(), cycle, lifecycle.OutcomeCancelled, string(acp.StopReasonCancelled)).err)
	require.Len(t, connection.lifecycleEventsOfType(t, "state_update"), settled,
		"a retired cycle reported a second ending")
	require.NoError(t, current.settleCycle(
		context.Background(), nil, lifecycle.OutcomeSuccess, string(acp.StopReasonEndTurn)).err)
}

func TestCorrectionAcceptPromptDeliveryFailure(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	current.stopPump()
	cycle, _, err := current.reservePromptCycle(context.Background(), "message", lifecycle.Submission{
		SubmissionID: "submission", ClientNonce: "nonce",
	})
	require.NoError(t, err)
	connection.mu.Lock()
	connection.updateErr = errors.New("delivery failed")
	connection.updateStarted = make(chan struct{})
	connection.updateRelease = make(chan struct{})
	started, release := connection.updateStarted, connection.updateRelease
	connection.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		result <- current.acceptPromptCycle(context.Background(), cycle, lifecycle.Submission{
			SubmissionID: "submission", ClientNonce: "nonce",
		})
	}()
	requireSignal(t, started)
	current.lifecycleMu.Lock()
	_ = current.cycle
	current.lifecycleMu.Unlock()
	close(release)
	require.Error(t, <-result)
}

func TestCorrectionLifecycleDirectFailureBranches(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := &session{}
	current.stampIncarnationGeneration(client, 7)
	require.NotNil(t, current.incarnation)
	stale := current.incarnation
	replacement := &nativeIncarnationBinding{client: client, registry: newActionRegistry()}
	current.incarnation = replacement
	current.failLifecycleDelivery(stale, errors.New("stale delivery failure"))
	require.Same(t, replacement, current.incarnation)
	require.NoError(t, current.lifecycleFailure())

	invalidCycle := &foregroundCycle{reserved: true}
	require.Error(t, current.acceptPromptCycle(context.Background(), invalidCycle, lifecycle.Submission{}))
	current.abandonPromptCycle(nil)

	stream := lifecycle.NewStream("stream", lifecycle.Negotiated{Version: 1})
	stream.Close()
	cycle := &foregroundCycle{
		id: "cycle", turnID: "turn", reserved: true, blockers: map[string]struct{}{}, signal: make(chan struct{}),
	}
	direct := &session{
		incarnation: &nativeIncarnationBinding{client: client, stream: stream, registry: newActionRegistry()},
		cycle:       cycle,
		delivery:    newSessionDelivery(nil, "session"),
	}
	direct.delivery.agent = nil
	require.Error(t, direct.acceptPromptCycle(context.Background(), cycle,
		lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}))

	validStream := lifecycle.NewStream("open", lifecycle.Negotiated{Version: 1})
	_, err := validStream.Emit(lifecycle.SnapshotEvent(
		lifecycle.Foreground{State: lifecycle.ForegroundIdle, CycleID: "cycle-0"}, nil, lifecycle.QuiescenceFact{},
	))
	require.NoError(t, err)
	closedDelivery := newSessionDelivery(nil, "session")
	closedDelivery.closed = true
	openFailure := &session{
		incarnation: &nativeIncarnationBinding{client: client, stream: validStream, registry: newActionRegistry()},
		delivery:    closedDelivery,
	}
	_, err = openFailure.openAgentCycleLocked(context.Background())
	require.Error(t, err)

	closeCycle := &foregroundCycle{
		id: "close-cycle", turnID: "close-turn", blockers: map[string]struct{}{}, signal: make(chan struct{}),
	}
	openFailure.cycle = closeCycle
	require.Error(t, openFailure.settleCloseCycle(context.Background(), closeCycle))
	require.False(t, openFailure.latchLifecycleIncarnationLossLocked(nil, "ignored", nil))
}
