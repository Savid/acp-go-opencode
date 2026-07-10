//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

func TestOpenCodeACPAgentBinarySessionLifecycle(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
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
	requireMethodNotFound(t, conn, ctx, "session/fork", map[string]any{"sessionId": "missing", "cwd": t.TempDir()})

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

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	home := t.TempDir()
	args := []string{"-opencode-question-tool"}
	if model := os.Getenv("ACP_GO_OPENCODE_MODEL"); model != "" {
		args = append(args, "-model", model)
	}
	agent := startLiveAgent(t, ctx, home, args...)
	defer agent.close()

	client := newRecordingClient()
	conn := acp.NewClientSideConnection(client, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
	}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	cwd := t.TempDir()
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}

	permissionPrompt := envOrDefault("ACP_GO_OPENCODE_PERMISSION_PROMPT", "Create a file named acp-permission-probe.txt in the working directory, then stop.")
	if _, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: session.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(permissionPrompt)}}); err != nil {
		t.Fatalf("permission prompt: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if client.permissionCount() == 0 {
		t.Fatalf("permission prompt did not reach session/request_permission; stderr:\n%s", agent.stderrString())
	}

	questionPrompt := envOrDefault("ACP_GO_OPENCODE_QUESTION_PROMPT", `Use the question tool to ask the user "Continue?" with options "Yes" and "No", then stop after receiving the answer.`)
	if _, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: session.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(questionPrompt)}}); err != nil {
		t.Fatalf("question prompt: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if client.elicitationCount() == 0 {
		t.Fatalf("question prompt did not reach elicitation/create; stderr:\n%s", agent.stderrString())
	}
}

func TestOpenCodeACPAgentBinaryImportRestoreReplay(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()

	home := t.TempDir()
	agent := startLiveAgent(t, ctx, home)
	defer agent.close()

	client := newRecordingClient()
	conn := acp.NewClientSideConnection(client, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	cwd := t.TempDir()
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(cwd))
	if err != nil {
		t.Fatalf("new session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	nativeID := nativeSessionIDFromMeta(t, session.Meta)
	xdgRoot := filepath.Join(home, safeIntegrationPathName(string(session.SessionId)))
	fixturePath := filepath.Join(t.TempDir(), "opencode-import.json")
	writeOpenCodeImportFixture(t, fixturePath, nativeID, cwd)
	importCmd := exec.CommandContext(ctx, integrationOpenCodePath(t), "import", fixturePath, "--pure") // #nosec G204,G702 -- opt-in integration test command.
	importCmd.Dir = cwd
	importCmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(xdgRoot, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(xdgRoot, "config"),
		"XDG_CACHE_HOME="+filepath.Join(xdgRoot, "cache"),
		"XDG_STATE_HOME="+filepath.Join(xdgRoot, "state"),
	)
	if output, err := importCmd.CombinedOutput(); err != nil {
		t.Fatalf("opencode import: %v\noutput:\n%s", err, output)
	}

	if _, err := conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId}); err != nil {
		t.Fatalf("close imported session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if err := os.RemoveAll(xdgRoot); err != nil {
		t.Fatalf("remove native XDG root: %v", err)
	}
	if _, err := conn.LoadSession(ctx, opencodeacp.LoadSessionRequest(session.SessionId, cwd)); err != nil {
		t.Fatalf("load imported session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if !client.hasUserText("fixture user text") || !client.hasAgentText("fixture assistant text") {
		t.Fatalf("restore replay updates = %#v\nstderr:\n%s", client.updatesSnapshot(), agent.stderrString())
	}
}

func nativeSessionIDFromMeta(t *testing.T, meta map[string]any) string {
	t.Helper()
	opencodeMeta, _ := meta["opencode"].(map[string]any)
	nativeID, _ := opencodeMeta["nativeSessionId"].(string)
	if nativeID == "" {
		t.Fatalf("missing native session id in meta: %#v", meta)
	}
	return nativeID
}

func writeOpenCodeImportFixture(t *testing.T, path string, nativeID string, cwd string) {
	t.Helper()
	now := time.Now().UnixMilli()
	model := map[string]any{"id": "openai/gpt-test", "providerID": "openai", "modelID": "gpt-test"}
	tokens := map[string]any{"input": 0, "output": 0, "reasoning": 0, "total": 0, "cache": map[string]any{"read": 0, "write": 0}}
	messageInfo := func(id string, role string, parentID string) map[string]any {
		return map[string]any{
			"id":         id,
			"sessionID":  nativeID,
			"role":       role,
			"parentID":   parentID,
			"agent":      "build",
			"mode":       "build",
			"modelID":    "gpt-test",
			"providerID": "openai",
			"path":       map[string]any{"cwd": cwd, "root": cwd},
			"cost":       0,
			"tokens":     tokens,
			"model":      model,
			"time":       map[string]any{"created": now, "completed": now},
		}
	}
	fixture := map[string]any{
		"info": map[string]any{
			"id":        nativeID,
			"version":   "1.0.0",
			"slug":      "fixture-restore",
			"title":     "Fixture restore",
			"directory": cwd,
			"agent":     "build",
			"model":     model,
			"time":      map[string]any{"created": now, "updated": now},
		},
		"messages": []map[string]any{
			{
				"info": messageInfo("msg_fixture_user", "user", ""),
				"parts": []map[string]any{{
					"id":        "prt_fixture_user",
					"sessionID": nativeID,
					"messageID": "msg_fixture_user",
					"type":      "text",
					"text":      "fixture user text",
				}},
			},
			{
				"info": messageInfo("msg_fixture_assistant", "assistant", "msg_fixture_user"),
				"parts": []map[string]any{{
					"id":        "prt_fixture_assistant",
					"sessionID": nativeID,
					"messageID": "msg_fixture_assistant",
					"type":      "text",
					"text":      "fixture assistant text",
				}},
			},
		},
	}
	data, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}
}

func safeIntegrationPathName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "session"
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "..", "_")
	return replacer.Replace(value)
}
