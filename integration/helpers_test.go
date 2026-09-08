//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	cleanupIntegrationBinary()

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
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run opencode integration tests", envRunIntegration)
	}
	path := os.Getenv(envHarnessPath)
	if path == "" {
		path = "opencode"
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		if os.Getenv(envRunLiveTokens) == "1" || os.Getenv("ACP_GO_OPENCODE_RUN_ATTENDED") == "1" || os.Getenv("ACP_GO_OPENCODE_RUN_KEYSTORE") == "1" {
			t.Fatalf("requested opencode integration tier requires the CLI: %v", err)
		}
		t.Skipf("opencode CLI absent for smoke: %v; set ACP_GO_OPENCODE_HARNESS_PATH", err)
	}
	return resolved
}

// agentCommand builds the wrapper invocation every integration launch goes
// through.
func agentCommand(t *testing.T, ctx context.Context, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, integrationBinaryPath(t), args...)
	cmd.Dir = repoRoot()
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ".."
	}
	return filepath.Dir(filepath.Dir(file))
}

type liveAgent struct{ *integrationProcess }

func startAgent(t *testing.T, ctx context.Context, args ...string) *liveAgent {
	t.Helper()
	return &liveAgent{startIntegrationProcess(t, agentCommand(t, ctx, args...))}
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

func TestIntegrationHarnessPrerequisites(t *testing.T) {
	if os.Args[len(os.Args)-1] == "harness-prerequisite-child" {
		path := integrationOpenCodePath(t)
		t.Log("resolved harness " + path)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, integration, tier, value, outcome string
		available                               bool
	}{
		{name: "ungated", outcome: "SKIP"},
		{name: "disabled", integration: "0", outcome: "SKIP"},
		{name: "invalid_gate", integration: "true", outcome: "SKIP"},
		{name: "missing_smoke", integration: "1", outcome: "SKIP"},
		{name: "disabled_live", integration: "1", tier: "RUN_LIVE_TOKENS", value: "0", outcome: "SKIP"},
		{name: "missing_live", integration: "1", tier: "RUN_LIVE_TOKENS", value: "1", outcome: "FAIL"},
		{name: "missing_attended", integration: "1", tier: "RUN_ATTENDED", value: "1", outcome: "FAIL"},
		{name: "missing_keystore", integration: "1", tier: "RUN_KEYSTORE", value: "1", outcome: "FAIL"},
		{name: "fake_path", integration: "1", outcome: "PASS", available: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, suffix := range []string{"RUN_INTEGRATION", "RUN_LIVE_TOKENS", "RUN_ATTENDED", "RUN_KEYSTORE"} {
				t.Setenv("ACP_GO_OPENCODE_"+suffix, "0")
			}
			t.Setenv("ACP_GO_OPENCODE_RUN_INTEGRATION", tc.integration)
			if tc.tier != "" {
				t.Setenv("ACP_GO_OPENCODE_"+tc.tier, tc.value)
			}
			dir := t.TempDir()
			harness := filepath.Join(dir, "opencode")
			if runtime.GOOS == "windows" {
				harness += ".exe"
			}
			if tc.available {
				// Resolution only: this file is never executed.
				if err := os.WriteFile(harness, []byte("fake harness path"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ACP_GO_OPENCODE_HARNESS_PATH", harness)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestIntegrationHarnessPrerequisites$", "-test.v", "--", "harness-prerequisite-child")
			cmd.WaitDelay = time.Second
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			if (runErr != nil) != (tc.outcome == "FAIL") {
				t.Fatalf("unexpected child result: %v\n%s", runErr, output)
			}
			if !strings.Contains(string(output), "--- "+tc.outcome+": TestIntegrationHarnessPrerequisites") {
				t.Fatalf("want child %s:\n%s", tc.outcome, output)
			}
			if tc.available && !strings.Contains(string(output), "resolved harness "+harness) {
				t.Fatalf("fake harness selection was lost:\n%s", output)
			}
		})
	}
}
