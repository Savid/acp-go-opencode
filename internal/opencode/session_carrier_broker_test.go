package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func testSessionCarrierBroker() *sessionCarrierBroker {
	return &sessionCarrierBroker{carriers: map[string]sessionCarrierPayload{}}
}

type failingSessionCarrierResponseWriter struct {
	header http.Header
	err    error
	wrote  bool
}

func (w *failingSessionCarrierResponseWriter) Header() http.Header {
	return w.header
}

func (*failingSessionCarrierResponseWriter) WriteHeader(int) {}

func (w *failingSessionCarrierResponseWriter) Write([]byte) (int, error) {
	w.wrote = true

	return 0, w.err
}

func TestSessionCarrierBrokerKeepsValuesOutOfTheReference(t *testing.T) {
	broker, err := startSessionCarrierBroker()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, broker.Close()) })

	payload := sessionCarrierPayload{
		Env:           map[string]string{"WAGIE_API_TOKEN": "operation-capability", "EMPTY": ""},
		ExtraPathDirs: []string{"/operation/bin"},
	}
	reference, err := broker.put(payload)
	require.NoError(t, err)
	require.NotContains(t, reference, "operation-capability")
	require.NotContains(t, reference, "/operation/bin")

	metadata := (&openCodeServer{sessionCarrierReference: reference}).sessionCarrierMetadata(nil)
	encodedMetadata, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NotContains(t, string(encodedMetadata), "operation-capability")
	require.NotContains(t, string(encodedMetadata), "/operation/bin")
	require.JSONEq(t, `{"acp-go-opencode":{"ref":"`+reference+`"}}`, string(encodedMetadata))

	payload.Env["WAGIE_API_TOKEN"] = "mutated"
	payload.ExtraPathDirs[0] = "/mutated"

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		broker.endpoint+sessionCarrierRoute+reference, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+broker.token)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))

	var resolved sessionCarrierPayload
	require.NoError(t, json.NewDecoder(response.Body).Decode(&resolved))
	require.NoError(t, response.Body.Close())
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "operation-capability", "EMPTY": ""}, resolved.Env)
	require.Equal(t, []string{"/operation/bin"}, resolved.ExtraPathDirs)

	broker.remove(reference)
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet,
		broker.endpoint+sessionCarrierRoute+reference, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+broker.token)
	response, err = http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestSessionCarrierBrokerRejectsUnauthenticatedReads(t *testing.T) {
	broker, err := startSessionCarrierBroker()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, broker.Close()) })

	reference, err := broker.put(sessionCarrierPayload{Env: map[string]string{"TOKEN": "secret"}})
	require.NoError(t, err)

	for _, authorization := range []string{"", "Bearer wrong"} {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet,
			broker.endpoint+sessionCarrierRoute+reference, nil)
		require.NoError(t, requestErr)
		request.Header.Set("Authorization", authorization)

		response, requestErr := http.DefaultClient.Do(request)
		require.NoError(t, requestErr)
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
}

func TestStartSessionCarrierBrokerFailsWithoutAuthorizationEntropy(t *testing.T) {
	reader := openCodeRandReader
	want := errors.New("entropy failed")
	openCodeRandReader = iotest.ErrReader(want)
	t.Cleanup(func() { openCodeRandReader = reader })

	broker, err := startSessionCarrierBroker()
	require.Nil(t, broker)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "broker authorization")
}

func TestNilSessionCarrierBrokerFailsClosed(t *testing.T) {
	var broker *sessionCarrierBroker

	_, err := broker.put(sessionCarrierPayload{Env: map[string]string{"TOKEN": "secret"}})
	require.ErrorContains(t, err, "broker is unavailable")
	require.NoError(t, broker.Close())
}

