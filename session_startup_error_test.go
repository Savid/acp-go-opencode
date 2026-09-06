package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// startupBodySecret stands in for what a native error body echoes back at
// session start: the MCP headers and session environment the failing request
// carried.
const startupBodySecret = "bearer-carrier-token"

func startupHTTPError(method string, path string) *opencode.HTTPError {
	return &opencode.HTTPError{
		Method:     method,
		Path:       path,
		Status:     "401 Unauthorized",
		StatusCode: http.StatusUnauthorized,
		Body:       `{"error":"` + startupBodySecret + `"}`,
	}
}

// TestSessionStartHTTPFailuresNameTheRouteAndNeverTheBody drives every session
// start boundary that talks to the native server and inspects exactly what the
// connection would send: the failing route and status, with nothing of the
// native response body anywhere in the encoded error.
func TestSessionStartHTTPFailuresNameTheRouteAndNeverTheBody(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		arrange func(*fakeOpenCodeClient, *Options, *opencode.HTTPError)
	}{
		{
			name:   "runtime start",
			method: http.MethodGet,
			path:   "/global/health",
			arrange: func(_ *fakeOpenCodeClient, options *Options, httpErr *opencode.HTTPError) {
				options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
					return nil, httpErr
				}
			},
		},
		{
			name:   "directory scope",
			method: http.MethodPost,
			path:   "/mcp",
			arrange: func(client *fakeOpenCodeClient, _ *Options, httpErr *opencode.HTTPError) {
				client.scopeErr = httpErr
			},
		},
		{
			name:   "native session create",
			method: http.MethodPost,
			path:   "/session",
			arrange: func(client *fakeOpenCodeClient, _ *Options, httpErr *opencode.HTTPError) {
				client.createErr = httpErr
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpErr := startupHTTPError(test.method, test.path)
			client := newFakeOpenCodeClient(t)
			client.createSession = testNativeSession("native-1")
			client.providers = testProviders()
			agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
				options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
					return client, nil
				}
				test.arrange(client, options, httpErr)
			})
			agent.setAgentClient(newRecordingAgentClient())

			ctx := t.Context()

			_, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(),
				WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test")))))
			require.Error(t, err)

			reqErr := requestError(ctx, err)
			require.Equal(t, -32603, reqErr.Code)

			data, ok := reqErr.Data.(map[string]any)
			require.True(t, ok, "error data = %#v", reqErr.Data)
			// A native loopback failure at startup reduces to one closed
			// vendor-scoped token plus the one documented class, never to prose
			// a host would have to parse.
			require.Equal(t, valInternalFailure, data[jsonFieldError])
			require.Equal(t, classNativeStartup, data[jsonFieldClass])
			require.Len(t, data, 2)

			encoded, marshalErr := json.Marshal(reqErr)
			require.NoError(t, marshalErr)
			require.NotContains(t, string(encoded), startupBodySecret)
		})
	}
}

// TestSessionStartRejectsAnUnsupportedNativeFieldAsInvalidParams proves the one
// startup failure that is the caller's to fix reaches them as such: a field the
// native runtime refuses is the uniform unsupported-field rejection, not an
// internal error.
func TestSessionStartRejectsAnUnsupportedNativeFieldAsInvalidParams(t *testing.T) {
	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return nil, fmt.Errorf("author seed files: %w",
				&opencode.UnsupportedFieldError{Field: "seedFiles[../escape.json]"})
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	_, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: valUnsupported,
		jsonFieldField: "seedFiles[../escape.json]",
	})
}

// TestStartupFailurePreservesTheChainItRetells keeps the two properties the
// retelling must not cost: the adapter still matches its own containment and
// cancellation sentinels through the wrapper, and a failure with nothing native
// behind it is passed through untouched.
func TestStartupFailurePreservesTheChainItRetells(t *testing.T) {
	require.NoError(t, startupFailure(nil))

	plain := errors.New("no runtime factory")
	require.Same(t, plain, startupFailure(plain))

	wrapped := startupFailure(errors.Join(
		ErrContainmentIncomplete,
		startupHTTPError(http.MethodDelete, "/mcp/gateway"),
	))
	require.ErrorIs(t, wrapped, ErrContainmentIncomplete)
	require.Equal(t, "opencode DELETE /mcp/gateway returned 401 Unauthorized", wrapped.Error())
	require.NotContains(t, wrapped.Error(), startupBodySecret)

	var httpErr *opencode.HTTPError
	require.True(t, errors.As(wrapped, &httpErr))
	require.Equal(t, http.StatusUnauthorized, httpErr.StatusCode)
}
