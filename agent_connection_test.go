package opencodeacp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"errors"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"sync"

	"time"
)

func TestInitializeAdvertisesRouteEnvelopeV1(t *testing.T) {
	response, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	capability, ok := response.AgentCapabilities.Meta[routeEnvelopeKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []int{1}, capability["versions"])
}

func TestScopedElicitationStampsExactRouteAndRejectsCollision(t *testing.T) {
	requestIDValue := acp.RequestIdStr("question-1")
	requestID := acp.RequestId{Str: &requestIDValue}
	form := acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{
		Message: "choose", RequestedSchema: acp.UnstableElicitationSchema{},
		Meta: map[string]any{"native": "preserved"},
	}}
	raw, err := scopedElicitationParams(form, elicitationScope{SessionID: "s", TurnNonce: "nonce", RequestID: &requestID})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	meta, ok := payload["_meta"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "preserved", meta["native"])
	route, ok := meta[routeEnvelopeKey].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 1, route["version"])
	require.Equal(t, "s", route["sessionId"])
	require.Equal(t, "nonce", route["turnNonce"])
	require.Equal(t, "question-1", route["requestId"])
	require.NotContains(t, route, "toolCallId")

	form.Form.Meta[routeEnvelopeKey] = map[string]any{"version": 99}
	_, err = scopedElicitationParams(form, elicitationScope{SessionID: "s", TurnNonce: "nonce", RequestID: &requestID})
	require.ErrorContains(t, err, "reserved key")

	urlRequest := acp.NewUnstableCreateElicitationRequestUrl("e1", "https://example.com")
	_, err = scopedElicitationParams(urlRequest, elicitationScope{SessionID: "s", TurnNonce: "nonce", ToolCallID: "tool-1"})
	require.NoError(t, err)
}

func TestLocalConnectionRequiresInitializeAndStrictCancelRoute(t *testing.T) {
	agent := NewAgent()
	conn := newLocalAgentConnection(agent, io.Discard, strings.NewReader(""))
	_, reqErr := conn.handle(context.Background(), acp.AgentMethodSessionList, json.RawMessage(`{}`))
	require.NotNil(t, reqErr)
	_, reqErr = conn.handle(context.Background(), acp.AgentMethodInitialize, mustJSON(t, acp.InitializeRequest{}))
	require.Nil(t, reqErr)
	_, reqErr = conn.handle(context.Background(), acp.AgentMethodSessionCancel, mustJSON(t, acp.CancelNotification{SessionId: "missing"}))
	require.NotNil(t, reqErr)
	require.Contains(t, reqErr.Data, "reason")
}

type wireCoverageClient struct {
	noopACPClient
	mu           sync.Mutex
	updates      []acp.SessionNotification
	permissions  []acp.RequestPermissionRequest
	elicitations []acp.UnstableCreateElicitationRequest
	extensions   []string
}

func (client *wireCoverageClient) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	client.mu.Lock()
	client.permissions = append(client.permissions, request)
	client.mu.Unlock()

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (client *wireCoverageClient) SessionUpdate(_ context.Context, update acp.SessionNotification) error {
	client.mu.Lock()
	client.updates = append(client.updates, update)
	client.mu.Unlock()

	return nil
}

func (client *wireCoverageClient) UnstableCreateElicitation(_ context.Context, request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	client.mu.Lock()
	client.elicitations = append(client.elicitations, request)
	client.mu.Unlock()

	return acp.NewUnstableCreateElicitationResponseDecline(), nil
}

func (*wireCoverageClient) UnstableCompleteElicitation(context.Context, acp.UnstableCompleteElicitationNotification) error {
	return nil
}

func (*wireCoverageClient) UnstableConnectMcp(context.Context, acp.UnstableConnectMcpRequest) (acp.UnstableConnectMcpResponse, error) {
	return acp.UnstableConnectMcpResponse{}, nil
}

func (*wireCoverageClient) UnstableDisconnectMcp(context.Context, acp.UnstableDisconnectMcpRequest) (acp.UnstableDisconnectMcpResponse, error) {
	return acp.UnstableDisconnectMcpResponse{}, nil
}

