package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// newAuthAgent builds an agent whose provider-auth surface is enabled, with one
// registered session and a fake native client standing in for the durable
// runtime store.
// authHarness is one enabled provider-auth surface with its runtime and
// registered session.
type authHarness struct {
	agent   *Agent
	broker  *providerAuth
	runtime *fakeOpenCodeClient
	session *session
}

func newAuthAgent(t *testing.T) authHarness {
	t.Helper()

	agent := NewAgent(
		WithProviderAuthRoot(t.TempDir()),
		WithHome(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithLogger(slog.New(slog.DiscardHandler)),
	)
	require.NotNil(t, agent.providerAuth)

	client := newFakeOpenCodeClient()
	session := testSession(t, agent, client)

	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	return authHarness{agent: agent, broker: agent.providerAuth, runtime: client, session: session}
}

// withBrokerFactory points the broker at a fake native server and returns it.
func withBrokerFactory(t *testing.T, agent *Agent) *fakeOpenCodeClient {
	t.Helper()

	broker := newFakeOpenCodeClient()
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		broker.mu.Lock()
		if broker.closed {
			broker.closed = false
			broker.closeSignal = make(chan struct{})
			broker.closeOnce = sync.Once{}
		}
		broker.mu.Unlock()

		return broker, nil
	}

	return broker
}

func requireAuthFailure(t *testing.T, err error, cause string) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32000, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, authFailedErrorTag, data[jsonFieldError])
	require.Equal(t, cause, data[jsonFieldCause])
	require.Contains(t, data, "retryable")
}

func requireInvalidParams(t *testing.T, err error, field string) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32602, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, field, data[jsonFieldField])
}

func TestAuthMethodNamesAreTheSevenAdvertisedLegs(t *testing.T) {
	require.Equal(t, []string{
		"_opencode/auth/methods",
		"_opencode/auth/authorize",
		"_opencode/auth/callback",
		"_opencode/auth/status",
		"_opencode/auth/cancel",
		"_opencode/auth/inventory",
		"_opencode/auth/disconnect",
	}, authMethodNames())
}

func TestProviderAuthCapabilityCarriesOnlyTheMethodsArray(t *testing.T) {
	broker := newAuthAgent(t).broker

	capability := broker.capability()
	require.Len(t, capability, 1)
	require.Equal(t, authMethodNames(), capability[providerAuthMethodsField])
}

func TestNewProviderAuthRequiresRootAndHome(t *testing.T) {
	cases := []struct {
		name    string
		options []Option
	}{
		{name: "no root", options: []Option{WithHome(t.TempDir())}},
		{name: "no home", options: []Option{WithProviderAuthRoot(t.TempDir())}},
		{name: "relative root", options: []Option{WithProviderAuthRoot("relative"), WithHome(t.TempDir())}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			agent := NewAgent(append(testCase.options, WithLogger(slog.New(slog.DiscardHandler)))...)
			require.Nil(t, agent.providerAuth)
		})
	}
}

// TestRelativeProviderAuthRootIsAConstructionVerdict pins a relative root as a
// configuration failure rather than a warning: an operator who supplied one
// asked for the surface, and silently dropping it leaves the agent running
// against options that never validated.
func TestRelativeProviderAuthRootIsAConstructionVerdict(t *testing.T) {
	require.NoError(t, validateProviderAuthRoot(Options{}))
	require.NoError(t, validateProviderAuthRoot(Options{ProviderAuthRoot: t.TempDir()}))
	require.Error(t, validateProviderAuthRoot(Options{ProviderAuthRoot: filepath.Join("relative", "ledger")}))

	options := []Option{
		WithProviderAuthRoot("relative"),
		WithHome(t.TempDir()),
		WithLogger(slog.New(slog.DiscardHandler)),
	}

	_, err := NewAgent(options...).Initialize(context.Background(), acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	require.Error(t, err)

	_, err = NewAgent(options...).NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir()})
	require.ErrorContains(t, err, "ProviderAuthRoot must be an absolute path")
}

func TestInitializeAdvertisesProviderAuthOnlyWhenEnabled(t *testing.T) {
	agent := newAuthAgent(t).agent

	response, err := agent.Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)

	vendor, ok := response.AgentCapabilities.Meta[opencodeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Contains(t, vendor, providerAuthCapabilityKey)

	plain := NewAgent(WithLogger(slog.New(slog.DiscardHandler)))

	plainResponse, err := plain.Initialize(context.Background(), acp.InitializeRequest{})
	require.NoError(t, err)

	plainVendor, ok := plainResponse.AgentCapabilities.Meta[opencodeMetaKey].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, plainVendor, providerAuthCapabilityKey)
}

