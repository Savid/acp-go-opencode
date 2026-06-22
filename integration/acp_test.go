//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	agent := startLiveAgent(t, ctx, t.TempDir())
	defer agent.close()

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-integration",
			Version: "test",
		},
	})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if initResp.AgentInfo == nil || initResp.AgentInfo.Name == "" {
		t.Fatalf("agent info = %#v", initResp.AgentInfo)
	}

	cwd := t.TempDir()
	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if session.SessionId == "" {
		t.Fatal("new session returned empty session id")
	}

	listResp, err := conn.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatalf("list sessions: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if len(listResp.Sessions) == 0 {
		t.Fatalf("list sessions returned no sessions for cwd %s", cwd)
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close session: %v\nstderr:\n%s", err, agent.stderrString())
	}
}

func TestOpenCodeACPAgentBinaryNativeSessionContinuity(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	agent := startLiveAgent(t, ctx, cwd)

	conn := acp.NewClientSideConnection(client, agent.stdin, agent.stdout)

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-native-continuity",
			Version: "test",
		},
	})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if !initResp.AgentCapabilities.LoadSession {
		t.Fatalf("loadSession capability not advertised: %#v", initResp.AgentCapabilities)
	}
	if initResp.AgentCapabilities.SessionCapabilities.List == nil {
		t.Fatalf("session/list capability not advertised: %#v", initResp.AgentCapabilities.SessionCapabilities)
	}
	if initResp.AgentCapabilities.SessionCapabilities.Resume == nil {
		t.Fatalf("session/resume capability not advertised: %#v", initResp.AgentCapabilities.SessionCapabilities)
	}
	if initResp.AgentCapabilities.SessionCapabilities.Fork == nil {
		t.Fatalf("session/fork capability not advertised: %#v", initResp.AgentCapabilities.SessionCapabilities)
	}

	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if session.SessionId == "" {
		t.Fatal("new session returned empty session id")
	}
	assertHasConfigOption(t, session.ConfigOptions, opencodeacp.OpenCodeConfigModel)
	assertHasConfigOption(t, session.ConfigOptions, opencodeacp.OpenCodeConfigMode)

	if _, err := conn.SetSessionConfigOption(
		ctx,
		opencodeacp.SetModeConfigRequest(session.SessionId, opencodeacp.OpenCodeModeBuild),
	); err != nil {
		t.Fatalf("set mode config option: %v\nstderr:\n%s", err, agent.stderrString())
	}

	if err := conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}); err != nil {
		t.Fatalf("cancel notification: %v\nstderr:\n%s", err, agent.stderrString())
	}

	listResp, err := conn.ListSessions(ctx, opencodeacp.ListSessionsRequest(opencodeacp.WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("list sessions: %v\nstderr:\n%s", err, agent.stderrString())
	}
	assertSessionListed(t, listResp, session.SessionId)

	loadResp, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd))
	if err != nil {
		t.Fatalf("load session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	assertHasConfigOption(t, loadResp.ConfigOptions, opencodeacp.OpenCodeConfigModel)

	resumeResp, err := conn.ResumeSession(ctx, opencodeacp.ResumeSessionRequest(session.SessionId, cwd))
	if err != nil {
		t.Fatalf("resume session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	assertHasConfigOption(t, resumeResp.ConfigOptions, opencodeacp.OpenCodeConfigModel)

	forkResp, err := conn.UnstableForkSession(ctx, opencodeacp.ForkSessionRequest(session.SessionId, cwd))
	if err != nil {
		t.Fatalf("fork session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if forkResp.SessionId == "" || forkResp.SessionId == session.SessionId {
		t.Fatalf("fork session id = %q, original %q", forkResp.SessionId, session.SessionId)
	}

	agent.close()

	restartedClient := &recordingClient{}
	restartedAgent := startLiveAgent(t, ctx, cwd)
	defer restartedAgent.close()

	restarted := acp.NewClientSideConnection(restartedClient, restartedAgent.stdin, restartedAgent.stdout)

	if _, err := restarted.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-native-continuity-restart",
			Version: "test",
		},
	}); err != nil {
		t.Fatalf("restart initialize: %v\nstderr:\n%s", err, restartedAgent.stderrString())
	}

	restartList, err := restarted.ListSessions(ctx, opencodeacp.ListSessionsRequest(opencodeacp.WithListSessionsCwd(cwd)))
	if err != nil {
		t.Fatalf("restart list sessions: %v\nstderr:\n%s", err, restartedAgent.stderrString())
	}
	assertSessionListed(t, restartList, session.SessionId)

	if _, err := restarted.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("restart load session: %v\nstderr:\n%s", err, restartedAgent.stderrString())
	}
	if _, err := restarted.ResumeSession(ctx, opencodeacp.ResumeSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("restart resume session: %v\nstderr:\n%s", err, restartedAgent.stderrString())
	}
	if restartedClient.updateCount() == 0 {
		t.Fatal("restart load/resume did not replay any session updates")
	}
}

