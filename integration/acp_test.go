//go:build integration

package integration

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestOpenCodeACPAgentBinarySessionLifecycle(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	home := t.TempDir()
	agent := startLiveAgent(t, ctx, home)
	defer agent.close()

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if initResp.AgentCapabilities.SessionCapabilities.Fork != nil {
		t.Fatalf("stable fork advertised: %#v", initResp.AgentCapabilities.SessionCapabilities.Fork)
	}
	if _, err := conn.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{}); err == nil {
		t.Fatal("stable session/fork unexpectedly succeeded")
	}

	cwd := t.TempDir()
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	fork, err := opencodeacp.CallForkSession(ctx, conn, opencodeacp.ForkSessionRequest(session.SessionId, cwd))
	if err != nil {
		t.Fatalf("extension fork session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if fork.SessionId == "" || fork.SessionId == session.SessionId {
		t.Fatalf("fork response = %#v", fork)
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
	if err := os.RemoveAll(filepath.Join(home, string(session.SessionId))); err != nil {
		t.Fatalf("delete native XDG state: %v", err)
	}
	if _, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("load after native XDG deletion: %v\nstderr:\n%s", err, agent.stderrString())
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
	t.Skip("live token E2E scaffold: permission and elicitation prompts require configured provider credentials")
}

type liveAgent struct {
	cmd    interface{ ProcessState() *os.ProcessState }
	stdin  io.WriteCloser
	stdout io.Reader
	stderr safeBuffer
	close  func()
	wait   func() error
}

func startLiveAgent(t *testing.T, ctx context.Context, home string) *liveAgent {
	t.Helper()
	cmd := agentCommand(ctx,
		"-path", integrationOpenCodePath(t),
		"-home", home,
		"-opencode-pure",
		"-opencode-health-timeout", "30s",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	agent := &liveAgent{stdin: stdin, stdout: stdout, wait: cmd.Wait}
	cmd.Stderr = &agent.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	agent.close = func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return agent
}

func (a *liveAgent) stderrString() string {
	return a.stderr.String()
}

type safeBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b = append(b.b, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.b)
}

type recordingClient struct{}

var _ acp.Client = (*recordingClient)(nil)

func (*recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}
func (*recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}
func (*recordingClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}
func (*recordingClient) SessionUpdate(context.Context, acp.SessionNotification) error { return nil }
func (*recordingClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}
func (*recordingClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}
func (*recordingClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}
func (*recordingClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}
func (*recordingClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}
