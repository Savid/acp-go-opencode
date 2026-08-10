package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// authFixture wires an enabled provider-auth surface, a session runtime, and a
// broker factory, and publishes a two-method catalog for provider "xai".
type authFixture struct {
	agent      *Agent
	broker     *providerAuth
	runtime    *fakeOpenCodeClient
	brokerNode *fakeOpenCodeClient
	session    *session
	generation string

	callbackRelease chan struct{}
	releaseOnce     sync.Once
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()

	harness := newAuthAgent(t)
	agent, broker, runtime, session := harness.agent, harness.broker, harness.runtime, harness.session
	brokerNode := withBrokerFactory(t, agent)

	runtime.providerCatalog = []opencode.ProviderCatalogEntry{
		{ID: "xai", Name: "xAI"},
		{ID: "deepseek", Name: "DeepSeek"},
	}
	runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {
			{Type: authMethodTypeOAuth, Label: "xAI Grok OAuth"},
			{Type: authMethodTypeAPI, Label: "Manually enter API Key"},
		},
	}
	brokerNode.authorization = opencode.ProviderAuthorization{
		URL:          "https://accounts.x.ai/oauth2/device?user_code=2XRG-QGNV",
		Method:       opencode.ProviderAuthNativeMethodAuto,
		Instructions: "Open the device page and enter the displayed code",
	}
	callbackRelease := make(chan struct{})
	brokerNode.authCallbackRelease = callbackRelease

	fixture := &authFixture{
		agent: agent, broker: broker, runtime: runtime, brokerNode: brokerNode, session: session,
		callbackRelease: callbackRelease,
	}
	fixture.refreshCatalog(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		fixture.broker.closeSession(ctx, fixture.session.id)
		fixture.releaseCallback()
	})

	return fixture
}

func (f *authFixture) releaseCallback() {
	f.releaseOnce.Do(func() {
		close(f.callbackRelease)
	})
}

func (f *authFixture) useCodeAuthorization() {
	f.brokerNode.mu.Lock()
	defer f.brokerNode.mu.Unlock()

	f.brokerNode.authorization = opencode.ProviderAuthorization{
		URL:          "https://accounts.x.ai/oauth2/authorize",
		Method:       opencode.ProviderAuthNativeMethodCode,
		Instructions: "Paste the code shown after login",
	}
	f.brokerNode.authCallbackRelease = nil
}

func (f *authFixture) refreshCatalog(t *testing.T) {
	t.Helper()

	result, err := f.broker.methods(context.Background(), mustJSON(t, map[string]any{authFieldSessionID: string(f.session.id)}))
	require.NoError(t, err)

	methods, ok := result.(authMethodsResult)
	require.True(t, ok)

	f.generation = methods.Generation
}

func (f *authFixture) authorizeParams(t *testing.T, overrides map[string]any) json.RawMessage {
	t.Helper()

	params := map[string]any{
		authFieldSessionID:          string(f.session.id),
		authFieldProviderID:         "xai",
		authFieldConnectionID:       "conn-1",
		authFieldMethodsGeneration:  f.generation,
		authFieldMethod:             "0",
		authFieldAuthorizeRequestID: "req-1",
	}
	for key, value := range overrides {
		if value == nil {
			delete(params, key)

			continue
		}

		params[key] = value
	}

	return mustJSON(t, params)
}

func (f *authFixture) authorize(t *testing.T, overrides map[string]any) authAuthorizeResult {
	t.Helper()

	result, err := f.broker.authorize(context.Background(), f.authorizeParams(t, overrides))
	require.NoError(t, err)

	presentation, ok := result.(authAuthorizeResult)
	require.True(t, ok)

	return presentation
}

func TestAuthorizeMintsADeviceFlow(t *testing.T) {
	fixture := newAuthFixture(t)

	result := fixture.authorize(t, nil)
	require.Equal(t, authInteractionWait, result.Interaction)
	require.Equal(t, "https://accounts.x.ai/oauth2/device?user_code=2XRG-QGNV", result.URL)
	require.Equal(t, "Open the device page and enter the displayed code", result.Message)
	require.Equal(t, "2XRG-QGNV", result.UserCode)
	require.Empty(t, result.CallbackInput)
	require.NotEmpty(t, result.FlowID)
	require.Positive(t, result.FlowExpiresAt)

	record, ok, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, authLedgerIntent, record.State)
	require.Equal(t, "conn-1", record.ConnectionID)
	require.Equal(t, result.FlowID, record.FlowID)
	require.Equal(t, "req-1", record.AuthorizeRequestID)
}

func TestAuthorizeDrivesWaitCompletionExactlyOnce(t *testing.T) {
	fixture := newAuthFixture(t)
	started := make(chan struct{}, 1)
	fixture.brokerNode.authCallbackStarted = started

	flow := fixture.authorize(t, nil)
	requireSignal(t, started)

	fixture.brokerNode.mu.Lock()
	fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{
		"xai": {Type: opencode.ProviderAuthTypeOAuth, Refresh: "r", Access: "a", Expires: 1783945909169},
	}
	fixture.brokerNode.mu.Unlock()
	fixture.releaseCallback()

	require.Eventually(t, func() bool {
		return fixture.status(t, flow.FlowID).State == authStateAuthenticated
	}, time.Second, time.Millisecond)

	for range 3 {
		require.Equal(t, authStateAuthenticated, fixture.status(t, flow.FlowID).State)
	}

	require.Equal(t, flow, fixture.authorize(t, nil))

	fixture.brokerNode.mu.Lock()
	require.Len(t, fixture.brokerNode.callbackCalls, 1)
	require.Empty(t, fixture.brokerNode.callbackCalls[0].code)
	fixture.brokerNode.mu.Unlock()

	fixture.runtime.mu.Lock()
	require.Len(t, fixture.runtime.setAuthCalls, 1)
	fixture.runtime.mu.Unlock()
}

func TestWaitCompletionRecordsNativeRefusal(t *testing.T) {
	fixture := newAuthFixture(t)
	started := make(chan struct{}, 1)
	fixture.brokerNode.authCallbackStarted = started
	fixture.brokerNode.authCallbackErr = &opencode.HTTPError{StatusCode: http.StatusBadRequest}

	flow := fixture.authorize(t, nil)
	requireSignal(t, started)
	fixture.releaseCallback()

	require.Eventually(t, func() bool {
		status := fixture.status(t, flow.FlowID)

		return status.State == authStateFailed && status.Reason == authReasonProviderRefused
	}, time.Second, time.Millisecond)

	fixture.brokerNode.mu.Lock()
	require.Len(t, fixture.brokerNode.callbackCalls, 1)
	fixture.brokerNode.mu.Unlock()

	fixture.runtime.mu.Lock()
	require.Empty(t, fixture.runtime.setAuthCalls)
	fixture.runtime.mu.Unlock()
}