func TestOpenCodeACPAgentBinaryProxySessionExtensions(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	agent := startLiveAgent(t, ctx, cwd)
	defer agent.close()

	conn := acp.NewClientSideConnection(client, agent.stdin, agent.stdout)

	initResp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-proxy-extensions",
			Version: "test",
		},
	})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	assertWrapperExtensionsAdvertised(t, initResp.AgentCapabilities.Meta)

	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}

	exportRaw, err := conn.CallExtension(ctx, opencodeacp.OpenCodeSessionExportMethod, map[string]any{
		"sessionId": session.SessionId,
	})
	if err != nil {
		t.Fatalf("proxy export extension: %v\nstderr:\n%s", err, agent.stderrString())
	}
	var exportResp struct {
		SessionID acp.SessionId `json:"sessionId"`
		Bytes     int           `json:"bytes"`
		RawJSON   string        `json:"rawJSON"`
	}
	if err := json.Unmarshal(exportRaw, &exportResp); err != nil {
		t.Fatalf("decode proxy export response: %v\nraw: %s", err, string(exportRaw))
	}
	if exportResp.SessionID != session.SessionId || exportResp.Bytes == 0 ||
		!strings.Contains(exportResp.RawJSON, string(session.SessionId)) {
		t.Fatalf("proxy export response = %#v", exportResp)
	}

	exportPath := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(exportPath, []byte(exportResp.RawJSON), 0o600); err != nil {
		t.Fatalf("write exported session: %v", err)
	}

	deleteRaw, err := conn.CallExtension(ctx, opencodeacp.OpenCodeSessionDeleteMethod, map[string]any{
		"sessionId": session.SessionId,
	})
	if err != nil {
		t.Fatalf("proxy delete extension: %v\nstderr:\n%s", err, agent.stderrString())
	}
	var deleteResp struct {
		Deleted   bool          `json:"deleted"`
		SessionID acp.SessionId `json:"sessionId"`
	}
	if err := json.Unmarshal(deleteRaw, &deleteResp); err != nil {
		t.Fatalf("decode proxy delete response: %v\nraw: %s", err, string(deleteRaw))
	}
	if !deleteResp.Deleted || deleteResp.SessionID != session.SessionId {
		t.Fatalf("proxy delete response = %#v", deleteResp)
	}

	importRaw, err := conn.CallExtension(ctx, opencodeacp.OpenCodeSessionImportMethod, map[string]any{
		"path": exportPath,
	})
	if err != nil {
		t.Fatalf("proxy import extension: %v\nstderr:\n%s", err, agent.stderrString())
	}
	var importResp struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(importRaw, &importResp); err != nil {
		t.Fatalf("decode proxy import response: %v\nraw: %s", err, string(importRaw))
	}
	if strings.TrimSpace(importResp.Output) == "" {
		t.Fatalf("proxy import response = %#v", importResp)
	}

	if _, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("load imported session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if _, err := conn.ResumeSession(ctx, opencodeacp.ResumeSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("resume imported session: %v\nstderr:\n%s", err, agent.stderrString())
	}
}

func TestOpenCodeACPAgentBinaryNativePromptToolAndUsage(t *testing.T) {
	requireRunLiveTokens(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	agent := startLiveAgent(t, ctx, cwd)
	defer agent.close()

	conn := acp.NewClientSideConnection(client, agent.stdin, agent.stdout)

	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "acp-go-opencode-native-live",
			Version: "test",
		},
	}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}

	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}

	resp, err := conn.Prompt(ctx, opencodeacp.TextPromptRequest(
		session.SessionId,
		"Create a file named acp_probe.txt in the current working directory with exactly this content: ACP TOOL OK",
	))
	if err != nil {
		t.Fatalf("prompt: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", resp.StopReason)
	}
	if resp.Usage == nil {
		t.Fatal("prompt response did not include usage")
	}

	data, err := os.ReadFile(filepath.Join(cwd, "acp_probe.txt"))
	if err != nil {
		t.Fatalf("read created file: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if strings.TrimSpace(string(data)) != "ACP TOOL OK" {
		t.Fatalf("created file content = %q", string(data))
	}

	updates := client.updateKinds()
	for _, kind := range []string{"tool_call", "tool_call_update", "usage_update"} {
		if updates[kind] == 0 {
			t.Fatalf("missing %s update; updates = %#v", kind, updates)
		}
	}
}

type liveAgent struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *lockedBuffer
	done   chan error
	once   sync.Once
}

func startLiveAgent(t *testing.T, ctx context.Context, cwd string) *liveAgent {
	t.Helper()

	cmd := agentCommand(ctx,
		"-opencode", integrationOpenCodePath(t),
		"-cwd", cwd,
		"-pure",
		"-hostname", "127.0.0.1",
		"-port", "0",
		"-print-logs",
		"-log-level", "INFO",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start acp-go-opencode: %v", err)
	}

	agent := &liveAgent{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		done:   make(chan error, 1),
	}
	go func() {
		agent.done <- cmd.Wait()
	}()

	return agent
}

