package opencodeacp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"encoding/json"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestOutputSchemaAccepted(t *testing.T) {
	schema := map[string]any{"type": "object"}
	meta, err := sessionMetaFromVendorOptions(OpenCodeOptions{OutputSchema: schema}.Meta())
	if err != nil {
		t.Fatalf("outputSchema rejected: %v", err)
	}
	if meta.OutputSchema["type"] != "object" {
		t.Fatalf("output schema meta = %#v", meta.OutputSchema)
	}
	schema["type"] = "mutated"
	if meta.OutputSchema["type"] != "object" {
		t.Fatalf("output schema was not cloned: %#v", meta.OutputSchema)
	}
}

func TestOutputSchemaInvalidRejected(t *testing.T) {
	_, err := sessionMetaFromVendorOptions(map[string]any{
		opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: "not-an-object"}},
	})
	if err == nil {
		t.Fatal("invalid outputSchema unexpectedly accepted")
	}
}

func TestServeCloseErrorAndAgentCloneFallbacks(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(errors.New("close failed"), ErrContainmentIncomplete)
	agent := NewAgent()
	session := testSession(t, agent, client)
	agent.sessions[session.id] = session

	oldNewAgent := newAgentForServe
	newAgentForServe = func(...Option) *Agent { return agent }
	t.Cleanup(func() { newAgentForServe = oldNewAgent })
	err := Serve(ctx, strings.NewReader(""), io.Discard)
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	oldMarshal := agentJSONMarshal
	oldUnmarshal := agentJSONUnmarshal
	t.Cleanup(func() {
		agentJSONMarshal = oldMarshal
		agentJSONUnmarshal = oldUnmarshal
	})
	agentJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
	if cloneClientCapabilities(acp.ClientCapabilities{Meta: map[string]any{"a": "b"}}).Meta["a"] != "b" {
		t.Fatal("cloneClientCapabilities marshal fallback changed caps")
	}
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	if agent.clientElicitationCapabilities() == nil {
		t.Fatal("clientElicitationCapabilities marshal fallback returned nil")
	}
	agentJSONMarshal = oldMarshal
	agentJSONUnmarshal = func([]byte, any) error { return errors.New("unmarshal failed") }
	if cloneClientCapabilities(acp.ClientCapabilities{Meta: map[string]any{"a": "b"}}).Meta["a"] != "b" {
		t.Fatal("cloneClientCapabilities unmarshal fallback changed caps")
	}
	if agent.clientElicitationCapabilities() == nil {
		t.Fatal("clientElicitationCapabilities unmarshal fallback returned nil")
	}
}

func TestAgentCloseAuthAndRawEventHelpers(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	agent := NewAgent()
	session := testSession(t, agent, client)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()
	if _, err := agent.Authenticate(ctx, acp.AuthenticateRequest{}); err == nil {
		t.Fatal("Authenticate accepted unsupported method")
	}
	if _, err := agent.Logout(ctx, acp.LogoutRequest{}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := agent.SetSessionMode(ctx, acp.SetSessionModeRequest{}); err == nil {
		t.Fatal("SetSessionMode accepted")
	}
	session.beginTurn(ctx, "nonce")
	if err := agent.Cancel(ctx, CancelRequest(session.id, "nonce")); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !session.wasCancelled() && client.abortCount() == 0 {
		t.Fatal("Cancel did not touch session/client")
	}
	if err := agent.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !client.closed {
		t.Fatal("client not closed")
	}
	payload, err := capRawEventPayload(map[string]any{
		"sessionId": "s",
		"sequence":  int64(1),
		"source":    "test",
		"event":     strings.Repeat("x", rawEventMaxBytes),
	})
	if err != nil {
		t.Fatalf("cap raw event: %v", err)
	}
	if event, _ := payload["event"].(map[string]any); event["truncated"] != true {
		t.Fatalf("raw event was not capped: %#v", payload)
	}
	if _, err := io.Copy(io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
}

func TestAgentAndRouteRemainingPublicBranches(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, Serve(cancelled, strings.NewReader(""), io.Discard), context.Canceled)

	agent := NewAgent()
	agent.deleted["deleted"] = struct{}{}
	_, err := agent.session("deleted")
	require.Error(t, err)

	_, err = parseInboundTurnRoute(map[string]any{routeEnvelopeKey: map[string]any{
		routeFieldVersion: 999,
		"turnNonce":       "nonce",
	}})
	require.Error(t, err)

	client := newFakeOpenCodeClient()
	client.forkSession = testNativeSession("native-child")
	client.getSession = testNativeSession("native-child")
	client.ensureSyncAggregate("native-child")
	agent.runtime = client
	parent := testSession(t, agent, client)
	parent.cwd = t.TempDir()
	agent.sessions[parent.id] = parent
	request := ForkSessionRequest(parent.id, parent.cwd)
	value, err := agent.HandleExtensionMethod(context.Background(), ForkSessionMethod, mustJSON(t, request))
	require.NoError(t, err)
	require.NotNil(t, value)
}

func TestConnectionRemainingDispatchValidationAndBackpressureBranches(t *testing.T) {
	agent := NewAgent()
	connection := &localAgentConnection{agent: agent}
	connection.initialized.Store(true)
	_, requestErr := connection.handle(context.Background(), "_unknown", json.RawMessage(`{}`))
	require.NotNil(t, requestErr)

	notification := localNotification(func(*Agent, context.Context, acp.CancelNotification) error { return nil })
	_, requestErr = notification(context.Background(), agent, json.RawMessage(`{`))
	require.NotNil(t, requestErr)
	_, requestErr = notification(context.Background(), agent, mustJSON(t, CancelRequest("session", "nonce")))
	require.Nil(t, requestErr)

	_, requestErr = localResponse(func(*Agent, context.Context, acp.PromptRequest) (acp.PromptResponse, error) {
		return acp.PromptResponse{}, nil
	})(context.Background(), agent, json.RawMessage(`{}`))
	require.NotNil(t, requestErr)

	for len(agent.clientCalls) < cap(agent.clientCalls) {
		agent.clientCalls <- struct{}{}
	}
	form := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{})
	_, err := connection.CreateElicitation(context.Background(), form, elicitationScope{SessionID: "session", TurnNonce: "nonce", ToolCallID: "tool"})
	require.Error(t, err)
	_, err = connection.RequestPermission(context.Background(), acp.RequestPermissionRequest{})
	require.Error(t, err)
	require.Error(t, connection.SessionUpdate(context.Background(), acp.SessionNotification{}))
	require.Error(t, connection.NotifyExtension(context.Background(), "_extension", nil))
}