func TestSessionCarrierBrokerRejectsUnsupportedRequests(t *testing.T) {
	broker := testSessionCarrierBroker()
	broker.token = "broker-authorization"

	request := httptest.NewRequest(http.MethodPost, sessionCarrierRoute+"reference", http.NoBody)
	response := httptest.NewRecorder()
	broker.serveHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)

	request = httptest.NewRequest(http.MethodPost, sessionCarrierRoute+"reference", http.NoBody)
	request.Header.Set("Authorization", "Bearer "+broker.token)
	response = httptest.NewRecorder()
	broker.serveHTTP(response, request)
	require.Equal(t, http.StatusMethodNotAllowed, response.Code)

	request = httptest.NewRequest(http.MethodGet, "/not-a-carrier/reference", http.NoBody)
	request.Header.Set("Authorization", "Bearer "+broker.token)
	response = httptest.NewRecorder()
	broker.serveHTTP(response, request)
	require.Equal(t, http.StatusNotFound, response.Code)

	request = httptest.NewRequest(http.MethodGet, "/ready", http.NoBody)
	request.Header.Set("Authorization", "Bearer "+broker.token)
	response = httptest.NewRecorder()
	broker.serveHTTP(response, request)
	require.Equal(t, http.StatusMethodNotAllowed, response.Code)
}

func TestSessionCarrierBrokerStopsOnResponseWriteFailure(t *testing.T) {
	want := errors.New("write failed")
	broker := testSessionCarrierBroker()
	broker.token = "broker-authorization"
	broker.carriers["reference"] = sessionCarrierPayload{Env: map[string]string{"TOKEN": "secret"}}

	request := httptest.NewRequest(http.MethodGet, sessionCarrierRoute+"reference", http.NoBody)
	request.Header.Set("Authorization", "Bearer "+broker.token)
	response := &failingSessionCarrierResponseWriter{header: http.Header{}, err: want}
	broker.serveHTTP(response, request)
	require.True(t, response.wrote)
	require.Equal(t, "no-store", response.header.Get("Cache-Control"))
}

func TestSessionCarrierBrokerFailsClosedWithoutReferenceEntropy(t *testing.T) {
	reader := openCodeRandReader
	want := errors.New("entropy failed")
	openCodeRandReader = iotest.ErrReader(want)
	t.Cleanup(func() { openCodeRandReader = reader })

	broker := testSessionCarrierBroker()
	_, err := broker.put(sessionCarrierPayload{Env: map[string]string{"TOKEN": "secret"}})
	require.ErrorIs(t, err, want)
	require.Empty(t, broker.carriers)
}

func TestScopeRequiresAnAvailableSessionCarrierBroker(t *testing.T) {
	_, err := (&openCodeServer{}).Scope(t.Context(), ScopeOptions{Directory: "/repo"})
	require.ErrorContains(t, err, "broker is unavailable")

	broker := testSessionCarrierBroker()
	broker.closed = true
	_, err = (&openCodeServer{sessionCarrierBroker: broker}).Scope(t.Context(), ScopeOptions{Directory: "/repo"})
	require.ErrorContains(t, err, "register OpenCode session carrier")
	require.ErrorContains(t, err, "broker is closed")
}

func TestSessionCarrierBrokerRejectsCarriersAfterClose(t *testing.T) {
	broker, err := startSessionCarrierBroker()
	require.NoError(t, err)
	require.NoError(t, broker.Close())

	_, err = broker.put(sessionCarrierPayload{Env: map[string]string{"TOKEN": "secret"}})
	require.ErrorContains(t, err, "broker is closed")
	require.Empty(t, broker.carriers)
}

func TestScopeCloseRevokesCarrierWhenMCPDisconnectFails(t *testing.T) {
	broker, err := startSessionCarrierBroker()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, broker.Close()) })
	reference, err := broker.put(sessionCarrierPayload{Env: map[string]string{"TOKEN": "secret"}})
	require.NoError(t, err)

	native := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(native.Close)
	scopeContext, cancel := context.WithCancel(context.Background())
	scope := &openCodeServer{
		httpClient: native.Client(), baseURL: native.URL, mcpNames: []string{"failing"},
		closed: make(chan struct{}), scopeCancel: cancel, sessionCarrierBroker: broker,
		sessionCarrierReference: reference,
	}
	t.Cleanup(cancel)
	require.ErrorIs(t, scope.Close(scopeContext), ErrMCPDisconnectUnproven)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		broker.endpoint+sessionCarrierRoute+reference, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+broker.token)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	require.NoError(t, response.Body.Close())
}
