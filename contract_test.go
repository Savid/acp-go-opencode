package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// TestInitializeCapabilitiesHardCutover pins the capabilities this adapter
// advertises and the ones it never will: a removed surface stays removed, and a
// host reads support off the advertisement alone.
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
	elicitation, _ := meta["elicitation"].(map[string]any)
	if elicitation["unstable"] != true || elicitation["tracks"] != "ACP v1 elicitation" {
		t.Fatalf("elicitation meta = %#v", elicitation)
	}
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

func TestClientElicitationCapabilityGating(t *testing.T) {
	t.Parallel()

	var explicitNull acp.ElicitationCapabilities
	require.NoError(t, json.Unmarshal([]byte(`{"form":null,"url":null}`), &explicitNull))

	for _, test := range []struct {
		name     string
		caps     *acp.ElicitationCapabilities
		wantForm bool
	}{
		{name: "nil or omitted top level", caps: nil, wantForm: false},
		{name: "empty object", caps: &acp.ElicitationCapabilities{}, wantForm: false},
		{name: "both modes explicit null", caps: &explicitNull, wantForm: false},
		{name: "url only", caps: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}, wantForm: false},
		{name: "form only", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}, wantForm: true},
		{name: "form and url", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}, Url: &acp.ElicitationUrlCapabilities{}}, wantForm: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			agent.clientCapabilities.Elicitation = test.caps
			require.Equal(t, test.wantForm, agent.clientSupportsFormElicitation())
		})
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

// mediaEnvelopeOf initializes an agent and returns the media bounds it
// advertises, which is the only thing a host can pre-check against.
func mediaEnvelopeOf(t *testing.T, agent *Agent) map[string]any {
	t.Helper()

	resp, err := agent.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	envelope, ok := resp.AgentCapabilities.Meta[mediaEnvelopeKey].(map[string]any)
	require.True(t, ok, "media envelope missing from agent capabilities")

	return envelope
}

// TestMediaEnvelopeAdvertisesTheBoundTheGateReports binds both advertised byte
// bounds to the numbers the gates actually reject on, across the configured
// values where the two could drift apart: inside the transport bound, disabled,
// and wider than a frame.
func TestMediaEnvelopeAdvertisesTheBoundTheGateReports(t *testing.T) {
	decoded := fixtureImage(t, "valid.png")

	t.Run("per image", func(t *testing.T) {
		tests := []struct {
			name       string
			configured int64
			want       int64
		}{
			{name: "inside the transport bound", configured: int64(len(decoded)) - 1, want: int64(len(decoded)) - 1},
			{name: "disabled", configured: 0, want: imageFrameBoundBytes},
			{name: "wider than a frame", configured: 100 * imageFrameBoundBytes, want: imageFrameBoundBytes},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				root := t.TempDir()
				path := writeHandoffFile(t, root, "shot.png", decoded)

				agent := NewAgent(WithInputHandoffRoot(root), WithImageLimits(ImageLimits{MaxInputBytesPerImage: tt.configured}))
				envelope := mediaEnvelopeOf(t, agent)
				require.Equal(t, tt.want, envelope[mediaEnvelopeFieldMaxBytes])

				// A declaration one byte past the advertised bound, so the gate
				// under test is the only one that can answer.
				declaration := handoffEnvelope(decoded)
				declaration[handoffFieldSizeBytes] = tt.want + 1

				block := handoffBlock(mimePNG, path, declaration)
				requireInvalidParamsData(t, validatePromptMediaError(testSession(t, agent, newFakeOpenCodeClient()), block), map[string]any{
					jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 0,
					jsonFieldSizeBytes: tt.want + 1, jsonFieldMaxBytes: envelope[mediaEnvelopeFieldMaxBytes],
				})
			})
		}
	})

	t.Run("per prompt", func(t *testing.T) {
		root := t.TempDir()
		path := writeHandoffFile(t, root, "shot.png", decoded)

		// The two bounds differ, so an advertisement that reported the per-image
		// number for both would disagree with the aggregate rejection.
		perImage := int64(len(decoded))
		perPrompt := 2*perImage - 1

		agent := NewAgent(WithInputHandoffRoot(root), WithImageLimits(ImageLimits{
			MaxInputBytesPerImage:  perImage,
			MaxInputBytesPerPrompt: perPrompt,
		}))
		envelope := mediaEnvelopeOf(t, agent)
		require.Equal(t, perImage, envelope[mediaEnvelopeFieldMaxBytes])
		require.Equal(t, perPrompt, envelope[mediaEnvelopeFieldMaxPromptBytes])

		block := handoffBlock(mimePNG, path, handoffEnvelope(decoded))
		requireInvalidParamsData(t, validatePromptMediaError(testSession(t, agent, newFakeOpenCodeClient()), block, block), map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorTooLarge, jsonFieldIndex: 1,
			jsonFieldSizeBytes: 2 * perImage, jsonFieldMaxBytes: envelope[mediaEnvelopeFieldMaxPromptBytes],
		})
	})

	t.Run("a disabled aggregate advertises no aggregate", func(t *testing.T) {
		envelope := mediaEnvelopeOf(t, NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: defaultImageLimitBytes})))
		require.Equal(t, int64(0), envelope[mediaEnvelopeFieldMaxPromptBytes])
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

