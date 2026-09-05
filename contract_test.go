package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestInitializeCapabilities(t *testing.T) {
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
	require.Equal(t, map[string]any{metaFieldVersion: handoffEnvelopeVersion}, with.AgentCapabilities.Meta[handoffEnvelopeKey])
	require.Equal(t, map[string]any{metaFieldVersion: routeEnvelopeVersion}, with.AgentCapabilities.Meta[routeEnvelopeKey])
}

func TestRouteCapabilityScalar(t *testing.T) {
	response, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	require.NoError(t, err)

	route, ok := response.AgentCapabilities.Meta["acp-go.dev/route"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 1, route["version"])
	require.Len(t, route, 1)
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

// TestAssistantTextIsAppendOnly is the append-only streaming conformance
// fixture. A client renders a turn's assistant text as the in-order
// concatenation of every chunk it received, so a chunk is never a snapshot, a
// repeat, or a correction of text already sent.
func TestAssistantTextIsAppendOnly(t *testing.T) {
	ctx := context.Background()

	// A streamed turn whose terminal frame repeats the assembled text: the
	// concatenation of the emitted chunks equals the final text exactly once.
	t.Run("terminal frame repeats the streamed text", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)

		const final = "the quick brown fox"

		require.NoError(t, session.applyNativeEvent(ctx, opencode.Event{
			Type:       opencode.EventMessageUpdated,
			Properties: json.RawMessage(`{"info":{"id":"asst","sessionID":"native-1","role":"assistant"}}`),
		}))

		for _, assembled := range []string{"the ", "the quick ", "the quick brown ", final} {
			require.NoError(t, session.applyNativeEvent(ctx, opencode.Event{
				Type: "message.part.updated",
				Properties: json.RawMessage(`{"part":{"id":"part-1","sessionID":"native-1","messageID":"asst","type":"text","text":` +
					strconv.Quote(assembled) + `}}`),
			}))
		}

		// The terminal full-message frame repeats the whole assembled text.
		require.NoError(t, session.emitMessage(ctx, opencode.NativeMessage{
			Info:  opencode.NativeMessageInfo{ID: "asst", SessionID: "native-1", Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{ID: "part-1", SessionID: "native-1", MessageID: "asst", Type: "text", Text: final}},
		}, false))

		require.Equal(t, final, assembledAgentText(conn))
	})

	// A harness that delivers only a terminal full-message frame, with no
	// deltas, produces exactly one chunk carrying that text.
	t.Run("deltas-free harness yields one chunk", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)

		require.NoError(t, session.emitMessage(ctx, opencode.NativeMessage{
			Info:  opencode.NativeMessageInfo{ID: "asst", SessionID: "native-1", Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{ID: "only", SessionID: "native-1", MessageID: "asst", Type: "text", Text: "one shot"}},
		}, false))

		require.Len(t, agentTextChunks(conn), 1)
		require.Equal(t, "one shot", assembledAgentText(conn))
	})

	// Several native assistant messages in one turn produce each message's text
	// exactly once, in native order, deduplicated on native identity.
	t.Run("multi-message turn emits each message once", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)

		first := opencode.NativeMessage{
			Info:  opencode.NativeMessageInfo{ID: "asst-1", SessionID: "native-1", Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{ID: "p1", SessionID: "native-1", MessageID: "asst-1", Type: "text", Text: "first."}},
		}
		second := opencode.NativeMessage{
			Info:  opencode.NativeMessageInfo{ID: "asst-2", SessionID: "native-1", Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{ID: "p2", SessionID: "native-1", MessageID: "asst-2", Type: "text", Text: "second."}},
		}

		require.NoError(t, session.emitMessage(ctx, first, false))
		require.NoError(t, session.emitMessage(ctx, second, false))
		// Repeating either native identity contributes nothing further.
		require.NoError(t, session.emitMessage(ctx, first, false))
		require.NoError(t, session.emitMessage(ctx, second, false))

		require.Len(t, agentTextChunks(conn), 2)
		require.Equal(t, "first.second.", assembledAgentText(conn))
	})
}