func TestCancelStopsWaitCompletionWithoutInstalling(t *testing.T) {
	fixture := newAuthFixture(t)
	started := make(chan struct{}, 1)
	fixture.brokerNode.authCallbackStarted = started

	flow := fixture.authorize(t, nil)
	requireSignal(t, started)

	_, err := fixture.broker.cancel(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     flow.FlowID,
	}))
	require.NoError(t, err)
	fixture.releaseCallback()

	require.Eventually(t, func() bool {
		status := fixture.status(t, flow.FlowID)

		return status.State == authStateCancelled && status.Reason == authReasonOwnerCancel
	}, time.Second, time.Millisecond)

	fixture.runtime.mu.Lock()
	require.Empty(t, fixture.runtime.setAuthCalls)
	fixture.runtime.mu.Unlock()
}

func TestWaitFlowRejectsASecondCompletionDriver(t *testing.T) {
	fixture := newAuthFixture(t)
	started := make(chan struct{}, 1)
	fixture.brokerNode.authCallbackStarted = started

	flow := fixture.authorize(t, nil)
	requireSignal(t, started)

	_, err := fixture.callback(t, flow.FlowID, "0", "")
	requireAuthFailure(t, err, authCauseFlowState)

	fixture.brokerNode.mu.Lock()
	require.Len(t, fixture.brokerNode.callbackCalls, 1)
	fixture.brokerNode.mu.Unlock()
}

func TestAuthorizeMintsAPasteBackFlow(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.brokerNode.authorization = opencode.ProviderAuthorization{
		URL:          "https://accounts.x.ai/oauth2/authorize",
		Method:       opencode.ProviderAuthNativeMethodCode,
		Instructions: "Paste the code shown after login",
	}

	result := fixture.authorize(t, nil)
	require.Equal(t, authInteractionCallback, result.Interaction)
	require.Equal(t, authCallbackInputCode, result.CallbackInput)
	require.Empty(t, result.UserCode)
}

func TestAuthorizeMintsASecretFlowWithoutANativeCall(t *testing.T) {
	fixture := newAuthFixture(t)

	result := fixture.authorize(t, map[string]any{authFieldMethod: "1"})
	require.Equal(t, authInteractionSecret, result.Interaction)
	require.Empty(t, result.URL)
	require.Equal(t, "Manually enter API Key", result.Message)
	require.Empty(t, fixture.brokerNode.authorizeCalls)
}

func TestAuthorizeReplaysARepeatedRequestID(t *testing.T) {
	fixture := newAuthFixture(t)

	first := fixture.authorize(t, nil)
	second := fixture.authorize(t, nil)
	require.Equal(t, first, second)
	require.Len(t, fixture.brokerNode.authorizeCalls, 1)
}

// TestAuthorizeReplaysARepeatedRequestIDAfterTheFlowTerminalized pins the whole
// point of the idempotency key: the repeat that matters is the one a caller
// sends after the first answer was lost, which is exactly when the flow it
// names has already completed. Superseding there would drive a fresh native
// login and destroy the credential the caller had already earned.
func TestAuthorizeReplaysARepeatedRequestIDAfterTheFlowTerminalized(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()

	first := fixture.authorize(t, nil)
	fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{
		"xai": {Type: opencode.ProviderAuthTypeOAuth, Refresh: "r", Access: "a", Expires: 1783945909169},
	}

	_, err := fixture.callback(t, first.FlowID, "0", "accepted")
	require.NoError(t, err)
	require.Equal(t, authStateAuthenticated, fixture.status(t, first.FlowID).State)

	before, _, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)

	installs := len(fixture.runtime.setAuthCalls)

	replayed := fixture.authorize(t, nil)
	require.Equal(t, first, replayed)

	// No supersede, no second native mint, no second install, and no ledger
	// revision: the repeat consumed nothing the first call had earned.
	require.Len(t, fixture.brokerNode.authorizeCalls, 1)
	require.Len(t, fixture.runtime.setAuthCalls, installs)

	after, _, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.Equal(t, before, after)

	require.Equal(t, authStateAuthenticated, fixture.status(t, first.FlowID).State)
}

// TestAuthorizeStopsReplayingOnceTheSessionCloses pins the other half: the
// record lives exactly as long as the session that owns it, and the closed
// session admits no leg that could mint a replacement.
func TestAuthorizeStopsReplayingOnceTheSessionCloses(t *testing.T) {
	fixture := newAuthFixture(t)

	first := fixture.authorize(t, nil)

	fixture.broker.closeSession(context.Background(), fixture.session.id)

	_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	requireInvalidParams(t, err, jsonFieldSessionID)

	_, err = fixture.broker.status(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     first.FlowID,
	}))
	requireInvalidParams(t, err, jsonFieldSessionID)
}

// TestAuthorizeMintFailureAddressesTheFlowItNames pins the flowId a failed mint
// returns against a record a caller can actually address, and pins that a
// different key retries.
func TestAuthorizeMintFailureAddressesTheFlowItNames(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.brokerNode.authorizeErr = errors.New("native authorize refused")

	_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	requireAuthFailure(t, err, authCauseTransport)

	flowID := authFailureFlowID(t, err)

	status := fixture.status(t, flowID)
	require.Equal(t, authStateFailed, status.State)
	require.Equal(t, authReasonTransport, status.Reason)

	fixture.brokerNode.authorizeErr = nil

	retried := fixture.authorize(t, map[string]any{authFieldAuthorizeRequestID: "req-2"})
	require.NotEqual(t, flowID, retried.FlowID)
	require.NotEmpty(t, retried.URL)
}

// TestAuthorizeReplaysAFailedMintVerbatim pins the half of the idempotency rule
// a failed mint is most likely to break: a repeat of the same key must answer
// with the same flowId and the same cause rather than drive a second native
// login, which would both mint a second flow at the provider and hand a caller
// a different retryability than the call it repeats.
func TestAuthorizeReplaysAFailedMintVerbatim(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.brokerNode.authorizeErr = &opencode.HTTPError{StatusCode: http.StatusBadRequest}

	_, first := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	requireAuthFailure(t, first, authCauseProviderRefused)

	fixture.brokerNode.authorizeErr = nil

	_, second := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
	requireAuthFailure(t, second, authCauseProviderRefused)

	require.Equal(t, authFailureFlowID(t, first), authFailureFlowID(t, second))
	require.Len(t, fixture.brokerNode.authorizeCalls, 1)
	require.False(t, authFailureRetryable(t, second))
}

// TestAuthorizeReplayWaitsOutAMintStillUnderWay pins that a repeat arriving
// while the first mint is still running answers that mint's outcome instead of
// racing past it into a second native login.
func TestAuthorizeReplayWaitsOutAMintStillUnderWay(t *testing.T) {
	fixture := newAuthFixture(t)

	release := make(chan struct{})
	authorization := fixture.brokerNode.authorization

	fixture.brokerNode.authorizeFunc = func(string, int, map[string]string) (opencode.ProviderAuthorization, error) {
		<-release

		return authorization, nil
	}

	params := fixture.authorizeParams(t, nil)
	minted := make(chan any, 1)
	replayed := make(chan any, 1)

	go func() {
		result, _ := fixture.broker.authorize(context.Background(), params)
		minted <- result
	}()

	// The repeat is registered against a flow whose mint has not settled, so it
	// blocks on that mint rather than starting one of its own.
	go func() {
		time.Sleep(50 * time.Millisecond)

		result, _ := fixture.broker.authorize(context.Background(), params)
		replayed <- result
	}()

	time.Sleep(150 * time.Millisecond)
	close(release)

	require.Equal(t, <-minted, <-replayed)
	require.Len(t, fixture.brokerNode.authorizeCalls, 1)
}

