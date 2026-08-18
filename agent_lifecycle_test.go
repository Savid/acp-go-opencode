package opencodeacp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// lifecycleOffer is the host's initialize offer.
func lifecycleOffer() map[string]any {
	return map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{1.0}}}
}

// lifecycleKey is the reserved literal on a surface that carries no lifecycle
// value. Its interior never matters: the key's presence is the refusal.
func lifecycleKey() map[string]any {
	return map[string]any{lifecycle.MetaKey: map[string]any{}}
}

func negotiatedAgent(t *testing.T, options ...Option) *Agent {
	t.Helper()

	agent := NewAgent(options...)
	response, err := agent.Initialize(context.Background(), acp.InitializeRequest{Meta: lifecycleOffer()})
	require.NoError(t, err)
	require.NotNil(t, response.Meta[lifecycle.MetaKey])

	return agent
}

// TestInitializeAnswersOnTheResponsesOwnMeta proves the answer rides the top-level
// response `_meta` and never the capability object a later protocol revision
// relocates, and never the vendor namespace.
func TestInitializeAnswersOnTheResponsesOwnMeta(t *testing.T) {
	t.Parallel()

	response, err := NewAgent().Initialize(context.Background(), acp.InitializeRequest{Meta: lifecycleOffer()})
	require.NoError(t, err)

	answer, ok := response.Meta[lifecycle.MetaKey].(map[string]any)
	require.True(t, ok, "the answer is absent from the response _meta")
	require.Equal(t, []int{1}, answer["versions"])
	require.Equal(t, false, answer["updatesOutsidePrompt"])
	require.Equal(t, false, answer["authoritativeQuiescence"])
	require.Equal(t, []string{}, answer["activityKinds"])
	require.NotContains(t, answer, "quiescenceSource")

	require.NotContains(t, response.AgentCapabilities.Meta, lifecycle.MetaKey)

	vendor, ok := response.AgentCapabilities.Meta[opencodeMetaKey].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, vendor, lifecycle.MetaKey)
	require.NotContains(t, vendor, "lifecycle")
}

// TestInitializeOmitsTheKeyWithoutACommonVersion proves the key is omitted whole
// rather than answered with an empty array, and that an absent offer is the host
// asking for nothing.
func TestInitializeOmitsTheKeyWithoutACommonVersion(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		meta map[string]any
	}{
		{"no offer", nil},
		{"no _meta member", map[string]any{"acp-go.dev/route": map[string]any{"versions": []any{1.0}}}},
		{"no common version", map[string]any{lifecycle.MetaKey: map[string]any{"versions": []any{2.0}}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			response, err := agent.Initialize(context.Background(), acp.InitializeRequest{Meta: row.meta})
			require.NoError(t, err)
			require.NotContains(t, response.Meta, lifecycle.MetaKey)
			require.False(t, agent.lifecycleNegotiated().Present())
		})
	}
}

// TestInitializeRefusesAMalformedOfferByPath proves the one family literal this
// adapter validates on initialize names the exact member that was wrong.
func TestInitializeRefusesAMalformedOfferByPath(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name  string
		value any
		field string
	}{
		{"not an object", []any{1.0}, lifecycle.MetaPath},
		{"unknown member", map[string]any{"versions": []any{1.0}, "activityKinds": []any{}}, lifecycle.MetaPath + ".activityKinds"},
		{"versions absent", map[string]any{}, lifecycle.MetaPath + ".versions"},
		{"versions empty", map[string]any{"versions": []any{}}, lifecycle.MetaPath + ".versions"},
		{"versions not integers", map[string]any{"versions": []any{"1"}}, lifecycle.MetaPath + ".versions"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			_, err := agent.Initialize(context.Background(), acp.InitializeRequest{
				Meta: map[string]any{lifecycle.MetaKey: row.value},
			})
			requireUnsupportedField(t, err, row.field)
			require.False(t, agent.lifecycleNegotiated().Present())
		})
	}
}