func TestScopedElicitationRemainingURLMetadataAndEncodingBranches(t *testing.T) {
	request := acp.NewUnstableCreateElicitationRequestUrl("id", "https://example.test")
	request.Url.Message = "message"
	request.Url.Meta = map[string]any{"native": true}
	raw, err := scopedElicitationParams(request, elicitationScope{SessionID: "session", TurnNonce: "nonce", ToolCallID: "tool"})
	require.NoError(t, err)
	require.Contains(t, string(raw), "native")

	request.Url.Meta = map[string]any{routeEnvelopeKey: map[string]any{}}
	_, err = scopedElicitationParams(request, elicitationScope{SessionID: "session", TurnNonce: "nonce", ToolCallID: "tool"})
	require.Error(t, err)

	form := acp.NewUnstableCreateElicitationRequestForm(acp.UnstableElicitationSchema{})
	form.Form.Meta = map[string]any{"cannotEncode": func() {}}
	_, err = scopedElicitationParams(form, elicitationScope{SessionID: "session", TurnNonce: "nonce", ToolCallID: "tool"})
	require.Error(t, err)
}

func TestSessionConfigAndCloneRemainingBranches(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	client.agents = []opencode.NativeAgent{{Name: "build"}, {Name: "plan"}}
	session := testSession(t, agent, client)
	agent.sessions[session.id] = session

	boolean := true
	_, err := agent.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{Boolean: &acp.SetSessionConfigOptionBoolean{SessionId: session.id, ConfigId: configMode, Value: boolean}})
	requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: jsonFieldType})
	_, err = agent.SetSessionConfigOption(context.Background(), SetConfigOptionRequest(session.id, configMode, "plan"))
	require.NoError(t, err)

	require.Equal(t, map[string]string{"key": "value"}, cloneAny(map[string]string{"key": "value"}))
}

// TestAgentCloseRunsTheDurableRungAWireCloseOwes proves the durable rung travels
// with the ladder. An embedded shutdown closes each session through the same
// committing boundary a wire `session/close` runs, so state the session took on
// since its last turn — the model set through a config option here — reaches the
// store instead of being dropped with the wrapper. A later load restores what the
// host last saw rather than what the last turn happened to leave behind.
func TestAgentCloseRunsTheDurableRungAWireCloseOwes(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-shutdown")
	client.getSession = testNativeSession("native-shutdown")
	client.agents = []opencode.NativeAgent{{Name: "build"}, {Name: "plan"}}

	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	agent.runtime = client
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)

	stored := func(t *testing.T) stateSnapshot {
		t.Helper()

		entries, loadErr := store.Load(ctx,
			SessionKey{SessionID: string(created.SessionId), Subpath: SessionStoreMainSubpath})
		require.NoError(t, loadErr)
		require.Len(t, entries, 1)

		var bundle stateSnapshot

		require.NoError(t, json.Unmarshal(entries[0], &bundle))

		return bundle
	}

	require.Equal(t, "build", stored(t).Session.Model.Agent)

	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(created.SessionId, configMode, "plan"))
	require.NoError(t, err)
	require.Equal(t, "build", stored(t).Session.Model.Agent,
		"the config option committed on its own, so this test proves nothing about the boundary")

	require.NoError(t, agent.Close())

	require.Equal(t, "plan", stored(t).Session.Model.Agent,
		"the embedded shutdown dropped state a wire close would have committed")
}