func agentTextChunks(conn *recordingAgentClient) []string {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	var chunks []string

	for _, update := range conn.updates {
		chunk := update.Update.AgentMessageChunk
		if chunk != nil && chunk.Content.Text != nil {
			chunks = append(chunks, chunk.Content.Text.Text)
		}
	}

	return chunks
}

func assembledAgentText(conn *recordingAgentClient) string {
	return strings.Join(agentTextChunks(conn), "")
}

// TestReservedPromptKeysRefusalShapes is the table-driven conformance fixture
// for the two reserved keys a `session/prompt` carries. An absent key is
// `missing` on the bare path; a present but unacceptable value is `unsupported`
// naming the offending member. Route validation runs first, so a prompt that
// fails both reports the route refusal alone.
func TestReservedPromptKeysRefusalShapes(t *testing.T) {
	ctx := context.Background()
	overBound := strings.Repeat("n", routeTurnNonceMaxBytes+1)
	goodRoute := map[string]any{routeFieldVersion: 1, routeFieldTurnNonce: "nonce"}
	goodLifecycle := map[string]any{
		"version":    1,
		"submission": map[string]any{"submissionId": "sub", "clientNonce": "cli"},
	}

	tests := map[string]struct {
		meta  map[string]any
		error string
		field string
	}{
		"route absent": {
			meta:  map[string]any{lifecycle.MetaKey: goodLifecycle},
			error: errValueMissing, field: routeMetaPath,
		},
		"route non-object": {
			meta:  map[string]any{routeEnvelopeKey: 7, lifecycle.MetaKey: goodLifecycle},
			error: errValueUnsupported, field: routeMetaPath,
		},
		"route wrong version": {
			meta:  map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: 2, routeFieldTurnNonce: "nonce"}},
			error: errValueUnsupported, field: routeMemberPath(routeFieldVersion),
		},
		"route fractional version": {
			meta:  map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: 1.5, routeFieldTurnNonce: "nonce"}},
			error: errValueUnsupported, field: routeMemberPath(routeFieldVersion),
		},
		"route empty nonce": {
			meta:  map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: 1, routeFieldTurnNonce: ""}},
			error: errValueUnsupported, field: routeMemberPath(routeFieldTurnNonce),
		},
		"route over-bound nonce": {
			meta:  map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: 1, routeFieldTurnNonce: overBound}},
			error: errValueUnsupported, field: routeMemberPath(routeFieldTurnNonce),
		},
		"route unknown member": {
			meta: map[string]any{routeEnvelopeKey: map[string]any{
				routeFieldVersion: 1, routeFieldTurnNonce: "nonce", "extra": true,
			}},
			error: errValueUnsupported, field: routeMemberPath("extra"),
		},
		"lifecycle absent": {
			meta:  map[string]any{routeEnvelopeKey: goodRoute},
			error: errValueMissing, field: lifecycle.MetaPath,
		},
		"lifecycle non-object": {
			meta:  map[string]any{routeEnvelopeKey: goodRoute, lifecycle.MetaKey: 7},
			error: errValueUnsupported, field: lifecycle.MetaPath,
		},
		"lifecycle wrong version": {
			meta: map[string]any{routeEnvelopeKey: goodRoute, lifecycle.MetaKey: map[string]any{
				"version": 2, "submission": map[string]any{"submissionId": "sub", "clientNonce": "cli"},
			}},
			error: errValueUnsupported, field: lifecycle.MetaPath + ".version",
		},
		"lifecycle empty identifier": {
			meta: map[string]any{routeEnvelopeKey: goodRoute, lifecycle.MetaKey: map[string]any{
				"version": 1, "submission": map[string]any{"submissionId": "", "clientNonce": "cli"},
			}},
			error: errValueUnsupported, field: lifecycle.MetaPath + ".submission.submissionId",
		},
		"lifecycle unknown member": {
			meta: map[string]any{routeEnvelopeKey: goodRoute, lifecycle.MetaKey: map[string]any{
				"version":    1,
				"submission": map[string]any{"submissionId": "sub", "clientNonce": "cli"},
				"extra":      true,
			}},
			error: errValueUnsupported, field: lifecycle.MetaPath + ".extra",
		},
		// Both wrong: the route refusal is the only one reported.
		"both malformed": {
			meta: map[string]any{
				routeEnvelopeKey:  map[string]any{routeFieldVersion: 2, routeFieldTurnNonce: "nonce"},
				lifecycle.MetaKey: 7,
			},
			error: errValueUnsupported, field: routeMemberPath(routeFieldVersion),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			agent := negotiatedAgent(t)
			agent.setAgentClient(newRecordingAgentClient())
			client := newFakeOpenCodeClient()
			session := testSession(t, agent, client)
			agent.sessions[session.id] = session

			dispatched := false
			client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
				dispatched = true

				return opencode.NativeMessage{}, nil
			}

			_, err := agent.Prompt(ctx, acp.PromptRequest{
				SessionId: session.id,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
				Meta:      test.meta,
			})
			requireInvalidParamsData(t, err, map[string]any{jsonFieldError: test.error, jsonFieldField: test.field})
			require.False(t, dispatched, "the prompt reached the harness")

			// The same verdict is owed on a session that does not exist: the
			// reserved keys are read before the session id is resolved.
			_, err = agent.Prompt(ctx, acp.PromptRequest{
				SessionId: "no-such-session",
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
				Meta:      test.meta,
			})
			requireInvalidParamsData(t, err, map[string]any{jsonFieldError: test.error, jsonFieldField: test.field})
		})
	}
}

