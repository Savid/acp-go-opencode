package opencodeacp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

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
