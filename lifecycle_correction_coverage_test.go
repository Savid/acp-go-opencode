package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type coverageInterruptClient struct {
	*recordingAgentClient
	once  sync.Once
	typed chan struct{}
	raw   chan struct{}
}

func (c *coverageInterruptClient) InterruptWrites() {
	c.once.Do(func() {
		close(c.typed)
		close(c.raw)
	})
}

func TestCorrectionDefensiveRegistrationAndEstablishmentBranches(t *testing.T) {
	(*Agent)(nil).interruptConnection()
	(*connectionTransport)(nil).interrupt()

	registrations := newHostRequestRegistrations()
	incomplete := <-registrations.expect(hostRequestKey{})
	require.ErrorContains(t, incomplete, "incomplete lifecycle correlation")

	key := hostRequestKey{method: acp.ClientMethodSessionRequestPermission, streamID: "stream", actionID: "action"}
	first := registrations.expect(key)
	require.ErrorContains(t, <-registrations.expect(key), "already registered")
	registrations.failIfPending(key, nil)
	require.Error(t, <-first)
	(*hostRequestRegistrations)(nil).complete(key, nil)

	for _, frame := range [][]byte{
		[]byte(`{`),
		[]byte(`{"method":"other","params":{}}`),
		[]byte(`{"method":"session/request_permission","params":[]}`),
		[]byte(`{"method":"session/request_permission","params":{"_meta":[]}}`),
		[]byte(`{"method":"session/request_permission","params":{"_meta":{"acp.lifecycle.v1":[]}}}`),
		[]byte(`{"method":"session/request_permission","params":{"_meta":{"acp.lifecycle.v1":{"streamId":"","action":{"actionId":""}}}}}`),
	} {
		_, ok := hostRequestKeyFromFrame(frame)
		require.False(t, ok)
	}

	panicking := &localAgentConnection{registrations: newHostRequestRegistrations()}
	permission := panicking.BeginRequestPermission(context.Background(), acp.RequestPermissionRequest{}, key)
	require.ErrorContains(t, (<-permission.answered).err, "panicked")
	require.Error(t, <-permission.registered)

	elicitationKey := hostRequestKey{method: acp.ClientMethodElicitationCreate, streamID: "stream", actionID: "elicit"}
	elicitation := panicking.BeginCreateElicitation(
		context.Background(), acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{
			Message: "choose", RequestedSchema: acp.UnstableElicitationSchema{},
		}}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, elicitationKey,
	)
	require.ErrorContains(t, (<-elicitation.answered).err, "panicked")
	require.Error(t, <-elicitation.registered)

	hooks := newEstablishmentHooks(nil)
	admitted := false
	hooks.queue("1", func() {}, func() { admitted = true })
	hooks.runAfterWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	require.True(t, admitted)

	for _, frame := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":,"result":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"result":}`),
	} {
		_, _, ok := establishmentResponseFrame(frame)
		require.False(t, ok)
	}
	require.NoError(t, runEstablishmentCallback(nil))
	runEstablishmentFailure(nil, nil, errors.New("ignored"))
	require.Equal(t, "", turnNonceFromContext(nil)) //nolint:staticcheck // nil is the defensive input under test.
}

