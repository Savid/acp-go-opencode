package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// permissionAskedEvent is the native announcement of one permission request
// against a tool call.
func permissionAskedEvent(nativeID, requestID, callID string) opencode.Event {
	return opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": requestID, "sessionID": nativeID, "action": "edit",
			"resources": []string{"file.go"},
			"tool":      map[string]any{"callID": callID},
		}),
	}
}

// TestUnstreamedActionIsRefusedBeforeHostRegistration proves an action without
// a negotiated lifecycle incarnation is answered natively and contained before
// the host can observe it.
func TestUnstreamedActionIsRefusedBeforeHostRegistration(t *testing.T) {
	agent := NewAgent()
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)

	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	err := current.routeNativePermission(context.Background(), opencode.PermissionRequest{
		ID: "permission-1", SessionID: current.idmap.NativeSessionID,
	})
	require.ErrorContains(t, err, "active lifecycle incarnation")
	require.Equal(t, 1, client.permissionReplyCount())
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Zero(t, connection.permissionRequestCount())
	require.Empty(t, connection.lifecycleEnvelopes(t))
	requireEventually(t, client.isClosed, "unstreamed action did not contain its native incarnation")
}

func TestCancellationTerminalizationRaceSettlesEachActionOnce(t *testing.T) {
	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	client, ok := current.currentClient().(*fakeOpenCodeClient)
	require.True(t, ok)

	for index := range 128 {
		binding := testIncarnation(current)
		action := &pendingAction{
			id: fmt.Sprintf("permission-race-%d", index), kind: actionPermission,
			binding:       binding,
			permission:    opencode.PermissionRequest{ID: fmt.Sprintf("permission-race-%d", index), SessionID: current.idmap.NativeSessionID},
			admissionDone: make(chan struct{}),
		}
		close(action.admissionDone)
		claimed, err := binding.registry.claim(action)
		require.NoError(t, err)
		require.True(t, claimed)

		start := make(chan struct{})
		var contenders sync.WaitGroup
		contenders.Add(2)
		go func() {
			defer contenders.Done()
			<-start
			current.finishAction(action, nativeActionOutcome{state: lifecycle.ActionAccepted, permissionReply: permissionReplyOnce})
		}()
		go func() {
			defer contenders.Done()
			<-start
			current.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})
		}()
		close(start)
		contenders.Wait()
	}

	require.Equal(t, 128, client.permissionReplyCount())
	require.False(t, testIncarnation(current).registry.blocked())
}

func TestActionRegistryFailsClosedAtEmitterQuota(t *testing.T) {
	registry := newActionRegistry()
	for index := range lifecycle.EmitterActionLimit {
		claimed, err := registry.claim(&pendingAction{id: fmt.Sprintf("action-%d", index)})
		require.NoError(t, err)
		require.True(t, claimed)
	}

	claimed, err := registry.claim(&pendingAction{id: "action-overflow"})
	require.Error(t, err)
	require.False(t, claimed)
	require.Len(t, registry.claimed, lifecycle.EmitterActionLimit)
	require.NotContains(t, registry.pending, "action-overflow")
}

func TestCancelActionsUsesOneAggregateSettlementDeadline(t *testing.T) {
	oldTimeout := actionSettlementTimeout
	actionSettlementTimeout = 200 * time.Millisecond
	t.Cleanup(func() { actionSettlementTimeout = oldTimeout })

	current, client, _ := lifecycleSession(t)
	binding := testIncarnation(current)
	started := make(chan string, 2)
	client.replyPermissionFunc = func(ctx context.Context, req opencode.PermissionRequest, _, _ string) error {
		started <- req.ID
		<-ctx.Done()

		return ctx.Err()
	}

	for _, id := range []string{"held-one", "held-two"} {
		admissionDone := make(chan struct{})
		close(admissionDone)
		action := &pendingAction{
			id: id, kind: actionPermission, binding: binding, admissionDone: admissionDone,
			permission: opencode.PermissionRequest{ID: id, SessionID: current.idmap.NativeSessionID},
		}
		claimed, err := binding.registry.claim(action)
		require.NoError(t, err)
		require.True(t, claimed)
	}

	done := make(chan struct{})
	go func() {
		current.cancelActions(context.Background())
		close(done)
	}()

	require.ElementsMatch(t, []string{"held-one", "held-two"}, []string{<-started, <-started})
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("two native replies received serial fresh settlement deadlines")
	}
	require.Empty(t, binding.registry.snapshot())
}

