//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

func TestOpenCodeACPAgentBinarySessionLifecycle(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()

	home := t.TempDir()
	agent := startLiveAgent(t, ctx, home)

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if initResp.AgentCapabilities.SessionCapabilities.Fork != nil {
		t.Fatalf("stable fork advertised: %#v", initResp.AgentCapabilities.SessionCapabilities.Fork)
	}

	cwd := t.TempDir()
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	listResp, err := conn.ListSessions(ctx, opencodeacp.ListSessionsRequest(opencodeacp.WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("list sessions: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if len(listResp.Sessions) == 0 {
		t.Fatal("session/list returned no sessions")
	}
	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if _, err := conn.ResumeSession(ctx, opencodeacp.ResumeSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("resume session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close resumed session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if _, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("load from shared runtime store: %v\nstderr:\n%s", err, agent.stderrString())
	}
	forkCwd := t.TempDir()
	fork, err := opencodeacp.CallForkSession(ctx, conn, opencodeacp.ForkSessionRequest(session.SessionId, forkCwd))
	if err != nil {
		t.Fatalf("extension fork session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if fork.SessionId == "" || fork.SessionId == session.SessionId {
		t.Fatalf("fork response = %#v", fork)
	}
	if _, err := conn.UnstableDeleteSession(ctx, opencodeacp.DeleteSessionRequest(fork.SessionId)); err != nil {
		t.Fatalf("delete forked session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if _, err := conn.UnstableDeleteSession(ctx, opencodeacp.DeleteSessionRequest(session.SessionId)); err != nil {
		t.Fatalf("delete session: %v\nstderr:\n%s", err, agent.stderrString())
	}
}

func TestOpenCodeACPAgentLivePromptPermissionElicitation(t *testing.T) {
	requireRunLiveTokens(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	home := t.TempDir()
	args := append([]string{"-opencode-question-tool"}, liveModelArgs()...)
	agent := startLiveAgent(t, ctx, home, args...)

	client := newRecordingClient()
	conn := acp.NewClientSideConnection(client, agent.stdin, agent.stdout)
	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
		Meta: lifecycleOffer(),
	})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if initResp.Meta[lifecycle.MetaKey] == nil {
		t.Fatalf("initialize answered no lifecycle capability: %#v", initResp.Meta)
	}
	cwd := t.TempDir()
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}

	permissionPrompt := envOrDefault("ACP_GO_OPENCODE_PERMISSION_PROMPT", "Create a file named acp-permission-probe.txt in the working directory, then stop.")
	permissionTurnNonce, err := opencodeacp.NewTurnNonce()
	if err != nil {
		t.Fatalf("create permission turn nonce: %v", err)
	}
	permissionResp, err := conn.Prompt(ctx, correlatedPrompt(session.SessionId, permissionTurnNonce, permissionPrompt))
	if err != nil {
		t.Fatalf("permission prompt: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if permissionResp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("permission prompt stop reason = %q, want %q\nstderr:\n%s", permissionResp.StopReason, acp.StopReasonEndTurn, agent.stderrString())
	}
	if client.permissionCount() == 0 {
		t.Fatalf("permission prompt did not reach session/request_permission; stderr:\n%s", agent.stderrString())
	}

	questionPrompt := envOrDefault("ACP_GO_OPENCODE_QUESTION_PROMPT", `Use the question tool to ask the user "Continue?" with options "Yes" and "No", then stop after receiving the answer.`)
	questionTurnNonce, err := opencodeacp.NewTurnNonce()
	if err != nil {
		t.Fatalf("create question turn nonce: %v", err)
	}
	questionResp, err := conn.Prompt(ctx, correlatedPrompt(session.SessionId, questionTurnNonce, questionPrompt))
	if err != nil {
		t.Fatalf("question prompt: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if questionResp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("question prompt stop reason = %q, want %q\nstderr:\n%s", questionResp.StopReason, acp.StopReasonEndTurn, agent.stderrString())
	}
	if client.elicitationCount() == 0 {
		t.Fatalf("question prompt did not reach elicitation/create; stderr:\n%s", agent.stderrString())
	}
}
