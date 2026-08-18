package opencodeacp

import (
	"context"
	"errors"
	"testing"

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

// TestUnstreamedBlockerIsCancelledWhenTheInterruptIsRefused proves the action
// registry is the authority on what is pending, not the lifecycle stream: a
// connection that negotiated nothing still registers its permission, and a turn
// whose native interrupt the harness refused cancels that blocker, answers
// OpenCode exactly once, and reports the settlement unproven rather than
// claiming a clean end.
func TestUnstreamedBlockerIsCancelledWhenTheInterruptIsRefused(t *testing.T) {
	agent := NewAgent()
	connection := newRecordingAgentClient()
	connection.permissionStarted = make(chan struct{})
	connection.permissionRelease = make(chan struct{})
	connection.permissionIgnoreContext = true
	started, release := connection.permissionStarted, connection.permissionRelease
	agent.setAgentClient(connection)

	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	current.markPublishedToolCall("call-1")
	client.abortFunc = func(string) error { return errors.New("harness refused the interrupt") }
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.publishEvent(permissionAskedEvent(id, "permission-1", "call-1"))

		return opencode.NativeMessage{}, nil
	}

	done := make(chan error, 1)

	go func() {
		_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		done <- err
	}()

	requireSignal(t, started)
	require.ErrorContains(t,
		agent.Cancel(context.Background(), CancelRequest(current.id, internalSeamTurnNonce)),
		"harness refused the interrupt")

	require.ErrorContains(t, <-done, "opencode_turn_settlement_unproven")

	// The cancelling settlement is the one that answered OpenCode.
	require.Equal(t, 1, client.permissionReplyCount())
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.Equal(t, reasonCancelled, client.permissionReply(0).message)

	// The host answer lands after the action was already settled; the request is
	// no longer held, so OpenCode is not answered a second time.
	close(release)
	current.stopPump()
	require.Equal(t, 1, client.permissionReplyCount())
	require.Empty(t, connection.lifecycleEnvelopes(t))
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
	client.dispatchCommand = func(_ context.Context, id string, _ opencode.CommandRequest) (opencode.NativeMessage, error) {
		client.stageAssistantMessage(id, "assistant-1")
		client.publishEvent(permissionAskedEvent(id, "permission-1", "call-1"))
		<-hold

		return opencode.NativeMessage{}, nil
	}

	current.markPublishedToolCall("call-1")

	done := make(chan acp.PromptResponse, 1)

	go func() {
		response, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "/review"))
		require.NoError(t, err)
		done <- response
	}()

	requireSignal(t, started)
	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "action_update")) == 1
	}, "the blocker was never announced")

	close(hold)
	require.Equal(t, acp.StopReasonEndTurn, (<-done).StopReason)

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

// TestReannouncedActionIsClaimedOnce proves the claim ledger is permanent: a
// native stream that re-announces a request it already announced neither shows
// the host a second request nor announces a second action.
func TestReannouncedActionIsClaimedOnce(t *testing.T) {
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

	client.publishEvent(permissionAskedEvent(native, "permission-1", "call-1"))

	close(release)
	requireEventually(t, func() bool {
		return len(connection.lifecycleEventsOfType(t, "action_update")) == 2
	}, "the action never terminalized")

	// The idle is queued behind the re-announcement, so the cycle it ends proves
	// the pump routed both.
	client.publishSessionIdle(native)
	requireEventually(t, func() bool { return current.currentCycle() == nil }, "the agent-origin turn never settled")

	require.Equal(t, 1, client.permissionReplyCount())
	require.Equal(t, 1, connection.permissionRequestCount(), "a re-announced request reached the host twice")
	require.Len(t, connection.lifecycleEventsOfType(t, "action_update"), 2)
	requireLifecycleReduces(t, connection)
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

	current.markPublishedToolCall("call-1")
	client.publishEvent(permissionAskedEvent(native, "permission-1", "call-1"))

	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "OpenCode was left blocked")
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)
	require.ErrorContains(t, current.lifecycleFailure(), "wire down")
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

	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Replied,
		Properties: mustJSONValue(map[string]any{
			"sessionID": native, "requestID": "permission-1", "reply": permissionReplyAlways,
		}),
	})

	requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "the delivery failure was swallowed")
	require.ErrorContains(t, current.cycleFailure(current.currentCycle()), "wire down")
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
		current.replyNative(ctx, &pendingAction{id: "permission-1", kind: actionPermission}, nativeActionOutcome{}),
		"no runtime client")

	_, err := current.finalAssistantMessage(ctx, &foregroundCycle{id: "cycle-1"})
	require.ErrorContains(t, err, "no runtime client")
}