func (client *wireCoverageClient) HandleExtensionMethod(_ context.Context, method string, _ json.RawMessage) (any, error) {
	client.mu.Lock()
	client.extensions = append(client.extensions, method)
	client.mu.Unlock()

	return map[string]any{}, nil
}

func newWireCoverageConnection(t *testing.T) (*Agent, *localAgentConnection, *wireCoverageClient) {
	t.Helper()
	agent := NewAgent()
	client := &wireCoverageClient{}
	clientToAgentReader, clientToAgentWriter := io.Pipe()
	agentToClientReader, agentToClientWriter := io.Pipe()
	local := newLocalAgentConnection(agent, agentToClientWriter, clientToAgentReader)
	agent.setAgentClient(local)
	_ = acp.NewClientSideConnection(client, clientToAgentWriter, agentToClientReader)
	t.Cleanup(func() {
		_ = clientToAgentWriter.Close()
		_ = clientToAgentReader.Close()
		_ = agentToClientWriter.Close()
		_ = agentToClientReader.Close()
	})

	return agent, local, client
}

func TestLocalAgentConnectionOutboundClientMethods(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agent, local, client := newWireCoverageConnection(t)

	form := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{})
	_, err := local.UnstableCreateElicitation(ctx, form)
	require.ErrorContains(t, err, "incomplete")
	requestIDValue := acp.RequestIdStr("request")
	response, err := local.CreateElicitation(ctx, form, elicitationScope{
		SessionID: "session", TurnNonce: "nonce", RequestID: &acp.RequestId{Str: &requestIDValue},
	})
	require.NoError(t, err)
	require.NotNil(t, response.Decline)
	url := acp.NewUnstableCreateElicitationRequestUrl("elicitation", "https://example.test/open")
	response, err = local.CreateElicitation(ctx, url, elicitationScope{
		SessionID: "session", TurnNonce: "nonce", ToolCallID: "tool",
	})
	require.NoError(t, err)
	require.NotNil(t, response.Decline)

	_, err = local.RequestPermission(ctx, acp.RequestPermissionRequest{SessionId: "session", Options: []acp.PermissionOption{}})
	require.NoError(t, err)
	require.NoError(t, local.SessionUpdate(ctx, acp.SessionNotification{SessionId: "session", Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{SessionUpdate: "agent_message_chunk", Content: acp.TextBlock("hello")},
	}}))
	require.Error(t, local.NotifyExtension(ctx, "bad", map[string]any{}))
	require.NoError(t, local.NotifyExtension(ctx, "_test/notification", map[string]any{"ok": true}))

	require.Eventually(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()

		return len(client.extensions) == 1
	}, time.Second, time.Millisecond)
	client.mu.Lock()
	require.Len(t, client.permissions, 1)
	require.Len(t, client.updates, 1)
	require.Len(t, client.elicitations, 2)
	require.NotNil(t, client.elicitations[0].Form)
	require.Equal(t, map[string]any{
		"version":   float64(1),
		"sessionId": "session",
		"turnNonce": "nonce",
		"requestId": "request",
	}, client.elicitations[0].Form.Meta[routeEnvelopeKey])
	require.NotNil(t, client.elicitations[1].Url)
	require.Equal(t, map[string]any{
		"version":    float64(1),
		"sessionId":  "session",
		"turnNonce":  "nonce",
		"toolCallId": "tool",
	}, client.elicitations[1].Url.Meta[routeEnvelopeKey])
	require.Equal(t, []string{"_test/notification"}, client.extensions)
	client.mu.Unlock()

	require.NoError(t, agent.Close())
	_, err = local.RequestPermission(ctx, acp.RequestPermissionRequest{})
	require.Error(t, err)
}

type failingWireWriter struct{}

