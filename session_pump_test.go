package opencodeacp

import (
	"context"
	"errors"
	"testing"

	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// TestSettleAgentCycleSettlesTheRecordedOutcome proves an agent-origin cycle
// settles as failed when it carries a failure, and that a prefix that cannot be
// committed fences the stream and surfaces the commit error instead of settling.
func TestSettleAgentCycleSettlesTheRecordedOutcome(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	failed := &foregroundCycle{id: "cycle-1", failure: errors.New("native turn failed"), signal: make(chan struct{})}
	require.NoError(t, session.settleAgentCycle(ctx, failed))
	require.True(t, failed.settled)

	store := &errorSessionStore{err: errors.New("commit refused")}
	blocked := testSession(t, NewAgent(WithSessionStore(store)), newFakeOpenCodeClient())
	cycle := &foregroundCycle{id: "cycle-2", signal: make(chan struct{})}
	require.ErrorContains(t, blocked.settleAgentCycle(ctx, cycle), "commit refused")
	require.False(t, cycle.settled)

	// An agent-origin terminal whose settlement fails records the failure on the
	// cycle it could not settle.
	open := &foregroundCycle{id: "cycle-3", origin: lifecycle.CauseActivity, signal: make(chan struct{})}
	blocked.lifecycleMu.Lock()
	blocked.cycle = open
	blocked.lifecycleMu.Unlock()
	blocked.markNativeTerminal(ctx)
	require.ErrorContains(t, blocked.cycleFailure(open), "commit refused")
}

// TestNativeRepliedActionStateMapsEveryResolution proves each native resolution
// form maps to exactly one terminal action state, including the reply forms that
// carry no accept/decline distinction.
func TestNativeRepliedActionStateMapsEveryResolution(t *testing.T) {
	t.Parallel()

	require.Equal(t, lifecycle.ActionCancelled,
		nativeRepliedActionState(opencode.EventQuestionRejected, opencode.ActionRepliedEvent{}))
	require.Equal(t, lifecycle.ActionDeclined,
		nativeRepliedActionState(opencode.EventPermissionReplied, opencode.ActionRepliedEvent{}))
	require.Equal(t, lifecycle.ActionAccepted,
		nativeRepliedActionState(opencode.EventQuestionV2Replied, opencode.ActionRepliedEvent{}))
}

// TestReconcileNativeActionsWithoutARuntimeIsANoOp proves a session whose
// runtime binding is gone reconciles nothing rather than failing.
func TestReconcileNativeActionsWithoutARuntimeIsANoOp(t *testing.T) {
	t.Parallel()

	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	session.stopPump()
	session.mu.Lock()
	session.client = nil
	session.mu.Unlock()

	require.NoError(t, session.reconcileNativeActions(context.Background()))
}

// TestRecordNativeFailureAttributesToTheOpenCycle proves a pump-side failure is
// recorded against the open cycle, and that a cycle already carrying a failure
// keeps the first one.
func TestRecordNativeFailureAttributesToTheOpenCycle(t *testing.T) {
	t.Parallel()

	session := testSession(t, NewAgent(), newFakeOpenCodeClient())

	session.lifecycleMu.Lock()
	cycle := &foregroundCycle{id: "cycle-1"}
	session.cycle = cycle
	session.lifecycleMu.Unlock()

	session.recordNativeFailure(nil)
	require.NoError(t, session.cycleFailure(cycle))

	session.recordNativeFailure(errors.New("late event failed"))
	require.ErrorContains(t, session.cycleFailure(cycle), "late event failed")

	session.recordNativeFailure(errors.New("second failure"))
	require.ErrorContains(t, session.cycleFailure(cycle), "late event failed")
}