// TestAuthorizeReplayAbandonsAMintOnCallerCancellation pins that the repeat's
// own context, not the mint it is queued behind, bounds how long it waits at
// the key's admission gate.
func TestAuthorizeReplayAbandonsAMintOnCallerCancellation(t *testing.T) {
	fixture := newAuthFixture(t)

	release := make(chan struct{})
	authorization := fixture.brokerNode.authorization

	fixture.brokerNode.authorizeFunc = func(string, int, map[string]string) (opencode.ProviderAuthorization, error) {
		<-release

		return authorization, nil
	}

	params := fixture.authorizeParams(t, nil)
	minted := make(chan struct{})

	go func() {
		defer close(minted)

		_, _ = fixture.broker.authorize(context.Background(), params)
	}()

	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fixture.broker.authorize(ctx, params)
	requireAuthFailure(t, err, authCauseTimeout)

	close(release)
	<-minted
}

func authFailureData(t *testing.T, err error) map[string]any {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)

	return data
}

func authFailureFlowID(t *testing.T, err error) string {
	t.Helper()

	flowID, ok := authFailureData(t, err)[authFieldFlowID].(string)
	require.True(t, ok)
	require.NotEmpty(t, flowID)

	return flowID
}

func authFailureRetryable(t *testing.T, err error) bool {
	t.Helper()

	retryable, ok := authFailureData(t, err)["retryable"].(bool)
	require.True(t, ok)

	return retryable
}

func TestAuthorizeSupersedesTheEarlierFlow(t *testing.T) {
	fixture := newAuthFixture(t)

	first := fixture.authorize(t, nil)
	second := fixture.authorize(t, map[string]any{authFieldAuthorizeRequestID: "req-2"})
	require.NotEqual(t, first.FlowID, second.FlowID)

	_, err := fixture.broker.status(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     first.FlowID,
	}))
	requireInvalidParams(t, err, authFieldFlowID)

	record, _, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.Equal(t, int64(2), record.Revision)
}

func TestAuthorizeSupersedeIgnoresATerminalFlow(t *testing.T) {
	fixture := newAuthFixture(t)

	first := fixture.authorize(t, nil)

	_, err := fixture.broker.cancel(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     first.FlowID,
	}))
	require.NoError(t, err)

	second := fixture.authorize(t, map[string]any{authFieldAuthorizeRequestID: "req-2"})
	require.NotEqual(t, first.FlowID, second.FlowID)
}

func TestAuthorizeAddressingFailures(t *testing.T) {
	fixture := newAuthFixture(t)

	cases := []struct {
		name      string
		overrides map[string]any
		field     string
	}{
		{name: "missing session", overrides: map[string]any{authFieldSessionID: nil}, field: authFieldSessionID},
		{name: "missing provider", overrides: map[string]any{authFieldProviderID: nil}, field: authFieldProviderID},
		{name: "missing connection", overrides: map[string]any{authFieldConnectionID: nil}, field: authFieldConnectionID},
		{name: "missing generation", overrides: map[string]any{authFieldMethodsGeneration: nil}, field: authFieldMethodsGeneration},
		{name: "missing method", overrides: map[string]any{authFieldMethod: nil}, field: authFieldMethod},
		{name: "missing request id", overrides: map[string]any{authFieldAuthorizeRequestID: nil}, field: authFieldAuthorizeRequestID},
		{name: "unknown session", overrides: map[string]any{authFieldSessionID: "missing"}, field: jsonFieldSessionID},
		{name: "stale generation", overrides: map[string]any{authFieldMethodsGeneration: "stale"}, field: authFieldMethodsGeneration},
		{name: "unknown method", overrides: map[string]any{authFieldMethod: "99"}, field: authFieldMethod},
		{name: "malformed inputs", overrides: map[string]any{authFieldInputs: "not-an-object"}, field: authFieldInputs},
		{name: "unexpected inputs", overrides: map[string]any{authFieldInputs: map[string]string{"a": "b"}}, field: authFieldInputs},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, testCase.overrides))
			requireInvalidParams(t, err, testCase.field)
		})
	}

	_, err := fixture.broker.authorize(context.Background(), mustJSON(t, map[string]any{"extra": 1}))
	requireInvalidParams(t, err, "extra")
}

func TestAuthorizeRejectsAMethodFromANeverMintedCatalog(t *testing.T) {
	harness := newAuthAgent(t)
	broker, session := harness.broker, harness.session

	_, err := broker.authorize(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:          string(session.id),
		authFieldProviderID:         "xai",
		authFieldConnectionID:       "conn-1",
		authFieldMethodsGeneration:  "unknown",
		authFieldMethod:             "0",
		authFieldAuthorizeRequestID: "req-1",
	}))
	requireInvalidParams(t, err, authFieldMethodsGeneration)
}

func TestAuthorizeNativeFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, fixture *authFixture)
		cause string
	}{
		{name: "flow id entropy", cause: authCauseProcess, setup: func(t *testing.T, _ *authFixture) {
			t.Helper()

			original := authRandRead
			authRandRead = func([]byte) (int, error) { return 0, errors.New("entropy") }

			t.Cleanup(func() { authRandRead = original })
		}},
		{name: "ledger write", cause: authCauseProcess, setup: func(t *testing.T, _ *authFixture) {
			t.Helper()

			ledgerRename = func(string, string) error { return errors.New("rename") }
			ledgerRemove = func(string) error { return nil }

			t.Cleanup(func() {
				ledgerRename = os.Rename
				ledgerRemove = os.Remove
			})
		}},
		{name: "broker start", cause: authCauseProcess, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
				return nil, errors.New("spawn")
			}
		}},
		{name: "native authorize", cause: authCauseTransport, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.brokerNode.authorizeErr = errors.New("http")
		}},
		{name: "native refusal", cause: authCauseProviderRefused, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.brokerNode.authorizeErr = &opencode.HTTPError{StatusCode: http.StatusBadRequest}
		}},
		{name: "loopback method", cause: authCauseUnsupportedVariant, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.brokerNode.authorization.URL = "http://127.0.0.1:39999/oauth/authorize"
		}},
		{name: "url over bound", cause: authCauseNativeVeto, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.brokerNode.authorization.URL = "http://accounts.x.ai/oauth2/device"
		}},
		{name: "instructions over bound", cause: authCauseNativeVeto, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.brokerNode.authorization.Instructions = strings.Repeat("m", authMaxMessageBytes+1)
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAuthFixture(t)
			testCase.setup(t, fixture)

			_, err := fixture.broker.authorize(context.Background(), fixture.authorizeParams(t, nil))
			requireAuthFailure(t, err, testCase.cause)
		})
	}
}