func TestActionIdentifierBoundsAreExact(t *testing.T) {
	boundary := strings.Repeat("i", lifecycle.IdentifierBound)
	overflow := boundary + "i"

	for _, tc := range []struct {
		name   string
		mutate func(*pendingAction, string)
	}{
		{name: "action", mutate: func(action *pendingAction, value string) { action.id = value }},
		{name: "owner", mutate: func(action *pendingAction, value string) { action.owner.ID = value }},
		{name: "tool call", mutate: func(action *pendingAction, value string) { action.permission.Tool.CallID = value }},
		{name: "tool message", mutate: func(action *pendingAction, value string) { action.permission.Tool.MessageID = value }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := &pendingAction{
				id: "action", kind: actionPermission, owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: "turn"},
				permission: opencode.PermissionRequest{Tool: opencode.PermissionTool{CallID: "call"}},
			}
			tc.mutate(action, boundary)
			require.NoError(t, validateActionIdentifiers(action))
			tc.mutate(action, overflow)
			require.Error(t, validateActionIdentifiers(action))
		})
	}
}

func TestActionAdmissionRefusesIdentifierAndFrameBoundsBeforeHostRegistration(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  opencode.PermissionRequest
	}{
		{
			name: "identifier 4097",
			req: opencode.PermissionRequest{
				ID: strings.Repeat("a", lifecycle.IdentifierBound+1), SessionID: "native-1",
			},
		},
		{
			name: "complete frame above native limit",
			req: opencode.PermissionRequest{
				ID: "permission-large", SessionID: "native-1",
				Resources: []string{strings.Repeat("s", maxACPFrameBytes)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current, _, connection := lifecycleSession(t)
			err := current.routeNativePermission(context.Background(), tc.req)
			require.Error(t, err)
			require.Zero(t, connection.permissionRequestCount())
			require.Error(t, current.lifecycleFailure())
		})
	}

	current, _, connection := lifecycleSession(t)
	err := current.routeNativePermission(context.Background(), opencode.PermissionRequest{
		ID: strings.Repeat("a", lifecycle.IdentifierBound), SessionID: "native-1",
	})
	require.NoError(t, err)
	require.Equal(t, 1, connection.permissionRequestCount())
}

// TestCommandCompletionCancelsAStillPendingBlocker proves the completion-reporting
// route settles its turn through the same ordering an event-settled turn uses: a
// blocker still awaiting its host answer when the run reports finished is
// cancelled, answered natively, and reported terminal before the ending idle.
func TestCommandCompletionCancelsAStillPendingBlocker(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	connection.permissionIgnoreContext = true
	started, release := connection.permissionStarted, connection.permissionRelease
	connection.mu.Unlock()

	hold := make(chan struct{})
	assistantObserved := make(chan struct{})
	current.mu.Lock()
	current.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
		if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "assistant-1" {
			signalTestHook(assistantObserved)
		}
	}
	current.mu.Unlock()
	client.dispatchCommand = func(_ context.Context, id string, request opencode.CommandRequest) (opencode.NativeMessage, error) {
		client.publishPromptEvidence(id, request.MessageID)
		client.stageAssistantMessage(id, "assistant-1")
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
		<-hold

		return opencode.NativeMessage{}, nil
	}

	current.markPublishedToolCall("call-1")

	done := make(chan promptResult, 1)

	go func() {
		response, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "/review"))
		done <- promptResult{response: response, err: err}
	}()

	requireSignal(t, started)
	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "action_update")) == 1
	}, "the blocker was never announced")

	close(hold)
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonEndTurn, result.response.StopReason)

	require.Equal(t, 1, client.permissionReplyCount())
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)

	actions := connection.lifecycleEventsOfType(t, "action_update")
	require.Len(t, actions, 2)
	terminal, ok := actions[1]["action"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "cancelled", terminal["state"])

	requireLifecycleReduces(t, connection)
	close(release)
}

// TestReannouncedActionIsRefusedAndFenced proves the claim ledger is permanent:
// the duplicate is answered natively, never shown to the host, and contains the
// exact incarnation instead of being accepted as a second action.
func TestReannouncedActionIsRefusedAndFenced(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID
	generation := current.runtimeGeneration

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	started, release := connection.permissionStarted, connection.permissionRelease
	connection.mu.Unlock()

	req := opencode.PermissionRequest{ID: "permission-1", SessionID: native}
	require.NoError(t, current.routeNativePermission(context.Background(), req))
	requireSignal(t, started)

	require.Error(t, current.routeNativePermission(context.Background(), req))
	require.NotZero(t, client.permissionReplyCount(), "duplicate native waiter was not rejected")
	for index := range client.permissionReplyCount() {
		require.Equal(t, "permission-1", client.permissionReply(index).requestID)
	}
	require.Equal(t, 1, connection.permissionRequestCount(), "a re-announced request reached the host twice")
	requireExactGenerationContained(t, current, client, generation)
	close(release)
}

