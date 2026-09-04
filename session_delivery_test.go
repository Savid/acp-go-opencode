package opencodeacp

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

type signalingWriteCloser struct {
	io.WriteCloser
	started chan struct{}
	once    sync.Once
}

func (w *signalingWriteCloser) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })

	return w.WriteCloser.Write(data)
}

type panickingUpdateClient struct{ *recordingAgentClient }

func (c *panickingUpdateClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	panic("SECRET_TYPED_WRITER_PANIC")
}

type panickingRawClient struct{ *recordingAgentClient }

func (c *panickingRawClient) NotifyExtension(context.Context, string, any) error {
	panic("SECRET_RAW_WRITER_PANIC")
}

func TestRawWriterStallNeverOccupiesAuthoritativeDelivery(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	connection := newRecordingAgentClient()
	connection.notifyStarted = make(chan struct{})
	connection.notifyRelease = make(chan struct{})
	agent := NewAgent()
	agent.setAgentClient(connection)
	delivery := newSessionDelivery(agent, "session-1")

	rawDone := make(chan error, 1)
	go func() {
		rawDone <- delivery.enqueueRaw(context.Background(), map[string]any{
			jsonFieldSessionID: acp.SessionId("session-1"),
			jsonFieldSource:    rawEventSource,
			jsonFieldEvent:     map[string]any{"type": "diagnostic"},
		})
	}()
	<-connection.notifyStarted

	receipt, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("typed"),
	})
	require.NoError(t, err)
	require.NoError(t, waitDelivery(context.Background(), receipt))

	delivery.close()
	require.Error(t, <-rawDone)
	connection.mu.Lock()
	require.Len(t, connection.updates, 1)
	connection.mu.Unlock()
}

func TestAuthoritativeWriterCloseInterruptsAndJoins(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	connection := newRecordingAgentClient()
	connection.updateStarted = make(chan struct{})
	connection.updateRelease = make(chan struct{})
	agent := NewAgent()
	agent.setAgentClient(connection)
	delivery := newSessionDelivery(agent, "session-1")

	receipt, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("typed"),
	})
	require.NoError(t, err)
	<-connection.updateStarted

	closed := make(chan struct{})
	go func() {
		delivery.close()
		close(closed)
	}()

	require.Error(t, waitDelivery(context.Background(), receipt))
	<-closed
}

// TestADrainedQueueReportsNothingHandedToTheTransport proves the lane tells its
// callers which of two very different failures they suffered. The notification
// the worker was sending when its context ended may have reached the client, and
// the one still queued behind it provably did not: nothing ever handed it over.
// A caller that treats the second as sent would report a fact to nobody.
func TestADrainedQueueReportsNothingHandedToTheTransport(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	connection := newRecordingAgentClient()
	connection.updateStarted = make(chan struct{})
	connection.updateRelease = make(chan struct{})
	agent := NewAgent()
	agent.setAgentClient(connection)
	delivery := newSessionDelivery(agent, "session-1")

	sending, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("sending"),
	})
	require.NoError(t, err)
	<-connection.updateStarted

	queued, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("queued"),
	})
	require.NoError(t, err)
	requireEventually(t, func() bool { return len(delivery.typed) == 1 },
		"the second notification never reached the queue")

	delivery.mu.Lock()
	cancel := delivery.cancel
	delivery.mu.Unlock()
	cancel()

	inFlight := waitDeliveryOutcome(context.Background(), sending)
	require.Error(t, inFlight.err)
	require.True(t, inFlight.handed,
		"a notification already in the send was reported as never sent")

	drained := waitDeliveryOutcome(context.Background(), queued)
	require.Error(t, drained.err)
	require.False(t, drained.handed,
		"a notification the stopping worker drained was reported as sent")

	connection.mu.Lock()
	require.Len(t, connection.updates, 1, "the drained notification reached the transport")
	connection.mu.Unlock()

	delivery.close()
}

// TestTheStoppingDrainReportsNothingHandedOver covers the drain the worker runs
// when its own context ends before it dequeues anything at all. Both of the
// lane's stopping triggers empty the queue through this one path, and neither
// hands a byte to the transport on the way.
func TestTheStoppingDrainReportsNothingHandedOver(t *testing.T) {
	agent := NewAgent()
	agent.setAgentClient(newRecordingAgentClient())

	delivery := newSessionDelivery(agent, "session-1")
	// No worker runs, so nothing can dequeue what this enqueues.
	delivery.started = true

	receipt, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("never sent"),
	})
	require.NoError(t, err)

	delivery.failQueued(context.Canceled)

	drained := waitDeliveryOutcome(context.Background(), receipt)
	require.ErrorIs(t, drained.err, context.Canceled)
	require.False(t, drained.handed,
		"a notification drained by the stopping worker was reported as sent")
}

func TestRealBlockedSDKWriterIsInterruptedAndJoinedWithoutReaderRelease(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	outputReader, outputWriter := io.Pipe()
	inputReader, inputWriter := io.Pipe()
	signalingOutput := &signalingWriteCloser{WriteCloser: outputWriter, started: make(chan struct{})}
	agent := NewAgent()
	connection := newLocalAgentConnection(agent, signalingOutput, inputReader)
	agent.setAgentClient(connection)
	delivery := newSessionDelivery(agent, "session-1")

	receipt, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("typed"),
	})
	require.NoError(t, err)
	requireSignal(t, signalingOutput.started)

	closed := make(chan struct{})
	go func() {
		delivery.close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery close did not interrupt the real blocked SDK writer")
	}
	require.Error(t, waitDelivery(context.Background(), receipt))

	_ = inputWriter.Close()
	_ = outputReader.Close()
	select {
	case <-connection.Done():
	case <-time.After(time.Second):
		t.Fatal("SDK connection worker did not join after transport interruption")
	}
}

