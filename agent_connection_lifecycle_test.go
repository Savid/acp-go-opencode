package opencodeacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

func TestServeContextAndInputDone(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(cancelled, strings.NewReader(""), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve canceled error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Serve(ctx, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("Serve EOF error = %v", err)
	}
}

func TestLocalAgentConnectionHandleRoutesAndErrors(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentPrompts: 1, MaxConcurrentClientCalls: 1}))
	conn := &localAgentConnection{agent: agent}

	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionList, json.RawMessage(`{}`)); reqErr == nil {
		t.Fatal("uninitialized session/list unexpectedly succeeded")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("malformed initialize unexpectedly succeeded")
	}
	initResp, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, mustJSON(t, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			PositionEncodings: []acp.PositionEncodingKind{acp.PositionEncodingKindUtf8},
		},
	}))
	if reqErr != nil {
		t.Fatalf("initialize reqErr = %v", reqErr)
	}
	if initResp.(acp.InitializeResponse).AgentCapabilities.PositionEncoding == nil {
		t.Fatalf("initialize response = %#v", initResp)
	}
	if _, reqErr := conn.handle(ctx, "missing/method", json.RawMessage(`{}`)); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("missing method reqErr = %#v", reqErr)
	}
	if _, reqErr := conn.handle(ctx, "_missing/method", json.RawMessage(`{}`)); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("missing extension reqErr = %#v", reqErr)
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionNew, mustJSON(t, acp.NewSessionRequest{})); reqErr == nil {
		t.Fatal("invalid new-session params unexpectedly succeeded")
	}
	if _, reqErr := conn.handle(ctx, ForkSessionMethod, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("malformed extension fork unexpectedly succeeded")
	}
	if _, reqErr := conn.handle(ctx, ForkSessionMethod, mustJSON(t, acp.UnstableForkSessionRequest{})); reqErr == nil {
		t.Fatal("invalid extension fork unexpectedly succeeded")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionCancel, json.RawMessage(`{`)); reqErr == nil {
		t.Fatal("malformed notification unexpectedly succeeded")
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionCancel, mustJSON(t, acp.CancelNotification{SessionId: "missing"})); reqErr == nil {
		t.Fatal("cancel notification error was not surfaced")
	}
	fakeClient := newFakeOpenCodeClient()
	fakeSession := testSession(agent, fakeClient)
	agent.mu.Lock()
	agent.sessions[fakeSession.id] = fakeSession
	agent.mu.Unlock()
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionCancel, mustJSON(t, acp.CancelNotification{SessionId: fakeSession.id})); reqErr != nil {
		t.Fatalf("cancel notification reqErr = %#v", reqErr)
	}
	if _, reqErr := conn.handle(ctx, acp.AgentMethodSessionSetMode, mustJSON(t, acp.SetSessionModeRequest{})); reqErr == nil || reqErr.Code != -32601 {
		t.Fatalf("set mode reqErr = %#v", reqErr)
	}
}

func TestLocalAgentConnectionClientCallErrors(t *testing.T) {
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentPrompts: 1, MaxConcurrentClientCalls: 1}))
	conn := &localAgentConnection{agent: agent}

	if err := conn.NotifyExtension(context.Background(), "bad/method", nil); err == nil {
		t.Fatal("NotifyExtension accepted stable method name")
	}
	agent.clientCalls <- struct{}{}
	if _, err := conn.RequestPermission(context.Background(), acp.RequestPermissionRequest{}); err == nil {
		t.Fatal("RequestPermission ignored client-call backpressure")
	}
	if err := conn.SessionUpdate(context.Background(), acp.SessionNotification{}); err == nil {
		t.Fatal("SessionUpdate ignored client-call backpressure")
	}
	if err := conn.NotifyExtension(context.Background(), "_test/event", nil); err == nil {
		t.Fatal("NotifyExtension ignored client-call backpressure")
	}
	if _, err := conn.CreateElicitation(context.Background(), acp.UnstableCreateElicitationRequest{}, elicitationScope{}); err == nil {
		t.Fatal("CreateElicitation accepted empty request before backpressure")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.RequestPermission(cancelled, acp.RequestPermissionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RequestPermission canceled error = %v", err)
	}
	<-agent.clientCalls
}