func TestReusedActionIDsAreAnsweredAndFenceTheirExactIncarnation(t *testing.T) {
	t.Run("post terminal", func(t *testing.T) {
		current, client, _ := lifecycleSession(t)
		generation := current.runtimeGeneration
		req := opencode.PermissionRequest{ID: "reused", SessionID: current.idmap.NativeSessionID}
		require.NoError(t, current.routeNativePermission(context.Background(), req))
		requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "first action never settled")

		require.Error(t, current.routeNativePermission(context.Background(), req))
		require.Equal(t, 2, client.permissionReplyCount(), "reused native request was left waiting")
		require.Equal(t, "reused", client.permissionReply(1).requestID)
		requireExactGenerationContained(t, current, client, generation)
	})

	t.Run("conflicting kind", func(t *testing.T) {
		current, client, connection := lifecycleSession(t)
		generation := current.runtimeGeneration
		connection.mu.Lock()
		connection.permissionStarted = make(chan struct{})
		connection.permissionRelease = make(chan struct{})
		started, release := connection.permissionStarted, connection.permissionRelease
		connection.mu.Unlock()

		require.NoError(t, current.routeNativePermission(context.Background(), opencode.PermissionRequest{
			ID: "conflict", SessionID: current.idmap.NativeSessionID,
		}))
		requireSignal(t, started)
		require.Error(t, current.routeNativeQuestion(context.Background(), opencode.QuestionRequest{
			ID: "conflict", SessionID: current.idmap.NativeSessionID,
		}))
		require.Equal(t, 1, client.questionRejectCount(), "conflicting native request was left waiting")
		requireExactGenerationContained(t, current, client, generation)
		close(release)
	})
}

// TestAnnouncementFailureSettlesTheActionNatively proves an action whose
// announcement could not be delivered is still answered: OpenCode is never left
// blocked on a request this adapter could not report, and the undeliverable
// terminal state is recorded as the action's failure rather than dropped.
func TestAnnouncementFailureSettlesTheActionNatively(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	// A cycle is already open, so the action's own announcement is the first
	// envelope it needs.
	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	requireEventually(t, func() bool { return current.currentCycle() != nil }, "no agent-origin cycle opened")

	connection.mu.Lock()
	connection.updateErr = errors.New("wire down")
	connection.mu.Unlock()
	client.permissionReplied = make(chan struct{}, 1)

	current.markPublishedToolCall("call-1")
	client.publishEvent(permissionAskedEvent(native, "permission-1", "call-1"))

	requireSignal(t, client.permissionReplied)
	require.Equal(t, 1, client.permissionReplyCount(), "OpenCode was left blocked")
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.ErrorContains(t, current.lifecycleFailure(), "lifecycle delivery failed")
}

// TestActionOpeningFailureDefersContainmentUntilNativeRefusal proves the first
// event of an agent-origin action obeys the same ordering as the action's own
// announcement. A delivery failure may fence new host traffic immediately, but
// it must not unpublish the native binding while the admitted request is still
// waiting for registration; refusal runs first, and containment follows it.
func TestActionOpeningFailureDefersContainmentUntilNativeRefusal(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	binding := testIncarnation(current)
	generation := testRuntimeGeneration(current)

	connection.mu.Lock()
	connection.updateErr = errors.New("wire down")
	connection.updateStarted = make(chan struct{})
	connection.updateRelease = make(chan struct{})
	connection.permissionRegistrationStarted = make(chan struct{})
	connection.permissionRegistrationRelease = make(chan struct{})
	updateStarted, updateRelease := connection.updateStarted, connection.updateRelease
	registrationStarted, registrationRelease := connection.permissionRegistrationStarted,
		connection.permissionRegistrationRelease
	connection.mu.Unlock()

	closeOnce := func(channel chan struct{}) {
		select {
		case <-channel:
		default:
			close(channel)
		}
	}
	t.Cleanup(func() {
		closeOnce(updateRelease)
		closeOnce(registrationRelease)
	})

	result := make(chan error, 1)
	go func() {
		result <- current.routeNativePermission(context.Background(), opencode.PermissionRequest{
			ID: "permission-1", SessionID: current.idmap.NativeSessionID,
		})
	}()

	requireSignal(t, registrationStarted)
	requireSignal(t, updateStarted)
	close(updateRelease)
	requireSignal(t, current.delivery.typedDone)

	current.agent.mu.Lock()
	runtimeStillPublished := current.agent.runtime != nil
	current.agent.mu.Unlock()
	require.True(t, runtimeStillPublished, "opening delivery failure contained before native refusal")
	require.True(t, current.incarnationIsCurrent(binding), "opening delivery failure unpublished the admitted request")
	require.False(t, client.isClosed(), "opening delivery failure closed the scope before native refusal")

	close(registrationRelease)
	require.Error(t, <-result)
	require.Equal(t, 1, client.permissionReplyCount(), "OpenCode was left blocked")
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	requireExactGenerationContained(t, current, client, generation)
}

