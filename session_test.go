package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"sync"

	"github.com/savid/acp-go-opencode/internal/opencode"

	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/stretchr/testify/require"
)

func TestTurnFenceHelperBranches(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	if !session.claimPermissionRequest("") || !session.claimQuestionRequest("") {
		t.Fatal("empty request ids should not be fenced")
	}
	session.processedPermission = nil
	session.processedQuestion = nil
	if !session.claimPermissionRequest("perm") || !session.claimQuestionRequest("question") {
		t.Fatal("nil processed request maps were not initialized")
	}
	session.markActiveMessageID("")
	session.activeMessageIDs = nil
	session.markActiveMessageID("message-1")
	session.failedMessageIDs = nil
	session.failedStreamEpochs = nil
	session.markStreamFailed(9)
	if !session.shouldSuppressEvent(opencode.Event{StreamEpoch: 9}) {
		t.Fatal("failed stream epoch was not suppressed")
	}
	if !session.shouldSuppressEvent(opencode.Event{
		Properties: json.RawMessage(`{"sessionID":"native-1","messageID":"message-1","type":"text","text":"late"}`),
	}) {
		t.Fatal("failed message id was not suppressed")
	}
	if session.shouldSuppressEvent(opencode.Event{
		Properties: json.RawMessage(`{"sessionID":"native-1","messageID":"message-2","type":"text","text":"ok"}`),
	}) {
		t.Fatal("unfailed message id was suppressed")
	}
	if err := session.handleEvent(context.Background(), opencode.Event{
		StreamEpoch: 9,
		Properties:  json.RawMessage(`{"sessionID":"native-1","messageID":"message-1","type":"text","text":"late"}`),
	}); err != nil {
		t.Fatalf("suppressed handleEvent: %v", err)
	}
}

func TestSessionContextWindow(t *testing.T) {
	ctx := context.Background()

	t.Run("caches lookups per model", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 1000 {
			t.Fatalf("first lookup = %d, want 1000", got)
		}
		client.providersErr = errors.New("boom")
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 1000 {
			t.Fatalf("cached lookup = %d, want 1000 (must not re-fetch)", got)
		}
	})

	t.Run("provider error reports unknown", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.providersErr = errors.New("boom")
		session := testSession(NewAgent(), client)
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 0 {
			t.Fatalf("provider error lookup = %d, want 0", got)
		}
	})

	t.Run("nil client reports unknown", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		session.client = nil
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 0 {
			t.Fatalf("nil client lookup = %d, want 0", got)
		}
	})
}

func TestModelValueSplitAndJoin(t *testing.T) {
	if splitProvider, splitModel := splitModelValue("model-only", "p", "m"); splitProvider != "p" || splitModel != "model-only" {
		t.Fatalf("split fallback = %q %q", splitProvider, splitModel)
	}
	if joinModelValue("", "m") != "m" || joinModelValue("p", "") != "p" {
		t.Fatal("joinModelValue fallback mismatch")
	}
}
func TestSessionTurnAdmissionAndCancellationFailureShapes(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	current := testSession(agent, client)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := current.acquireTurn(cancelled)
	require.ErrorIs(t, err, context.Canceled)

	current.poisonCause = "broken"
	_, err = current.acquireTurn(context.Background())
	require.Error(t, err)
	current.poisonCause = ""
	current.cancelling = true
	_, err = current.acquireTurn(context.Background())
	require.ErrorContains(t, err, "session_cancelling")
	current.cancelling = false

	release, err := current.acquireCommandTurn(context.Background())
	require.NoError(t, err)
	_, err = current.acquireTurn(context.Background())
	require.Error(t, err)
	_, err = current.acquireCommandTurn(context.Background())
	require.Error(t, err)
	release()

	release, err = current.acquireTurn(context.Background())
	require.NoError(t, err)
	_, err = current.acquireTurn(context.Background())
	require.Error(t, err)
	release()
}

