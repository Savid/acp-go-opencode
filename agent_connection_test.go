package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestInitializeAdvertisesRouteEnvelopeV1(t *testing.T) {
	response, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)
	capability, ok := response.AgentCapabilities.Meta[routeEnvelopeKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1, capability["version"])
}

func TestScopedElicitationStampsExactRouteAndRejectsCollision(t *testing.T) {
	requestIDValue := acp.RequestIdStr("question-1")
	requestID := acp.RequestId{Str: &requestIDValue}
	form := acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{
		Message: "choose", RequestedSchema: acp.UnstableElicitationSchema{},
		Meta: map[string]any{"native": "preserved"},
	}}
	boundaryNonce := strings.Repeat("n", routeTurnNonceMaxBytes)
	raw, err := scopedElicitationParams(form, elicitationScope{SessionID: "s", TurnNonce: boundaryNonce, RequestID: &requestID})
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
	require.Equal(t, boundaryNonce, route["turnNonce"])
	require.Equal(t, "question-1", route["requestId"])
	require.NotContains(t, route, "toolCallId")

	oversizeForm := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{})
	_, err = scopedElicitationParams(oversizeForm, elicitationScope{
		SessionID: "s", TurnNonce: strings.Repeat("n", routeTurnNonceMaxBytes+1), RequestID: &requestID,
	})
	require.ErrorContains(t, err, "maximum size")

	form.Form.Meta[routeEnvelopeKey] = map[string]any{"version": 99}
	_, err = scopedElicitationParams(form, elicitationScope{SessionID: "s", TurnNonce: "nonce", RequestID: &requestID})
	require.ErrorContains(t, err, "reserved key")

	urlRequest := acp.NewUnstableCreateElicitationRequestUrl("e1", "https://example.com")
	_, err = scopedElicitationParams(urlRequest, elicitationScope{SessionID: "s", TurnNonce: "nonce", ToolCallID: "tool-1"})
	require.NoError(t, err)
}

func TestHostRequestRegistrationRequiresTheExactFullyWrittenFrame(t *testing.T) {
	frame := func(method, streamID, actionID string) []byte {
		return fmt.Appendf(nil,
			`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"_meta":{%q:{"version":1,"streamId":%q,"action":{"actionId":%q}}}}}`,
			method, lifecycle.MetaKey, streamID, actionID,
		)
	}

	for _, method := range []string{acp.ClientMethodSessionRequestPermission, acp.ClientMethodElicitationCreate} {
		t.Run(method, func(t *testing.T) {
			registrations := newHostRequestRegistrations()
			oldKey := hostRequestKey{method: method, streamID: "stream-old", actionID: "action-1"}
			freshKey := hostRequestKey{method: method, streamID: "stream-fresh", actionID: "action-1"}
			oldRegistered := registrations.expect(oldKey)
			freshRegistered := registrations.expect(freshKey)

			var output bytes.Buffer
			written, err := registrations.wrap(&output).Write(frame(method, oldKey.streamID, oldKey.actionID))
			require.NoError(t, err)
			require.Equal(t, output.Len(), written)
			require.NoError(t, <-oldRegistered)
			select {
			case err := <-freshRegistered:
				t.Fatalf("old frame released fresh registration: %v", err)
			default:
			}

			registrations.failIfPending(freshKey, errors.New("test cleanup"))
			require.ErrorContains(t, <-freshRegistered, "test cleanup")
		})
	}

	for name, writer := range map[string]io.Writer{
		"short write": shortWriter{},
		"write error": failingWriter{err: errors.New("transport gone")},
	} {
		t.Run(name, func(t *testing.T) {
			registrations := newHostRequestRegistrations()
			key := hostRequestKey{
				method: acp.ClientMethodSessionRequestPermission, streamID: "stream-1", actionID: "action-1",
			}
			registered := registrations.expect(key)
			_, writeErr := registrations.wrap(writer).Write(frame(key.method, key.streamID, key.actionID))
			require.Error(t, writeErr)
			select {
			case err := <-registered:
				t.Fatalf("incomplete transport write released registration: %v", err)
			default:
			}

			registrations.failIfPending(key, errors.New("transport incomplete"))
			require.ErrorContains(t, <-registered, "transport incomplete")
		})
	}
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
	requireInvalidParamsData(t, reqErr,
		map[string]any{jsonFieldError: valMissing, jsonFieldField: routeMetaPath})
}