// TestNativeResolutionFailureIsRecordedAgainstTheCycle proves a natively resolved
// action whose terminal state cannot be delivered records the delivery failure
// instead of reporting the action resolved on a stream that never carried it.
func TestNativeResolutionFailureIsRecordedAgainstTheCycle(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	started, release := connection.permissionStarted, connection.permissionRelease
	connection.mu.Unlock()

	current.markPublishedToolCall("call-1")
	client.publishEvent(permissionAskedEvent(native, "permission-1", "call-1"))
	requireSignal(t, started)
	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "action_update")) == 1
	}, "the blocker was never announced")

	connection.mu.Lock()
	connection.updateErr = errors.New("wire down")
	connection.mu.Unlock()

	cycle := current.currentCycle()
	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Replied,
		Properties: mustJSONValue(map[string]any{
			"sessionID": native, "requestID": "permission-1", "reply": permissionReplyAlways,
		}),
	})

	requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "the delivery failure was swallowed")
	require.Error(t, current.cycleFailure(cycle))
	require.NotContains(t, current.cycleFailure(cycle).Error(), "wire down")
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the broken agent cycle remained open")
	require.Zero(t, client.permissionReplyCount(), "an action OpenCode resolved was answered anyway")

	close(release)
}

// TestActionResolutionsThisSessionDoesNotHoldAreIgnored proves both halves of the
// exactly-once settlement fence: a native resolution naming a request this
// session never held terminalizes nothing, and a settlement for an action the
// session no longer holds answers OpenCode nothing.
func TestActionResolutionsThisSessionDoesNotHoldAreIgnored(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Replied,
		Properties: mustJSONValue(map[string]any{
			"sessionID": native, "requestID": "never-registered", "reply": permissionReplyOnce,
		}),
	})

	// Work queued behind the resolution proves the pump consumed it.
	client.publishEvent(opencode.Event{
		Type:       opencode.EventSessionStatus,
		Properties: mustJSONValue(map[string]any{"sessionID": native, "status": map[string]any{"type": "busy"}}),
	})
	requireEventually(t, func() bool { return current.currentCycle() != nil }, "the pump stalled")

	current.settleActionNatively(context.Background(),
		&pendingAction{id: "gone", kind: actionPermission}, nativeActionOutcome{state: lifecycle.ActionCancelled})

	require.Zero(t, client.permissionReplyCount())
	require.Empty(t, connection.lifecycleEventsOfType(t, "action_update"))
}

// TestStreamlessIncarnationRefusesToOpenAnAgentTurn proves a negotiated session
// whose stream could not be opened refuses the actions it cannot announce: the
// host is owed an ordered stream, and an action shown outside one would be an
// action this session could never report terminal.
func TestStreamlessIncarnationRefusesToOpenAnAgentTurn(t *testing.T) {
	agent := negotiatedAgent(t)
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)

	old := newTurnNonceRead
	newTurnNonceRead = failingRouteReader{}.Read

	current := newSession(agent, "session-1", absTestPath("tmp", "project"), nil, testNativeSession("native-1"),
		newFakeOpenCodeClient(), sessionMeta{}, idmapRecord{
			SessionID: "session-1", NativeSessionID: "native-1", Format: SessionStoreFormat,
		})

	newTurnNonceRead = old

	require.ErrorContains(t, current.lifecycleFailure(), "create turn nonce")
	require.True(t, current.lifecycleStreamAbsent())
	require.Empty(t, current.lifecycleStreamID(), "a streamless incarnation named a stream")

	require.ErrorContains(t, current.routeNativePermission(context.Background(), opencode.PermissionRequest{
		ID: "permission-1", SessionID: "native-1",
	}), "create turn nonce")
	require.Zero(t, connection.permissionRequestCount(), "an unannounceable action reached the host")
}

// TestActionAndTranscriptRepliesNeedARuntimeBinding proves the two reads a
// settling turn makes against the native runtime fail loudly when the binding is
// gone, rather than being attributed to a client this session no longer owns.
func TestActionAndTranscriptRepliesNeedARuntimeBinding(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	current.stopPump()

	current.mu.Lock()
	current.client = nil
	current.mu.Unlock()

	require.ErrorContains(t,
		current.replyNative(ctx, &pendingAction{
			id: "permission-1", kind: actionPermission, binding: &nativeIncarnationBinding{},
		}, nativeActionOutcome{}),
		"no runtime client")

	_, err := current.finalAssistantMessage(ctx, &foregroundCycle{id: "cycle-1"})
	require.ErrorContains(t, err, "no runtime client")
}