func TestUnadvertisedAuthLegsReturnMethodNotFound(t *testing.T) {
	agent := NewAgent(WithLogger(slog.New(slog.DiscardHandler)))

	for _, method := range authMethodNames() {
		_, err := agent.HandleExtensionMethod(context.Background(), method, json.RawMessage(`{}`))

		var reqErr *acp.RequestError

		require.ErrorAs(t, err, &reqErr)
		require.Equal(t, -32601, reqErr.Code)
	}
}

func TestAdvertisedAuthLegsAllAnswer(t *testing.T) {
	harness := newAuthAgent(t)
	agent, client, session := harness.agent, harness.runtime, harness.session
	withBrokerFactory(t, agent)

	client.providerCatalog = []opencode.ProviderCatalogEntry{{ID: "deepseek", Name: "DeepSeek"}}

	params := mustJSON(t, map[string]any{authFieldSessionID: string(session.id)})

	methods, err := agent.HandleExtensionMethod(context.Background(), AuthMethodsMethod, params)
	require.NoError(t, err)

	result, ok := methods.(authMethodsResult)
	require.True(t, ok)
	require.NotEmpty(t, result.Generation)

	inventory, err := agent.HandleExtensionMethod(context.Background(), AuthInventoryMethod, params)
	require.NoError(t, err)
	require.Equal(t, authInventoryResult{Entries: []authInventoryEntry{}}, inventory)

	// Every remaining leg answers rather than reporting method-not-found; each
	// one's own semantics are covered beside the leg.
	for _, method := range []string{AuthAuthorizeMethod, AuthCallbackMethod, AuthStatusMethod, AuthCancelMethod, AuthDisconnectMethod} {
		_, err := agent.HandleExtensionMethod(context.Background(), method, json.RawMessage(`{}`))

		var reqErr *acp.RequestError

		require.ErrorAs(t, err, &reqErr)
		require.NotEqual(t, -32601, reqErr.Code)
	}
}

func TestHandleAuthExtensionMethodIgnoresForeignMethods(t *testing.T) {
	agent := newAuthAgent(t).agent

	result, handled, err := agent.handleAuthExtensionMethod(context.Background(), "_opencode/other", nil)
	require.Nil(t, result)
	require.False(t, handled)
	require.NoError(t, err)
}

func TestAuthFailureCarriesTheClosedShape(t *testing.T) {
	failure := &authFailedError{cause: authCauseNativeVeto, providerID: "xai", method: "1", flowID: "flow"}
	require.Equal(t, "opencode_auth_failed: native_veto", failure.Error())

	reqErr := failure.requestError()
	require.Equal(t, -32000, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{
		jsonFieldError:      authFailedErrorTag,
		jsonFieldCause:      authCauseNativeVeto,
		"retryable":         false,
		authFieldProviderID: "xai",
		authFieldMethod:     "1",
		authFieldFlowID:     "flow",
	}, data)

	bare := (&authFailedError{cause: authCauseTransport}).requestError()

	bareData, ok := bare.Data.(map[string]any)
	require.True(t, ok)
	require.Len(t, bareData, 3)
	require.Equal(t, true, bareData["retryable"])
}

func TestAuthCauseRetryable(t *testing.T) {
	for _, cause := range []string{authCauseTransport, authCauseProcess, authCauseTimeout} {
		require.True(t, authCauseRetryable(cause))
	}

	for _, cause := range []string{
		authCauseNativeVeto, authCauseProviderRefused, authCauseHarvestFailed,
		authCauseUnsupportedVariant, authCauseFlowExpired, authCauseFlowState,
		authCauseFlowCancelled, authCausePolicy, authCauseBindingConflict,
	} {
		require.False(t, authCauseRetryable(cause))
	}
}

func TestAuthFlowTransitionMatrix(t *testing.T) {
	cases := []struct {
		cause    string
		inFlight bool
		state    string
		reason   string
	}{
		{cause: authCauseNativeVeto, state: authStateFailed, reason: authReasonNativeVeto},
		{cause: authCauseUnsupportedVariant, state: authStateFailed, reason: authReasonNativeVeto},
		{cause: authCauseProviderRefused, state: authStateFailed, reason: authReasonProviderRefused},
		{cause: authCauseTransport, state: authStateFailed, reason: authReasonTransport},
		{cause: authCauseTransport, inFlight: true, state: authStateFailed, reason: authReasonAcceptanceUnknown},
		{cause: authCauseProcess, state: authStateFailed, reason: authReasonProcess},
		{cause: authCauseProcess, inFlight: true, state: authStateFailed, reason: authReasonAcceptanceUnknown},
		{cause: authCauseTimeout, state: authStateFailed, reason: authReasonTransport},
		{cause: authCauseTimeout, inFlight: true, state: authStateFailed, reason: authReasonAcceptanceUnknown},
		{cause: authCauseHarvestFailed, state: authStateFailed, reason: authReasonHarvestFailed},
		{cause: authCauseFlowExpired, state: authStateExpired, reason: authReasonDeadline},
		{cause: authCausePolicy},
		{cause: authCauseBindingConflict},
		{cause: authCauseFlowState},
		{cause: authCauseFlowCancelled},
	}

	for _, testCase := range cases {
		state, reason := authFlowTransition(testCase.cause, testCase.inFlight)
		require.Equal(t, testCase.state, state, testCase.cause)
		require.Equal(t, testCase.reason, reason, testCase.cause)
	}
}