func TestTypedWriterAndCommitPanicsFailReceiptsQueueAndFence(t *testing.T) {
	t.Run("writer", func(t *testing.T) {
		current, _, connection := lifecycleSession(t)
		current.agent.setAgentClient(&panickingUpdateClient{recordingAgentClient: connection})

		receipt, err := current.delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
			SessionId: current.id, Update: acp.UpdateAgentMessageText("typed"),
		})
		require.NoError(t, err)
		require.ErrorIs(t, waitDelivery(context.Background(), receipt), errAuthoritativeDeliveryPanic)
		requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "typed panic did not fence the incarnation")
	})

	t.Run("commit", func(t *testing.T) {
		current, _, _ := lifecycleSession(t)
		commitStarted := make(chan struct{})
		releaseCommit := make(chan struct{})
		first, err := current.delivery.enqueueUpdateCommitted(context.Background(), acp.SessionNotification{
			SessionId: current.id, Update: acp.UpdateAgentMessageText("first"),
		}, func() {
			close(commitStarted)
			<-releaseCommit
			panic("SECRET_COMMIT_PANIC")
		})
		require.NoError(t, err)
		requireSignal(t, commitStarted)

		second, err := current.delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
			SessionId: current.id, Update: acp.UpdateAgentMessageText("second"),
		})
		require.NoError(t, err)
		close(releaseCommit)

		require.ErrorIs(t, waitDelivery(context.Background(), first), errAuthoritativeDeliveryPanic)
		require.ErrorIs(t, waitDelivery(context.Background(), second), errAuthoritativeDeliveryPanic)
		requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "commit panic did not fence the incarnation")
	})

	t.Run("failure callback", func(t *testing.T) {
		current, _, connection := lifecycleSession(t)
		connection.mu.Lock()
		connection.updateErr = errors.New("wire failed")
		connection.mu.Unlock()

		receipt, err := current.delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
			SessionId: current.id, Update: acp.UpdateAgentMessageText("typed"),
		}, func(error) { panic("SECRET_FAILURE_CALLBACK_PANIC") })
		require.NoError(t, err)
		require.Error(t, waitDelivery(context.Background(), receipt))
		requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "failure callback panic did not fence the incarnation")
	})
}

func TestRawWriterPanicIsObservedWithoutFencing(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	current.agent.setAgentClient(&panickingRawClient{recordingAgentClient: connection})
	require.Error(t, current.delivery.enqueueRaw(context.Background(), map[string]any{
		jsonFieldSessionID: current.id,
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"type": "diagnostic"},
	}))
	require.NoError(t, current.delivery.flushRaw(context.Background()))
	require.NoError(t, current.lifecycleFailure())

	closed := make(chan struct{})
	go func() {
		current.delivery.close()
		close(closed)
	}()
	requireSignal(t, closed)
}

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

func TestCorrectionDeliveryDefensiveBranches(t *testing.T) {
	agent := NewAgent()
	delivery := newSessionDelivery(agent, "missing")
	delivery.terminalizePanic("ignored")
	(*sessionDelivery)(nil).terminalizePanic("ignored")

	full := newSessionDelivery(agent, "session")
	full.started = true
	for range authoritativeDeliveryCapacity {
		full.typed <- authoritativeDelivery{done: make(chan deliveryOutcome, 1)}
	}
	_, err := full.enqueueUpdate(context.Background(), acp.SessionNotification{})
	require.ErrorContains(t, err, "queue is full")
	for range authoritativeDeliveryCapacity {
		<-full.typed
	}

	closed := newSessionDelivery(agent, "session")
	closed.closed = true
	require.ErrorContains(t, closed.enqueueRaw(context.Background(), map[string]any{"type": "ignored"}), "closed")
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

func TestCorrectionAuxiliaryDeliveryAndAuthBranches(t *testing.T) {
	delivery := newSessionDelivery(nil, "raw")
	ctx, cancel := context.WithCancel(context.Background())
	go delivery.runRaw(ctx)
	require.Error(t, delivery.enqueueRaw(context.Background(), map[string]any{"value": true}))
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
	cancelled, cancelQueue := context.WithCancel(context.Background())
	cancelQueue()
	require.ErrorIs(t, droppedRaw.enqueueRaw(cancelled, map[string]any{"overflow": true}), context.Canceled)
}
func TestDeliveryCancellationEdges(t *testing.T) {
	stoppedBeforeSend := newSessionDelivery(nil, "raw-before")
	stoppedBeforeSend.started = true
	stoppedBeforeSend.raw = make(chan rawDelivery)
	close(stoppedBeforeSend.rawDone)
	require.ErrorContains(t, stoppedBeforeSend.enqueueRaw(t.Context(), map[string]any{}), "stopped")

	stoppedAfterSend := newSessionDelivery(nil, "raw-after")
	stoppedAfterSend.started = true
	received := make(chan struct{})
	go func() {
		<-stoppedAfterSend.raw
		close(stoppedAfterSend.rawDone)
		close(received)
	}()
	require.ErrorContains(t, stoppedAfterSend.enqueueRaw(t.Context(), map[string]any{}), "stopped")
	<-received
}