func TestAuthorizeCarriesValidatedInputsIntoTheNativeMint(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {{Type: authMethodTypeOAuth, Label: "xAI", Prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "note", Message: "Note"},
		}}},
	}
	fixture.refreshCatalog(t)

	fixture.authorize(t, map[string]any{authFieldInputs: map[string]string{"note": "hello"}})
	require.Equal(t, map[string]string{"note": "hello"}, fixture.brokerNode.authorizeCalls[0].inputs)
}

func TestCallbackAppliesASecretAndReachesSaved(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {{Type: authMethodTypeAPI, Label: "Manually enter API Key", Prompts: []opencode.ProviderAuthPrompt{
			{Type: "text", Key: "instanceUrl2", Message: "Instance"},
		}}},
	}
	fixture.refreshCatalog(t)

	flow := fixture.authorize(t, map[string]any{authFieldInputs: map[string]string{"instanceUrl2": "value"}})

	result, err := fixture.broker.callback(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldMethod:     "0",
		authFieldFlowID:     flow.FlowID,
		authFieldInput:      "sk-secret",
	}))
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, result)

	require.Equal(t, opencode.ProviderAuthCredential{
		Type: opencode.ProviderAuthTypeAPI, Key: "sk-secret", Metadata: map[string]string{"instanceUrl2": "value"},
	}, fixture.runtime.setAuthCalls[0].credential)
	require.Equal(t, 1, fixture.runtime.disposed)

	record, _, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.Equal(t, authLedgerConfirmed, record.State)

	status := fixture.status(t, flow.FlowID)
	require.Equal(t, authStateSaved, status.State)
	require.Zero(t, status.ExpiresAt)
}

func (f *authFixture) status(t *testing.T, flowID string) authStatusResult {
	t.Helper()

	result, err := f.broker.status(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(f.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     flowID,
	}))
	require.NoError(t, err)

	status, ok := result.(authStatusResult)
	require.True(t, ok)

	return status
}

func (f *authFixture) callback(t *testing.T, flowID string, method string, input string) (any, error) {
	t.Helper()

	return f.broker.callback(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(f.session.id),
		authFieldProviderID: "xai",
		authFieldMethod:     method,
		authFieldFlowID:     flowID,
		authFieldInput:      input,
	}))
}

func TestCallbackDrivesCodeOAuthCompletion(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()

	flow := fixture.authorize(t, nil)
	fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{
		"xai": {Type: opencode.ProviderAuthTypeOAuth, Refresh: "r", Access: "a", Expires: 1783945909169},
	}

	result, err := fixture.callback(t, flow.FlowID, "0", "accepted")
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, result)
	require.Equal(t, "accepted", fixture.brokerNode.callbackCalls[0].code)
	require.Equal(t, "xai", fixture.runtime.setAuthCalls[0].providerID)

	status := fixture.status(t, flow.FlowID)
	require.Equal(t, authStateAuthenticated, status.State)
	require.Equal(t, int64(1783945909169), status.ExpiresAt)
	require.Empty(t, status.Reason)
}

func TestCallbackAddressingFailures(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, nil)

	_, err := fixture.broker.callback(context.Background(), mustJSON(t, map[string]any{"extra": 1}))
	requireInvalidParams(t, err, "extra")

	base := map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldMethod:     "0",
		authFieldFlowID:     flow.FlowID,
		authFieldInput:      "",
	}

	for _, field := range []string{authFieldSessionID, authFieldProviderID, authFieldMethod, authFieldFlowID, authFieldInput} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, fieldErr := fixture.broker.callback(context.Background(), mustJSON(t, params))
		requireInvalidParams(t, fieldErr, field)
	}

	_, err = fixture.callback(t, flow.FlowID, "1", "")
	requireInvalidParams(t, err, authFieldMethod)

	_, err = fixture.broker.callback(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  "missing",
		authFieldProviderID: "xai",
		authFieldMethod:     "0",
		authFieldFlowID:     flow.FlowID,
		authFieldInput:      "",
	}))
	requireInvalidParams(t, err, jsonFieldSessionID)

	_, err = fixture.callback(t, "unknown-flow", "0", "")
	requireInvalidParams(t, err, authFieldFlowID)
}

func TestCallbackOnATerminalFlowIsAFlowStateFailure(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()

	flow := fixture.authorize(t, nil)

	_, err := fixture.broker.cancel(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     flow.FlowID,
	}))
	require.NoError(t, err)

	_, err = fixture.callback(t, flow.FlowID, "0", "accepted")
	requireAuthFailure(t, err, authCauseFlowState)
}

func TestCallbackInputValidation(t *testing.T) {
	fixture := newAuthFixture(t)

	waitFlow := fixture.authorize(t, nil)

	_, err := fixture.callback(t, waitFlow.FlowID, "0", "unexpected")
	requireInvalidParams(t, err, authFieldInput)

	fixture.useCodeAuthorization()

	codeFlow := fixture.authorize(t, map[string]any{authFieldAuthorizeRequestID: "req-2"})

	_, err = fixture.callback(t, codeFlow.FlowID, "0", "")
	requireInvalidParams(t, err, authFieldInput)

	_, err = fixture.callback(t, codeFlow.FlowID, "0", strings.Repeat("c", authMaxTextInputBytes+1))
	requireInvalidParams(t, err, authFieldInput)

	secretFlow := fixture.authorize(t, map[string]any{authFieldMethod: "1", authFieldAuthorizeRequestID: "req-3"})

	_, err = fixture.callback(t, secretFlow.FlowID, "1", "")
	requireInvalidParams(t, err, authFieldInput)

	_, err = fixture.callback(t, secretFlow.FlowID, "1", strings.Repeat("k", authMaxTextInputBytes+1))
	requireInvalidParams(t, err, authFieldInput)
}

func TestCallbackFailuresTerminalizeTheFlow(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(fixture *authFixture)
		cause  string
		reason string
	}{
		{name: "native callback transport", cause: authCauseTransport, reason: authReasonAcceptanceUnknown, setup: func(fixture *authFixture) {
			fixture.brokerNode.authCallbackErr = errors.New("http")
		}},
		{name: "native callback refusal", cause: authCauseProviderRefused, reason: authReasonProviderRefused, setup: func(fixture *authFixture) {
			fixture.brokerNode.authCallbackErr = &opencode.HTTPError{StatusCode: http.StatusBadRequest}
		}},
		{name: "store read fails", cause: authCauseHarvestFailed, reason: authReasonHarvestFailed, setup: func(fixture *authFixture) {
			fixture.brokerNode.storedAuthErr = errors.New("io")
		}},
		{name: "store is empty", cause: authCauseHarvestFailed, reason: authReasonHarvestFailed, setup: func(fixture *authFixture) {}},
		{name: "install fails", cause: authCauseTransport, reason: authReasonAcceptanceUnknown, setup: func(fixture *authFixture) {
			fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{"xai": {Type: opencode.ProviderAuthTypeOAuth}}
			fixture.runtime.setAuthErr = errors.New("http")
		}},
		{name: "dispose fails", cause: authCauseTransport, reason: authReasonAcceptanceUnknown, setup: func(fixture *authFixture) {
			fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{"xai": {Type: opencode.ProviderAuthTypeOAuth}}
			fixture.runtime.disposeErr = errors.New("http")
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAuthFixture(t)
			fixture.useCodeAuthorization()
			flow := fixture.authorize(t, nil)

			testCase.setup(fixture)

			_, err := fixture.callback(t, flow.FlowID, "0", "accepted")
			requireAuthFailure(t, err, testCase.cause)

			status := fixture.status(t, flow.FlowID)
			require.Equal(t, authStateFailed, status.State)
			require.Equal(t, testCase.reason, status.Reason)
		})
	}
}