func TestRequestErrorAndCapabilityHelpers(t *testing.T) {
	if requestError(nil) != nil {
		t.Fatal("requestError(nil) returned non-nil")
	}
	reqErr := acp.NewInvalidParams(map[string]any{"x": "y"})
	if requestError(reqErr) != reqErr {
		t.Fatal("requestError did not preserve request error")
	}
	if got := requestError(context.Canceled); got == nil || got.Code != -32800 {
		t.Fatalf("requestError canceled = %#v", got)
	}
	if got := requestError(errors.New("plain")); got == nil || got.Code != -32603 {
		t.Fatalf("requestError plain = %#v", got)
	}
	requestIDStr := acp.RequestIdStr("request")
	requestID := acp.RequestId{Str: &requestIDStr}
	urlParams, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: "e",
			Message:       "open",
			Url:           "https://example.test",
			Meta:          map[string]any{"k": "v"},
		},
	}, elicitationScope{SessionID: "s", ToolCallID: "tool", RequestID: &requestID})
	if err != nil || !strings.Contains(string(urlParams), `"requestId":"request"`) {
		t.Fatalf("url scoped elicitation = %s err=%v", urlParams, err)
	}
	if selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf8}) != acp.PositionEncodingKindUtf8 {
		t.Fatal("utf8 position encoding not selected")
	}
	if selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf16}) != acp.PositionEncodingKindUtf16 {
		t.Fatal("utf16 position encoding not selected")
	}
	if selectPositionEncoding(nil) != acp.PositionEncodingKindUtf16 {
		t.Fatal("default position encoding mismatch")
	}

	for _, tt := range []struct {
		name     string
		caps     *acp.ElicitationCapabilities
		wantForm bool
		wantURL  bool
	}{
		{name: "nil", caps: nil, wantForm: false, wantURL: false},
		{name: "empty object", caps: &acp.ElicitationCapabilities{}, wantForm: true, wantURL: false},
		{name: "url only", caps: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}, wantForm: false, wantURL: true},
		{name: "form explicit", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}, wantForm: true, wantURL: false},
	} {
		t.Run("elicitation "+tt.name, func(t *testing.T) {
			agent := NewAgent()
			agent.clientCapabilities.Elicitation = tt.caps
			if got := agent.clientSupportsFormElicitation(); got != tt.wantForm {
				t.Fatalf("clientSupportsFormElicitation() = %v, want %v", got, tt.wantForm)
			}
			if got := agent.clientSupportsURLElicitation(); got != tt.wantURL {
				t.Fatalf("clientSupportsURLElicitation() = %v, want %v", got, tt.wantURL)
			}
		})
	}
}

func TestNewLocalAgentConnectionDone(t *testing.T) {
	var output bytes.Buffer
	conn := newLocalAgentConnection(NewAgent(), &output, strings.NewReader(""))
	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatal("local connection did not close after EOF input")
	}
}

func TestLifecycleCommandUpdateAfterResponseBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	agent := NewAgent()
	agent.options.clientFactory = func(_ context.Context, opts openCodeStartOptions) (openCodeClient, error) {
		client := newFakeOpenCodeClient()
		xdg, err := createXDGDirs(t.TempDir(), string(opts.ACPSessionID))
		if err != nil {
			return nil, err
		}
		client.xdg = xdg
		client.createSession = testNativeSession("native-1")
		client.commands = []nativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		return client, nil
	}
	conn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(conn)

	lines := make(chan string, 4)
	go func() {
		scanner := bufio.NewScanner(a2cR)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	writeJSONRPC := func(payload string) {
		t.Helper()
		if _, err := io.WriteString(c2aW, payload+"\n"); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}
	readLine := func() string {
		t.Helper()
		select {
		case line := <-lines:
			return line
		case <-ctx.Done():
			t.Fatal("timed out waiting for JSON-RPC line")
			return ""
		}
	}

	writeJSONRPC(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`)
	if line := readLine(); !strings.Contains(line, `"id":1`) || !strings.Contains(line, `"result"`) {
		t.Fatalf("initialize line = %s", line)
	}
	cwd := t.TempDir()
	writeJSONRPC(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":` + strconv.Quote(cwd) + `,"mcpServers":[]}}`)
	responseLine := readLine()
	if !strings.Contains(responseLine, `"id":2`) || !strings.Contains(responseLine, `"result"`) {
		t.Fatalf("session/new response line = %s", responseLine)
	}
	updateLine := readLine()
	if !strings.Contains(updateLine, `"method":"session/update"`) ||
		!strings.Contains(updateLine, `"sessionUpdate":"available_commands_update"`) ||
		!strings.Contains(updateLine, `"name":"review"`) {
		t.Fatalf("post-response update line = %s", updateLine)
	}
}

