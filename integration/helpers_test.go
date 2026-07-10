//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
)

const (
	envRunIntegration = "ACP_GO_OPENCODE_RUN_INTEGRATION"
	envRunLiveTokens  = "ACP_GO_OPENCODE_RUN_LIVE_TOKENS"
	envHarnessPath    = "ACP_GO_OPENCODE_HARNESS_PATH"
	envAgentBinary    = "ACP_GO_OPENCODE_AGENT_BINARY"
)

var integrationLogger = slog.New(slog.DiscardHandler)

// requireMethodNotFound dispatches a raw method name over the wire and asserts
// the agent rejects it with method-not-found (-32601).
func requireMethodNotFound(
	t *testing.T,
	conn *acp.ClientSideConnection,
	ctx context.Context,
	method string,
	params any,
) {
	t.Helper()

	_, err := conn.CallExtension(ctx, method, params)
	if err == nil {
		t.Fatalf("%s unexpectedly succeeded", method)
	}

	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) || reqErr.Code != -32601 {
		t.Fatalf("%s error = %#v, want method-not-found", method, err)
	}
}

func TestMain(m *testing.M) {
	previousLogger := slog.Default()
	slog.SetDefault(integrationLogger)

	code := m.Run()

	slog.SetDefault(previousLogger)
	os.Exit(code)
}

type recordingClient struct {
	mu           sync.Mutex
	permissions  []acp.RequestPermissionRequest
	elicitations []acp.UnstableCreateElicitationRequest
	updates      []acp.SessionNotification
}

var _ acp.Client = (*recordingClient)(nil)

func newRecordingClient() *recordingClient {
	return &recordingClient{}
}

func (*recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}
func (*recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}
func (c *recordingClient) RequestPermission(_ context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, req)
	c.mu.Unlock()
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("once")}, nil
}
func (c *recordingClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, notification)
	c.mu.Unlock()
	return nil
}
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

func (c *recordingClient) UnstableCreateElicitation(_ context.Context, req acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	c.elicitations = append(c.elicitations, req)
	c.mu.Unlock()
	content := map[string]any{"question_1": "Yes"}
	if req.Form != nil {
		for _, key := range req.Form.RequestedSchema.Required {
			content[key] = "Yes"
		}
	}
	return acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: content},
	}, nil
}

func (c *recordingClient) permissionCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.permissions)
}

func (c *recordingClient) elicitationCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.elicitations)
}

func (c *recordingClient) hasUserText(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, update := range c.updates {
		chunk := update.Update.UserMessageChunk
		if chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == text {
			return true
		}
	}
	return false
}

func (c *recordingClient) hasAgentText(text string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, update := range c.updates {
		chunk := update.Update.AgentMessageChunk
		if chunk != nil && chunk.Content.Text != nil && chunk.Content.Text.Text == text {
			return true
		}
	}
	return false
}

func (c *recordingClient) updatesSnapshot() []acp.SessionNotification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acp.SessionNotification(nil), c.updates...)
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

func requireRunIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run live OpenCode integration tests", envRunIntegration)
	}
}

func requireRunLiveTokens(t *testing.T) {
	t.Helper()
	requireRunIntegration(t)
	if os.Getenv(envRunLiveTokens) != "1" {
		t.Skipf("set %s=1 to run live OpenCode integration tests that spend model tokens", envRunLiveTokens)
	}
}

func integrationOpenCodePath(t *testing.T) string {
	t.Helper()
	path := os.Getenv(envHarnessPath)
	if path == "" {
		path = "opencode"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		t.Fatalf("find opencode CLI: %v", err)
	}
	return resolved
}

func agentCommand(ctx context.Context, args ...string) *exec.Cmd {
	if binary := os.Getenv(envAgentBinary); binary != "" {
		return exec.CommandContext(ctx, binary, args...) // #nosec G204,G702 -- opt-in integration test command.
	}
	commandArgs := make([]string, 0, 2+len(args))
	commandArgs = append(commandArgs, "run", "./cmd/acp-go-opencode")
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, "go", commandArgs...) // #nosec G204,G702 -- test runs the local wrapper command.
	cmd.Dir = repoRoot()
	return cmd
}

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ".."
	}
	return filepath.Dir(filepath.Dir(file))
}

type liveAgent struct {
	cmd    interface{ ProcessState() *os.ProcessState }
	stdin  io.WriteCloser
	stdout io.Reader
	stderr safeBuffer
	close  func()
	wait   func() error
}

func startLiveAgent(t *testing.T, ctx context.Context, home string, extraArgs ...string) *liveAgent {
	t.Helper()
	args := []string{
		"-path", integrationOpenCodePath(t),
		"-home", home,
		"-opencode-pure",
		"-opencode-health-timeout", "60s",
	}
	args = append(args, extraArgs...)
	cmd := agentCommand(ctx, args...)
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

func envOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
