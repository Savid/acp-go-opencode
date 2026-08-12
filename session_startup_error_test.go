package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestSessionStartupFailureBoundaries(t *testing.T) {
	secret := "startup-secret-must-not-cross-acp"
	cases := []struct {
		name  string
		stage SessionStartupStage
		run   func(*testing.T, error) error
	}{
		{
			name:  "runtime start",
			stage: SessionStartupRuntimeStart,
			run: func(t *testing.T, failure error) error {
				t.Helper()

				agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
					options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
						return nil, failure
					}
				})
				_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))

				return err
			},
		},
		{
			name:  "carrier proof",
			stage: SessionStartupCarrierProof,
			run: func(t *testing.T, failure error) error {
				t.Helper()

				agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
					options.clientFactory = func(ctx context.Context, start opencode.StartOptions) (opencode.Client, error) {
						start.ObserveStartupStage(ctx, string(RuntimeResourceRuntime), string(RuntimeStartupCarrier), 0, failure)

						return nil, failure
					}
				})
				_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))

				return err
			},
		},
		{
			name:  "scope",
			stage: SessionStartupScope,
			run: func(t *testing.T, failure error) error {
				t.Helper()

				client := newFakeOpenCodeClient()
				client.scopeErr = failure
				agent := NewAgent()
				agent.runtime = client
				_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))

				return err
			},
		},
		{
			name:  "catalog",
			stage: SessionStartupCatalog,
			run: func(t *testing.T, failure error) error {
				t.Helper()

				client := newFakeOpenCodeClient()
				client.providersErr = failure
				agent := NewAgent()
				agent.runtime = client
				_, err := agent.NewSession(t.Context(), NewSessionRequest(
					t.TempDir(),
					WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test"))),
				))

				return err
			},
		},
		{
			name:  "native session create",
			stage: SessionStartupNativeSessionCreate,
			run: func(t *testing.T, failure error) error {
				t.Helper()

				client := newFakeOpenCodeClient()
				client.createErr = failure
				agent := NewAgent()
				agent.runtime = client
				_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))

				return err
			},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directErr := test.run(t, errors.New(secret))
			require.Error(t, directErr)
			require.ErrorContains(t, directErr, secret)

			failure, ok := SessionStartupFailureFromError(directErr)
			require.True(t, ok)
			require.Equal(t, SessionStartupFailure{Stage: test.stage}, failure)
			directDiagnostic, err := json.Marshal(failure)
			require.NoError(t, err)
			require.NotContains(t, string(directDiagnostic), secret)

			wireErr := requestError(directErr)
			require.Equal(t, -32603, wireErr.Code)
			failure, ok = SessionStartupFailureFromError(wireErr)
			require.True(t, ok)
			require.Equal(t, SessionStartupFailure{Stage: test.stage}, failure)

			encoded, err := json.Marshal(wireErr)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), secret)
		})
	}
}

func TestSessionStartupFailureProjectsOnlyAllowlistedHTTPMetadata(t *testing.T) {
	const secret = "body-url-query-model-config-credential-secret"

	tests := []struct {
		name      string
		stage     SessionStartupStage
		method    string
		path      string
		pathClass string
	}{
		{name: "runtime health", stage: SessionStartupRuntimeStart, method: http.MethodGet, path: sessionStartupRouteRuntimeHealth, pathClass: sessionStartupPathRuntimeHealth},
		{name: "runtime contract", stage: SessionStartupRuntimeStart, method: http.MethodGet, path: sessionStartupRouteRuntimeContract, pathClass: sessionStartupPathRuntimeContract},
		{name: "carrier configuration", stage: SessionStartupCarrierProof, method: http.MethodGet, path: sessionStartupRouteRuntimeConfig, pathClass: sessionStartupPathRuntimeConfig},
		{name: "scope MCP", stage: SessionStartupScope, method: http.MethodPost, path: sessionStartupRouteMCP, pathClass: sessionStartupPathMCPRegistration},
		{name: "scope MCP member", stage: SessionStartupScope, method: http.MethodDelete, path: sessionStartupRouteMCP + "/" + secret, pathClass: sessionStartupPathMCPRegistration},
		{name: "catalog", stage: SessionStartupCatalog, method: http.MethodGet, path: sessionStartupRouteProviderCatalog, pathClass: sessionStartupPathProviderCatalog},
		{name: "native session", stage: SessionStartupNativeSessionCreate, method: http.MethodPost, path: sessionStartupRouteSession, pathClass: sessionStartupPathSessionCollection},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nativeErr := &opencode.HTTPError{
				Method: test.method, Path: test.path, Status: "401 " + secret,
				StatusCode: http.StatusUnauthorized, Body: secret,
			}
			directErr := errors.Join(wrapSessionStartupError(test.stage, nativeErr), errors.New(secret))

			failure, ok := SessionStartupFailureFromError(directErr)
			require.True(t, ok)
			require.Equal(t, test.stage, failure.Stage)
			require.Equal(t, &SessionStartupHTTPFailure{
				Method: test.method, PathClass: test.pathClass, StatusCode: http.StatusUnauthorized,
			}, failure.HTTP)
			directDiagnostic, err := json.Marshal(failure)
			require.NoError(t, err)
			require.NotContains(t, string(directDiagnostic), secret)
			require.NotContains(t, string(directDiagnostic), test.path)

			wireErr := requestError(directErr)
			failure, ok = SessionStartupFailureFromError(wireErr)
			require.True(t, ok)
			encoded, err := json.Marshal(wireErr)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), secret)
			require.NotContains(t, string(encoded), test.path)
		})
	}
}