func TestFinalAssistantMessageNeverFallsBackToAnUnrelatedAssistant(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	client.mu.Lock()
	client.messages = []opencode.NativeMessage{{
		Info: opencode.NativeMessageInfo{
			ID: "assistant-a", SessionID: current.idmap.NativeSessionID, Role: "assistant", Finish: "stop",
			Tokens: opencode.NativeTokens{Total: 999, Output: 999},
		},
		Parts: []opencode.NativePart{{
			SessionID: current.idmap.NativeSessionID, MessageID: "assistant-a", Type: "text", Text: "A output",
		}},
	}}
	client.mu.Unlock()

	message, err := current.finalAssistantMessage(context.Background(), &foregroundCycle{
		id: "cycle-b", assistantID: "assistant-b",
	})
	require.ErrorIs(t, err, errTurnAssistantIdentityMissing)
	require.Empty(t, message.Info.ID)
	require.Empty(t, message.Parts)
}

func TestRemovedOldGenerationActionsCannotTouchRecoveredStream(t *testing.T) {
	current, oldClient, connection := lifecycleSession(t)
	current.stopPump()
	oldGeneration := current.runtimeGeneration
	oldBinding := testIncarnation(current)
	oldRegistry := oldBinding.registry
	cycle := acceptOpenTestTurn(t, current)

	actions := []*pendingAction{
		{id: "permission-old", kind: actionPermission, binding: oldBinding, cycle: cycle,
			permission: opencode.PermissionRequest{ID: "permission-old", SessionID: current.idmap.NativeSessionID}},
		{id: "question-old", kind: actionElicitation, binding: oldBinding, cycle: cycle,
			question: opencode.QuestionRequest{ID: "question-old", SessionID: current.idmap.NativeSessionID}},
	}
	for _, action := range actions {
		claimed, err := oldRegistry.claim(action)
		require.NoError(t, err)
		require.True(t, claimed)
		_, held := oldRegistry.take(action.id)
		require.True(t, held)
	}

	replacement := newFakeOpenCodeClient()
	replacement.ensureSyncAggregate(current.idmap.NativeSessionID)
	current.mu.Lock()
	current.client = replacement
	current.runtimeGeneration = oldGeneration + 1
	current.mu.Unlock()
	current.reopenLifecycleStream()
	require.NoError(t, current.publishLifecycleStream(context.Background()))
	newBinding := testIncarnation(current)
	before := newBinding.stream.State().ReducedThrough

	staleCtx := withNativeIncarnationBinding(context.Background(), oldBinding)
	err := current.routeNativePermission(staleCtx, opencode.PermissionRequest{
		ID: "permission-stale-admission", SessionID: current.idmap.NativeSessionID,
	})
	require.ErrorContains(t, err, "binding is stale")
	require.Empty(t, newBinding.registry.snapshot())
	require.Zero(t, connection.permissionRequestCount())

	for _, action := range actions {
		require.NoError(t, current.replyNative(context.Background(), action,
			nativeActionOutcome{state: lifecycle.ActionAccepted, permissionReply: permissionReplyOnce}))
		require.NoError(t, current.terminalizeAction(context.Background(), action, lifecycle.ActionAccepted))
	}

	require.Zero(t, oldClient.permissionReplyCount())
	require.Zero(t, oldClient.questionReplyCount())
	require.Zero(t, oldClient.questionRejectCount())
	require.Equal(t, before, newBinding.stream.State().ReducedThrough)
	require.NoError(t, current.lifecycleFailure())
}

// TestAnAbandonedHostAskRecordsNoCycleFailure proves an ask this adapter walked
// away from is not what ends the turn. Every path that abandons an outstanding
// host request cancels its context first and then records whatever actually
// resolved the action — OpenCode answering it itself, or the cycle ending under
// it — so the cancellation the abandonment produces must never reach the cycle's
// single failure slot and shadow the real reason. The reply OpenCode receives is
// the observable proof the abandoned request reached its own verdict first.
func TestAnAbandonedHostAskRecordsNoCycleFailure(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	native := current.idmap.NativeSessionID

	connection.mu.Lock()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	started := connection.permissionStarted
	connection.mu.Unlock()

	current.markPublishedToolCall("call-1")
	client.publishEvent(permissionAskedEvent(native, "permission-1", "call-1"))
	requireSignal(t, started)

	var pending *pendingAction

	for _, action := range testIncarnation(current).registry.snapshot() {
		if action.id == "permission-1" {
			pending = action
		}
	}

	require.NotNil(t, pending, "the action was never registered")
	pending.cancel()

	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "OpenCode was left blocked")
	require.NoError(t, current.cycleFailure(current.currentCycle()), "an abandoned ask ended the cycle")
	require.NoError(t, current.lifecycleFailure())
}