// TestLifecycleTruthTableIsResolvedPerConfiguration proves the answer comes from
// the containment mode that enforces the boundary rather than from a compiled-in
// constant, and that every configuration answers the degenerate row it can prove.
func TestLifecycleTruthTableIsResolvedPerConfiguration(t *testing.T) {
	t.Parallel()

	for _, mode := range []RuntimeContainmentMode{
		RuntimeContainmentAuthoritative,
		RuntimeContainmentBestEffort,
		RuntimeContainmentSharedIdentity,
		RuntimeContainmentUnavailable,
		RuntimeContainmentMode("unnamed"),
	} {
		facts := provenLifecycleFacts(mode)
		require.False(t, facts.UpdatesOutsidePrompt, mode)
		require.False(t, facts.AuthoritativeQuiescence, mode)
		require.Empty(t, facts.QuiescenceSource, mode)
		require.Equal(t, []lifecycle.ActivityKind{}, facts.ActivityKinds, mode)
	}

	agent := NewAgent()
	agent.containmentMode = RuntimeContainmentAuthoritative

	response, err := agent.Initialize(context.Background(), acp.InitializeRequest{Meta: lifecycleOffer()})
	require.NoError(t, err)

	answer, ok := response.Meta[lifecycle.MetaKey].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, answer, "quiescenceSource")
}

// TestReservedLifecycleKeyIsRefusedOnEveryCarryingRoute walks the complete
// dispatch table of inbound routes that carry no lifecycle value. A family literal
// is never a foreign namespace and never a no-op, so each one names the exact path.
func TestReservedLifecycleKeyIsRefusedOnEveryCarryingRoute(t *testing.T) {
	t.Parallel()

	for _, row := range []struct {
		name string
		call func(*Agent) error
	}{
		{"session/new", func(a *Agent) error {
			_, err := a.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/tmp", Meta: lifecycleKey()})

			return err
		}},
		{"session/load", func(a *Agent) error {
			_, err := a.LoadSession(context.Background(), acp.LoadSessionRequest{
				SessionId: "session-1", Cwd: "/tmp", Meta: lifecycleKey(),
			})

			return err
		}},
		{"session/resume", func(a *Agent) error {
			_, err := a.ResumeSession(context.Background(), acp.ResumeSessionRequest{
				SessionId: "session-1", Cwd: "/tmp", Meta: lifecycleKey(),
			})

			return err
		}},
		{"session/list", func(a *Agent) error {
			_, err := a.ListSessions(context.Background(), acp.ListSessionsRequest{Meta: lifecycleKey()})

			return err
		}},
		{"session/close", func(a *Agent) error {
			_, err := a.CloseSession(context.Background(), acp.CloseSessionRequest{
				SessionId: "session-1", Meta: lifecycleKey(),
			})

			return err
		}},
		{"session/delete", func(a *Agent) error {
			_, err := a.UnstableDeleteSession(context.Background(), acp.UnstableDeleteSessionRequest{
				SessionId: "session-1", Meta: lifecycleKey(),
			})

			return err
		}},
		{"session/set_config_option", func(a *Agent) error {
			request := SetModelRequest("session-1", "openai/gpt-test")
			request.ValueId.Meta = lifecycleKey()

			_, err := a.SetSessionConfigOption(context.Background(), request)

			return err
		}},
		{"session/set_config_option boolean", func(a *Agent) error {
			_, err := a.SetSessionConfigOption(context.Background(), acp.SetSessionConfigOptionRequest{
				Boolean: &acp.SetSessionConfigOptionBoolean{Meta: lifecycleKey()},
			})

			return err
		}},
		{"authenticate", func(a *Agent) error {
			_, err := a.Authenticate(context.Background(), acp.AuthenticateRequest{
				MethodId: "none", Meta: lifecycleKey(),
			})

			return err
		}},
		{"logout", func(a *Agent) error {
			_, err := a.Logout(context.Background(), acp.LogoutRequest{Meta: lifecycleKey()})

			return err
		}},
		{"session/cancel", func(a *Agent) error {
			return a.Cancel(context.Background(), acp.CancelNotification{
				SessionId: "session-1",
				Meta: map[string]any{
					routeEnvelopeKey:  map[string]any{routeFieldVersion: 1, routeFieldTurnNonce: "nonce"},
					lifecycle.MetaKey: map[string]any{},
				},
			})
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			requireUnsupportedField(t, row.call(NewAgent()), lifecycle.MetaPath)
		})
	}
}