func TestLocalConnectionRejectsClosedBeforeDispatchOrDecode(t *testing.T) {
	agent := NewAgent()
	require.NoError(t, agent.Close())

	tests := []struct {
		name        string
		initialized bool
		method      string
		params      json.RawMessage
	}{
		{name: "before initialization gate", method: acp.AgentMethodSessionList},
		{name: "initialize", method: acp.AgentMethodInitialize, params: json.RawMessage(`{bad`)},
		{name: "unknown stable method", initialized: true, method: "unknown"},
		{name: "unknown extension method", initialized: true, method: "_unknown"},
		{name: "known malformed params", initialized: true, method: acp.AgentMethodAuthenticate, params: json.RawMessage(`{bad`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := &localAgentConnection{agent: agent}
			conn.initialized.Store(tc.initialized)

			_, reqErr := conn.handle(context.Background(), tc.method, tc.params)
			require.NotNil(t, reqErr)
			require.Equal(t, -32600, reqErr.Code)
			require.Equal(t, map[string]any{jsonFieldError: valAgentClosed}, reqErr.Data)
		})
	}
}

func TestSDKTransportDiagnosticsAreClosedAndRedacted(t *testing.T) {
	const (
		frameSecret  = "FRAME_SECRET_SENTINEL"
		readerSecret = "READER_SECRET_SENTINEL"
		writerSecret = "WRITER_SECRET_SENTINEL"
	)

	var logs bytes.Buffer
	agent := NewAgent()
	agent.log = slog.New(slog.NewTextHandler(&logs, nil))
	input := io.MultiReader(
		strings.NewReader("{\"broken\":\""+frameSecret+"\"\n"+
			`{"jsonrpc":"2.0","params":{"payload":"`+frameSecret+`"}}`+"\n"+
			`{"jsonrpc":"2.0","id":1,"params":{"payload":"`+frameSecret+`"}}`+"\n"),
		errorReader{err: errors.New(readerSecret)},
	)
	var wire bytes.Buffer
	connection := newLocalAgentConnection(agent, &wire, input)
	<-connection.Done()

	require.NotContains(t, logs.String(), frameSecret)
	require.NotContains(t, logs.String(), readerSecret)
	require.NotContains(t, wire.String(), frameSecret)
	require.NotContains(t, wire.String(), readerSecret)

	connection = newLocalAgentConnection(agent,
		failingWriter{err: errors.New(writerSecret)},
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`+"\n"),
	)
	<-connection.Done()
	require.NotContains(t, logs.String(), writerSecret)
}

type wireCoverageClient struct {
	noopACPClient
	mu           sync.Mutex
	updates      []acp.SessionNotification
	permissions  []acp.RequestPermissionRequest
	elicitations []acp.UnstableCreateElicitationRequest
	extensions   []string
	order        []string
}

func (client *wireCoverageClient) RequestPermission(_ context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	client.mu.Lock()
	client.permissions = append(client.permissions, request)
	client.order = append(client.order, "permission:"+string(request.ToolCall.ToolCallId))
	client.mu.Unlock()

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

func (client *wireCoverageClient) SessionUpdate(_ context.Context, update acp.SessionNotification) error {
	client.mu.Lock()
	client.updates = append(client.updates, update)
	if update.Update.ToolCall != nil {
		client.order = append(client.order, "tool_call:"+string(update.Update.ToolCall.ToolCallId))
	} else if update.Update.ToolCallUpdate != nil {
		client.order = append(client.order, "tool_call_update:"+string(update.Update.ToolCallUpdate.ToolCallId))
	} else {
		client.order = append(client.order, "session_update")
	}
	client.mu.Unlock()

	return nil
}

func (client *wireCoverageClient) UnstableCreateElicitation(_ context.Context, request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
	client.mu.Lock()
	client.elicitations = append(client.elicitations, request)
	var meta map[string]any
	if request.Form != nil {
		meta = request.Form.Meta
	} else if request.Url != nil {
		meta = request.Url.Meta
	}
	if route, ok := meta[routeEnvelopeKey].(map[string]any); ok {
		if toolCallID, ok := route["toolCallId"].(string); ok {
			client.order = append(client.order, "elicitation:"+toolCallID)
		}
	}
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

func newWireCoverageConnection(t *testing.T) (*Agent, *localAgentConnection, *wireCoverageClient, *acp.ClientSideConnection) {
	t.Helper()
	agent := NewAgent()
	client := &wireCoverageClient{}
	clientToAgentReader, clientToAgentWriter := io.Pipe()
	agentToClientReader, agentToClientWriter := io.Pipe()
	local := newLocalAgentConnection(agent, agentToClientWriter, clientToAgentReader)
	agent.setAgentClient(local)
	peer := acp.NewClientSideConnection(client, clientToAgentWriter, agentToClientReader)
	t.Cleanup(func() {
		_ = clientToAgentWriter.Close()
		_ = clientToAgentReader.Close()
		_ = agentToClientWriter.Close()
		_ = agentToClientReader.Close()
	})

	return agent, local, client, peer
}

func TestLocalAgentConnectionOutboundClientMethods(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agent, local, client, _ := newWireCoverageConnection(t)

	form := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{})
	_, err := local.UnstableCreateElicitation(ctx, form)
	require.ErrorContains(t, err, "out-of-prompt elicitation requires lifecycle correlation")
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

func TestCommandCatalogFollowsTheResponseAndTurnUpdatesCarryExactRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agent, _, wireClient, peer := newWireCoverageConnection(t)
	nativeClient := newFakeOpenCodeClient(t)
	nativeClient.createSession = testNativeSession("native-boundary")
	nativeClient.commands = []opencode.NativeCommand{{Name: "review", Description: "Review changes"}}
	agent.runtime = nativeClient

	_, err := peer.Initialize(ctx, acp.InitializeRequest{})
	require.NoError(t, err)
	created, err := peer.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	// The initial catalog is owed to the host only once the establishing
	// response has been written, so it lands after session/new returns rather
	// than before it.
	require.Eventually(t, func() bool {
		wireClient.mu.Lock()
		defer wireClient.mu.Unlock()

		return len(wireClient.updates) == 1
	}, time.Second, time.Millisecond, "the command catalog never followed session/new")

	wireClient.mu.Lock()
	require.Nil(t, wireClient.updates[0].Meta)
	require.NotNil(t, wireClient.updates[0].Update.AvailableCommandsUpdate)
	wireClient.mu.Unlock()

	turn := 0
	nativeClient.dispatchMessage = func(ctx context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		turn++
		messageID := fmt.Sprintf("assistant-%d", turn)
		partID := fmt.Sprintf("part-%d", turn)
		streamed := fmt.Sprintf("stream-%d", turn)
		event := opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: json.RawMessage(fmt.Sprintf(
				`{"info":{"id":%q,"sessionID":%q,"role":"assistant","parentID":%q,"finish":"stop"}}`,
				messageID, id, request.MessageID,
			)),
		}
		nativeClient.publishEvent(event)
		event = opencode.Event{
			Type: opencode.EventMessagePartCreated,
			Properties: json.RawMessage(fmt.Sprintf(
				`{"id":%q,"sessionID":%q,"messageID":%q,"type":"text","text":%q}`,
				partID, id, messageID, streamed,
			)),
		}
		nativeClient.publishEvent(event)

		nativeClient.mu.Lock()
		nativeClient.messages = []opencode.NativeMessage{{
			Info: opencode.NativeMessageInfo{ID: messageID, SessionID: id, Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{
				ID: partID, SessionID: id, MessageID: messageID, Type: partTypeText, Text: streamed + "-terminal",
			}},
		}}
		nativeClient.mu.Unlock()
		nativeClient.publishSessionIdle(id)

		return opencode.NativeMessage{}, ctx.Err()
	}

	for index, nonce := range []string{"route-turn-one", "route-turn-two"} {
		wireClient.mu.Lock()
		start := len(wireClient.updates)
		wireClient.mu.Unlock()

		response, promptErr := peer.Prompt(ctx, TextPromptRequest(created.SessionId, nonce, fmt.Sprintf("turn %d", index+1)))
		require.NoError(t, promptErr)
		require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

		wireClient.mu.Lock()
		turnUpdates := append([]acp.SessionNotification(nil), wireClient.updates[start:]...)
		wireClient.mu.Unlock()
		require.NotEmpty(t, turnUpdates)
		for _, notification := range turnUpdates {
			require.Len(t, notification.Meta, 1, "unexpected unscoped turn notification: %#v", notification.Update)
			route, ok := notification.Meta[routeEnvelopeKey].(map[string]any)
			require.True(t, ok)
			require.Len(t, route, 2)
			require.EqualValues(t, routeEnvelopeVersion, route[routeFieldVersion])
			require.Equal(t, nonce, route[routeFieldTurnNonce])
			chunk := notification.Update.AgentMessageChunk
			if chunk != nil {
				require.NotNil(t, chunk.MessageId)
				require.Equal(t, fmt.Sprintf("assistant-%d", index+1), *chunk.MessageId)
			}
		}
	}
}

func TestConnectionInputGateAndGenericResponseHandlers(t *testing.T) {
	gate := newConnectionInputGate(strings.NewReader("payload"))
	readDone := make(chan string, 1)
	go func() {
		buffer := make([]byte, 7)
		n, _ := gate.Read(buffer)
		readDone <- string(buffer[:n])
	}()
	select {
	case <-readDone:
		t.Fatal("connection input gate read before open")
	case <-time.After(10 * time.Millisecond):
	}
	gate.open()
	require.Equal(t, "payload", <-readDone)

	success := localResponse(func(_ *Agent, _ context.Context, request acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
		return acp.LoadSessionResponse{Meta: map[string]any{"id": request.SessionId}}, nil
	})
	result, reqErr := success(context.Background(), NewAgent(), json.RawMessage(`{"sessionId":"session","cwd":"/repo","mcpServers":[]}`))
	require.Nil(t, reqErr)
	require.NotNil(t, result)
	_, reqErr = success(context.Background(), NewAgent(), json.RawMessage(`{`))
	require.NotNil(t, reqErr)

	failure := localResponse(func(*Agent, context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
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

// TestRequestErrorReportsAnHonoredCancelAheadOfATypedRequestError pins the
// discriminator that only a request context can supply: an honored
// $/cancel_request cancels that context with cause context.Canceled, and it
// outranks whatever typed error the aborted work was carrying. Answering a
// withdrawn request with a complaint about its parameters is never honest, so
// -32800 wins over the passthrough.
func TestRequestErrorReportsAnHonoredCancelAheadOfATypedRequestError(t *testing.T) {
	withdrawn, cancel := context.WithCancelCause(t.Context())
	cancel(context.Canceled)

	invalid := acp.NewInvalidParams(map[string]any{jsonFieldError: "cwd must be absolute"})

	for name, err := range map[string]error{
		"typed request error": invalid,
		"wrapped typed error": fmt.Errorf("start session: %w", invalid),
		"joined typed error":  errors.Join(invalid, context.Canceled),
		"plain failure":       errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, -32800, requestError(withdrawn, err).Code)
		})
	}
}

// TestRequestErrorPassesATypedRequestErrorThroughOnALiveContext pins the other
// side of that ordering: with nothing cancelled, the caller still receives the
// exact error value the handler produced rather than a flattened -32603.
func TestRequestErrorPassesATypedRequestErrorThroughOnALiveContext(t *testing.T) {
	live := t.Context()
	invalid := acp.NewInvalidParams(map[string]any{jsonFieldError: "cwd must be absolute"})

	require.Same(t, invalid, requestError(live, invalid))
	require.Same(t, invalid, requestError(live, fmt.Errorf("start session: %w", invalid)))
}

// TestRequestErrorReportsAnExpiredDeadlineAsAnInternalFailure pins that an
// adapter-owned deadline is a failure of the turn, not a withdrawal by the
// client: its cause is context.DeadlineExceeded, which the cancel check
// excludes by name instead of by accident of error matching.
func TestRequestErrorReportsAnExpiredDeadlineAsAnInternalFailure(t *testing.T) {
	expired, cancel := context.WithTimeout(t.Context(), -time.Second)
	defer cancel()

	<-expired.Done()
	require.Equal(t, context.DeadlineExceeded, context.Cause(expired))
	require.Equal(t, -32603, requestError(expired, context.DeadlineExceeded).Code)
	require.Equal(t, -32603, requestError(expired, errors.New("boom")).Code)
}

// TestRequestErrorReportsATornDownConnectionByItsOwnError pins that transport
// teardown is not a cancel. The SDK cancels the parent context with the
// transport cause rather than context.Canceled, so a request killed by it
// reports what actually failed even though the derived context error is
// context.Canceled.
func TestRequestErrorReportsATornDownConnectionByItsOwnError(t *testing.T) {
	tornDown, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("connection closed"))

	require.ErrorIs(t, tornDown.Err(), context.Canceled)

	invalid := acp.NewInvalidParams(map[string]any{jsonFieldError: "cwd must be absolute"})
	require.Same(t, invalid, requestError(tornDown, errors.Join(invalid, context.Canceled)))
	require.Equal(t, -32603, requestError(tornDown, context.Canceled).Code)
}

// Requests pass through exported Serve, SDK defaults, and local dispatch. No
// session is created, so none can launch a native process.
func metadataWireRPC(t *testing.T, options ...Option) func(string, string) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	closePipes := func() {
		_ = inReader.Close()
		_ = inWriter.Close()
		_ = outReader.Close()
		_ = outWriter.Close()
	}
	stop := context.AfterFunc(ctx, closePipes)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, inReader, outWriter, options...) }()
	t.Cleanup(func() { cancel(); closePipes(); stop(); <-done })
	decoder := json.NewDecoder(outReader)
	id := 0

	return func(method, params string) map[string]json.RawMessage {
		t.Helper()
		id++
		_, err := fmt.Fprintf(inWriter, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":%q,\"params\":%s}\n", id, method, params)
		require.NoError(t, err)
		var response map[string]json.RawMessage
		require.NoError(t, decoder.Decode(&response))
		require.JSONEq(t, fmt.Sprint(id), string(response["id"]))

		return response
	}
}

func requireWireMetadataError(t *testing.T, response map[string]json.RawMessage, code int, token, field string) {
	t.Helper()
	var failure acp.RequestError
	require.NoError(t, json.Unmarshal(response["error"], &failure))
	require.Equal(t, code, failure.Code)
	if code == -32602 {
		require.Equal(t, "Invalid params", failure.Message)
	}
	want := map[string]any{"error": token}
	if field != "" {
		want["field"] = field
	}
	require.Equal(t, want, failure.Data)
}

func TestServePreservesLifecycleMetadata(t *testing.T) {
	const owned = `"acp-go.dev/lifecycle":`
	for _, row := range []struct{ name, metadata, field string }{
		{"valid", `"_meta":{` + owned + `{"version":1}}`, ""},
		{"foreign duplicates", `"_meta":{"foreign":{"a":0,"a":1}},"_meta":{"foreign":true}`, ""},
		{"duplicate version", `"_meta":{` + owned + `{"version":2,"version":1}}`, ".version"},
		{"SDK alias", `"_META":{` + owned + `{"version":2,"version":1}}`, ".version"},
		{"aliased erasure", `"_META":{` + owned + `{"version":1}},"_meta":null`, "root"},
		{"rounded fraction", `"_meta":{` + owned + `{"version":1.0000000000000001}}`, ".version"},
		{"decimal", `"_meta":{` + owned + `{"version":1.0}}`, ".version"},
		{"exponent", `"_meta":{` + owned + `{"version":1e0}}`, ".version"},
		{"overflow", `"_meta":{` + owned + `{"version":1e400}}`, ".version"},
		{"duplicate namespace", `"_meta":{` + owned + `{"version":2},` + owned + `{"version":1}}`, "root"},
		{"erased namespace", `"_meta":{` + owned + `{"version":2}},"_meta":{}`, "root"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rpc := metadataWireRPC(t)
			response := rpc("initialize", `{"protocolVersion":1,`+row.metadata+`}`)
			if row.field == "" {
				require.NotContains(t, response, "error")

				return
			}
			field := lifecycle.MetaPath
			if row.field != "root" {
				field += row.field
			}
			requireWireMetadataError(t, response, -32602, "unsupported", field)
		})
	}
	t.Run("construction precedes owned decode", func(t *testing.T) {
		rpc := metadataWireRPC(t, WithInputHandoffRoot("relative"))
		response := rpc("initialize", `{"protocolVersion":1,"_meta":{`+owned+`{"version":1e400}}}`)
		requireWireMetadataError(t, response, -32603, "opencode_invalid_options", "")
	})
}

func TestServePromptLifecyclePreservesMembersAndRoutePrecedence(t *testing.T) {
	rpc := metadataWireRPC(t)
	require.NotContains(t, rpc("initialize", `{"protocolVersion":1,"_meta":{"acp-go.dev/lifecycle":{"version":1}}}`), "error")
	const route = `"acp-go.dev/route":{"version":1,"turnNonce":"nonce"}`
	for _, row := range []struct{ value, suffix string }{
		{`{"version":2,"version":1,"submission":{"submissionId":"s","clientNonce":"n"}}`, ".version"},
		{`{"version":1.0000000000000001,"submission":{"submissionId":"s","clientNonce":"n"}}`, ".version"},
		{`{"version":1,"submission":{"submissionId":"old","submissionId":"s","clientNonce":"n"}}`, ".submission.submissionId"},
	} {
		response := rpc("session/prompt", `{"sessionId":"missing","prompt":[],"_meta":{`+route+`,"acp-go.dev/lifecycle":`+row.value+`}}`)
		requireWireMetadataError(t, response, -32602, "unsupported", lifecycle.MetaPath+row.suffix)
	}
	response := rpc("session/prompt", `{"sessionId":"missing","prompt":[],"_meta":{"acp-go.dev/lifecycle":{"version":1e400}}}`)
	requireWireMetadataError(t, response, -32602, "missing", routeMetaPath)
	response = rpc("session/prompt", `{"sessionId":"missing","prompt":[],"_meta":{`+route+`}}`)
	requireWireMetadataError(t, response, -32602, "missing", lifecycle.MetaPath)
	response = rpc("session/new", `{"cwd":"/unused","mcpServers":[],"_META":{"acp-go.dev/lifecycle":{"version":1}},"_meta":{}}`)
	requireWireMetadataError(t, response, -32602, "unsupported", lifecycle.MetaPath)
}

func TestServeRejectsInexactRouteNumbers(t *testing.T) {
	rpc := metadataWireRPC(t)
	require.NotContains(t, rpc("initialize", `{"protocolVersion":1}`), "error")
	for _, number := range []string{"1.0000000000000001", "1e400", "-1e-400"} {
		response := rpc("session/prompt", `{"sessionId":"missing","prompt":[],"_META":{"acp-go.dev/route":{"version":`+number+`,"turnNonce":"nonce"}}}`)
		requireWireMetadataError(t, response, -32602, "unsupported", routeMemberPath(routeFieldVersion))
	}
	// Route uses numeric value, without lifecycle's scalar spelling policy.
	for _, number := range []string{"1.0", "1e0"} {
		response := rpc("session/prompt", `{"sessionId":"missing","prompt":[],"_meta":{"acp-go.dev/route":{"version":`+number+`,"turnNonce":"nonce"}}}`)
		requireWireMetadataError(t, response, -32602, valSessionUnknown, "sessionId")
	}
}

func TestSDKDecodedHandoffPreservesExactNumbers(t *testing.T) {
	root := t.TempDir()
	png := fixtureImage(t, "valid.png")
	path := writeHandoffFile(t, root, "input.png", png)
	session := handoffSession(t, root)
	for _, row := range []struct{ name, version, size, cause string }{
		{"version fraction", "1.0000000000000001", "1", handoffCauseVersion},
		{"size fraction", "1", "1.0000000000000001", handoffCauseSizeBytes},
		{"negative underflow", "1", "-1e-400", handoffCauseSizeBytes},
		{"size overflow", "1", "1e400", handoffCauseSizeBytes},
		{"exact alternate spelling", "1e0", fmt.Sprint(len(png)) + ".0", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			params := fmt.Appendf(nil, `{"sessionId":"s","prompt":[{"type":"image","mimeType":"image/png","data":"","uri":%q,"_META":{"acp-go.dev/handoff":{"version":%s,"sizeBytes":%s,"digest":%q}}}]}`, "file://"+testURIPath(path), row.version, row.size, handoffDigestHex(png))
			request, refusal := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](params)
			require.Nil(t, refusal)
			if row.cause != "" {
				requireHandoffVerdict(t, session, request.Prompt[0], imageErrorInvalidHandoff, row.cause)

				return
			}
			require.NoError(t, validatePromptMediaError(session, request.Prompt[0]))
		})
	}
	params := fmt.Appendf(nil, `{"sessionId":"s","prompt":[{"type":"image","mimeType":"image/png","data":%q,"_meta":{"acp-go.dev/handoff":{"version":1e400,"sizeBytes":-1e-400}}}]}`, fixtureImageBase64(t, "valid.png"))
	request, refusal := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](params)
	require.Nil(t, refusal)
	require.NoError(t, validatePromptMediaError(session, request.Prompt[0]), "nonempty embedded data wins before handoff validation")
}
