package opencodeacp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestLifecycleStreamFailureAndFenceBranches(t *testing.T) {
	ctx := context.Background()
	(&session{}).openLifecycleStream()

	agent := negotiatedAgent(t)
	oldRead := newTurnNonceRead
	newTurnNonceRead = func([]byte) (int, error) { return 0, errors.New("entropy") }
	broken := &session{agent: agent}
	broken.openLifecycleStream()
	require.ErrorContains(t, broken.lifecycleFailed, "entropy")
	newTurnNonceRead = oldRead

	s := &session{agent: agent, id: "s"}
	s.openLifecycleStream()
	require.ErrorContains(t, s.emitLifecycle(ctx, lifecycle.SnapshotEvent(
		lifecycle.Foreground{State: lifecycle.ForegroundIdle, CycleID: "idle"}, nil, lifecycle.QuiescenceFact{},
	)), "no ACP connection")
	require.Error(t, s.emitLifecycle(ctx, lifecycle.Event{}), "delivery failure latches")

	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	invalid := &session{agent: agent, id: "invalid"}
	invalid.openLifecycleStream()
	require.Error(t, invalid.emitLifecycle(ctx, lifecycle.Event{}))
	s = &session{agent: agent, id: "s", lifecycleCycle: "idle", lifecycleSettled: true}
	s.openLifecycleStream()
	require.NoError(t, s.emitLifecycle(ctx, lifecycle.SnapshotEvent(
		lifecycle.Foreground{State: lifecycle.ForegroundIdle, CycleID: "idle"}, nil, lifecycle.QuiescenceFact{},
	)))
	conn.updateErr = errors.New("wire")
	require.ErrorContains(t, s.beginLifecycleTurn(ctx), "wire")
	require.Error(t, s.lifecycleAction(ctx, lifecycle.ActionUpdate{}))
	require.Error(t, s.lifecycleActionPending(ctx, lifecycle.ActionUpdate{}))
	require.Error(t, s.lifecycleActionResolved(ctx, "action", lifecycle.ActionFailed))

	absent := &session{lifecycleFailed: errors.New("absent")}
	require.Error(t, absent.beginLifecycleTurn(ctx))
	require.Error(t, absent.lifecycleAction(ctx, lifecycle.ActionUpdate{}))
	require.Nil(t, absent.lifecycleActionMeta("a", lifecycle.Owner{}))
	absent.fenceLifecycle("ignored")

	conn.updateErr = nil
	active := &session{agent: agent, id: "active", turnNonce: "turn", turnEpoch: 1,
		submission: lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"}}
	active.openLifecycleStream()
	require.NoError(t, active.beginLifecycleTurn(ctx))
	meta := active.lifecycleActionMeta("a", active.currentLifecycleOwner())
	require.Contains(t, meta, lifecycle.MetaKey)
	active.fenceLifecycle("runtime exited")
	require.ErrorContains(t, active.lifecycleFailed, "runtime exited")
	require.Error(t, active.settleLifecycleTurn(ctx, "", lifecycle.OutcomeFailed))

	settled := &session{lifecycleSettled: true}
	require.NoError(t, settled.settleLifecycleTurn(ctx, "", lifecycle.OutcomeFailed))
	require.Equal(t, map[string]any{"a": 1, "b": 2}, mergeMeta(map[string]any{"a": 1}, map[string]any{"b": 2}))
	require.Nil(t, cloneAvailableCommands(nil))
}

func TestLifecyclePropagationFailureBranches(t *testing.T) {
	ctx := context.Background()
	agent := negotiatedAgent(t)
	conn := newRecordingAgentClient()
	agent.setAgentClient(conn)
	client := newFakeOpenCodeClient()
	s := testSession(agent, client)
	s.beginTurn(ctx, "turn")
	s.recordSubmission(lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"})
	require.NoError(t, s.beginLifecycleTurn(ctx))
	s.lifecycleMu.Lock()
	s.lifecycleFailed = errors.New("pending failed")
	s.lifecycleMu.Unlock()
	require.ErrorContains(t, s.handlePermission(ctx, opencode.PermissionRequest{
		ID: "pending-permission", SessionID: s.idmap.NativeSessionID,
	}), "pending failed")
	require.ErrorContains(t, s.handleQuestion(ctx, opencode.QuestionRequest{
		ID: "pending-question", SessionID: s.idmap.NativeSessionID,
	}), "pending failed")
	s.lifecycleMu.Lock()
	s.lifecycleFailed = nil
	s.lifecycleMu.Unlock()

	setFailed := func() {
		s.lifecycleMu.Lock()
		s.lifecycleFailed = errors.New("latched")
		s.lifecycleMu.Unlock()
	}

	conn.permissionStarted = make(chan struct{})
	conn.permissionRelease = make(chan struct{})
	permissionDone := make(chan error, 1)
	go func() {
		permissionDone <- s.handlePermission(ctx, opencode.PermissionRequest{
			ID: "permission", SessionID: s.idmap.NativeSessionID,
		})
	}()
	<-conn.permissionStarted
	setFailed()
	close(conn.permissionRelease)
	require.ErrorContains(t, <-permissionDone, "latched")

	// A fresh stream reaches the corresponding elicitation resolution guards.
	s = testSession(agent, client)
	s.beginTurn(ctx, "turn")
	s.recordSubmission(lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"})
	require.NoError(t, s.beginLifecycleTurn(ctx))
	conn.elicitationStarted = make(chan struct{})
	conn.elicitationRelease = make(chan struct{})
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	questionDone := make(chan error, 1)
	go func() {
		questionDone <- s.handleQuestion(ctx, opencode.QuestionRequest{
			ID: "question", SessionID: s.idmap.NativeSessionID,
			Questions: []opencode.QuestionInfo{{Question: "Choose"}},
		})
	}()
	<-conn.elicitationStarted
	setFailed()
	close(conn.elicitationRelease)
	require.ErrorContains(t, <-questionDone, "latched")

	// Decline takes the other terminal elicitation path.
	s = testSession(agent, client)
	s.beginTurn(ctx, "turn")
	s.recordSubmission(lifecycle.Submission{SubmissionID: "submission", ClientNonce: "nonce"})
	require.NoError(t, s.beginLifecycleTurn(ctx))
	conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
	conn.elicitationStarted = make(chan struct{})
	conn.elicitationRelease = make(chan struct{})
	questionDone = make(chan error, 1)
	go func() {
		questionDone <- s.handleQuestion(ctx, opencode.QuestionRequest{
			ID: "declined", SessionID: s.idmap.NativeSessionID,
			Questions: []opencode.QuestionInfo{{Question: "Choose"}},
		})
	}()
	<-conn.elicitationStarted
	setFailed()
	close(conn.elicitationRelease)
	require.ErrorContains(t, <-questionDone, "latched")

	// Exercise validation and delivery failure before native dispatch.
	bad := testSession(agent, client)
	_, err := bad.Prompt(ctx, acp.PromptRequest{Meta: map[string]any{lifecycle.MetaKey: map[string]any{}}})
	require.Error(t, err)
	conn.updateErr = errors.New("wire")
	request := TextPromptRequest(bad.id, internalSeamTurnNonce, "hello")
	request.Meta[lifecycle.MetaKey] = map[string]any{"version": 1, "submission": map[string]any{
		"submissionId": "submission", "clientNonce": "nonce",
	}}
	_, err = bad.Prompt(ctx, request)
	require.ErrorContains(t, err, "wire")

	conn.updateErr = nil
	settlement := testSession(agent, client)
	client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		conn.updateErr = errors.New("settlement wire")

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
	}
	request = TextPromptRequest(settlement.id, internalSeamTurnNonce, "hello")
	request.Meta[lifecycle.MetaKey] = map[string]any{"version": 1, "submission": map[string]any{
		"submissionId": "submission", "clientNonce": "nonce",
	}}
	_, err = settlement.Prompt(ctx, request)
	require.ErrorContains(t, err, "settlement wire")

	// Allow background goroutines from deliberately failed prompts to finish.
	time.Sleep(time.Millisecond)
}

func TestCloseAndDeleteRemainAtomicOnPrerequisiteFailure(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	s := testSession(agent, client)
	agent.sessions[s.id] = s
	s.poisonCause = "snapshot unavailable"
	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: s.id})
	require.ErrorContains(t, err, "snapshot unavailable")
	require.False(t, client.closed)
	require.Contains(t, agent.sessions, s.id)

	s.poisonCause = ""
	client.deleteErr = errors.New("native delete")
	_, err = agent.UnstableDeleteSession(context.Background(), acp.UnstableDeleteSessionRequest{SessionId: s.id})
	require.ErrorContains(t, err, "native delete")
	require.Contains(t, agent.sessions, s.id)
	require.False(t, agent.isDeleted(s.id))
}