func TestCorrectionPureOwnershipAndCallbackBranches(t *testing.T) {
	registry := newActionRegistry()
	action := &pendingAction{id: "action"}
	require.False(t, func() bool {
		_, ok := registry.takeIf("missing", action)

		return ok
	}())

	require.Error(t, validateActionIdentifiers(&pendingAction{id: "", owner: lifecycle.Owner{ID: "turn"}}))
	require.NoError(t, validateActionIdentifiers(&pendingAction{id: "action", owner: lifecycle.Owner{ID: "turn"}}))

	current := &session{idmap: idmapRecord{NativeSessionID: "native"}}
	require.NoError(t, current.refuseNativeAction(context.Background(), &pendingAction{}, false, nil))
	require.Error(t, current.refuseNativeAction(
		context.Background(), &pendingAction{}, false, errors.New("refused"),
	))
	require.Nil(t, withNativeIncarnationBinding(context.Background(), nil).Value(nativeIncarnationContextKey{}))
	current.failNativeIncarnation(nil, nil)
	current.failPendingDispatch(errors.New("ignored"))

	require.False(t, cycleOwnsAction(nil, "action"))
	require.False(t, cycleOwnsAction(&foregroundCycle{blockers: map[string]struct{}{}}, ""))
	require.True(t, cycleOwnsAction(&foregroundCycle{blockers: map[string]struct{}{"action": {}}}, "action"))

	original := nativeMessageIDEntropy
	nativeMessageIDEntropy = failingRouteReader{}
	_, err := nativePromptMessageID()
	nativeMessageIDEntropy = original
	require.ErrorContains(t, err, "create native prompt message id")

	transport := newConnectionTransport(io.Discard, strings.NewReader(""))
	transport.interrupt()
}

func TestCorrectionActionPreflightAndAnnouncementFailures(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)

	require.Error(t, current.validateHostActionFrame(&pendingAction{
		id: "question-action", kind: actionElicitation, turnNonce: strings.Repeat("n", lifecycle.IdentifierBound+1),
		question: opencode.QuestionRequest{
			ID: "question-action", SessionID: current.idmap.NativeSessionID,
			Questions: []opencode.QuestionInfo{{Question: "answer", Header: "header"}},
		},
		binding: binding,
	}))

	current.lifecycleMu.Lock()
	preflightCycle, err := current.openAgentCycleLocked(context.Background())
	require.NoError(t, err)
	for index := range lifecycle.EmitterActionLimit {
		actionID := fmt.Sprintf("preflight-quota-%d", index)
		_, err = binding.stream.Emit(lifecycle.ActionEvent(lifecycle.PendingAction(
			actionID, lifecycle.ActionPermission,
			lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: preflightCycle.turnID}, true,
		)))
		require.NoError(t, err)
		_, err = binding.stream.Emit(lifecycle.ActionEvent(lifecycle.ResolvedAction(
			actionID, lifecycle.ActionAccepted,
		)))
		require.NoError(t, err)
	}
	current.lifecycleMu.Unlock()
	_, err = current.beginAction(context.Background(), &pendingAction{
		id: "preflight", binding: binding,
		owner:      lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: preflightCycle.turnID},
		permission: opencode.PermissionRequest{ID: "preflight", SessionID: current.idmap.NativeSessionID},
	})
	require.Error(t, err)

	announcer, _, announceConnection := lifecycleSession(t)
	announcer.stopPump()
	binding = testIncarnation(announcer)
	announcer.lifecycleMu.Lock()
	cycle, err := announcer.openAgentCycleLocked(context.Background())
	announcer.lifecycleMu.Unlock()
	require.NoError(t, err)
	require.NotNil(t, cycle)
	announced, err := announcer.announceAction(context.Background(), &pendingAction{
		id: "bad-transition", binding: binding,
		owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: cycle.turnID},
	}, &foregroundCycle{id: "wrong-cycle", turnID: cycle.turnID, origin: cycle.origin})
	require.True(t, announced)
	require.Error(t, err)

	announceConnection.mu.Lock()
	announceConnection.updateErr = errors.New("delivery failed")
	announceConnection.mu.Unlock()
	announcer.lifecycleMu.Lock()
	announcer.lifecycleFailed = nil
	announcer.installLifecycleStream(announcer.agent.lifecycleNegotiated())
	announcer.lifecycleMu.Unlock()
	require.Error(t, announcer.publishLifecycleStream(context.Background()))
}