// TestReservedLifecycleKeyIsRefusedOnEveryExtensionRoute proves fork and every
// provider-auth leg refuse the literal before their own dispatch, whether or not
// the broker behind the leg is configured.
func TestReservedLifecycleKeyIsRefusedOnEveryExtensionRoute(t *testing.T) {
	t.Parallel()

	params, err := json.Marshal(map[string]any{"sessionId": "session-1", "_meta": lifecycleKey()})
	require.NoError(t, err)

	for _, method := range []string{
		ForkSessionMethod,
		AuthMethodsMethod,
		AuthAuthorizeMethod,
		AuthCallbackMethod,
		AuthStatusMethod,
		AuthCancelMethod,
		AuthInventoryMethod,
		AuthDisconnectMethod,
	} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			configured := NewAgent()
			_, err := configured.HandleExtensionMethod(context.Background(), method, params)
			requireUnsupportedField(t, err, lifecycle.MetaPath)

			unconfigured := NewAgent()
			unconfigured.providerAuth = nil

			_, err = unconfigured.HandleExtensionMethod(context.Background(), method, params)
			requireUnsupportedField(t, err, lifecycle.MetaPath)
		})
	}
}

// TestUnimplementedRoutesKeepTheirOwnAnswer proves a method this adapter does not
// implement answers method not found before any parameter is inspected, and that a
// params body carrying no reserved key is left to its own decoder.
func TestUnimplementedRoutesKeepTheirOwnAnswer(t *testing.T) {
	t.Parallel()

	agent := NewAgent()

	_, err := agent.SetSessionMode(context.Background(), acp.SetSessionModeRequest{
		SessionId: "session-1", ModeId: "build", Meta: lifecycleKey(),
	})
	require.ErrorContains(t, err, "Method not found")

	_, err = agent.HandleExtensionMethod(context.Background(), "_opencode/unknown",
		json.RawMessage(`{"_meta":{"acp-go.dev/lifecycle":{}}}`))
	require.ErrorContains(t, err, "Method not found")

	_, err = agent.HandleExtensionMethod(context.Background(), ForkSessionMethod, json.RawMessage(`not json`))
	require.ErrorContains(t, err, "Invalid params")
}

// TestPromptCorrelationIsRequiredWhileNegotiated proves the submission value is
// read after the route and before anything is dispatched to the harness.
func TestPromptCorrelationIsRequiredWhileNegotiated(t *testing.T) {
	t.Parallel()

	agent := negotiatedAgent(t)
	client := newFakeOpenCodeClient()
	session := testSession(agent, client)
	agent.sessions[session.id] = session

	request := TextPromptRequest(session.id, "nonce", "hello")

	dispatched := false
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched = true

		return opencode.NativeMessage{}, nil
	}

	_, err := agent.Prompt(context.Background(), request)
	requireUnsupportedField(t, err, lifecycle.MetaPath)
	require.False(t, dispatched, "the prompt reached the harness")

	// A stale route nonce is refused before the correlation is examined, so a
	// prompt never reports two rejections.
	stale := TextPromptRequest(session.id, "", "hello")
	stale.Meta = map[string]any{routeEnvelopeKey: map[string]any{routeFieldVersion: 1, routeFieldTurnNonce: ""}}

	_, err = agent.Prompt(context.Background(), stale)
	require.ErrorContains(t, err, "invalid_route_envelope")
}