func TestCallbackFailsWhenTheLedgerConfirmationCannotBeWritten(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()

	flow := fixture.authorize(t, nil)
	fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{"xai": {Type: opencode.ProviderAuthTypeOAuth}}

	ledgerRename = func(string, string) error { return errors.New("rename") }
	ledgerRemove = func(string) error { return nil }

	t.Cleanup(func() {
		ledgerRename = os.Rename
		ledgerRemove = os.Remove
	})

	_, err := fixture.callback(t, flow.FlowID, "0", "accepted")
	requireAuthFailure(t, err, authCauseProcess)
}

func TestCallbackFailsWhenTheRuntimeIsGone(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()

	flow := fixture.authorize(t, nil)
	fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{"xai": {Type: opencode.ProviderAuthTypeOAuth}}

	fixture.session.mu.Lock()
	fixture.session.client = nil
	fixture.session.mu.Unlock()

	_, err := fixture.callback(t, flow.FlowID, "0", "accepted")
	requireAuthFailure(t, err, authCauseTransport)
}

// TestCallbackOutlivingCancelDoesNotReTerminalizeTheFlow pins the one leg on
// this surface that can answer after the flow it addresses is closed. The
// native callback route blocks until the provider settles and runs on a client
// with no request timeout, so cancel is what unblocks it — and the answer that
// then arrives must not overwrite the owner's own terminal transition, nor
// install anything into the durable store.
func TestCallbackOutlivingCancelDoesNotReTerminalizeTheFlow(t *testing.T) {
	cases := []struct {
		name    string
		native  error
		expects string
	}{
		{name: "native callback fails", native: errors.New("http"), expects: authCauseFlowCancelled},
		{name: "native callback settles", native: nil, expects: authCauseFlowCancelled},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAuthFixture(t)
			fixture.useCodeAuthorization()
			flow := fixture.authorize(t, nil)
			fixture.brokerNode.storedAuth = map[string]opencode.ProviderAuthCredential{
				"xai": {Type: opencode.ProviderAuthTypeOAuth, Refresh: "r", Access: "a"},
			}

			cancelled := make(chan struct{})
			fixture.brokerNode.authCallbackFunc = func(string, int, string) error {
				<-cancelled

				return testCase.native
			}

			answered := make(chan error, 1)

			go func() {
				_, err := fixture.broker.callback(context.Background(), mustJSON(t, map[string]any{
					authFieldSessionID:  string(fixture.session.id),
					authFieldProviderID: "xai",
					authFieldMethod:     "0",
					authFieldFlowID:     flow.FlowID,
					authFieldInput:      "accepted",
				}))
				answered <- err
			}()

			time.Sleep(50 * time.Millisecond)

			_, err := fixture.broker.cancel(context.Background(), mustJSON(t, map[string]any{
				authFieldSessionID:  string(fixture.session.id),
				authFieldProviderID: "xai",
				authFieldFlowID:     flow.FlowID,
			}))
			require.NoError(t, err)
			close(cancelled)

			requireAuthFailure(t, <-answered, testCase.expects)

			status := fixture.status(t, flow.FlowID)
			require.Equal(t, authStateCancelled, status.State)
			require.Equal(t, authReasonOwnerCancel, status.Reason)
			require.Empty(t, fixture.runtime.setAuthCalls)
		})
	}
}

// TestSecretApplyOutlivingCancelAnswersForTheClosedFlow pins the secret leg
// against the oauth one it sits beside. Its native apply blocks too, so cancel
// closes the flow underneath it just as readily — and an apply that then failed
// wrote nothing, so answering with a retryable native cause reports a failure
// against a flow that no longer exists. An apply that landed is reported as the
// acceptance it was, because the value is in the store either way.
func TestSecretApplyOutlivingCancelAnswersForTheClosedFlow(t *testing.T) {
	cases := []struct {
		name      string
		native    error
		expects   string
		installed int
	}{
		{name: "native apply fails", native: errors.New("http"), expects: authCauseFlowCancelled},
		{name: "native apply lands", native: nil, installed: 1},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAuthFixture(t)
			fixture.runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
				"xai": {{Type: authMethodTypeAPI, Label: "Manually enter API Key"}},
			}
			fixture.refreshCatalog(t)

			flow := fixture.authorize(t, nil)

			cancelled := make(chan struct{})
			fixture.runtime.setAuthFunc = func(string, opencode.ProviderAuthCredential) error {
				<-cancelled

				return testCase.native
			}

			callbackParams := mustJSON(t, map[string]any{
				authFieldSessionID:  string(fixture.session.id),
				authFieldProviderID: "xai",
				authFieldMethod:     "0",
				authFieldFlowID:     flow.FlowID,
				authFieldInput:      "sk-secret",
			})
			cancelParams := mustJSON(t, map[string]any{
				authFieldSessionID:  string(fixture.session.id),
				authFieldProviderID: "xai",
				authFieldFlowID:     flow.FlowID,
			})

			answered := make(chan error, 1)

			go func() {
				_, err := fixture.broker.callback(context.Background(), callbackParams)
				answered <- err
			}()

			time.Sleep(50 * time.Millisecond)

			_, err := fixture.broker.cancel(context.Background(), cancelParams)
			require.NoError(t, err)
			close(cancelled)

			if testCase.expects == "" {
				require.NoError(t, <-answered)
			} else {
				requireAuthFailure(t, <-answered, testCase.expects)
			}

			status := fixture.status(t, flow.FlowID)
			require.Equal(t, authStateCancelled, status.State)
			require.Equal(t, authReasonOwnerCancel, status.Reason)
			require.Len(t, fixture.runtime.setAuthCalls, testCase.installed)
		})
	}
}

