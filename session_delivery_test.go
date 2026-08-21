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

	delivery.enqueueRaw(context.Background(), map[string]any{
		jsonFieldSessionID: acp.SessionId("session-1"),
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"type": "diagnostic"},
	})
	<-connection.notifyStarted

	receipt, err := delivery.enqueueUpdate(context.Background(), acp.SessionNotification{
		SessionId: "session-1", Update: acp.UpdateAgentMessageText("typed"),
	})
	require.NoError(t, err)
	require.NoError(t, waitDelivery(context.Background(), receipt))

	delivery.close()
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

func TestRawWriterPanicFencesWithoutBlockingClose(t *testing.T) {
	current, _, connection := lifecycleSession(t)
	current.agent.setAgentClient(&panickingRawClient{recordingAgentClient: connection})
	current.delivery.enqueueRaw(context.Background(), map[string]any{
		jsonFieldSessionID: current.id,
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     map[string]any{"type": "diagnostic"},
	})
	require.NoError(t, current.delivery.flushRaw(context.Background()))
	requireEventually(t, func() bool { return current.lifecycleFailure() != nil }, "raw panic did not fence the incarnation")

	closed := make(chan struct{})
	go func() {
		current.delivery.close()
		close(closed)
	}()
	requireSignal(t, closed)
}
