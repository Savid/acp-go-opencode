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
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

const (
	envRunIntegration = "ACP_GO_OPENCODE_RUN_INTEGRATION"
	envRunLiveTokens  = "ACP_GO_OPENCODE_RUN_LIVE_TOKENS"
	envHarnessPath    = "ACP_GO_OPENCODE_HARNESS_PATH"
	envAgentBinary    = "ACP_GO_OPENCODE_AGENT_BINARY"
	envModel          = "ACP_GO_OPENCODE_MODEL"

	// defaultLiveModel is the model every token-spending test runs under unless
	// ACP_GO_OPENCODE_MODEL names another. OpenCode publishes it at zero cost
	// with tool calling, so the permission and question flows run for free.
	defaultLiveModel = "opencode/muse-spark-1.3-contributor-free"

	// agentExitGrace bounds how long a closed stdin may take to shut the wrapper
	// and the native runtime it owns down before the process is killed outright.
	agentExitGrace = 5 * time.Second
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

// agentCommand builds the wrapper invocation every integration launch goes
// through.
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
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.Reader
	stderr    safeBuffer
	done      chan error
	closeOnce sync.Once
}

// startAgent launches the wrapper with the given flags and registers its
// teardown with t.Cleanup, so a failed test still shuts the wrapper and the
// native runtime it owns down instead of leaving an orphaned opencode serve.
func startAgent(t *testing.T, ctx context.Context, args ...string) *liveAgent {
	t.Helper()

	cmd := agentCommand(ctx, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	// A cancelled test context ends the ACP connection the way a host does
	// rather than killing the wrapper before it can tear its runtime down; the
	// kill only follows a wrapper that ignores the closed input.
	cmd.Cancel = stdin.Close
	cmd.WaitDelay = agentExitGrace

	agent := &liveAgent{cmd: cmd, stdin: stdin, stdout: stdout, done: make(chan error, 1)}
	cmd.Stderr = &agent.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	go func() { agent.done <- cmd.Wait() }()

	t.Cleanup(agent.close)

	return agent
}

func startLiveAgent(t *testing.T, ctx context.Context, home string, extraArgs ...string) *liveAgent {
	t.Helper()
	args := []string{
		"-path", integrationOpenCodePath(t),
		"-scratch-dir", home,
		"-opencode-pure",
		"-opencode-health-timeout", "60s",
	}
	args = append(args, extraArgs...)

	return startAgent(t, ctx, args...)
}

// close shuts the wrapper down the way a host does: closing stdin ends the ACP
// connection and the wrapper tears its native runtime down before it exits.
// Killing the process is the fallback for a wrapper that does not exit in time,
// and a killed wrapper cannot stop its runtime, so the grace period comes first.
func (a *liveAgent) close() {
	a.closeOnce.Do(func() {
		_ = a.stdin.Close()

		select {
		case <-a.done:
		case <-time.After(agentExitGrace):
			_ = a.cmd.Process.Kill()
			<-a.done
		}
	})
}

func (a *liveAgent) stderrString() string {
	return a.stderr.String()
}

// liveModelArgs names the model a token-spending test runs under.
func liveModelArgs() []string {
	return []string{"-model", envOrDefault(envModel, defaultLiveModel)}
}

// lifecycleOffer is the initialize offer that enables the lifecycle extension.
// This adapter admits native actions such as permission requests only on a
// connection that negotiated it, so every token-spending test offers it.
func lifecycleOffer() map[string]any {
	return map[string]any{lifecycle.MetaKey: map[string]any{"version": 1}}
}

// correlatedPrompt stamps both envelopes a negotiated prompt carries: the turn
// route and the lifecycle submission correlation.
func correlatedPrompt(sessionID acp.SessionId, turnNonce, text string) acp.PromptRequest {
	request := opencodeacp.TextPromptRequest(sessionID, turnNonce, text)
	request.Meta[lifecycle.MetaKey] = map[string]any{
		"version": 1,
		"submission": map[string]any{
			"submissionId": "submission-" + turnNonce,
			"clientNonce":  "client-" + turnNonce,
		},
	}

	return request
}

func envOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
