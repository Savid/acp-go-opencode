// Family contract pins: capability hard cutover, the stable-fork -32601
// route, and lifecycle _meta strictness. These assertions guard wire behavior
// that hosts depend on across adapter releases.
package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
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

// TestInitializeAdvertisesTheMediaEnvelope pins the family-reserved media
// envelope: the exact field set, this adapter's effective values, and a
// document list emitted as an empty array rather than null.
func TestInitializeAdvertisesTheMediaEnvelope(t *testing.T) {
	resp, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	envelope, ok := resp.AgentCapabilities.Meta[mediaEnvelopeKey].(map[string]any)
	require.True(t, ok, "media envelope missing from agent capabilities")
	require.Equal(t, map[string]any{
		mediaEnvelopeFieldMaxBytes:        defaultImageLimitBytes,
		mediaEnvelopeFieldMaxPromptBytes:  defaultImageLimitBytes,
		mediaEnvelopeFieldMaxDimension:    0,
		mediaEnvelopeFieldImageFormats:    []string{mimePNG, mimeJPEG, mimeGIF, mimeWebP},
		mediaEnvelopeFieldDocumentFormats: []string{},
	}, envelope)

	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"documentFormats":[]`)
	require.NotContains(t, string(encoded), "null")

	formats, ok := envelope[mediaEnvelopeFieldImageFormats].([]string)
	require.True(t, ok)
	formats[0] = "mutated"
	require.Equal(t, mimePNG, imageInputFormats[0], "advertisement aliases the input allowlist")
}

// TestMediaEnvelopeMatchesTheEnforcedGate binds the advertised bytes to the
// bound the input gate actually rejects on, so the two cannot drift.
func TestMediaEnvelopeMatchesTheEnforcedGate(t *testing.T) {
	decoded := fixtureImage(t, "valid.png")
	gate := int64(len(decoded)) - 1

	agent := NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: gate, MaxInputBytesPerPrompt: gate}))
	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	envelope, ok := resp.AgentCapabilities.Meta[mediaEnvelopeKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, gate, envelope[mediaEnvelopeFieldMaxBytes])
	require.Equal(t, gate, envelope[mediaEnvelopeFieldMaxPromptBytes])

	session := testSession(agent, newFakeOpenCodeClient())
	requireInvalidParamsData(t, validatePromptMediaError(session, acp.ContentBlock{Image: &acp.ContentBlockImage{
		Type: "image", MimeType: mimePNG, Data: base64.StdEncoding.EncodeToString(decoded),
	}}), map[string]any{
		jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
		jsonFieldSizeBytes: int64(len(decoded)), jsonFieldMaxBytes: envelope[mediaEnvelopeFieldMaxBytes],
	})
}

// TestInitializeAdvertisesHandoffOnlyWhenConfigured pins the handoff
// advertisement as the answer to whether the host's read root reached this
// adapter: present with a root, absent without one.
func TestInitializeAdvertisesHandoffOnlyWhenConfigured(t *testing.T) {
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}

	without, err := NewAgent().Initialize(context.Background(), request)
	require.NoError(t, err)
	require.NotContains(t, without.AgentCapabilities.Meta, handoffEnvelopeKey)

	root := t.TempDir()

	with, err := NewAgent(WithInputHandoffRoot(root)).Initialize(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, map[string]any{metaFieldVersions: []int{handoffEnvelopeVersion}}, with.AgentCapabilities.Meta[handoffEnvelopeKey])
	require.Equal(t, map[string]any{metaFieldVersions: []int{routeEnvelopeVersion}}, with.AgentCapabilities.Meta[routeEnvelopeKey])
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
