package opencodeacp

import (
	"context"
	"errors"
	"fmt"
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

	type promptResult struct {
		response acp.PromptResponse
		err      error
	}
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

	current := newSession(agent, "session-1", "/tmp/project", nil, testNativeSession("native-1"),
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