func TestCorrectionActionDeliveryAndBlockedTerminalBranches(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	current.stopPump()
	binding := testIncarnation(current)
	current.lifecycleMu.Lock()
	cycle, err := current.openAgentCycleLocked(context.Background())
	current.lifecycleMu.Unlock()
	require.NoError(t, err)
	require.NoError(t, current.emitUpdate(context.Background(), acp.UpdateAgentMessageText("ready")))

	connection.mu.Lock()
	connection.updateErr = errors.New("delivery failed")
	connection.updateStarted = make(chan struct{})
	connection.updateRelease = make(chan struct{})
	started, release := connection.updateStarted, connection.updateRelease
	connection.mu.Unlock()
	result := make(chan struct {
		announced bool
		err       error
	}, 1)
	go func() {
		announced, announceErr := current.announceAction(context.Background(), &pendingAction{
			id: "delivery-failure", binding: binding,
			owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: cycle.turnID},
		}, cycle)
		result <- struct {
			announced bool
			err       error
		}{announced: announced, err: announceErr}
	}()
	requireSignal(t, started)
	current.lifecycleMu.Lock()
	_ = current.cycle
	current.lifecycleMu.Unlock()
	close(release)
	announcement := <-result
	announced, err := announcement.announced, announcement.err
	require.True(t, announced)
	require.Error(t, err)

	blocked, _, _ := lifecycleSession(t)
	blocked.stopPump()
	blockedBinding := testIncarnation(blocked)
	blocked.lifecycleMu.Lock()
	blockedCycle, err := blocked.openAgentCycleLocked(context.Background())
	blocked.lifecycleMu.Unlock()
	require.NoError(t, err)
	action := &pendingAction{
		id: "blocked-action", binding: blockedBinding, cycle: blockedCycle,
		owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: blockedCycle.turnID},
	}
	announced, err = blocked.announceAction(context.Background(), action, blockedCycle)
	require.True(t, announced)
	require.NoError(t, err)
	blocked.lifecycleMu.Lock()
	blockedCycle.blockers[action.id] = struct{}{}
	blockedCycle.blockers["other-action"] = struct{}{}
	blocked.lifecycleMu.Unlock()
	require.NoError(t, blocked.terminalizeAction(context.Background(), action, lifecycle.ActionCancelled))

	failingTerminal, _, terminalConnection := lifecycleSession(t)
	failingTerminal.stopPump()
	terminalBinding := testIncarnation(failingTerminal)
	failingTerminal.lifecycleMu.Lock()
	terminalCycle, err := failingTerminal.openAgentCycleLocked(context.Background())
	failingTerminal.lifecycleMu.Unlock()
	require.NoError(t, err)
	terminalAction := &pendingAction{
		id: "terminal-delivery", binding: terminalBinding, cycle: terminalCycle,
		owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: terminalCycle.turnID},
	}
	announced, err = failingTerminal.announceAction(context.Background(), terminalAction, terminalCycle)
	require.True(t, announced)
	require.NoError(t, err)
	failingTerminal.lifecycleMu.Lock()
	terminalCycle.blockers[terminalAction.id] = struct{}{}
	failingTerminal.lifecycleMu.Unlock()
	terminalConnection.mu.Lock()
	terminalConnection.updateErr = errors.New("terminal delivery failed")
	terminalConnection.updateStarted = make(chan struct{})
	terminalConnection.updateRelease = make(chan struct{})
	terminalStarted, terminalRelease := terminalConnection.updateStarted, terminalConnection.updateRelease
	terminalConnection.mu.Unlock()
	terminalResult := make(chan error, 1)
	go func() {
		terminalResult <- failingTerminal.terminalizeAction(
			context.Background(), terminalAction, lifecycle.ActionCancelled,
		)
	}()
	requireSignal(t, terminalStarted)
	failingTerminal.lifecycleMu.Lock()
	_ = failingTerminal.cycle
	failingTerminal.lifecycleMu.Unlock()
	close(terminalRelease)
	require.Error(t, <-terminalResult)
}

