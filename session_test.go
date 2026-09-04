package opencodeacp

import (
	"context"
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
	registry := testIncarnation(session).registry
	claimed, err := registry.claim(&pendingAction{id: "perm"})
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = registry.claim(&pendingAction{id: "perm"})
	require.ErrorContains(t, err, "native action id was reused")
	require.False(t, claimed, "a reused id was claimed twice")
	session.markActiveMessageID("")
	session.activeMessageIDs = nil
	session.markActiveMessageID("message-1")
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
	current := newSession(agent, "session", absTestPath("repo"), []string{"/other"}, native, client, sessionMeta{},
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

	selector, present := current.modelSelector()
	require.True(t, present)
	require.Equal(t, "openai", selector.ProviderID)
	current.setModel("")
	_, present = current.modelSelector()
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
	assertTurnFailed(t, current.runtimeFailure(), causeTransport, "")
	require.NotContains(t, current.runtimeFailure().Error(), "runtime exited")

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

// acceptOpenTestTurn opens one prompt-origin turn on the session's own stream and
// leaves it running: the turn the stream introduced holds no native terminal
// evidence, which is the state a boundary has to obtain evidence for.
func acceptOpenTestTurn(t *testing.T, current *session) *foregroundCycle {
	t.Helper()

	ctx := withTurnRoute(context.Background(), "nonce-1")
	submission := lifecycle.Submission{SubmissionID: "submission-1", ClientNonce: "nonce-1"}
	cycle, _, err := current.reservePromptCycle(ctx, "native-message-1", submission)
	require.NoError(t, err)
	err = current.acceptPromptCycle(ctx, cycle, submission)
	require.NoError(t, err)

	return cycle
}

// acceptTestTurn opens one prompt-origin turn on the session's own stream and
// hands it the native terminal evidence a settled turn holds, so a close can
// report it over without a live harness.
func acceptTestTurn(t *testing.T, current *session) *foregroundCycle {
	t.Helper()

	cycle := acceptOpenTestTurn(t, current)

	current.lifecycleMu.Lock()
	cycle.idle = true
	cycle.wake()
	current.lifecycleMu.Unlock()

	return cycle
}

// lifecycleFenced reports whether this session has no live incarnation capable
// of emitting: either its stream is fenced or the exact failed binding has
// already been unpublished. On its own that says nothing about what the native
// boundary proved.
func lifecycleFenced(current *session) bool {
	current.lifecycleMu.Lock()
	defer current.lifecycleMu.Unlock()

	return current.incarnation == nil || current.incarnationFencedLocked()
}

// lifecycleFailure reports the latched stream failure, if any. A latched stream
// can carry nothing further: the sequence it consumed is spent, and a later
// delivery would hide that gap behind an apparently contiguous stream. Nothing
// in production asks — the latch is read where it is written, under the lock —
// so the accessor belongs to the cases that assert on it.
func (s *session) lifecycleFailure() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.lifecycleFailed
}

// lifecycleStreamID reports the incarnation this session's stream speaks for,
// which is how a case proves a recovered incarnation opened a stream of its own
// rather than inheriting the fenced one's identity.
func (s *session) lifecycleStreamID() string {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.incarnation == nil || s.incarnation.stream == nil {
		return ""
	}

	return s.incarnation.stream.ID()
}

// testRuntimeGeneration reports the runtime binding this session holds, which the
// production loss ladder addresses by generation.
func testRuntimeGeneration(current *session) uint64 {
	current.mu.Lock()
	defer current.mu.Unlock()

	return current.runtimeGeneration
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

// TestAnIncompleteContainmentTerminalizesNothingAndStillFences proves the failing
// branch of the same boundary: a containment that does not complete terminalizes
// nothing, commits nothing new, emits no quiescence fact, and answers with the
// containment error — and it still fences, because the fence is where this
// incarnation stopped speaking rather than a claim about what the boundary
// proved. The session stays addressable, and nothing the fence leaves behind is
// readable as evidence: the stream is not latched, and the settlement proof the
// harness refused is still owed.
func TestAnIncompleteContainmentTerminalizesNothingAndStillFences(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(errors.New("close failed"), ErrContainmentIncomplete)
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
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.False(t, committed, "an unproven containment committed a resumable snapshot anyway")
	require.False(t, cycle.settled, "an unproven containment terminalized the turn anyway")

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		require.NotEqual(t, "idle", transition["state"], "an unproven containment ended the turn on the stream")
	}

	require.True(t, lifecycleFenced(current), "the boundary left the incarnation speaking after it had stopped")
	require.NoError(t, current.lifecycleFailure(), "a failed boundary latched the stream it said nothing on")
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

// TestCloseOnAFencedIncarnationEmitsNothingAndKeepsItsDurableRecord proves the
// close ladder's stream rungs apply only to a live incarnation. An incarnation
// loss that left a turn unsettled and a session that never opened an incarnation
// at all each close successfully with no lifecycle event on the dead stream, while
// the rungs that are not emissions run to whatever they can prove: the containment
// proof always, and the durable record either committed fresh or retained intact.
// A turn the loss ended is never restated as cancelled: the two paths do not share
// a terminal state, and rewriting one as the other would tell a host a contained
// end from a lost one.
func TestCloseOnAFencedIncarnationEmitsNothingAndKeepsItsDurableRecord(t *testing.T) {
	t.Parallel()

	// The loss branch runs the production ladder: detachRuntime records the loss
	// before it fences, and a recorded loss is exactly what tells the capture the
	// interrupted native generation is no longer an online snapshot source. So the
	// durable guarantee here is retention, not a fresh commit — the last committed
	// generation survives the boundary unrewritten.
	t.Run("incarnation lost with a turn still open", func(t *testing.T) {
		t.Parallel()

		ctx := context.Background()
		client := newFakeOpenCodeClient()
		store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
		agent := negotiatedAgent(t, WithSessionStore(store))
		connection := newRecordingAgentClient()
		agent.setAgentClient(connection)
		current := testSession(t, agent, client)

		agent.mu.Lock()
		agent.sessions[current.id] = current
		agent.mu.Unlock()

		cycle := acceptOpenTestTurn(t, current)

		key := SessionKey{SessionID: string(current.id)}
		require.NoError(t, current.snapshotToStore(ctx))

		committed, err := store.Load(ctx, key)
		require.NoError(t, err)
		require.NotEmpty(t, committed, "the session committed no generation for the boundary to retain")

		current.detachRuntime(testRuntimeGeneration(current), "shared OpenCode runtime exited")
		require.Error(t, cycle.lost, "the loss left the open cycle without its terminal evidence")
		require.True(t, lifecycleFenced(current), "the loss left the incarnation unfenced")

		fenced := len(connection.lifecycleEnvelopes(t))

		var wrote int

		store.onReplace = func(SessionKey) error {
			wrote++

			return nil
		}

		_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: current.id})
		require.NoError(t, err, "a fenced incarnation blocked the containment boundary")
		require.NotContains(t, agent.sessions, current.id, "the boundary never released the session")
		require.True(t, client.isClosed(), "the fenced incarnation's scope was never released")
		require.Len(t, connection.lifecycleEnvelopes(t), fenced, "the boundary emitted on a fenced stream")

		retained, err := store.Load(ctx, key)
		require.NoError(t, err)
		require.Equal(t, committed, retained, "the boundary lost the last committed generation")
		require.Zero(t, wrote, "the boundary wrote over a generation it could no longer read")

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

// TestCloseAfterALatchedLiveStreamContainsAndRemovesSession proves an
// authoritative delivery failure immediately retires the exact incarnation.
// A later close therefore contains the already-lost cycle without attempting to
// write another transition on the gapped stream, closes the native scope, and
// removes the session.
func TestCloseAfterALatchedLiveStreamContainsAndRemovesSession(t *testing.T) {
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

	// One update the wire dropped, on an incarnation nothing fenced.
	connection.mu.Lock()
	connection.updateErr = errors.New("wire down")
	connection.mu.Unlock()

	require.ErrorContains(t, current.emitLifecycle(context.Background(), lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, cycle.id, cycle.turnID, lifecycle.CauseSubmission,
	)), "authoritative session update delivery failed")

	// Delivery is restored: the connection is healthy again and the gap is not.
	connection.mu.Lock()
	connection.updateErr = nil
	connection.mu.Unlock()

	require.ErrorContains(t, current.lifecycleFailure(), "lifecycle delivery failed")
	require.NotContains(t, current.lifecycleFailure().Error(), "wire down")
	require.True(t, lifecycleFenced(current), "the delivery failure left the exact incarnation published")

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.NoError(t, err)
	require.True(t, client.isClosed(), "the latched stream stopped the containment proof")
	require.NotContains(t, agent.sessions, current.id, "the completed boundary kept the session addressable")
}

// TestCloseRefusesToSettleOnAnInterruptTheHarnessRefused proves a refused native
// interrupt is not the incarnation loss the close boundary accepts as terminal
// evidence. The harness is still there and declined to stop, so nothing fenced
// the incarnation and the work this boundary asked it to put down was never proved
// stopped. The ladder stops at that rung exactly as it does for a cycle that never
// reported at all: the scope stays untouched and no idle or cancelled end is
// reported over native work that may still be running.
func TestCloseRefusesToSettleOnAnInterruptTheHarnessRefused(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	client.abortErr = errors.New("harness refused the interrupt")
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := negotiatedAgent(t, WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptOpenTestTurn(t, current)

	// A host cancel the harness refuses escalates the open cycle — it is not
	// going to report the idle the cancellation needs — and fences nothing. The
	// notification carries no response, so the refusal reaches no host either.
	require.ErrorContains(t, current.cancelTurn(context.Background()), "harness refused the interrupt")
	require.Error(t, cycle.lost, "the refusal left the cycle waiting on evidence that cannot arrive")
	require.False(t, lifecycleFenced(current), "a refused interrupt fenced the incarnation")

	var committed int

	store.onReplace = func(SessionKey) error {
		committed++

		return nil
	}

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorContains(t, err, "harness refused the interrupt")
	require.False(t, client.isClosed(), "the scope was contained over work never proved stopped")
	require.Zero(t, committed, "an unproven stop committed a generation anyway")
	require.False(t, cycle.settled, "an unproven stop terminalized the turn anyway")
	require.Contains(t, agent.sessions, current.id, "the session was released on an unproven boundary")

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		require.NotEqual(t, "idle", transition["state"],
			"close reported the turn over on work it never proved stopped")
	}
}

// TestDeleteSettlesOnTheSameEvidenceCloseDoes proves the delete boundary waits on
// the settlement the close boundary waits on, because it is that boundary with a
// deletion in front of it. The tombstone is durable before anything is torn down,
// so a fenced incarnation — evidence close accepts as terminal — must not be the
// thing that makes delete report failure for a session it has already answered
// for.
func TestDeleteSettlesOnTheSameEvidenceCloseDoes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := newFakeOpenCodeClient()
	store := NewInMemorySessionStore()
	agent := negotiatedAgent(t, WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptOpenTestTurn(t, current)

	current.detachRuntime(testRuntimeGeneration(current), "shared OpenCode runtime exited")
	require.Error(t, cycle.lost, "the loss left the open cycle without its terminal evidence")
	require.True(t, lifecycleFenced(current), "the loss left the incarnation unfenced")

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(current.id))
	require.NoError(t, err, "delete failed on the loss close accepts, after its tombstone already landed")
	require.True(t, agent.isDeleted(current.id))
	require.NotContains(t, agent.sessions, current.id)

	stored, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, stored, "the deleted session is listable again")
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

// TestCloseRetriesTheWholeBoundaryAfterARefusedDurableRung proves the rung order
// of the close boundary and the retry contract of one that failed. Terminalization
// comes first: the still-open turn is told this boundary ended it, and only then is
// the generation a reload restores written. A store that refuses that write fails
// the close with the stream fenced behind a terminal transition the host has
// already seen — the durable rung is what failed, not the transition that preceded
// it.
//
// The failed boundary proved nothing durable and released nothing, so the session
// stays addressable and the next close re-runs every rung the failed one owed —
// including the commit, against a store that has healed. A retry that answered
// success over a generation nobody wrote would report a boundary no one completed.
//
// The retry runs across the fence the failed boundary left, which costs it the
// emission rung and nothing else: the terminal transition is not restated on a
// fenced stream, the retained capture makes the durable rung runnable without the
// loopback API the containment took away, and the retry answers silently because
// the first close already reported the failure to its caller.
func TestCloseRetriesTheWholeBoundaryAfterARefusedDurableRung(t *testing.T) {
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

	var commits int

	store.onReplace = func(SessionKey) error {
		commits++

		return errors.New("store refused the resumable commit")
	}

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorContains(t, err, "store refused the resumable commit")
	require.Equal(t, 1, commits)
	require.True(t, cycle.settled, "the boundary refused the commit without first ending the turn it was closing")
	require.True(t, lifecycleFenced(current), "the failed boundary left the incarnation speaking after it had stopped")
	require.Contains(t, agent.sessions, current.id, "a failed boundary released the session")

	terminal := endingIdleTransitions(t, connection)
	require.Len(t, terminal, 1, "the refused commit came before the terminal transition the host is owed")
	require.Equal(t, string(lifecycle.OutcomeCancelled), terminal[0]["outcome"])

	entries, loadErr := store.Load(context.Background(), SessionKey{SessionID: string(current.id)})
	require.NoError(t, loadErr)
	require.Empty(t, entries, "a refused commit landed a generation anyway")

	store.onReplace = func(SessionKey) error {
		commits++

		return nil
	}

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.NoError(t, err)
	require.Equal(t, 2, commits, "the retry answered success without re-running the durable rung")
	require.NotContains(t, agent.sessions, current.id, "the completed boundary kept the session addressable")

	entries, loadErr = store.Load(context.Background(), SessionKey{SessionID: string(current.id)})
	require.NoError(t, loadErr)
	require.NotEmpty(t, entries, "the retry reported success with no durable commit")

	require.Len(t, endingIdleTransitions(t, connection), 1,
		"the retry emitted a terminal transition on a stream its predecessor had already fenced")
}

// endingIdleTransitions reports every terminal foreground transition the stream
// carried, in the order the host received them.
func endingIdleTransitions(t *testing.T, connection *recordingAgentClient) []map[string]any {
	t.Helper()

	ending := make([]map[string]any, 0)

	for _, transition := range connection.lifecycleEventsOfType(t, "state_update") {
		if transition["state"] == "idle" {
			ending = append(ending, transition)
		}
	}

	return ending
}

// TestAFailedCloseLaundersNoUnprovenStop proves the fence is never evidence. A
// harness that refuses the interrupt leaves the stop unproved, so the close fails
// classified against the containment sentinel and still fences on its way out —
// and that fence answers for nothing. Settlement is judged from the cycle's own
// loss, which a refused interrupt is not, so the retry and the delete behind it
// both fail on the same unproved stop and nothing is deleted natively over work
// still running.
//
// The mutation this pins: read fence presence as terminal evidence instead of the
// cycle's loss and the second close reports success over a harness that never
// stopped.
func TestAFailedCloseLaundersNoUnprovenStop(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	client.abortErr = errors.New("harness refused the interrupt")
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := negotiatedAgent(t, WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptTestTurn(t, current)

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorIs(t, err, errRuntimeConfigurationIncomplete)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.ErrorContains(t, err, "harness refused the interrupt")
	require.True(t, lifecycleFenced(current), "the failed close left the incarnation speaking after it had stopped")
	require.NoError(t, current.lifecycleFailure(), "the failed close latched a stream it said nothing on")
	require.False(t, client.isClosed(), "an unproved stop contained the scope anyway")

	// The very same unproved stop is presented to the boundary again, now with
	// this close's fence standing. Nothing it left behind may answer for it.
	require.Error(t, current.awaitCloseSettlement(context.Background(), cycle),
		"the failed close turned a refused interrupt into accepted terminal evidence")

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorIs(t, err, errRuntimeConfigurationIncomplete)

	_, err = agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(current.id))
	require.ErrorIs(t, err, errRuntimeConfigurationIncomplete)
	require.Empty(t, client.deleted, "delete removed native state over a stop nobody proved")
}

// TestDeleteOnAFencedIncarnationSettlesBothBoundaries proves the real fenced path
// still settles. An incarnation the runtime lost fenced its own stream and woke
// its cycle with that loss, which is terminal evidence rather than an unproved
// stop: the delete boundary reads it, contains what is left, and answers.
func TestDeleteOnAFencedIncarnationSettlesBothBoundaries(t *testing.T) {
	t.Parallel()

	client := newFakeOpenCodeClient()
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := negotiatedAgent(t, WithSessionStore(store))
	agent.setAgentClient(newRecordingAgentClient())
	current := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	cycle := acceptOpenTestTurn(t, current)
	current.detachRuntime(testRuntimeGeneration(current), "shared OpenCode runtime exited")
	require.Error(t, cycle.lost)
	require.True(t, lifecycleFenced(current))

	_, err := agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(current.id))
	require.NoError(t, err, "a fenced incarnation failed the delete boundary")
	require.True(t, agent.isDeleted(current.id))
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
