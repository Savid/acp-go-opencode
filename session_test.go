package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/lifecycle"
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
		t.Fatalf("suppressed applyNativeEvent: %v", err)
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
	current := newSession(agent, "session", "/repo", []string{"/other"}, native, client, sessionMeta{},
		idmapRecord{SessionID: "session", Format: SessionStoreFormat})
	require.Equal(t, "OpenCode session", current.title)
	require.Equal(t, "fallback-model", current.modelID)
	require.Equal(t, "build", current.mode)
	require.Equal(t, "native", current.idmap.NativeSessionID)

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

	// A session whose binding is already gone has no native session to delete,
	// so deletion is the containment boundary alone.
	current.mu.Lock()
	current.client = nil
	current.mu.Unlock()
	require.NoError(t, current.DeleteNativeAndClose(context.Background()))

	client = newFakeOpenCodeClient()
	client.deleteErr = errors.New("delete failed")
	current = testSession(t, agent, client)
	require.ErrorContains(t, current.DeleteNativeAndClose(context.Background()), "delete failed")
	require.NotEmpty(t, client.deleted)
	require.True(t, client.isClosed(), "a refused native deletion left the scope uncontained")

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

// TestInterruptNativeWorkNeedsABindingAndReportsRefusals proves the failure-path
// interrupt is bounded by the session's own binding: with no runtime bound it
// asks nobody, and a refused interrupt is recorded rather than swallowed.
func TestInterruptNativeWorkNeedsABindingAndReportsRefusals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)
	current.stopPump()

	client.abortErr = errors.New("harness refused the interrupt")
	current.interruptNativeWork(ctx)
	require.Equal(t, []string{"native-1"}, client.abortedSessions())

	current.mu.Lock()
	current.client = nil
	current.mu.Unlock()

	current.interruptNativeWork(ctx)
	require.Equal(t, []string{"native-1"}, client.abortedSessions(), "a session with no binding interrupted anyway")
}

// TestCloneAvailableCommandsKeepsTheAbsentCatalogAbsent proves an absent command
// catalog clones as absent: an empty catalog is a published fact, and inventing
// one would report a catalog this session never read.
func TestCloneAvailableCommandsKeepsTheAbsentCatalogAbsent(t *testing.T) {
	t.Parallel()

	require.Nil(t, cloneAvailableCommands(nil))
	require.Equal(t, []acp.AvailableCommand{}, cloneAvailableCommands([]acp.AvailableCommand{}))
}

// openTestCycle installs one foreground cycle on a session, optionally already
// holding its native terminal evidence.
func openTestCycle(current *session, terminal bool) *foregroundCycle {
	cycle := &foregroundCycle{id: "cycle-1", turnID: "turn-1", blockers: map[string]struct{}{}, signal: make(chan struct{})}
	if terminal {
		cycle.idle = true

		close(cycle.signal)
	}

	current.lifecycleMu.Lock()
	current.cycle = cycle
	current.lifecycleMu.Unlock()

	return cycle
}

// acceptTestTurn opens one prompt-origin turn on the session's own stream and
// hands it the native terminal evidence a settled turn holds, so a close can
// report it over without a live harness.
func acceptTestTurn(t *testing.T, current *session) *foregroundCycle {
	t.Helper()

	cycle, err := current.acceptPromptCycle(context.Background(),
		lifecycle.Submission{SubmissionID: "submission-1", ClientNonce: "nonce-1"})
	require.NoError(t, err)

	current.lifecycleMu.Lock()
	cycle.idle = true
	cycle.wake()
	current.lifecycleMu.Unlock()

	return cycle
}