func (a *liveAgent) close() {
	a.once.Do(func() {
		_ = a.stdin.Close()
		select {
		case <-a.done:
			return
		case <-time.After(10 * time.Second):
			if a.cmd.Process != nil {
				_ = a.cmd.Process.Kill()
			}
			<-a.done
		}
	})
}

func (a *liveAgent) stderrString() string {
	if a == nil || a.stderr == nil {
		return ""
	}

	return a.stderr.String()
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

type recordingClient struct {
	mu      sync.Mutex
	updates []acp.SessionUpdate
}

var _ acp.Client = (*recordingClient)(nil)
var _ acp.ExtensionMethodHandler = (*recordingClient)(nil)

func (c *recordingClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{Content: ""}, nil
}

func (c *recordingClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (c *recordingClient) RequestPermission(
	_ context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindAllowOnce || option.Kind == acp.PermissionOptionKindAllowAlways {
			return acp.RequestPermissionResponse{
				Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId),
			}, nil
		}
	}

	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeCancelled(),
	}, nil
}

func (c *recordingClient) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.updates = append(c.updates, params.Update)

	return nil
}

func (c *recordingClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (c *recordingClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (c *recordingClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}

func (c *recordingClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (c *recordingClient) WaitForTerminalExit(
	context.Context,
	acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (c *recordingClient) HandleExtensionMethod(
	_ context.Context,
	method string,
	params json.RawMessage,
) (any, error) {
	if !strings.HasPrefix(method, "_") {
		return nil, fmt.Errorf("unexpected extension method %q", method)
	}
	if len(params) == 0 {
		return map[string]any{}, nil
	}

	var decoded any
	if err := json.Unmarshal(params, &decoded); err != nil {
		return nil, err
	}

	return map[string]any{}, nil
}

func assertSessionListed(t *testing.T, resp acp.ListSessionsResponse, sessionID acp.SessionId) {
	t.Helper()

	for _, session := range resp.Sessions {
		if session.SessionId == sessionID {
			return
		}
	}

	t.Fatalf("session %q not listed in %#v", sessionID, resp.Sessions)
}

func assertHasConfigOption(t *testing.T, options []acp.SessionConfigOption, id acp.SessionConfigId) {
	t.Helper()

	for _, option := range options {
		if option.Select != nil && option.Select.Id == id {
			return
		}
		if option.Boolean != nil && option.Boolean.Id == id {
			return
		}
	}

	t.Fatalf("config option %q not found in %#v", id, options)
}

func assertWrapperExtensionsAdvertised(t *testing.T, meta map[string]any) {
	t.Helper()

	packageMeta, ok := meta["github.com/savid/acp-go-opencode"].(map[string]any)
	if !ok {
		t.Fatalf("wrapper metadata missing from agent capabilities: %#v", meta)
	}
	extensions, ok := packageMeta["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("wrapper extensions missing from metadata: %#v", packageMeta)
	}
	for name, method := range map[string]string{
		"sessionExport": opencodeacp.OpenCodeSessionExportMethod,
		"sessionImport": opencodeacp.OpenCodeSessionImportMethod,
		"sessionDelete": opencodeacp.OpenCodeSessionDeleteMethod,
	} {
		extension, ok := extensions[name].(map[string]any)
		if !ok || extension["method"] != method {
			t.Fatalf("extension %s = %#v, want method %s in %#v", name, extensions[name], method, extensions)
		}
	}
}

func (c *recordingClient) updateCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.updates)
}

func (c *recordingClient) updateKinds() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string]int)
	for _, update := range c.updates {
		out[sessionUpdateKind(update)]++
	}

	return out
}

func sessionUpdateKind(update acp.SessionUpdate) string {
	switch {
	case update.UserMessageChunk != nil:
		return "user_message_chunk"
	case update.AgentMessageChunk != nil:
		return "agent_message_chunk"
	case update.AgentThoughtChunk != nil:
		return "agent_thought_chunk"
	case update.ToolCall != nil:
		return "tool_call"
	case update.ToolCallUpdate != nil:
		return "tool_call_update"
	case update.Plan != nil:
		return "plan"
	case update.PlanUpdate != nil:
		return "plan_update"
	case update.PlanRemoved != nil:
		return "plan_removed"
	case update.AvailableCommandsUpdate != nil:
		return "available_commands_update"
	case update.CurrentModeUpdate != nil:
		return "current_mode_update"
	case update.ConfigOptionUpdate != nil:
		return "config_option_update"
	case update.SessionInfoUpdate != nil:
		return "session_info_update"
	case update.UsageUpdate != nil:
		return "usage_update"
	default:
		return "unknown"
	}
}