func TestSessionStartupFailureRejectsUntrustedDiagnosticFields(t *testing.T) {
	const secret = "arbitrary-secret-value"

	for name, nativeErr := range map[string]*opencode.HTTPError{
		"method": {Method: secret, Path: sessionStartupRouteSession, StatusCode: http.StatusUnauthorized, Body: secret},
		"path":   {Method: http.MethodPost, Path: sessionStartupRouteSession + "?token=" + secret, StatusCode: http.StatusUnauthorized, Body: secret},
		"status": {Method: http.MethodPost, Path: sessionStartupRouteSession, StatusCode: 999, Body: secret},
	} {
		t.Run(name, func(t *testing.T) {
			wireErr := requestError(wrapSessionStartupError(SessionStartupNativeSessionCreate, nativeErr))
			failure, ok := SessionStartupFailureFromError(wireErr)
			require.True(t, ok)
			require.Nil(t, failure.HTTP)

			encoded, err := json.Marshal(wireErr)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), secret)
		})
	}

	for name, data := range map[string]map[string]any{
		"stage": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: secret,
		},
		"method": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP:  map[string]any{jsonFieldMethod: secret, jsonFieldPathClass: sessionStartupPathMCPRegistration, jsonFieldStatusCode: 401},
		},
		"path class": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP:  map[string]any{jsonFieldMethod: http.MethodPost, jsonFieldPathClass: secret, jsonFieldStatusCode: 401},
		},
		"path class for another stage": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP:  map[string]any{jsonFieldMethod: http.MethodPost, jsonFieldPathClass: sessionStartupPathSessionCollection, jsonFieldStatusCode: 401},
		},
		"fractional status": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP:  map[string]any{jsonFieldMethod: http.MethodPost, jsonFieldPathClass: sessionStartupPathMCPRegistration, jsonFieldStatusCode: 401.5},
		},
		"successful status": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP:  map[string]any{jsonFieldMethod: http.MethodPost, jsonFieldPathClass: sessionStartupPathMCPRegistration, jsonFieldStatusCode: 204},
		},
		"wrong method for path class": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupCatalog),
			jsonFieldHTTP:  map[string]any{jsonFieldMethod: http.MethodDelete, jsonFieldPathClass: sessionStartupPathProviderCatalog, jsonFieldStatusCode: 401},
		},
	} {
		t.Run("decode "+name, func(t *testing.T) {
			_, ok := SessionStartupFailureFromError(acp.NewInternalError(data))
			require.False(t, ok)
		})
	}
}

func TestSessionStartupFailureRequiresExactHTTPRouteTuples(t *testing.T) {
	tests := []struct {
		name       string
		stage      SessionStartupStage
		method     string
		path       string
		statusCode int
	}{
		{name: "runtime health method", stage: SessionStartupRuntimeStart, method: http.MethodPost, path: sessionStartupRouteRuntimeHealth, statusCode: 500},
		{name: "runtime contract method", stage: SessionStartupRuntimeStart, method: http.MethodDelete, path: sessionStartupRouteRuntimeContract, statusCode: 500},
		{name: "carrier method", stage: SessionStartupCarrierProof, method: http.MethodPost, path: sessionStartupRouteRuntimeConfig, statusCode: 500},
		{name: "MCP collection method", stage: SessionStartupScope, method: http.MethodDelete, path: sessionStartupRouteMCP, statusCode: 500},
		{name: "MCP member method", stage: SessionStartupScope, method: http.MethodPost, path: sessionStartupRouteMCP + "/name", statusCode: 500},
		{name: "MCP member query", stage: SessionStartupScope, method: http.MethodDelete, path: sessionStartupRouteMCP + "/name?token=secret", statusCode: 500},
		{name: "catalog method", stage: SessionStartupCatalog, method: http.MethodPost, path: sessionStartupRouteProviderCatalog, statusCode: 500},
		{name: "native session method", stage: SessionStartupNativeSessionCreate, method: http.MethodGet, path: sessionStartupRouteSession, statusCode: 500},
		{name: "successful status", stage: SessionStartupNativeSessionCreate, method: http.MethodPost, path: sessionStartupRouteSession, statusCode: 204},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpFailure := safeSessionStartupHTTP(test.stage, &opencode.HTTPError{
				Method: test.method, Path: test.path, StatusCode: test.statusCode, Body: "secret",
			})
			require.Nil(t, httpFailure)
		})
	}
}