func TestAbortWaitIdleEveryResult(t *testing.T) {
	ctx := context.Background()
	require.Error(t, abortAndWaitIdle(ctx, nil, "native"))
	client := newFakeOpenCodeClient()
	require.Error(t, abortAndWaitIdle(ctx, client, ""))

	client.abortErr = errors.New("abort failed")
	require.ErrorContains(t, abortAndWaitIdle(ctx, client, "native"), "abort native session")
	client.abortErr = nil
	client.statusErr = errors.New("status failed")
	require.ErrorContains(t, abortAndWaitIdle(ctx, client, "native"), "read native session status")
	client.statusErr = nil
	client.statuses = map[string]opencode.NativeSessionStatus{"native": {Type: "mystery"}}
	require.ErrorContains(t, abortAndWaitIdle(ctx, client, "native"), "unknown native session status")
	client.statuses = map[string]opencode.NativeSessionStatus{"native": {Type: "idle"}}
	require.NoError(t, abortAndWaitIdle(ctx, client, "native"))
	client.statuses = map[string]opencode.NativeSessionStatus{}
	require.NoError(t, abortAndWaitIdle(ctx, client, "native"))

	client.statuses = map[string]opencode.NativeSessionStatus{"native": {Type: "busy"}}
	short, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	require.ErrorIs(t, abortAndWaitIdle(short, client, "native"), context.DeadlineExceeded)

	client.statuses = map[string]opencode.NativeSessionStatus{"native": {Type: "busy"}}
	go func() {
		time.Sleep(75 * time.Millisecond)
		client.mu.Lock()
		client.statuses["native"] = opencode.NativeSessionStatus{Type: "idle"}
		client.mu.Unlock()
	}()
	settles, settleCancel := context.WithTimeout(ctx, time.Second)
	defer settleCancel()
	require.NoError(t, abortAndWaitIdle(settles, client, "native"))
}

func TestSessionIdentityModeOwnershipAndCloseHelpers(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	native := opencode.NativeSession{ID: "native"}
	native.Model.ID = "fallback-model"
	current := newSession(agent, "session", "/repo", []string{"/other"}, native, client, sessionMeta{}, idmapRecord{})
	require.Equal(t, "OpenCode session", current.title)
	require.Equal(t, "fallback-model", current.modelID)
	require.Equal(t, "build", current.mode)
	require.Equal(t, "session", current.idmap.SessionID)
	require.Equal(t, SessionStoreFormat, current.idmap.Format)

	current.setMode("plan")
	require.Equal(t, "plan", current.currentMode())
	current.setModel("openai/gpt-test")
	require.Equal(t, "openai/gpt-test", current.currentModel())
	mode, model := current.commandContext()
	require.Equal(t, "plan", mode)
	require.Equal(t, "openai/gpt-test", model)
	require.Equal(t, acp.SessionId("session"), current.info().SessionId)

	current.markActiveToolCallID("")
	require.False(t, current.ownsCurrentToolCall(""))
	require.False(t, current.ownsCurrentToolCall("tool"))
	turnCtx, turnCancel := context.WithCancel(context.Background())
	current.mu.Lock()
	current.cancel = turnCancel
	current.activeToolCallIDs = nil
	current.mu.Unlock()
	current.markActiveToolCallID("tool")
	require.True(t, current.ownsCurrentToolCall("tool"))
	current.mu.Lock()
	current.cancelling = true
	current.mu.Unlock()
	require.False(t, current.ownsCurrentToolCall("tool"))
	turnCancel()
	<-turnCtx.Done()

	current.mu.Lock()
	current.cancel = nil
	current.cancelling = false
	current.mu.Unlock()
	selector, present, err := current.validatedModelSelector(context.Background(), "model")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "openai", selector.ProviderID)
	current.setModel("")
	_, present, err = current.validatedModelSelector(context.Background(), "model")
	require.NoError(t, err)
	require.False(t, present)

	released := false
	current.directoryRelease = func() { released = true }
	require.NoError(t, current.Close(context.Background()))
	require.True(t, released)
	require.NoError(t, current.Close(context.Background()))
	current.detachRuntime(0, "ignored after close")
}

func TestSessionCloseReleasesDirectoryOnlyAfterNativeMCPDisconnect(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.closeErr = errors.New("disconnect failed")
	current := testSession(NewAgent(), client)
	releases := 0
	current.directoryRelease = func() { releases++ }

	require.ErrorContains(t, current.Close(context.Background()), "disconnect failed")
	require.Zero(t, releases)
	require.NotNil(t, current.directoryRelease)

	client.closeErr = nil
	require.NoError(t, current.Close(context.Background()))
	require.Equal(t, 1, releases)
	require.Nil(t, current.directoryRelease)
	require.NoError(t, current.Close(context.Background()))
	require.Equal(t, 1, releases)
}