// TestCloseStopsAtTheFirstUnprovenStep proves the close and delete boundary is a
// ladder: the interrupt and its native acknowledgement have to succeed before
// the session is contained, and a rung that fails stops the ladder there with
// the native scope untouched.
func TestCloseStopsAtTheFirstUnprovenStep(t *testing.T) {
	t.Parallel()

	t.Run("refused interrupt", func(t *testing.T) {
		t.Parallel()

		client := newFakeOpenCodeClient()
		client.abortErr = errors.New("harness refused the interrupt")
		current := testSession(t, NewAgent(), client)
		current.stopPump()
		openTestCycle(current, false)

		require.ErrorContains(t, current.Close(context.Background()), "harness refused the interrupt")
		require.False(t, client.isClosed(), "the scope was contained on an unproven interrupt")
	})

	t.Run("unacknowledged interrupt", func(t *testing.T) {
		t.Parallel()

		client := newFakeOpenCodeClient()
		current := testSession(t, NewAgent(), client)
		current.stopPump()
		cycle := openTestCycle(current, false)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		_, _, err := current.settleBeforeContainment(ctx, true)
		require.ErrorContains(t, err, "did not report idle after the interrupt")
		require.False(t, cycle.settled, "an unacknowledged cycle was reported settled")
		require.False(t, client.isClosed(), "the scope was contained on an unacknowledged interrupt")
	})
}

// TestCloseCommitsOnlyAfterTheContainmentProof proves the close ladder's order:
// the resumable snapshot is read while the loopback API can still answer, but it
// is written only once this session's native scope is proven contained, and the
// turn's terminal transition follows that durable write.
func TestCloseCommitsOnlyAfterTheContainmentProof(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := negotiatedAgent(t, WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptTestTurn(t, current)

	var before, after int

	store.onReplace = func(SessionKey) error {
		if client.isClosed() {
			after++
		} else {
			before++
		}

		return nil
	}

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.NoError(t, err)
	require.Zero(t, before, "the close boundary committed before it had proved containment")
	require.Positive(t, after, "the close boundary committed nothing after its containment proof")
	require.True(t, cycle.settled, "the contained session never reported its turn over")
	requireLifecycleOutcome(t, connection, lifecycle.OutcomeCancelled)
	require.NotContains(t, agent.sessions, current.id)
}

// TestCloseFencesTheStreamOnAnIncompleteContainment proves the failing branch of
// the same boundary: a containment that does not complete terminalizes nothing,
// commits nothing new, answers with the containment error, and still fences the
// stream, because the incarnation behind it is one this session can no longer
// speak for.
func TestCloseFencesTheStreamOnAnIncompleteContainment(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(errors.New("close failed"), opencode.ErrProcessContainmentIncomplete)
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := negotiatedAgent(t, WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptTestTurn(t, current)

	var committed bool

	store.onReplace = func(SessionKey) error {
		committed = true

		return nil
	}

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.False(t, committed, "an unproven containment committed a resumable snapshot anyway")
	require.False(t, cycle.settled, "an unproven containment terminalized the turn anyway")

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		require.NotEqual(t, "idle", transition["state"], "an unproven containment ended the turn on the stream")
	}

	require.ErrorContains(t, current.lifecycleFailure(), "session closed")
	require.Contains(t, agent.sessions, current.id, "the session was released without a containment proof")
}

// TestRecoveryWithoutACommittedGenerationRefusesThePrompt proves recovery is
// store-backed: with the loss already cleared by a concurrent recovery and no
// committed generation to restore, the prompt is refused rather than rebound to a
// native session this adapter cannot prove it owns.
func TestRecoveryWithoutACommittedGenerationRefusesThePrompt(t *testing.T) {
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := NewAgent(WithSessionStore(store))
	current := testSession(t, agent, newFakeOpenCodeClient())

	store.onLoad = func(SessionKey) ([]SessionStoreEntry, error) {
		// A concurrent recovery installs a fresh binding while this read is in
		// flight, so the loss that sent this prompt here is no longer recorded.
		current.mu.Lock()
		current.runtimeLostCause = ""
		current.mu.Unlock()

		return nil, nil
	}

	agent.mu.Lock()
	agent.runtimeGeneration++
	agent.mu.Unlock()

	require.ErrorContains(t, current.ensureRuntime(context.Background()), "opencode_recovery_generation_missing")
}

// TestCloseOnAFencedIncarnationEmitsNothingAndStillCommits proves the close
// ladder's stream rungs apply only to a live incarnation. A cancel that retired
// the native generation, an incarnation loss that left a turn unsettled, and a
// session that never opened an incarnation at all each close successfully with no
// lifecycle event on the dead stream, while the rungs that are not emissions —
// the containment proof and the durable commit — run exactly as they do on the
// live branch. A turn the loss ended is never restated as cancelled: the two
// paths do not share a terminal state, and rewriting one as the other would tell
// a host a contained end from a lost one.
func TestCloseOnAFencedIncarnationEmitsNothingAndStillCommits(t *testing.T) {
	t.Parallel()

	t.Run("incarnation lost with a turn still open", func(t *testing.T) {
		t.Parallel()

		client := newFakeOpenCodeClient()
		store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
		agent := negotiatedAgent(t, WithSessionStore(store))
		connection := newRecordingAgentClient()
		agent.setAgentClient(connection)
		current := testSession(t, agent, client)

		agent.mu.Lock()
		agent.sessions[current.id] = current
		agent.mu.Unlock()

		cycle := acceptTestTurn(t, current)
		current.fenceLifecycle("native generation retired")
		require.Error(t, cycle.lost, "the fence left the open cycle without its loss")

		fenced := len(connection.lifecycleEnvelopes(t))

		var committed int

		store.onReplace = func(SessionKey) error {
			committed++

			return nil
		}

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
		require.NoError(t, err, "a fenced incarnation blocked the containment boundary")
		require.True(t, client.isClosed(), "the fenced incarnation was never contained")
		require.Positive(t, committed, "the fenced branch skipped the durable commit")
		require.Len(t, connection.lifecycleEnvelopes(t), fenced, "the boundary emitted on a fenced stream")

		for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
			require.NotEqual(t, "idle", transition["state"],
				"close restated a turn the incarnation loss already ended")
		}
	})

	t.Run("never opened", func(t *testing.T) {
		t.Parallel()

		client := newFakeOpenCodeClient()
		store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
		agent := NewAgent(WithSessionStore(store))
		connection := newRecordingAgentClient()
		agent.setAgentClient(connection)
		current := testSession(t, agent, client)
		require.True(t, current.lifecycleStreamAbsent(), "the unnegotiated session opened an incarnation")

		agent.mu.Lock()
		agent.sessions[current.id] = current
		agent.mu.Unlock()

		cycle := acceptTestTurn(t, current)

		var committed int

		store.onReplace = func(SessionKey) error {
			committed++

			return nil
		}

		_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
		require.NoError(t, err)
		require.True(t, client.isClosed(), "a session with no incarnation was never contained")
		require.Positive(t, committed, "a session with no incarnation skipped the durable commit")
		require.True(t, cycle.settled, "the boundary left its cycle addressable")
		require.Empty(t, connection.lifecycleEnvelopes(t), "a session with no incarnation emitted an envelope")
	})
}