// TestSupersededSecretApplyLeavesTheSuccessorsLedgerEntry pins the provenance
// half of an apply that outlived its own flow. The replacing authorize cancels
// the flow before the apply's native write returns, but it cannot claim the
// provider's next revision until that write has confirmed its own — so the
// entry ends up naming the successor, and no closed flow's binding is left
// sitting over the one that replaced it.
func TestSupersededSecretApplyLeavesTheSuccessorsLedgerEntry(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{
		"xai": {{Type: authMethodTypeAPI, Label: "Manually enter API Key"}},
	}
	fixture.refreshCatalog(t)

	flow := fixture.authorize(t, nil)

	superseded := make(chan struct{})
	fixture.runtime.setAuthFunc = func(string, opencode.ProviderAuthCredential) error {
		<-superseded

		return nil
	}

	callbackParams := mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldMethod:     "0",
		authFieldFlowID:     flow.FlowID,
		authFieldInput:      "sk-secret",
	})

	answered := make(chan error, 1)

	go func() {
		_, err := fixture.broker.callback(context.Background(), callbackParams)
		answered <- err
	}()

	time.Sleep(50 * time.Millisecond)

	replaced := make(chan authAuthorizeResult, 1)

	go func() {
		replaced <- fixture.authorize(t, map[string]any{authFieldAuthorizeRequestID: "req-2"})
	}()

	// The successor supersedes the flow immediately and then parks on the slot,
	// so it is the apply's own write that decides the entry it later replaces.
	time.Sleep(authAdmissionSettleWait)
	close(superseded)

	require.NoError(t, <-answered)

	successor := <-replaced
	require.Len(t, fixture.runtime.setAuthCalls, 1)
	require.Equal(t, authStatePending, fixture.status(t, successor.FlowID).State)

	record, ok, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, successor.FlowID, record.FlowID)
	require.Equal(t, int64(2), record.Revision)
	require.Equal(t, authLedgerIntent, record.State)
}

// TestTerminalizeKeepsTheFirstTerminalTransition pins the record itself: a
// flow has one terminal transition, and a later one is dropped rather than
// overwriting the owner's.
func TestTerminalizeKeepsTheFirstTerminalTransition(t *testing.T) {
	fixture := newAuthFixture(t)
	flow := fixture.authorize(t, nil)
	record := fixture.broker.byID[flow.FlowID]

	fixture.broker.terminalize(record, authStateCancelled, authReasonOwnerCancel, 0)
	fixture.broker.terminalize(record, authStateFailed, authReasonTransport, 0)

	status := fixture.status(t, flow.FlowID)
	require.Equal(t, authStateCancelled, status.State)
	require.Equal(t, authReasonOwnerCancel, status.Reason)
}

// TestCallbackOutlivingCompletionAnswersFlowState pins the other terminal
// state a leg can find on its return: a flow another owner already completed
// is not a cancelled one, and the two answers stay distinguishable.
func TestCallbackOutlivingCompletionAnswersFlowState(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.useCodeAuthorization()
	flow := fixture.authorize(t, nil)

	settled := make(chan struct{})
	fixture.brokerNode.authCallbackFunc = func(string, int, string) error {
		<-settled

		return nil
	}

	answered := make(chan error, 1)

	go func() {
		_, err := fixture.broker.callback(context.Background(), mustJSON(t, map[string]any{
			authFieldSessionID:  string(fixture.session.id),
			authFieldProviderID: "xai",
			authFieldMethod:     "0",
			authFieldFlowID:     flow.FlowID,
			authFieldInput:      "accepted",
		}))
		answered <- err
	}()

	time.Sleep(50 * time.Millisecond)

	fixture.broker.terminalize(fixture.broker.byID[flow.FlowID], authStateAuthenticated, "", 0)
	close(settled)

	requireAuthFailure(t, <-answered, authCauseFlowState)
	require.Equal(t, authStateAuthenticated, fixture.status(t, flow.FlowID).State)
}

func TestCallbackFailsWhenTheBrokerIsAlreadyGone(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, nil)

	fixture.broker.mu.Lock()
	fixture.broker.byID[flow.FlowID].broker = nil
	fixture.broker.mu.Unlock()

	_, err := fixture.callback(t, flow.FlowID, "0", "")
	requireAuthFailure(t, err, authCauseFlowState)
}

func TestStatusAddressingFailures(t *testing.T) {
	fixture := newAuthFixture(t)

	_, err := fixture.broker.status(context.Background(), mustJSON(t, map[string]any{"extra": 1}))
	requireInvalidParams(t, err, "extra")

	base := map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     "flow",
	}

	for _, field := range []string{authFieldSessionID, authFieldProviderID, authFieldFlowID} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, fieldErr := fixture.broker.status(context.Background(), mustJSON(t, params))
		requireInvalidParams(t, fieldErr, field)
	}

	_, err = fixture.broker.status(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  "missing",
		authFieldProviderID: "xai",
		authFieldFlowID:     "flow",
	}))
	requireInvalidParams(t, err, jsonFieldSessionID)

	flow := fixture.authorize(t, nil)

	_, err = fixture.broker.status(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "deepseek",
		authFieldFlowID:     flow.FlowID,
	}))
	requireInvalidParams(t, err, authFieldFlowID)
}

func TestCancelIsIdempotentAndDestroysTheBrokerHome(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, nil)

	fixture.broker.mu.Lock()
	home := fixture.broker.byID[flow.FlowID].broker.home
	fixture.broker.mu.Unlock()

	params := mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     flow.FlowID,
	})

	result, err := fixture.broker.cancel(context.Background(), params)
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, result)
	require.NoDirExists(t, home)
	require.True(t, fixture.brokerNode.closed)

	again, err := fixture.broker.cancel(context.Background(), params)
	require.NoError(t, err)
	require.Equal(t, authFlowIDResult{FlowID: flow.FlowID}, again)

	status := fixture.status(t, flow.FlowID)
	require.Equal(t, authStateCancelled, status.State)
	require.Equal(t, authReasonOwnerCancel, status.Reason)
}

func TestCancelAddressingFailure(t *testing.T) {
	fixture := newAuthFixture(t)

	_, err := fixture.broker.cancel(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:  string(fixture.session.id),
		authFieldProviderID: "xai",
		authFieldFlowID:     "unknown",
	}))
	requireInvalidParams(t, err, authFieldFlowID)
}

func TestDisconnectBumpsTheGenerationThenRemovesAndVerifies(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, map[string]any{authFieldMethod: "1"})

	_, err := fixture.callback(t, flow.FlowID, "1", "sk-secret")
	require.NoError(t, err)

	result, err := fixture.disconnect(t, "conn-1", 1)
	require.NoError(t, err)
	require.Equal(t, struct{}{}, result)
	require.Equal(t, []string{"xai"}, fixture.runtime.removedAuth)

	record, _, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.Equal(t, authLedgerRemoved, record.State)
	require.Equal(t, int64(2), record.BindingGeneration)

	inventory, err := fixture.broker.inventory(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID: string(fixture.session.id),
	}))
	require.NoError(t, err)
	entries, ok := inventory.(authInventoryResult)
	require.True(t, ok)
	require.Empty(t, entries.Entries)
}

func (f *authFixture) disconnect(t *testing.T, connectionID string, generation int64) (any, error) {
	t.Helper()

	return f.broker.disconnect(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:         string(f.session.id),
		authFieldProviderID:        "xai",
		authFieldConnectionID:      connectionID,
		authFieldBindingGeneration: generation,
	}))
}