func TestSessionFailRuntimeAndDeleteNativeBranches(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	current := testSession(agent, client)
	released := false
	current.directoryRelease = func() { released = true }
	current.beginTurn(context.Background(), "nonce")
	current.detachRuntime(0, "runtime exited")
	require.True(t, released)
	require.NoError(t, current.ensureNotPoisoned())
	require.ErrorContains(t, current.runtimeFailure(), "runtime exited")

	client = newFakeOpenCodeClient()
	client.deleteErr = errors.New("delete failed")
	current = testSession(agent, client)
	require.NoError(t, current.DeleteNativeAndClose(context.Background()), "native deletion is best-effort")
	require.NotEmpty(t, client.deleted)

	require.Equal(t, "fallback", firstNonEmpty("", "fallback"))
	provider, model := splitModelValue("malformed", "provider", "fallback")
	require.Equal(t, "provider", provider)
	require.Equal(t, "malformed", model)
	provider, model = splitModelValue("/model", "provider", "fallback")
	require.Equal(t, "provider", provider)
	require.Equal(t, "/model", model)
	require.Equal(t, "model", joinModelValue("", "model"))
	require.Equal(t, "provider", joinModelValue("provider", ""))
}

func TestSessionCancellationEpochCoordinationRemainingBranches(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	current := testSession(agent, client)

	_, err := current.beginCancellation("missing", true, true)
	require.Error(t, err)
	require.NoError(t, current.resolveCancellation(context.Background(), 0))

	done := make(chan struct{})
	current.cancelling = true
	current.cancellationEpoch = 7
	current.cancellationDone = done
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, current.resolveCancellation(cancelled, 7), context.Canceled)
	originalObserve := observeCancellationWait
	waiting := make(chan struct{}, 1)
	var observeOnce sync.Once
	observeCancellationWait = func() { observeOnce.Do(func() { waiting <- struct{}{} }) }
	t.Cleanup(func() { observeCancellationWait = originalObserve })
	resolved := make(chan error, 1)
	go func() { resolved <- current.resolveCancellation(context.Background(), 7) }()
	<-waiting
	current.mu.Lock()
	current.cancelling = false
	current.mu.Unlock()
	close(done)
	require.NoError(t, <-resolved)

	current = testSession(agent, client)
	current.beginTurn(context.Background(), "nonce")
	client.statuses = map[string]opencode.NativeSessionStatus{"native-1": {Type: "idle"}}
	epoch, err := current.beginCancellation("nonce", true, true)
	require.NoError(t, err)
	require.NoError(t, current.resolveCancellation(context.Background(), epoch))
	epoch, err = current.beginCancellation("nonce", true, true)
	require.NoError(t, err)
	require.EqualValues(t, current.turnEpoch, epoch)

	current = testSession(agent, newFakeOpenCodeClient())
	current.beginTurn(context.Background(), "nonce")
	current.cancelling = true
	current.cancellationEpoch = current.turnEpoch
	epoch, err = current.beginCancellation("", false, true)
	require.NoError(t, err)
	require.EqualValues(t, current.turnEpoch, epoch)

	errorClient := newFakeOpenCodeClient()
	errorClient.abortErr = errors.New("abort failed")
	current = testSession(agent, errorClient)
	current.beginTurn(context.Background(), "nonce")
	epoch, err = current.beginCancellation("", false, false)
	require.NoError(t, err)
	current.mu.Lock()
	current.cancel = nil
	current.mu.Unlock()
	require.Error(t, current.resolveCancellation(context.Background(), epoch))
	require.Error(t, current.ensureNotPoisoned())
}

func TestDeleteNativeAndCloseFencesActiveTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.statuses = map[string]opencode.NativeSessionStatus{"native-1": {Type: "idle"}}
	current := testSession(NewAgent(), client)
	current.beginTurn(context.Background(), "nonce")
	require.NoError(t, current.DeleteNativeAndClose(context.Background()))
	require.NotEmpty(t, client.deleted)
}