func TestCorrectionDeliveryDefensiveBranches(t *testing.T) {
	agent := NewAgent()
	delivery := newSessionDelivery(agent, "missing")
	delivery.terminalizePanic("ignored")
	(*sessionDelivery)(nil).terminalizePanic("ignored")

	full := newSessionDelivery(agent, "session")
	full.started = true
	for range authoritativeDeliveryCapacity {
		full.typed <- authoritativeDelivery{done: make(chan error, 1)}
	}
	_, err := full.enqueueUpdate(context.Background(), acp.SessionNotification{})
	require.ErrorContains(t, err, "queue is full")
	for range authoritativeDeliveryCapacity {
		<-full.typed
	}

	closed := newSessionDelivery(agent, "session")
	closed.closed = true
	closed.enqueueRaw(context.Background(), map[string]any{"type": "ignored"})
	require.ErrorContains(t, closed.flushRaw(context.Background()), "closed")
	closed.close()
	(*sessionDelivery)(nil).close()

	queueBlocked := newSessionDelivery(agent, "session")
	queueBlocked.started = true
	for range rawDeliveryCapacity {
		queueBlocked.raw <- rawDelivery{}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, queueBlocked.flushRaw(cancelled), context.Canceled)
	for range rawDeliveryCapacity {
		<-queueBlocked.raw
	}

	waiting := newSessionDelivery(agent, "session")
	waiting.started = true
	received := make(chan struct{})
	go func() {
		<-waiting.raw
		close(received)
	}()
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitDone := make(chan error, 1)
	go func() { waitDone <- waiting.flushRaw(waitCtx) }()
	requireSignal(t, received)
	waitCancel()
	require.ErrorIs(t, <-waitDone, context.Canceled)

	rawWorker := newSessionDelivery(agent, "session")
	rawCtx, rawCancel := context.WithCancel(context.Background())
	go rawWorker.runRaw(rawCtx)
	flushDone := make(chan error, 1)
	rawWorker.raw <- rawDelivery{done: flushDone}
	require.NoError(t, <-flushDone)
	rawCancel()
	requireSignal(t, rawWorker.rawDone)

	require.ErrorContains(t,
		newSessionDelivery(NewAgent(), "session").deliverRaw(context.Background(), map[string]any{}, 1),
		"no ACP connection",
	)
	rawAgent := NewAgent()
	rawAgent.setAgentClient(newRecordingAgentClient())
	require.Error(t, newSessionDelivery(rawAgent, "session").deliverRaw(
		context.Background(), map[string]any{"invalid": func() {}, "_meta": func() {}}, 1,
	))

	queuedRaw := newSessionDelivery(agent, "session")
	rawReceipt := make(chan error, 1)
	queuedRaw.raw <- rawDelivery{}
	queuedRaw.raw <- rawDelivery{done: rawReceipt}
	queuedRaw.failRawQueued(errors.New("stopped"))
	require.ErrorContains(t, <-rawReceipt, "stopped")

	typedDone, rawDone := make(chan struct{}), make(chan struct{})
	interruptAgent := NewAgent()
	interruptAgent.setAgentClient(&coverageInterruptClient{
		recordingAgentClient: newRecordingAgentClient(), typed: typedDone, raw: rawDone,
	})
	alreadyClosed := newSessionDelivery(interruptAgent, "session")
	alreadyClosed.closed = true
	alreadyClosed.started = true
	alreadyClosed.typedDone = typedDone
	alreadyClosed.rawDone = rawDone
	alreadyClosed.close()
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

	original := newTurnNonceRead
	newTurnNonceRead = func([]byte) (int, error) { return 0, errors.New("nonce failed") }
	_, err := nativePromptMessageID()
	newTurnNonceRead = original
	require.ErrorContains(t, err, "create native prompt message id")

	transport := newConnectionTransport(io.Discard, strings.NewReader(""))
	transport.interrupt()
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

func TestCorrectionEstablishmentStateBranches(t *testing.T) {
	failure := errors.New("establishment failed")
	failed := &session{establishmentFailed: failure}
	require.ErrorIs(t, failed.establishCurrentIncarnation(context.Background(), false), failure)

	cancelledAttempt := make(chan struct{})
	waitCancelled := &session{establishing: true, establishmentAttempt: cancelledAttempt}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, waitCancelled.establishCurrentIncarnation(ctx, false), context.Canceled)

	failedAttempt := make(chan struct{})
	close(failedAttempt)
	waitFailed := &session{
		establishing: true, establishmentAttempt: failedAttempt, establishmentAttemptErr: failure,
		establishmentFailed: failure,
	}
	require.ErrorIs(t, waitFailed.establishCurrentIncarnation(context.Background(), false), failure)
	successfulAttempt := make(chan struct{})
	close(successfulAttempt)
	require.NoError(t, (&session{
		establishing: true, establishmentAttempt: successfulAttempt,
	}).establishCurrentIncarnation(context.Background(), false))

	established := &session{established: true}
	established.failEstablishment(errors.New("ignored"))
	require.Nil(t, established.establishmentFailed)

	ready := make(chan struct{})
	awaitReady := &session{establishmentWritten: true, establishmentReady: ready}
	readyCtx, readyCancel := context.WithCancel(context.Background())
	readyCancel()
	require.ErrorIs(t, awaitReady.requireEstablished(readyCtx), context.Canceled)

	agent := NewAgent()
	local := &localAgentConnection{agent: agent, hooks: newEstablishmentHooks(agent.log)}
	params := mustJSON(t, map[string]any{establishmentHookParam: "7"})
	local.queueEstablishment(context.Background(), acp.AgentMethodSessionNew, params,
		acp.NewSessionResponse{SessionId: "missing"})
	require.Empty(t, local.hooks.all)

	done := make(chan struct{})
	close(done)
	agent.runtimeRetirements[77] = &runtimeRetirement{generation: 77, done: done, err: errors.New("contained")}
	agent.containSharedRuntimeGeneration(77, "coverage")
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
	conflictCycle := &foregroundCycle{assistantID: "assistant-a"}
	current.cycle = conflictCycle
	current.lifecycleMu.Unlock()
	conflictCtx := context.WithValue(context.Background(), nativeEventObservationKey{},
		&nativeEventObservation{binding: binding, ownership: nativeEventCycle, cycle: conflictCycle})
	require.ErrorContains(t, current.applyNativeMessageInfo(conflictCtx, opencode.Event{
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "assistant-b", "sessionID": current.idmap.NativeSessionID, "role": roleAssistant,
		}}),
	}), "conflicting assistant")

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

