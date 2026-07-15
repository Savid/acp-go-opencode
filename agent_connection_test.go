package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
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

func TestLifecycleUpdatesFinishBeforeImmediatePromptAndTurnUpdatesCarryExactRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agent, _, wireClient, peer := newWireCoverageConnection(t)
	nativeClient := newFakeOpenCodeClient()
	nativeClient.createSession = testNativeSession("native-boundary")
	nativeClient.commands = []opencode.NativeCommand{{Name: "review", Description: "Review changes"}}
	agent.runtime = nativeClient

	_, err := peer.Initialize(ctx, acp.InitializeRequest{})
	require.NoError(t, err)
	created, err := peer.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	wireClient.mu.Lock()
	require.Len(t, wireClient.updates, 1, "lifecycle command discovery must finish before session/new returns")
	require.Nil(t, wireClient.updates[0].Meta)
	require.NotNil(t, wireClient.updates[0].Update.AvailableCommandsUpdate)
	wireClient.mu.Unlock()

	turn := 0
	nativeClient.sendMessage = func(ctx context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		turn++
		messageID := fmt.Sprintf("assistant-%d", turn)
		partID := fmt.Sprintf("part-%d", turn)
		streamed := fmt.Sprintf("stream-%d", turn)
		nativeClient.events <- opencode.Event{
			Type:       eventMessageUpdated,
			Properties: json.RawMessage(fmt.Sprintf(`{"info":{"id":%q,"sessionID":%q,"role":"assistant"}}`, messageID, id)),
		}
		nativeClient.events <- opencode.Event{
			Type: eventMessagePartCreated,
			Properties: json.RawMessage(fmt.Sprintf(
				`{"id":%q,"sessionID":%q,"messageID":%q,"type":"text","text":%q}`,
				partID, id, messageID, streamed,
			)),
		}

		for {
			wireClient.mu.Lock()
			seen := false
			for _, notification := range wireClient.updates {
				chunk := notification.Update.AgentMessageChunk
				if chunk != nil && chunk.MessageId != nil && *chunk.MessageId == messageID {
					seen = true

					break
				}
			}
			wireClient.mu.Unlock()
			if seen {
				break
			}

			select {
			case <-ctx.Done():
				return opencode.NativeMessage{}, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}

		return opencode.NativeMessage{
			Info: opencode.NativeMessageInfo{ID: messageID, SessionID: id, Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{
				ID: partID, SessionID: id, MessageID: messageID, Type: partTypeText, Text: streamed + "-terminal",
			}},
		}, nil
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
			require.Len(t, notification.Meta, 1)
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