func TestSessionStartupFailureWireRoundTrip(t *testing.T) {
	direct := wrapSessionStartupError(SessionStartupNativeSessionCreate, &opencode.HTTPError{
		Method: http.MethodPost, Path: sessionStartupRouteSession, StatusCode: http.StatusTooManyRequests,
	})
	encoded, err := json.Marshal(requestError(direct))
	require.NoError(t, err)

	var decoded acp.RequestError
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	failure, ok := SessionStartupFailureFromError(&decoded)
	require.True(t, ok)
	require.Equal(t, SessionStartupNativeSessionCreate, failure.Stage)
	require.Equal(t, http.StatusTooManyRequests, failure.HTTP.StatusCode)

	encodedFailure, err := json.Marshal(failure)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(encodedFailure), "error"))
}

func TestSessionStartupFailurePreservesProtocolAndCancellationErrors(t *testing.T) {
	require.NoError(t, wrapSessionStartupError(SessionStartupRuntimeStart, nil))

	requestErr := acp.NewInvalidRequest(map[string]any{jsonFieldError: "closed"})
	require.Same(t, requestErr, wrapSessionStartupError(SessionStartupRuntimeStart, requestErr))

	cancelled := errors.Join(context.Canceled, errors.New("cleanup context"))
	require.Same(t, cancelled, wrapSessionStartupError(SessionStartupScope, cancelled))
	require.Equal(t, -32800, requestError(cancelled).Code)

	_, ok := SessionStartupFailureFromError(cancelled)
	require.False(t, ok)
}

func TestSessionStartupFailurePreservesJoinedProtocolPrecedence(t *testing.T) {
	startupErr := wrapSessionStartupError(SessionStartupScope, errors.New("native startup failure"))
	protocolErrors := []*acp.RequestError{
		acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid"}),
		acp.NewAuthRequired(map[string]any{jsonFieldError: "auth"}),
	}

	for _, protocolErr := range protocolErrors {
		for name, joined := range map[string]error{
			"protocol first": errors.Join(protocolErr, startupErr),
			"startup first":  errors.Join(startupErr, protocolErr),
		} {
			t.Run(protocolErr.Message+"/"+name, func(t *testing.T) {
				require.Same(t, protocolErr, requestError(joined))
				_, ok := SessionStartupFailureFromError(joined)
				require.False(t, ok)
			})
		}
	}

	for name, joined := range map[string]error{
		"cancellation first": errors.Join(context.Canceled, startupErr),
		"startup first":      errors.Join(startupErr, context.Canceled),
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, -32800, requestError(joined).Code)
			_, ok := SessionStartupFailureFromError(joined)
			require.False(t, ok)
		})
	}
}

func TestSessionStartupFailureDecoderRequiresInternalErrorCode(t *testing.T) {
	data := map[string]any{
		jsonFieldError: sessionStartupErrorTag,
		jsonFieldStage: string(SessionStartupScope),
	}

	for _, requestErr := range []*acp.RequestError{
		acp.NewInvalidParams(data),
		acp.NewAuthRequired(data),
		acp.NewRequestCancelled(data),
	} {
		_, ok := SessionStartupFailureFromError(requestErr)
		require.False(t, ok)
	}

	for _, data := range []any{
		"not-an-object",
		map[string]any{jsonFieldError: "wrong-tag", jsonFieldStage: string(SessionStartupScope)},
	} {
		_, ok := SessionStartupFailureFromError(acp.NewInternalError(data))
		require.False(t, ok)
	}
}

func TestSessionStartupFailureDecoderRejectsWrongFieldTypes(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"stage": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: 1,
		},
		"http": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP:  "not-an-object",
		},
		"status": {
			jsonFieldError: sessionStartupErrorTag,
			jsonFieldStage: string(SessionStartupScope),
			jsonFieldHTTP: map[string]any{
				jsonFieldMethod:     http.MethodPost,
				jsonFieldPathClass:  sessionStartupPathMCPRegistration,
				jsonFieldStatusCode: "401",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := SessionStartupFailureFromError(acp.NewInternalError(data))
			require.False(t, ok)
		})
	}

	require.Empty(t, safeSessionStartupHTTPPathClass(SessionStartupStage("unknown"), http.MethodPost, sessionStartupRouteMCP))
	require.False(t, safeSessionStartupHTTPTuple(SessionStartupStage("unknown"), http.MethodPost, sessionStartupPathMCPRegistration))
}