func TestPostResponseHookBranches(t *testing.T) {
	ctx := context.Background()
	writer := newPostResponseWriter(errWriter{})
	writer.afterNextWrite(func() { t.Fatal("hook ran after failed write") })
	if _, err := writer.Write([]byte("response\n")); err == nil {
		t.Fatal("postResponseWriter write error = nil")
	}

	handler := localLifecycleResponse(
		func(*Agent, context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
			return acp.NewSessionResponse{}, errors.New("new failed")
		},
		func(_ acp.NewSessionRequest, resp acp.NewSessionResponse) acp.SessionId { return resp.SessionId },
	)
	_, reqErr := handler(ctx, NewAgent(), mustJSON(t, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}}))
	if reqErr == nil || !strings.Contains(reqErr.Message, "Internal error") {
		t.Fatalf("lifecycle call error = %#v", reqErr)
	}

	emptyIDHandler := localLifecycleResponse(
		func(*Agent, context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
			return acp.NewSessionResponse{}, nil
		},
		func(_ acp.NewSessionRequest, resp acp.NewSessionResponse) acp.SessionId { return resp.SessionId },
	)
	result, reqErr := emptyIDHandler(ctx, NewAgent(), mustJSON(t, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}}))
	if reqErr != nil {
		t.Fatalf("empty id handler: %#v", reqErr)
	}
	if _, ok := result.(localPostResponse); ok {
		t.Fatal("empty session id registered post response hook")
	}

	missingAgent := NewAgent()
	missingAgent.refreshCommandsAfterResponse("missing")()

	refreshErrClient := newFakeOpenCodeClient()
	refreshErrClient.commandsErr = errors.New("commands failed")
	refreshErrAgent := NewAgent()
	refreshErrSession := testSession(refreshErrAgent, refreshErrClient)
	refreshErrAgent.sessions[refreshErrSession.id] = refreshErrSession
	refreshErrAgent.refreshCommandsAfterResponse(refreshErrSession.id)()

	parentClient := newFakeOpenCodeClient()
	parentClient.forkSession = testNativeSession("native-child")
	parentAgent := NewAgent()
	parentAgent.options.clientFactory = func(_ context.Context, opts openCodeStartOptions) (openCodeClient, error) {
		child := newFakeOpenCodeClient()
		xdg, err := createXDGDirs(t.TempDir(), string(opts.ACPSessionID))
		if err != nil {
			return nil, err
		}
		child.xdg = xdg
		child.getSession = testNativeSession("native-child")
		return child, nil
	}
	parent := testSession(parentAgent, parentClient)
	parentAgent.sessions[parent.id] = parent
	conn := &localAgentConnection{agent: parentAgent, postWriter: newPostResponseWriter(io.Discard)}
	conn.initialized.Store(true)
	result, reqErr = conn.handle(ctx, ForkSessionMethod, mustJSON(t, ForkSessionRequest(parent.id, t.TempDir())))
	if reqErr != nil {
		t.Fatalf("fork extension handle: %#v", reqErr)
	}
	if _, ok := result.(acp.UnstableForkSessionResponse); !ok {
		t.Fatalf("fork result = %#v", result)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestLocalAgentConnectionClientCallsOverPipes(t *testing.T) {
	ctx := context.Background()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	client := &pipeACPClient{}
	_ = acp.NewClientSideConnection(client, c2aW, a2cR)
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 2}))
	conn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setAgentClient(conn)

	permission, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{Options: []acp.PermissionOption{
		{OptionId: "once", Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow once"},
	}})
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if permission.Outcome.Selected == nil || permission.Outcome.Selected.OptionId != "once" {
		t.Fatalf("permission = %#v", permission)
	}
	if err := conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: "s", Update: acp.UpdateAgentMessageText("hello")}); err != nil {
		t.Fatalf("SessionUpdate: %v", err)
	}
	if err := conn.NotifyExtension(ctx, "_opencode/test", map[string]any{"ok": true}); err != nil {
		t.Fatalf("NotifyExtension: %v", err)
	}
	resp, err := conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "m",
			Mode:    "form",
			RequestedSchema: acp.UnstableElicitationSchema{
				Type: acp.UnstableElicitationSchemaTypeObject,
			},
		},
	})
	if err != nil {
		t.Fatalf("UnstableCreateElicitation: %v", err)
	}
	if resp.Accept == nil {
		t.Fatalf("elicitation resp = %#v", resp)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.updates != 1 || len(client.extensions) != 1 || client.elicitations != 1 {
		t.Fatalf("client state updates=%d extensions=%#v elicitations=%d", client.updates, client.extensions, client.elicitations)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %T: %v", value, err)
	}
	return data
}

type pipeACPClient struct {
	mu           sync.Mutex
	updates      int
	extensions   []string
	elicitations int
}

var _ acp.Client = (*pipeACPClient)(nil)
var _ acp.ClientExperimental = (*pipeACPClient)(nil)
var _ acp.ExtensionMethodHandler = (*pipeACPClient)(nil)

func (*pipeACPClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, nil
}

func (*pipeACPClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, nil
}

func (*pipeACPClient) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("once")}, nil
}

func (c *pipeACPClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates++
	return nil
}

func (*pipeACPClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (*pipeACPClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*pipeACPClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, nil
}

func (*pipeACPClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*pipeACPClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func (*pipeACPClient) UnstableCompleteElicitation(context.Context, acp.UnstableCompleteElicitationNotification) error {
	return nil
}

func (c *pipeACPClient) UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	c.elicitations++
	c.mu.Unlock()
	return acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{}},
	}, nil
}

func (*pipeACPClient) UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error) {
	return acp.UnstableConnectMcpResponse{}, nil
}

func (*pipeACPClient) UnstableDisconnectMcp(context.Context, acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error) {
	return acp.UnstableDisconnectMcpResponse{}, nil
}

func (c *pipeACPClient) HandleExtensionMethod(_ context.Context, method string, _ json.RawMessage) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.extensions = append(c.extensions, method)
	return map[string]any{"ok": true}, nil
}
