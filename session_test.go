package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"

	"github.com/coder/acp-go-sdk"

	"github.com/stretchr/testify/require"
)

func TestTurnFenceHelperBranches(t *testing.T) {
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	require.True(t, session.actions.claim(&pendingAction{id: "perm"}))
	require.False(t, session.actions.claim(&pendingAction{id: "perm"}), "an id is claimed once for the life of the session")
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
	if err := session.applyNativeEvent(context.Background(), opencode.Event{
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
		session := testSession(t, NewAgent(), client)
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
		session := testSession(t, NewAgent(), client)
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 0 {
			t.Fatalf("provider error lookup = %d, want 0", got)
		}
	})

	t.Run("nil client reports unknown", func(t *testing.T) {
		session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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
	current := testSession(t, agent, client)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := current.acquireTurn(cancelled)
	require.ErrorIs(t, err, context.Canceled)

	current.poisonCause = "broken"
	_, err = current.acquireTurn(context.Background())
	require.Error(t, err)
	current.poisonCause = ""

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

	current.markPublishedToolCall("")
	require.False(t, current.publishedToolCall(""))
	require.False(t, current.publishedToolCall("tool"))
	current.mu.Lock()
	current.publishedToolCalls = nil
	current.mu.Unlock()
	current.markPublishedToolCall("tool")
	require.True(t, current.publishedToolCall("tool"), "a published call stays answerable across turns")

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
	current := testSession(t, NewAgent(), client)
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
	current := testSession(t, agent, client)
	released := false
	current.directoryRelease = func() { released = true }
	current.beginTurn(context.Background(), "nonce")
	current.detachRuntime(current.runtimeGeneration, "runtime exited")
	require.True(t, released)
	require.NoError(t, current.ensureNotPoisoned())
	require.ErrorContains(t, current.runtimeFailure(), "runtime exited")

	client = newFakeOpenCodeClient()
	client.deleteErr = errors.New("delete failed")
	current = testSession(t, agent, client)
	require.ErrorContains(t, current.DeleteNativeAndClose(context.Background()), "delete failed")
	require.NotEmpty(t, client.deleted)
	require.False(t, client.closed, "failed native deletion must remain retryable")

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

// TestCancelRequiresTheActiveTurnRoute proves a cancel that does not address the
// session's current turn is refused before any native interrupt.
func TestCancelRequiresTheActiveTurnRoute(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)

	require.Error(t, current.requireActiveTurn("missing"), "a cancel with no active turn was admitted")

	current.beginTurn(context.Background(), "nonce")
	require.Error(t, current.requireActiveTurn("stale"))
	require.NoError(t, current.requireActiveTurn("nonce"))
	current.finishTurn()

	require.Zero(t, client.abortCount(), "a refused cancel reached the harness")
}

// TestCancelTurnInterruptsOnlyTheAddressedNativeSession proves the interrupt names
// this session's native id and reports a refusal from the harness as a failure the
// open cycle escalates on.
func TestCancelTurnInterruptsOnlyTheAddressedNativeSession(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)
	current.beginTurn(context.Background(), "nonce")

	require.NoError(t, current.cancelTurn(context.Background()))
	require.Equal(t, []string{current.idmap.NativeSessionID}, client.abortedSessions())
	require.True(t, current.wasCancelled())

	// The turn is interrupted once: repeating the cancel touches nothing.
	require.NoError(t, current.cancelTurn(context.Background()))
	require.Len(t, client.abortedSessions(), 1)
	current.finishTurn()

	// A refused interrupt is reported to the caller on the next turn's cancel.
	client.abortErr = errors.New("interrupt refused")
	current.beginTurn(context.Background(), "nonce-2")
	require.ErrorContains(t, current.cancelTurn(context.Background()), "interrupt refused")
	current.finishTurn()
}

// TestAwaitNativeSettlementReportsTheMissingAcknowledgement proves a native session
// that never reports idle after an interrupt is a settlement failure rather than a
// clean cancellation.
func TestAwaitNativeSettlementReportsTheMissingAcknowledgement(t *testing.T) {
	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	require.NoError(t, current.awaitNativeSettlement(context.Background(), nil))

	cycle := &foregroundCycle{id: "cycle-1", turnID: "turn-1", signal: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorContains(t, current.awaitNativeSettlement(ctx, cycle), "did not report idle")

	cycle.wake()
	require.NoError(t, current.awaitNativeSettlement(context.Background(), cycle))
}

func TestDeleteNativeAndCloseSettlesTheSession(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)
	current.beginTurn(context.Background(), "nonce")
	require.NoError(t, current.DeleteNativeAndClose(context.Background()))
	require.NotEmpty(t, client.deleted)
}