// TestOffPromptInternalErrorVocabulary drives every off-prompt `-32603` this
// adapter can reach and pins the closed token, the closed `class`/`cause`
// values, and the absence of any prose: no `message` member, no Go error text,
// no native text.
func TestOffPromptInternalErrorVocabulary(t *testing.T) {
	ctx := context.Background()

	tests := map[string]struct {
		reach func(t *testing.T) error
		data  map[string]any
	}{
		"construction verdict": {
			reach: func(t *testing.T) error {
				t.Helper()
				_, err := NewAgent(WithTurnTimeout(-time.Second)).Initialize(ctx, acp.InitializeRequest{})

				return err
			},
			data: map[string]any{jsonFieldError: errValueInvalidOptions},
		},
		"unrestorable store entry": {
			reach: func(t *testing.T) error {
				t.Helper()

				store := NewInMemorySessionStore()
				require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
					Key:     SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath},
					Entries: []SessionStoreEntry{[]byte(`{"format":"opencode-sync-events-v1"}`)},
				}}))

				agent := NewAgent(WithSessionStore(store))
				agent.runtime = newFakeOpenCodeClient()

				_, err := agent.ResumeSession(ctx, ResumeSessionRequest("session", t.TempDir()))

				return err
			},
			data: map[string]any{jsonFieldError: errValueRestoreFailed},
		},
		"un-containable runtime": {
			reach: func(t *testing.T) error {
				t.Helper()

				agent := NewAgent()
				agent.runtime = newFakeOpenCodeClient()
				agent.runtimeFatalErr = errors.Join(ErrContainmentIncomplete, errors.New("native tree still alive"))

				_, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))

				return err
			},
			data: map[string]any{jsonFieldError: errValueRuntimeUnavailable},
		},
		"poisoned session": {
			reach: func(t *testing.T) error {
				t.Helper()

				agent := NewAgent()
				agent.setAgentClient(newRecordingAgentClient())
				session := testSession(t, agent, newFakeOpenCodeClient())

				return session.poison(ctx, poisonCauseNativeSessionDrift)
			},
			data: map[string]any{jsonFieldError: errValueSessionPoisoned, jsonFieldCause: poisonCauseNativeSessionDrift},
		},
		"unclassified failure": {
			reach: func(t *testing.T) error {
				t.Helper()

				return errors.New("a native detail no host may read")
			},
			data: map[string]any{jsonFieldError: errValueInternalFailure},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := test.reach(t)
			require.Error(t, err)

			reqErr := requestError(ctx, err)
			require.Equal(t, -32603, reqErr.Code)
			require.Equal(t, "Internal error", reqErr.Message)
			require.Equal(t, test.data, reqErr.Data)

			encoded, marshalErr := json.Marshal(reqErr.Data)
			require.NoError(t, marshalErr)
			require.NotContains(t, string(encoded), jsonFieldMessage)
			require.NotContains(t, string(encoded), "native detail")
		})
	}
}