func TestCorrectionActionAdmissionAndSettlementBranches(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	binding := testIncarnation(current)

	stale := &pendingAction{id: "stale", binding: &nativeIncarnationBinding{}, owner: lifecycle.Owner{ID: "turn"}}
	_, err := current.beginAction(context.Background(), stale)
	require.ErrorContains(t, err, "binding is stale")

	wrongOwner := &pendingAction{
		id: "wrong-owner", binding: binding, owner: lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: "wrong"},
		permission: opencode.PermissionRequest{ID: "wrong-owner", SessionID: current.idmap.NativeSessionID},
	}
	_, err = current.beginAction(context.Background(), wrongOwner)
	require.ErrorContains(t, err, "owner changed")

	require.Error(t, current.validateHostActionFrame(&pendingAction{
		id: "marshal", kind: actionPermission, owner: lifecycle.Owner{ID: "turn"},
		permission: opencode.PermissionRequest{Metadata: map[string]any{"invalid": func() {}}},
	}))
	require.ErrorContains(t, current.validateHostActionFrame(&pendingAction{
		id: "large", kind: actionPermission, owner: lifecycle.Owner{ID: "turn"},
		permission: opencode.PermissionRequest{Metadata: map[string]any{"large": strings.Repeat("x", maxACPFrameBytes)}},
	}), "exceeds")

	cycle := &foregroundCycle{
		id: "cycle", turnID: "turn", origin: lifecycle.CauseActivity,
		blockers: map[string]struct{}{}, signal: make(chan struct{}),
	}
	current.lifecycleMu.Lock()
	current.cycle = cycle
	current.lifecycleMu.Unlock()

	cancelledAdmission := &pendingAction{
		id: "cancelled-admission", binding: binding,
		owner:              lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: cycle.turnID},
		permission:         opencode.PermissionRequest{ID: "cancelled-admission", SessionID: current.idmap.NativeSessionID},
		admissionCancelled: true,
	}
	answered, err := current.beginAction(context.Background(), cancelledAdmission)
	require.NoError(t, err)
	require.True(t, answered)

	require.False(t, func() bool {
		_, announceErr := current.announceAction(context.Background(), &pendingAction{binding: &nativeIncarnationBinding{}}, cycle)

		return announceErr == nil
	}())

	for _, action := range []*pendingAction{
		{admissionCancelled: true, cycle: &foregroundCycle{signal: make(chan struct{})}},
		{cycle: func() *foregroundCycle {
			settledCycle := &foregroundCycle{signal: make(chan struct{})}
			close(settledCycle.signal)

			return settledCycle
		}()},
	} {
		answered := make(chan nativeActionOutcome, 1)
		answered <- nativeActionOutcome{state: lifecycle.ActionAccepted}
		current.awaitActionOutcome(action, answered)
	}

	innerCancelled := &pendingAction{
		cycle: &foregroundCycle{signal: make(chan struct{})}, binding: nil,
	}
	innerCancelled.admissionMu.Lock()
	innerAnswered := make(chan nativeActionOutcome)
	innerDone := make(chan struct{})
	go func() {
		current.awaitActionOutcome(innerCancelled, innerAnswered)
		close(innerDone)
	}()
	innerAnswered <- nativeActionOutcome{state: lifecycle.ActionAccepted}
	close(innerCancelled.cycle.signal)
	innerCancelled.admissionMu.Unlock()
	<-innerDone

	unheld := &pendingAction{id: "unheld", binding: binding}
	current.settleActionNatively(context.Background(), unheld, nativeActionOutcome{})
	current.settleActionNatively(context.Background(), &pendingAction{
		id: "stale-settlement", binding: &nativeIncarnationBinding{},
	}, nativeActionOutcome{})
	require.ErrorContains(t, current.replyNative(context.Background(), nil, nativeActionOutcome{}), "no incarnation binding")

	require.NoError(t, current.terminalizeAction(context.Background(), &pendingAction{
		id: "no-cycle", binding: binding,
	}, lifecycle.ActionCancelled))

	withoutBinding := &session{}
	withoutBinding.nativeActionResolved(context.Background(), opencode.ActionRepliedEvent{}, lifecycle.ActionCancelled)

	unannounced := &pendingAction{
		id: "unannounced", binding: binding, cycle: cycle, admissionDone: make(chan struct{}),
	}
	close(unannounced.admissionDone)
	binding.registry.pending[unannounced.id] = unannounced
	binding.registry.claimed[unannounced.id] = struct{}{}
	current.nativeActionResolved(context.Background(), opencode.ActionRepliedEvent{RequestID: unannounced.id}, lifecycle.ActionCancelled)

	_ = client
	_ = connection
}

// TestUnroutableNativeActionsAreDroppedOnARetiredIncarnation proves the courtesy
// answer to a request this session cannot route is addressed to the incarnation
// that asked for it. Once that incarnation is retired, the answer is dropped
// rather than sent down a stream this session no longer speaks for.
func TestUnroutableNativeActionsAreDroppedOnARetiredIncarnation(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	current.stopPump()

	native := current.idmap.NativeSessionID
	retired := &nativeIncarnationBinding{client: client, registry: newActionRegistry()}
	ctx := withNativeIncarnationBinding(context.Background(), retired)

	current.declineNativePermission(ctx, opencode.PermissionRequest{ID: "permission-1", SessionID: native})
	current.declineNativeQuestion(ctx, opencode.QuestionRequest{ID: "question-1", SessionID: native})

	require.Zero(t, client.permissionReplyCount(), "a retired incarnation answered a permission")
	require.Zero(t, client.questionRejectCount(), "a retired incarnation answered a question")
}
