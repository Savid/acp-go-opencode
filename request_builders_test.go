package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestRequestBuilders(t *testing.T) {
	httpServer := HTTPMCPServer("http", "https://example.com", map[string]string{"X": "Y"})
	stdioServer := StdioMCPServer("stdio", "cmd", []string{"arg"}, map[string]string{"E": "V"})
	sseServer := acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse", Url: "https://sse.example"}}
	acpServer := acp.McpServer{Acp: &acp.McpServerAcpInline{Id: "acp-1", Name: "acp"}}
	meta := map[string]any{"foreign": map[string]any{"a": []any{"b"}}}
	req := NewSessionRequest("/tmp/project",
		WithSessionAdditionalDirectories("/tmp/other"),
		WithSessionMCPServers(httpServer, stdioServer, sseServer, acpServer),
		WithSessionMeta(meta),
		WithSessionRawEvents(true),
		WithSessionOutputSchema(map[string]any{"type": "object"}),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeModel("openai/gpt"),
			WithOpenCodeMode("plan"),
			WithOpenCodePermission("allow"),
		)),
	)
	if req.Cwd != "/tmp/project" || len(req.McpServers) != 4 || len(req.AdditionalDirectories) != 1 {
		t.Fatalf("NewSessionRequest = %#v", req)
	}
	if !rawMessageConfigFromMeta(req.Meta).Enabled() {
		t.Fatalf("raw events not enabled in meta: %#v", req.Meta)
	}
	meta, ok := req.Meta[opencodeMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("meta missing opencode key: %#v", req.Meta)
	}
	options, ok := meta[metaOptionsKey].(map[string]any)
	if !ok {
		t.Fatalf("meta missing options key: %#v", req.Meta)
	}
	if options[metaPermissionKey] != "allow" {
		t.Fatalf("permission not set in meta: %#v", req.Meta)
	}
	if ResumeSessionRequest("s", "/tmp/project", WithSessionMCPServers(httpServer)).SessionId != "s" {
		t.Fatal("ResumeSessionRequest did not set session id")
	}
	if prompt := TextPromptRequest("s", "nonce", "hello"); prompt.SessionId != "s" || len(prompt.Prompt) != 1 {
		t.Fatalf("TextPromptRequest = %#v", prompt)
	}
	if cancel := CancelRequest("s", "nonce"); cancel.SessionId != "s" || cancel.Meta[routeEnvelopeKey] == nil {
		t.Fatalf("CancelRequest = %#v", cancel)
	}
	list := ListSessionsRequest(WithListSessionsCursor("next"), WithListSessionsMeta(map[string]any{"a": "b"}))
	if list.Cursor == nil || *list.Cursor != "next" || list.Meta["a"] != "b" {
		t.Fatalf("ListSessionsRequest = %#v", list)
	}
	unstable := unstableMCPServersFromStable(req.McpServers)
	if len(unstable) != 4 || unstable[0].Http == nil || unstable[1].Stdio == nil || unstable[2].Sse == nil || unstable[3].Acp == nil {
		t.Fatalf("unstable MCP servers = %#v", unstable)
	}
}

func TestTurnRequestBuildersFailClosedOnInvalidNonce(t *testing.T) {
	tests := []struct {
		name      string
		turnNonce string
		wantRoute bool
	}{
		{name: "empty", turnNonce: ""},
		{name: "whitespace only", turnNonce: " \t\n"},
		{name: "opaque surrounding whitespace", turnNonce: " nonce ", wantRoute: true},
		{name: "maximum bytes", turnNonce: strings.Repeat("n", routeTurnNonceMaxBytes), wantRoute: true},
		{name: "over maximum bytes", turnNonce: strings.Repeat("n", routeTurnNonceMaxBytes+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prompt := PromptRequest("s", tc.turnNonce)
			cancel := CancelRequest("s", tc.turnNonce)
			if (prompt.Meta != nil) != tc.wantRoute {
				t.Fatalf("PromptRequest route presence = %t, want %t", prompt.Meta != nil, tc.wantRoute)
			}
			if (cancel.Meta != nil) != tc.wantRoute {
				t.Fatalf("CancelRequest route presence = %t, want %t", cancel.Meta != nil, tc.wantRoute)
			}
		})
	}
}

func TestRequestBuilderCloneEdgeBranches(t *testing.T) {
	rawOnly := NewSessionRequest("/tmp/project", WithSessionRawEvents(true))
	if !rawMessageConfigFromMeta(rawOnly.Meta).Enabled() {
		t.Fatalf("rawOnly meta = %#v", rawOnly.Meta)
	}
	outputSchema := NewOpenCodeOptions(WithOpenCodeOutputSchema(map[string]any{"type": "object"}))
	if outputSchema.OutputSchema["type"] != "object" {
		t.Fatalf("output schema options = %#v", outputSchema)
	}
	outputSchema.OutputSchema["type"] = "changed"
	outputSchemaClone := NewOpenCodeOptions(WithOpenCodeOutputSchema(outputSchema.OutputSchema))
	outputSchema.OutputSchema["type"] = "mutated"
	if outputSchemaClone.OutputSchema["type"] != "changed" {
		t.Fatalf("output schema was not cloned: %#v", outputSchemaClone.OutputSchema)
	}
	if cloneMCPServers(nil) != nil || cloneMCPServerStdio(nil) != nil || cloneHTTPHeaders(nil) != nil ||
		cloneEnvVariables(nil) != nil || unstableMCPServersFromStable(nil) != nil {
		t.Fatal("nil clone helper returned non-nil")
	}
	if cloneMCPServer(acp.McpServer{}).Http != nil {
		t.Fatal("empty MCP clone was populated")
	}
	if unstableMCPServerFromStable(acp.McpServer{}).Http != nil {
		t.Fatal("empty unstable MCP clone was populated")
	}
	meta := map[string]any{opencodeMetaKey: map[string]any{"a": "b"}}
	ensured := ensureMetaMap(meta, opencodeMetaKey)
	ensured["a"] = "changed"
	stored, ok := meta[opencodeMetaKey].(map[string]any)
	if !ok || stored["a"] != "changed" {
		t.Fatalf("ensureMetaMap did not store clone: %#v", meta)
	}
}