func TestAuthParamFieldsRejectsClosedObjectViolations(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		field string
	}{
		{name: "not an object", raw: `[]`, field: "params"},
		{name: "unknown field", raw: `{"sessionId":"a","extra":1}`, field: "extra"},
		{name: "duplicate field", raw: `{"sessionId":"a","sessionId":"b"}`, field: authFieldSessionID},
		{name: "malformed value", raw: `{"sessionId":}`, field: authFieldSessionID},
		{name: "truncated object", raw: `{"sessionId":"a"`, field: "params"},
		{name: "trailing content", raw: `{"sessionId":"a"} {}`, field: "params"},
		{name: "non string key", raw: `{,}`, field: "params"},
		{name: "unterminated object", raw: `{`, field: "params"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := authParamFields(json.RawMessage(testCase.raw), authFieldSessionID)
			requireInvalidParams(t, err, testCase.field)
		})
	}

	fields, err := authParamFields(json.RawMessage(`{"sessionId":"a"}`), authFieldSessionID)
	require.NoError(t, err)
	require.Len(t, fields, 1)
}

func TestAuthFieldDecoders(t *testing.T) {
	fields := map[string]json.RawMessage{
		"empty":  json.RawMessage(`""`),
		"text":   json.RawMessage(`"value"`),
		"number": json.RawMessage(`7`),
	}

	_, err := authRequiredString(fields, "missing")
	requireInvalidParams(t, err, "missing")

	_, err = authRequiredString(fields, "empty")
	requireInvalidParams(t, err, "empty")

	_, err = authRequiredString(fields, "number")
	requireInvalidParams(t, err, "number")

	value, err := authRequiredString(fields, "text")
	require.NoError(t, err)
	require.Equal(t, "value", value)

	_, err = authString(fields, "missing")
	requireInvalidParams(t, err, "missing")

	_, err = authString(fields, "number")
	requireInvalidParams(t, err, "number")

	empty, err := authString(fields, "empty")
	require.NoError(t, err)
	require.Empty(t, empty)

	_, err = authRequiredInt64(fields, "missing")
	requireInvalidParams(t, err, "missing")

	_, err = authRequiredInt64(fields, "text")
	requireInvalidParams(t, err, "text")

	number, err := authRequiredInt64(fields, "number")
	require.NoError(t, err)
	require.Equal(t, int64(7), number)
}

func TestAuthSessionRejectsUnknownSession(t *testing.T) {
	broker := newAuthAgent(t).broker

	_, err := broker.authSession("missing")
	requireInvalidParams(t, err, jsonFieldSessionID)
}

func TestNativeClientReportsTheSessionRuntime(t *testing.T) {
	harness := newAuthAgent(t)
	client, session := harness.runtime, harness.session
	require.Equal(t, opencode.Client(client), session.nativeClient())
}

func TestGoSafeRecoversPanics(t *testing.T) {
	broker := newAuthAgent(t).broker

	done := make(chan struct{})

	broker.goSafe("test", func() {
		defer close(done)

		panic("boom")
	})

	<-done
}

func TestLoggableError(t *testing.T) {
	attr := loggableError(errors.New("failure"))
	require.Equal(t, jsonFieldError, attr.Key)
	require.Equal(t, "operation failed", attr.Value.String())
}

func TestValidateProviderAuthOptionsRejectsDirectHome(t *testing.T) {
	require.NoError(t, validateProviderAuthOptions(Options{}))

	err := validateProviderAuthOptions(Options{ProviderAuthDirectHome: "/home/opencode"})

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32602, reqErr.Code)
	require.Equal(t, map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: optionFieldProviderAuthDirectHome,
	}, reqErr.Data)
}

func TestSessionStartRejectsProviderAuthDirectHome(t *testing.T) {
	agent := NewAgent(
		WithProviderAuthDirectHome("/home/opencode"),
		WithLogger(slog.New(slog.DiscardHandler)),
	)

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir()})
	requireInvalidParams(t, err, optionFieldProviderAuthDirectHome)

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{SessionId: "session-1", Cwd: t.TempDir()})
	requireInvalidParams(t, err, optionFieldProviderAuthDirectHome)

	_, err = agent.forkSession(context.Background(), acp.UnstableForkSessionRequest{SessionId: "session-1", Cwd: t.TempDir()})
	requireInvalidParams(t, err, optionFieldProviderAuthDirectHome)
}