func TestCorrectionAuxiliaryDeliveryAndAuthBranches(t *testing.T) {
	delivery := newSessionDelivery(nil, "raw")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go delivery.runRaw(ctx)
	delivery.raw <- rawDelivery{payload: map[string]any{"value": true}, done: done}
	require.Error(t, <-done)
	cancel()
	<-delivery.rawDone

	auth := &providerAuth{}
	authCtx, authCancel := context.WithCancel(context.Background())
	authCancel()
	auth.waitCompletion(authCtx, &authFlow{completionDone: make(chan struct{})})

	droppedRaw := newSessionDelivery(nil, "full-raw")
	droppedRaw.started = true
	for index := 0; index < cap(droppedRaw.raw); index++ {
		droppedRaw.raw <- rawDelivery{payload: map[string]any{"index": index}}
	}
	droppedRaw.enqueueRaw(context.Background(), map[string]any{"overflow": true})
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

func TestCorrectionCloseBoundaryRejectsLatchedTerminal(t *testing.T) {
	current, _, _ := lifecycleSession(t)
	current.stopPump()
	cycle := &foregroundCycle{
		id: "close-cycle", turnID: "close-turn", origin: lifecycle.CauseActivity,
		interrupted: true, blockers: map[string]struct{}{}, signal: make(chan struct{}),
	}
	close(cycle.signal)
	current.lifecycleMu.Lock()
	current.cycle = cycle
	current.lifecycleFailed = errors.New("latched lifecycle failure")
	current.lifecycleMu.Unlock()
	require.ErrorContains(t, current.closeBoundary(false), "latched lifecycle failure")
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

func TestCorrectionLifecycleDirectFailureBranches(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := &session{}
	current.stampIncarnationGeneration(client, 7)
	require.NotNil(t, current.incarnation)

	invalidCycle := &foregroundCycle{reserved: true}
	require.Error(t, current.acceptPromptCycle(context.Background(), invalidCycle, lifecycle.Submission{}))
	current.abandonPromptCycle(nil)

	stream := lifecycle.NewStream("stream", lifecycle.Negotiated{Versions: []int{1}})
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

	validStream := lifecycle.NewStream("open", lifecycle.Negotiated{Versions: []int{1}})
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

func TestCorrectionPromptDispatchAndEmissionFailureBranches(t *testing.T) {
	original := newTurnNonceRead
	newTurnNonceRead = func([]byte) (int, error) { return 0, errors.New("nonce failed") }
	current := &session{}
	_, commandErr := current.commandDispatch(context.Background(), acp.PromptRequest{
		Prompt: []acp.ContentBlock{acp.TextBlock("/command")},
	}, slashCommandPrompt{}, opencode.NativeCommand{Name: "command"})
	_, messageErr := current.messageDispatch(context.Background(), acp.PromptRequest{
		Prompt: []acp.ContentBlock{acp.TextBlock("message")},
	})
	newTurnNonceRead = original
	require.ErrorContains(t, commandErr, "native prompt message id")
	require.ErrorContains(t, messageErr, "native prompt message id")

	donePump := &sessionPump{done: make(chan struct{}), hold: make(chan chan error), released: make(chan struct{}, 1)}
	close(donePump.done)
	dispatchSession := &session{pump: donePump}
	acceptedCycle, turnCtx, completionResult, err := dispatchSession.dispatchAndAccept(
		context.Background(), "turn", lifecycle.Submission{}, nativeDispatch{},
	)
	require.Nil(t, completionResult)
	require.Nil(t, acceptedCycle)
	require.Equal(t, "turn", turnNonceFromContext(turnCtx))
	require.ErrorContains(t, err, "pump stopped")

	cycle := &foregroundCycle{dispatchProven: true, dispatchEvidence: make(chan struct{})}
	reserved := &session{cycle: cycle}
	completion, err := reserved.sendReservedNativeFrame(context.Background(), cycle, nativeDispatch{
		send: func(context.Context) error { return errors.New("completion failed") },
	})
	require.NoError(t, err)
	require.ErrorContains(t, <-completion, "completion failed")

	completion, err = reserved.sendReservedNativeFrame(context.Background(), cycle, nativeDispatch{
		send: func(context.Context) error { return nil }, reportsCompletion: true,
	})
	require.NoError(t, err)
	require.NoError(t, <-completion)

	cancelClient := newFakeOpenCodeClient()
	cancelSession := testSession(t, NewAgent(), cancelClient)
	cancelCycle := &foregroundCycle{signal: make(chan struct{})}
	cancelSession.lifecycleMu.Lock()
	cancelSession.cycle = cancelCycle
	cancelSession.lifecycleMu.Unlock()
	cancelClient.abortFunc = func(string) error {
		cancelSession.lifecycleMu.Lock()
		cancelCycle.lost = errors.New("lost after interrupt")
		cancelCycle.wake()
		cancelSession.lifecycleMu.Unlock()

		return nil
	}
	end := cancelSession.settleCancelledTurn(context.Background(), cancelCycle)
	require.ErrorContains(t, end.lost, "lost after interrupt")

	emitFailure := testSession(t, NewAgent(), newFakeOpenCodeClient())
	emitFailure.delivery.close()
	err = emitFailure.emitPartUpdates(context.Background(), roleAssistant, opencode.NativePart{
		ID: "part", MessageID: "assistant", Type: partTypeText, Text: "text",
	}, "", false)
	require.Error(t, err)

	emitFailure.emittedUsage = map[string]emittedUsageState{}
	err = emitFailure.emitUsageUpdate(context.Background(), "assistant", opencode.NativeTokens{Input: 1}, 10)
	require.Error(t, err)
}