// TestPromptCorrelationIsRefusedWhileUnnegotiated proves a present key on a
// connection that answered nothing is refused rather than ignored.
func TestPromptCorrelationIsRefusedWhileUnnegotiated(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	client := newFakeOpenCodeClient()
	session := testSession(agent, client)
	agent.sessions[session.id] = session

	request := TextPromptRequest(session.id, "nonce", "hello")
	request.Meta[lifecycle.MetaKey] = map[string]any{
		"version":    1,
		"submission": map[string]any{"submissionId": "sub-1", "clientNonce": "nonce-1"},
	}

	dispatched := false
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched = true

		return opencode.NativeMessage{}, nil
	}

	_, err := agent.Prompt(context.Background(), request)
	requireUnsupportedField(t, err, lifecycle.MetaPath)
	require.False(t, dispatched, "the prompt reached the harness")
}

// TestPromptBindsBothEnvelopesToTheSameTurn proves the route nonce and the
// submission identity are recorded together on the turn the prompt opened, and
// that neither is derived from the other.
func TestPromptBindsBothEnvelopesToTheSameTurn(t *testing.T) {
	t.Parallel()

	agent := negotiatedAgent(t)
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	client := newFakeOpenCodeClient()
	session := testSession(agent, client)
	agent.sessions[session.id] = session

	var (
		observed lifecycle.Submission
		nonce    string
	)

	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		observed = session.currentSubmission()
		nonce = session.currentTurnNonce()

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{
			ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop",
		}}, nil
	}

	request := TextPromptRequest(session.id, "route-nonce", "hello")
	request.Meta[lifecycle.MetaKey] = map[string]any{
		"version": 1,
		"submission": map[string]any{
			"submissionId": "sub-1",
			"clientNonce":  "client-nonce",
			"runId":        "run-1",
		},
	}

	_, err := agent.Prompt(context.Background(), request)
	require.NoError(t, err)

	require.Equal(t, lifecycle.Submission{
		SubmissionID: "sub-1",
		ClientNonce:  "client-nonce",
		RunID:        "run-1",
	}, observed)
	require.Equal(t, "route-nonce", nonce)
	require.GreaterOrEqual(t, connection.updateCount(), 4, "snapshot, acceptance, running, and terminal idle")

	// The turn's identities are released with the turn.
	require.Equal(t, lifecycle.Submission{}, session.currentSubmission())
}

// TestCancelCarryingTheKeyNeverReachesTheHarness proves the refusal lands before
// the native interrupt and before any local turn state moves.
func TestCancelCarryingTheKeyNeverReachesTheHarness(t *testing.T) {
	t.Parallel()

	agent := negotiatedAgent(t)
	client := newFakeOpenCodeClient()
	session := testSession(agent, client)
	agent.sessions[session.id] = session
	session.beginTurn(context.Background(), "nonce")

	err := agent.Cancel(context.Background(), acp.CancelNotification{
		SessionId: session.id,
		Meta: map[string]any{
			routeEnvelopeKey:  map[string]any{routeFieldVersion: 1, routeFieldTurnNonce: "nonce"},
			lifecycle.MetaKey: map[string]any{},
		},
	})
	requireUnsupportedField(t, err, lifecycle.MetaPath)

	client.mu.Lock()
	aborts := len(client.aborts)
	client.mu.Unlock()

	require.Zero(t, aborts)
	require.False(t, session.wasCancelled())
}

// TestNegotiatedAnswerIsAbsentWithoutAnAgent proves a session with no owning agent
// states no lifecycle fact, which is what keeps an envelope illegal rather than
// merely unsent.
func TestNegotiatedAnswerIsAbsentWithoutAnAgent(t *testing.T) {
	t.Parallel()

	var absent *Agent

	require.False(t, absent.lifecycleNegotiated().Present())
}

// requireUnsupportedField asserts the uniform invalid-params refusal naming one
// exact request path.
func requireUnsupportedField(t *testing.T, err error, field string) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: field,
	}).Error(), reqErr.Error())
}