// TestStableForkRouteMethodNotFound pins that the stable fork route does not
// exist here: forking is the namespaced extension method and nothing else.
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

// TestLifecycleMetaStrictAllowlist pins the strictness of the reserved lifecycle
// key: an unknown member of the offer is refused rather than ignored.
func TestLifecycleMetaStrictAllowlist(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		err  bool
	}{
		{name: "foreign ignored", meta: map[string]any{"codex": map[string]any{"deleted": true}}},
		{name: "trace ignored", meta: map[string]any{"traceparent": "00-abc"}},
		{name: "own unknown rejected", meta: map[string]any{opencodeMetaKey: map[string]any{"unknown": []any{}}}, err: true},
		{name: "own option unknown rejected", meta: map[string]any{opencodeMetaKey: map[string]any{"options": map[string]any{"foo": "bar"}}}, err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sessionMetaFromVendorOptions(tt.meta)
			if tt.err && err == nil {
				t.Fatal("expected error")
			}
			if !tt.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestProviderAuthCapabilityWireShape(t *testing.T) {
	harness := newAuthAgent(t)

	response, err := harness.agent.Initialize(context.Background(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	encoded, err := json.Marshal(response.AgentCapabilities.Meta[opencodeMetaKey])
	require.NoError(t, err)

	var vendor map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &vendor))

	var capability map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(vendor[providerAuthCapabilityKey], &capability))

	// The array is the host's only discovery surface for which legs exist, and
	// injectionKey is absent because this adapter accepts no injected binding.
	require.Len(t, capability, 1)

	var methods []string
	require.NoError(t, json.Unmarshal(capability[providerAuthMethodsField], &methods))
	require.Equal(t, []string{
		"_opencode/auth/methods",
		"_opencode/auth/authorize",
		"_opencode/auth/callback",
		"_opencode/auth/status",
		"_opencode/auth/cancel",
		"_opencode/auth/inventory",
		"_opencode/auth/disconnect",
	}, methods)

	require.NotContains(t, methods, "_opencode/auth/credential")
}

func TestProviderAuthFailureWireShape(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.brokerNode.authorizeErr = errors.New("dial tcp 127.0.0.1:1: connection refused")

	_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32000, reqErr.Code)

	encoded, marshalErr := json.Marshal(reqErr.Data)
	require.NoError(t, marshalErr)

	var data map[string]any
	require.NoError(t, json.Unmarshal(encoded, &data))
	require.Equal(t, "opencode_auth_failed", data[jsonFieldError])
	require.Equal(t, authCauseTransport, data[jsonFieldCause])
	require.Equal(t, true, data["retryable"])
	require.NotContains(t, encoded, "connection refused")
}