func TestDisconnectFencesOnTheLedgerEntry(t *testing.T) {
	fixture := newAuthFixture(t)

	_, err := fixture.disconnect(t, "conn-1", 1)
	requireAuthFailure(t, err, authCauseBindingConflict)

	require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "conn-1", BindingGeneration: 1, State: authLedgerConfirmed,
	}))

	_, err = fixture.disconnect(t, "other", 1)
	requireAuthFailure(t, err, authCauseBindingConflict)

	_, err = fixture.disconnect(t, "conn-1", 9)
	requireAuthFailure(t, err, authCauseBindingConflict)

	// Each refusal landed before the generation bump and before the native
	// removal, so the entry the live binding names is untouched.
	live, ok, readErr := fixture.broker.ledger.read("xai")
	require.NoError(t, readErr)
	require.True(t, ok)
	require.Equal(t, int64(1), live.BindingGeneration)
	require.Equal(t, authLedgerConfirmed, live.State)
}

func TestDisconnectAddressingFailures(t *testing.T) {
	fixture := newAuthFixture(t)

	_, err := fixture.broker.disconnect(context.Background(), mustJSON(t, map[string]any{"extra": 1}))
	requireInvalidParams(t, err, "extra")

	base := map[string]any{
		authFieldSessionID:         string(fixture.session.id),
		authFieldProviderID:        "xai",
		authFieldConnectionID:      "conn-1",
		authFieldBindingGeneration: 1,
	}

	for _, field := range []string{authFieldSessionID, authFieldProviderID, authFieldConnectionID, authFieldBindingGeneration} {
		params := map[string]any{}
		for key, value := range base {
			params[key] = value
		}

		delete(params, field)

		_, fieldErr := fixture.broker.disconnect(context.Background(), mustJSON(t, params))
		requireInvalidParams(t, fieldErr, field)
	}

	_, err = fixture.broker.disconnect(context.Background(), mustJSON(t, map[string]any{
		authFieldSessionID:         "missing",
		authFieldProviderID:        "xai",
		authFieldConnectionID:      "conn-1",
		authFieldBindingGeneration: 1,
	}))
	requireInvalidParams(t, err, jsonFieldSessionID)
}

func TestDisconnectNativeFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, fixture *authFixture)
		cause string
	}{
		{name: "ledger read", cause: authCauseHarvestFailed, setup: func(t *testing.T, _ *authFixture) {
			t.Helper()

			ledgerReadFile = func(string) ([]byte, error) { return nil, errors.New("io") }

			t.Cleanup(func() { ledgerReadFile = os.ReadFile })
		}},
		{name: "generation write", cause: authCauseProcess, setup: func(t *testing.T, _ *authFixture) {
			t.Helper()

			ledgerRename = func(string, string) error { return errors.New("rename") }
			ledgerRemove = func(string) error { return nil }

			t.Cleanup(func() {
				ledgerRename = os.Rename
				ledgerRemove = os.Remove
			})
		}},
		{name: "runtime gone", cause: authCauseTransport, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.session.mu.Lock()
			fixture.session.client = nil
			fixture.session.mu.Unlock()
		}},
		{name: "native removal", cause: authCauseTransport, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.runtime.removeAuthErr = errors.New("http")
		}},
		{name: "still resident", cause: authCauseHarvestFailed, setup: func(_ *testing.T, fixture *authFixture) {
			fixture.runtime.removeAuthErr = errors.New("http")
			fixture.runtime.storedAuth = map[string]opencode.ProviderAuthCredential{"xai": {Type: opencode.ProviderAuthTypeAPI}}
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newAuthFixture(t)
			require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
				ProviderID: "xai", ConnectionID: "conn-1", BindingGeneration: 1, State: authLedgerConfirmed,
			}))

			testCase.setup(t, fixture)

			_, err := fixture.disconnect(t, "conn-1", 1)
			require.Error(t, err)
		})
	}
}

func TestDisconnectVerifiesAbsence(t *testing.T) {
	fixture := newAuthFixture(t)

	require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "conn-1", BindingGeneration: 1, State: authLedgerConfirmed,
	}))

	fixture.runtime.storedAuth = map[string]opencode.ProviderAuthCredential{"xai": {Type: opencode.ProviderAuthTypeAPI}}
	fixture.runtime.removeAuthErr = nil

	_, err := fixture.disconnect(t, "conn-1", 1)
	require.NoError(t, err)
}

func TestDisconnectFailsWhenTheRemovalRecordCannotBeWritten(t *testing.T) {
	fixture := newAuthFixture(t)

	require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "conn-1", BindingGeneration: 1, State: authLedgerConfirmed,
	}))

	writes := 0
	realRename := os.Rename
	ledgerRename = func(from string, to string) error {
		writes++
		if writes > 1 {
			return errors.New("rename")
		}

		return realRename(from, to)
	}
	ledgerRemove = func(string) error { return nil }

	t.Cleanup(func() {
		ledgerRename = os.Rename
		ledgerRemove = os.Remove
	})

	_, err := fixture.disconnect(t, "conn-1", 1)
	requireAuthFailure(t, err, authCauseProcess)
}

func TestSessionCloseCancelsPendingFlows(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, nil)

	fixture.broker.mu.Lock()
	home := fixture.broker.byID[flow.FlowID].broker.home
	fixture.broker.mu.Unlock()

	require.NoError(t, fixture.session.Close(context.Background()))
	require.NoDirExists(t, home)

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()
	require.Empty(t, fixture.broker.flows)
	require.Empty(t, fixture.broker.byID)
}

func TestCloseSessionSkipsTerminalAndForeignFlows(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, nil)

	fixture.broker.mu.Lock()
	record := fixture.broker.byID[flow.FlowID]
	record.state = authStateSaved
	fixture.broker.mu.Unlock()

	fixture.broker.closeSession(context.Background(), "other-session")

	fixture.broker.mu.Lock()
	require.Len(t, fixture.broker.flows, 1)
	fixture.broker.mu.Unlock()

	fixture.broker.closeSession(context.Background(), fixture.session.id)

	fixture.broker.mu.Lock()
	defer fixture.broker.mu.Unlock()
	require.Empty(t, fixture.broker.flows)
	require.Equal(t, authStateSaved, record.state)
}

func TestWaitCompletionClaimAndOAuthInputFailures(t *testing.T) {
	broker := &providerAuth{}
	claimed := &authFlow{state: authStatePending, claimed: true}
	broker.driveWaitCompletion(claimed, nil)
	require.Nil(t, claimed.completionDone)

	flow := &authFlow{presentation: authAuthorizeResult{CallbackInput: authCallbackInputCode}}
	_, err := broker.completeOAuth(context.Background(), nil, flow, "")
	require.Error(t, err)
}