// TestCloseCommitsAGenerationCapturedBeforeAConcurrentFence proves the durable
// commit is not a stream rung. The generation is read while the loopback API can
// still answer, the incarnation is fenced underneath the boundary before the
// write, and the captured generation still lands: a fence decides what may be
// emitted, never what has already been proved worth keeping.
func TestCloseCommitsAGenerationCapturedBeforeAConcurrentFence(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := negotiatedAgent(t, WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptTestTurn(t, current)

	// Containment sits between the capture and the commit, so fencing there is
	// the narrowest way to land a fence on a generation already captured.
	client.closeHook = func() { current.fenceLifecycle("native generation retired mid-close") }

	var committed int

	store.onReplace = func(SessionKey) error {
		committed++

		return nil
	}

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.NoError(t, err)
	require.Positive(t, committed, "a concurrent fence discarded a generation already captured")
	require.True(t, cycle.settled, "the boundary left its cycle addressable")

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		require.NotEqual(t, "idle", transition["state"], "the boundary emitted on a stream fenced under it")
	}
}

// TestCloseFailsOnACaptureItCannotRead proves the capture rung is not optional.
// The state a reload restores from is read before containment, and a read the
// boundary cannot complete fails the close rather than contain a session whose
// durable generation this adapter could not prove.
func TestCloseFailsOnACaptureItCannotRead(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	client.syncHistoryErr = errors.New("sync history unavailable")
	agent := negotiatedAgent(t, WithSessionStore(NewInMemorySessionStore()))
	agent.setAgentClient(newRecordingAgentClient())
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	acceptTestTurn(t, current)

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorContains(t, err, "sync history unavailable")
	require.False(t, client.isClosed(), "an unreadable capture contained the session anyway")
	require.Contains(t, agent.sessions, current.id, "the session was released without a durable capture")
}