func (failingWireWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestPostResponseWriterAllWireShapes(t *testing.T) {
	var hooks []acp.SessionId
	writer := newPostResponseWriter(io.Discard, func(id acp.SessionId) func() {
		hooks = append(hooks, id)

		return func() {}
	})

	for _, line := range [][]byte{
		[]byte("not json"),
		[]byte(`{"id":1}`),
		[]byte(`{"id":1,"method":"unknown","params":{}}`),
		[]byte(`{"id":2,"method":"session/load","params":{}}`),
		[]byte(`{"id":3,"method":"session/new","params":{}}`),
		[]byte(`{"id":"resume","method":"session/resume","params":{"sessionId":"existing"}}`),
	} {
		writer.observeRequestLine(line)
	}

	for _, line := range [][]byte{
		[]byte("not json"),
		[]byte(`{"id":3,"method":"notification"}`),
		[]byte(`{"id":999,"result":{}}`),
		[]byte(`{"id":3,"error":{"code":-1}}`),
	} {
		require.Empty(t, writer.hooksForResponseLine(line))
	}

	writer.observeRequestLine([]byte(`{"id":4,"method":"session/new","params":{}}`))
	require.Empty(t, writer.hooksForResponseLine([]byte(`{"id":4,"result":"bad"}`)))
	writer.observeRequestLine([]byte(`{"id":5,"method":"session/new","params":{}}`))
	require.Empty(t, writer.hooksForResponseLine([]byte(`{"id":5,"result":{}}`)))
	writer.observeRequestLine([]byte(`{"id":6,"method":"session/new","params":{}}`))
	require.Len(t, writer.hooksForResponseLine([]byte(`{"id":6,"result":{"sessionId":"new"}}`)), 1)
	require.Len(t, writer.hooksForResponseLine([]byte(`{"id":"resume","result":{}}`)), 1)
	require.Equal(t, []acp.SessionId{"new", "existing"}, hooks)

	withoutHook := newPostResponseWriter(io.Discard, nil)
	require.Nil(t, withoutHook.hooksForResponseLine([]byte(`{"id":1,"result":{}}`)))

	failing := newPostResponseWriter(failingWireWriter{}, nil)
	_, err := failing.Write([]byte("payload"))
	require.ErrorContains(t, err, "write failed")
	_, err = writer.Write([]byte(`{"id":100,"result":{}}`))
	require.NoError(t, err)

	_, ok := jsonRPCIDKey(nil)
	require.False(t, ok)
	_, ok = jsonRPCIDKey(json.RawMessage(`{`))
	require.False(t, ok)
	key, ok := jsonRPCIDKey(json.RawMessage(` { "a" : 1 } `))
	require.True(t, ok)
	require.Equal(t, `{"a":1}`, key)
}

func TestPostResponseInputFramingAndGenericLifecycleHandlers(t *testing.T) {
	var hookCalls []acp.SessionId
	writer := newPostResponseWriter(io.Discard, func(id acp.SessionId) func() {
		hookCalls = append(hookCalls, id)

		return func() {}
	})
	gate := newConnectionInputGate(strings.NewReader(""), writer)
	gate.observeInput([]byte(`{"id":1,"method":"session/new","params":{}}`))
	gate.observeInput([]byte("\n"))
	require.Contains(t, writer.lifecycle, "1")

	success := localLifecycleResponse(func(_ *Agent, _ context.Context, request acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
		return acp.LoadSessionResponse{Meta: map[string]any{"id": request.SessionId}}, nil
	})
	result, reqErr := success(context.Background(), NewAgent(), json.RawMessage(`{"sessionId":"session","cwd":"/repo","mcpServers":[]}`))
	require.Nil(t, reqErr)
	require.NotNil(t, result)
	_, reqErr = success(context.Background(), NewAgent(), json.RawMessage(`{`))
	require.NotNil(t, reqErr)

	failure := localLifecycleResponse(func(*Agent, context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
		return acp.LoadSessionResponse{}, errors.New("failed")
	})
	_, reqErr = failure(context.Background(), NewAgent(), json.RawMessage(`{"sessionId":"session","cwd":"/repo","mcpServers":[]}`))
	require.NotNil(t, reqErr)

	notification := localNotification(func(*Agent, context.Context, acp.CancelNotification) error {
		return errors.New("cancel failed")
	})
	_, reqErr = notification(context.Background(), NewAgent(), mustJSON(t, CancelRequest("session", "nonce")))
	require.NotNil(t, reqErr)
}