func TestCloseSessionStopsWaitingWhenContextEnds(t *testing.T) {
	sessionID := acp.SessionId("session")
	key := authFlowKey{sessionID: sessionID, providerID: "provider"}
	completion := make(chan struct{})
	flow := &authFlow{id: "flow", sessionID: sessionID, providerID: "provider", state: authStatePending, completionDone: completion, disarm: make(chan struct{})}
	broker := &providerAuth{
		closedSessions: map[acp.SessionId]struct{}{},
		flows:          map[authFlowKey]*authFlow{key: flow},
		byID:           map[string]*authFlow{flow.id: flow},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broker.closeSession(ctx, sessionID)
	require.Equal(t, authStateCancelled, flow.state)
}

func TestExpireTerminalizesTheFlowOnTheDeadline(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, nil)
	record := fixture.broker.byID[flow.FlowID]

	fixture.broker.expire(record)

	fixture.broker.mu.Lock()
	require.Equal(t, authStateExpired, record.state)
	require.Equal(t, authReasonDeadline, record.reason)
	require.Nil(t, record.broker)
	fixture.broker.mu.Unlock()

	// A second expiry of the same flow finds it terminal and changes nothing.
	fixture.broker.expire(record)
	require.Equal(t, authStateExpired, record.state)
}

func TestCompleterExpiresAPendingFlow(t *testing.T) {
	fixture := newAuthFixture(t)

	original := authNow
	authNow = func() time.Time { return original().Add(-authSafetyDeadline).Add(10 * time.Millisecond) }

	t.Cleanup(func() { authNow = original })

	// The completer publishes the expired state before it destroys the flow's
	// broker home, so the test joins the destruction too. Returning on the state
	// alone leaves that goroutine reading the broker seams a later test restores.
	restoreBrokerSeams(t)

	removeAll := brokerRemoveAll
	destroyed := make(chan struct{})
	brokerRemoveAll = func(path string) error {
		defer close(destroyed)

		return removeAll(path)
	}

	flow := fixture.authorize(t, nil)

	require.Eventually(t, func() bool {
		return fixture.status(t, flow.FlowID).State == authStateExpired
	}, time.Second, 5*time.Millisecond)

	select {
	case <-destroyed:
	case <-time.After(time.Second):
		t.Fatal("the expired flow's broker home was never destroyed")
	}
}

func TestNewAuthTokenReportsEntropyFailure(t *testing.T) {
	original := authRandRead
	authRandRead = func([]byte) (int, error) { return 0, errors.New("entropy") }

	t.Cleanup(func() { authRandRead = original })

	_, err := newAuthToken()
	require.Error(t, err)
}

func TestAuthUserCodeFromURL(t *testing.T) {
	code, ok := authUserCodeFromURL("https://accounts.x.ai/oauth2/device?user_code=2XRG-QGNV")
	require.True(t, ok)
	require.Equal(t, "2XRG-QGNV", code)

	for _, raw := range []string{"://", "https://accounts.x.ai/oauth2/device", "https://accounts.x.ai/?user_code=bad code"} {
		_, ok := authUserCodeFromURL(raw)
		require.False(t, ok, raw)
	}
}

func TestAuthNativeCauseDropsNativeText(t *testing.T) {
	require.Equal(t, authCauseProviderRefused, authNativeCause(&opencode.HTTPError{StatusCode: http.StatusBadRequest, Body: "secret"}))
	require.Equal(t, authCauseTransport, authNativeCause(errors.New("dial tcp")))
}

func TestStopCompleterIsIdempotent(t *testing.T) {
	flow := &authFlow{disarm: make(chan struct{})}
	flow.stopCompleter()
	flow.stopCompleter()
}

func TestDestroyBrokerToleratesNoBroker(t *testing.T) {
	broker := newAuthAgent(t).broker
	broker.destroyBroker(context.Background(), &authFlow{})
}

func TestApplySecretFailsWhenTheInstallFails(t *testing.T) {
	fixture := newAuthFixture(t)

	flow := fixture.authorize(t, map[string]any{authFieldMethod: "1"})
	fixture.runtime.setAuthErr = errors.New("http")

	_, err := fixture.callback(t, flow.FlowID, "1", "sk-secret")
	requireAuthFailure(t, err, authCauseTransport)

	require.Equal(t, authStateFailed, fixture.status(t, flow.FlowID).State)
}

func TestDisconnectFailsWhenTheAbsenceProbeFails(t *testing.T) {
	fixture := newAuthFixture(t)

	require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "conn-1", BindingGeneration: 1, State: authLedgerConfirmed,
	}))

	fixture.runtime.storedAuthErr = errors.New("io")

	_, err := fixture.disconnect(t, "conn-1", 1)
	requireAuthFailure(t, err, authCauseHarvestFailed)
}

// adversarialConnectionIDs are the caller-minted values the bound refuses. Each
// is a shape the id would otherwise carry into a durable ledger entry and into
// the adapter's own logs, and the two replacement-rune spellings are one Go
// string reached from two different wire encodings, which aliases one
// connection onto another's entry.
func adversarialConnectionIDs() map[string]string {
	return map[string]string{
		"empty":              "",
		"path separators":    "../../../etc/passwd",
		"windows separators": `..\..\connection`,
		"newline":            "connection\n1",
		"nul":                "connection\x00 1",
		"bidi override":      "connection\u202e1",
		"space":              "connection 1",
		"colon":              "connection:1",
		"replacement rune":   "connection-�",
		"non ascii":          "connection-é",
		"unbounded":          strings.Repeat("c", authConnectionIDMaxBytes+1),
	}
}

func TestConnectionIDIsRefusedAtEverySurfaceEntry(t *testing.T) {
	fixture := newAuthFixture(t)

	require.NoError(t, fixture.broker.ledger.write(authLedgerRecord{
		ProviderID: "xai", ConnectionID: "conn-1", BindingGeneration: 1, State: authLedgerConfirmed,
	}))

	for name, connectionID := range adversarialConnectionIDs() {
		t.Run(name, func(t *testing.T) {
			_, err := fixture.broker.authorize(context.Background(),
				fixture.authorizeParams(t, map[string]any{authFieldConnectionID: connectionID}))
			requireInvalidParams(t, err, authFieldConnectionID)

			_, err = fixture.disconnect(t, connectionID, 1)
			requireInvalidParams(t, err, authFieldConnectionID)
		})
	}

	// Every refusal landed before the leg read the entry the live binding names,
	// so nothing recorded a value the bound rejects.
	live, ok, err := fixture.broker.ledger.read("xai")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "conn-1", live.ConnectionID)
	require.Equal(t, int64(1), live.BindingGeneration)
	require.Equal(t, authLedgerConfirmed, live.State)
}

func TestConnectionIDAcceptsTheOpaqueTokenAConsumerMints(t *testing.T) {
	for _, connectionID := range []string{
		"pac_2f1c9b4e-8d3a-4c17-9f21-0b6e5a7c8d90",
		"conn-1",
		"C0",
		strings.Repeat("c", authConnectionIDMaxBytes),
	} {
		require.True(t, authValidConnectionID(connectionID), connectionID)
	}
}