func TestCallForkSessionHelper(t *testing.T) {
	ctx := context.Background()
	for name, handler := range map[string]forkExtensionAgent{
		"success": {
			Agent:    NewAgent(),
			response: acp.UnstableForkSessionResponse{SessionId: "forked"},
		},
		"agent error": {
			Agent: NewAgent(),
			err:   errors.New("fork failed"),
		},
		"decode error": {
			Agent:    NewAgent(),
			response: json.RawMessage(`"bad"`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			conn, closeConn := forkClientConnection(t, handler)
			defer closeConn()
			resp, err := CallForkSession(ctx, conn, ForkSessionRequest("s", "/tmp/project"))
			switch name {
			case "success":
				if err != nil || resp.SessionId != "forked" {
					t.Fatalf("CallForkSession resp=%#v err=%v", resp, err)
				}
			default:
				if err == nil {
					t.Fatal("CallForkSession unexpectedly succeeded")
				}
			}
		})
	}
}

type forkExtensionAgent struct {
	*Agent
	response any
	err      error
}

func (a forkExtensionAgent) HandleExtensionMethod(context.Context, string, json.RawMessage) (any, error) {
	return a.response, a.err
}

func forkClientConnection(t *testing.T, agent forkExtensionAgent) (*acp.ClientSideConnection, func()) {
	t.Helper()
	clientToAgentReader, clientToAgentWriter := io.Pipe()
	agentToClientReader, agentToClientWriter := io.Pipe()
	_ = acp.NewAgentSideConnection(agent, agentToClientWriter, clientToAgentReader)
	conn := acp.NewClientSideConnection(noopACPClient{}, clientToAgentWriter, agentToClientReader)

	return conn, func() {
		_ = clientToAgentWriter.Close()
		_ = clientToAgentReader.Close()
		_ = agentToClientWriter.Close()
		_ = agentToClientReader.Close()
	}
}

type noopACPClient struct{}

func (noopACPClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}

func (noopACPClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (noopACPClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, nil
}

func (noopACPClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	return nil
}

func (noopACPClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, nil
}

func (noopACPClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (noopACPClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}

func (noopACPClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (noopACPClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func TestOpenCodeExtraPathDirsBuilderClones(t *testing.T) {
	dirs := []string{"/session/bin"}
	req := NewSessionRequest("/tmp/project", WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeExtraPathDirs(dirs...),
	)))

	dirs[0] = "/mutated"

	meta, err := sessionMetaFromLifecycle(req.Meta)
	if err != nil {
		t.Fatalf("session meta from builder: %v", err)
	}
	if len(meta.ExtraPathDirs) != 1 || meta.ExtraPathDirs[0] != "/session/bin" {
		t.Fatalf("session extra path dirs = %#v", meta.ExtraPathDirs)
	}

	clearMeta := NewOpenCodeOptions(WithOpenCodeExtraPathDirs()).Meta()
	clearNamespace, ok := clearMeta[opencodeMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("cleared options namespace = %#v", clearMeta[opencodeMetaKey])
	}
	clearValues, ok := clearNamespace[metaOptionsKey].(map[string]any)
	if !ok {
		t.Fatalf("cleared options values = %#v", clearNamespace[metaOptionsKey])
	}
	if cleared, clearedOK := clearValues[metaExtraPathDirsKey].([]string); !clearedOK || cleared == nil || len(cleared) != 0 {
		t.Fatalf("cleared extra path dirs = %#v", clearValues[metaExtraPathDirsKey])
	}

	empty, ok := NewOpenCodeOptions().Meta()[opencodeMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("empty options meta = %#v", empty)
	}
	if values, ok := empty[metaOptionsKey].(map[string]any); !ok || len(values) != 0 {
		t.Fatalf("empty options meta values = %#v", values)
	}
}

func TestOpenCodeEnvBuilderClones(t *testing.T) {
	env := map[string]string{"WAGIE_API_TOKEN": "bearer", "CLEARED": ""}
	req := NewSessionRequest("/tmp/project", WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeEnv(env),
	)))

	env["WAGIE_API_TOKEN"] = "mutated"
	delete(env, "CLEARED")

	meta, err := sessionMetaFromLifecycle(req.Meta)
	if err != nil {
		t.Fatalf("session meta from builder: %v", err)
	}

	want := map[string]string{"WAGIE_API_TOKEN": "bearer", "CLEARED": ""}
	if !maps.Equal(meta.Env, want) {
		t.Fatalf("session env = %#v, want %#v", meta.Env, want)
	}

	// An empty map is a value the request carries, not an omission.
	cleared := NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{})).Meta()
	namespace, ok := cleared[opencodeMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("cleared options namespace = %#v", cleared[opencodeMetaKey])
	}
	values, ok := namespace[metaOptionsKey].(map[string]any)
	if !ok {
		t.Fatalf("cleared options values = %#v", namespace[metaOptionsKey])
	}
	if _, present := values[metaEnvKey]; !present {
		t.Fatalf("cleared options omit %q: %#v", metaEnvKey, values)
	}
}
