package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestInitializeCapabilitiesHardCutover(t *testing.T) {
	agent := NewAgent()
	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if resp.AgentInfo == nil || resp.AgentInfo.Name != "acp-go-opencode" {
		t.Fatalf("AgentInfo = %#v", resp.AgentInfo)
	}
	if resp.AgentCapabilities.SessionCapabilities.Fork != nil {
		t.Fatalf("stable fork capability advertised: %#v", resp.AgentCapabilities.SessionCapabilities.Fork)
	}
	if resp.AgentCapabilities.McpCapabilities.Acp {
		t.Fatal("ACP MCP capability advertised")
	}
	if resp.AgentCapabilities.McpCapabilities.Sse {
		t.Fatal("SSE MCP capability advertised")
	}
	if !resp.AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("image prompt capability missing")
	}
	if !resp.AgentCapabilities.PromptCapabilities.EmbeddedContext {
		t.Fatal("embedded context capability missing")
	}
	meta, _ := resp.AgentCapabilities.Meta[opencodeMetaKey].(map[string]any)
	structured, _ := meta["structuredOutput"].(map[string]any)
	if structured["config"] != "_meta.opencode.options.outputSchema" ||
		structured["result"] != "_meta.opencode.structuredOutput" ||
		structured["schema"] != "json_schema" {
		t.Fatalf("structuredOutput meta = %#v", structured)
	}
	if store, _ := meta["sessionStore"].(map[string]any); store["format"] != SessionStoreFormat {
		t.Fatalf("sessionStore meta = %#v", store)
	}
}

func TestStableForkRouteMethodNotFound(t *testing.T) {
	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	_, reqErr := conn.handle(context.Background(), acp.AgentMethodSessionFork, json.RawMessage(`{}`))
	if reqErr == nil {
		t.Fatal("session/fork unexpectedly succeeded")
	}
	if reqErr.Code != -32601 {
		t.Fatalf("code = %d, want -32601", reqErr.Code)
	}
}

func TestLifecycleMetaStrictAllowlist(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		err  bool
	}{
		{name: "foreign ignored", meta: map[string]any{"codex": map[string]any{"deleted": true}}},
		{name: "trace ignored", meta: map[string]any{"traceparent": "00-abc"}},
		{name: "own unknown rejected", meta: map[string]any{opencodeMetaKey: map[string]any{"goals": []any{}}}, err: true},
		{name: "own option unknown rejected", meta: map[string]any{opencodeMetaKey: map[string]any{"options": map[string]any{"foo": "bar"}}}, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sessionMetaFromLifecycle(tt.meta)
			if tt.err && err == nil {
				t.Fatal("expected error")
			}
			if !tt.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestOutputSchemaAccepted(t *testing.T) {
	schema := map[string]any{"type": "object"}
	meta, err := sessionMetaFromLifecycle(OpenCodeOptions{OutputSchema: schema}.Meta())
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
	_, err := sessionMetaFromLifecycle(map[string]any{
		opencodeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: "not-an-object"}},
	})
	if err == nil {
		t.Fatal("invalid outputSchema unexpectedly accepted")
	}
}

func TestServeCloseErrorAndAgentCloneFallbacks(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.closeErr = errors.New("close failed")
	agent := NewAgent()
	session := testSession(agent, client)
	agent.sessions[session.id] = session

	oldNewAgent := newAgentForServe
	newAgentForServe = func(...Option) *Agent { return agent }
	t.Cleanup(func() { newAgentForServe = oldNewAgent })
	if err := Serve(ctx, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("Serve: %v", err)
	}

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
	session := testSession(agent, client)
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
	if err := agent.Cancel(ctx, acp.CancelNotification{SessionId: session.id}); err != nil {
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
	payload := capRawEventPayload(map[string]any{
		"sessionId": "s",
		"sequence":  int64(1),
		"source":    "test",
		"event":     strings.Repeat("x", rawEventMaxBytes),
	})
	if event, _ := payload["event"].(map[string]any); event["truncated"] != true {
		t.Fatalf("raw event was not capped: %#v", payload)
	}
	if _, err := io.Copy(io.Discard, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
}
