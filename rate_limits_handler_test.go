package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type rateLimitsTransport func(*http.Request) (*http.Response, error)

func (f rateLimitsTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newRateLimitsFixtureAgent(t *testing.T) *Agent {
	t.Helper()
	agent := NewAgent(WithDefaultModel("opencode-go/model"), WithOpenCodeDirectAPI(false))
	for _, id := range []acp.SessionId{"session-1", "😀: session "} {
		agent.sessions[id] = &session{id: id, providerID: "opencode-go", modelID: "model"}
	}

	return agent
}

func quotaAgentWithSource(t *testing.T, providerID string, body string, onRead func(*http.Request)) *Agent {
	t.Helper()
	agent := NewAgent(WithDefaultModel(providerID + "/model"))
	base, npm := "https://opencode.ai/zen/go/v1", "@ai-sdk/openai-compatible"
	if providerID == "openrouter" {
		base, npm = "https://openrouter.ai/api/v1", "@openrouter/ai-sdk-provider"
	}
	catalog, err := json.Marshal(map[string]any{"providers": []any{map[string]any{"id": providerID, "key": "private-key", "models": map[string]any{"model": map[string]any{"api": map[string]string{"url": base, "npm": npm}}}}}})
	require.NoError(t, err)
	client := &fakeOpenCodeClient{providers: opencode.ProvidersResponse{Raw: catalog}}
	agent.runtime = client
	agent.sessions["session-1"] = &session{id: "session-1", providerID: providerID, modelID: "model", client: client}
	agent.quotaHTTPClient = &http.Client{Transport: rateLimitsTransport(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, "Bearer private-key", request.Header.Get("Authorization"))
		if request.URL.Path == "/api/v1/credits" {
			return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody, Header: make(http.Header), Request: request}, nil
		}
		if onRead != nil {
			onRead(request)
		}

		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}

	return agent
}

func TestRateLimitsMapsPopulatedProviderResponses(t *testing.T) {
	for _, test := range []struct{ name, provider, body string }{
		{"go", "opencode-go", fmt.Sprintf(`{"usage":{"rolling":{"percent":0,"status":"ok","resetsAt":%q},"monthly":{"percent":100,"status":"rate-limited","resetsAt":%q}}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339))},
		{"router capped", "openrouter", `{"data":{"limit":20,"limit_remaining":-1,"limit_reset":"monthly","usage":500}}`},
		{"router uncapped", "openrouter", `{"data":{"limit":null,"limit_remaining":null,"limit_reset":null,"usage":0}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := quotaAgentWithSource(t, test.provider, test.body, nil)
			result, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, json.RawMessage(`{"sessionId":"session-1"}`))
			require.NoError(t, err)
			response, ok := result.(RateLimitsResponse)
			require.True(t, ok)
			require.Equal(t, "available", response.Availability)
			require.Len(t, response.Pools, 1)
			pool := response.Pools[0]
			if test.provider == "opencode-go" {
				require.Len(t, pool.Windows, 2)
				require.Equal(t, "rolling", pool.Windows[0].ID)
				require.Equal(t, 0.0, *pool.Windows[0].UsedPercent)
				require.Equal(t, "exhausted", pool.Windows[1].Status)
				require.Nil(t, pool.Windows[1].DurationSeconds)
			} else {
				require.NotNil(t, pool.Windows)
				require.Empty(t, pool.Windows)
				require.Len(t, pool.Balances, 1)
				balance := pool.Balances[0]
				if test.name == "router capped" {
					require.Equal(t, &RateLimitMoney{Amount: -1, Currency: "USD"}, balance.Remaining)
					require.Nil(t, balance.Used)
					require.Equal(t, "monthly", balance.ResetInterval)
				} else {
					require.True(t, balance.Uncapped)
					require.Equal(t, &RateLimitMoney{Amount: 0, Currency: "USD"}, balance.Used)
					require.Nil(t, balance.Limit)
				}
			}
			encoded, err := json.Marshal(response)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private-key")
		})
	}
}

func TestRateLimitsMapsAccountCreditsSeparatelyFromKeyAllowance(t *testing.T) {
	for _, keyBody := range []string{
		`{"data":{"limit":null,"usage":0}}`,
		`{"data":{"limit":20,"limit_remaining":7,"usage":13}}`,
	} {
		t.Run(keyBody, func(t *testing.T) {
			agent := quotaAgentWithSource(t, "openrouter", keyBody, nil)
			agent.quotaHTTPClient = &http.Client{Transport: rateLimitsTransport(func(request *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer private-key", request.Header.Get("Authorization"))
				body := keyBody
				if request.URL.Path == "/api/v1/credits" {
					body = `{"data":{"total_credits":50,"total_usage":40.812047315}}`
				}

				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
			})}
			response, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{"providerId":"openrouter","sessionId":"session-1"}`))
			require.NoError(t, err)
			require.Equal(t, "available", response.Availability)
			require.Len(t, response.Pools, 2)
			require.Equal(t, "openrouter", response.Pools[0].ID)
			require.Len(t, response.Pools[0].Balances, 1)
			require.Equal(t, "key", response.Pools[0].Balances[0].ID)
			account := response.Pools[1]
			require.Equal(t, "openrouter-account", account.ID)
			require.NotNil(t, account.Windows)
			require.Empty(t, account.Windows)
			require.Len(t, account.Balances, 1)
			credits := account.Balances[0]
			require.Equal(t, "credits", credits.ID)
			require.Equal(t, &RateLimitMoney{Amount: 40.812047315, Currency: "USD"}, credits.Used)
			require.NotNil(t, credits.Remaining)
			require.Equal(t, "USD", credits.Remaining.Currency)
			require.InDelta(t, 9.187952685, credits.Remaining.Amount, 0.000000001)
			require.Nil(t, credits.Limit, "purchased credits are not a spending cap")
			require.False(t, credits.Uncapped)
			require.NotEmpty(t, credits.ObservedAt)
			require.Empty(t, credits.ResetsAt)
			require.Empty(t, credits.ResetInterval)
		})
	}
}

func TestRateLimitsFencesSessionChangesWithoutTakingTurnLock(t *testing.T) {
	for _, mutation := range []string{"model", "model round trip", "close", "delete", "poison", "delete cancelled", "runtime"} {
		t.Run(mutation, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			agent := quotaAgentWithSource(t, "openrouter", `{"data":{"limit":null,"usage":0}}`, func(*http.Request) { close(started); <-release })
			selected := agent.sessions["session-1"]
			selected.turn = make(chan struct{}, 1)
			selected.turn <- struct{}{}
			type callResult struct {
				response RateLimitsResponse
				err      error
			}
			done := make(chan callResult, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				response, err := agent.handleRateLimits(ctx, json.RawMessage(`{"sessionId":"session-1"}`))
				done <- callResult{response: response, err: err}
			}()
			<-started
			switch mutation {
			case "model":
				selected.setModel("openrouter/different")
			case "model round trip":
				selected.setModel("openrouter/different")
				selected.setModel("openrouter/model")
			case "close":
				selected.mu.Lock()
				selected.closed = true
				selected.mu.Unlock()
			case "delete", "delete cancelled":
				agent.mu.Lock()
				agent.deleted[selected.id] = struct{}{}
				agent.mu.Unlock()
				if mutation == "delete cancelled" {
					cancel()
				}
			case "poison":
				selected.mu.Lock()
				selected.poisonCause = poisonCauseNativeSessionDrift
				selected.mu.Unlock()
			case "runtime":
				agent.mu.Lock()
				agent.runtimeGeneration++
				agent.mu.Unlock()
			}
			close(release)
			result := <-done
			switch mutation {
			case "close", "delete":
				requireInvalidParamsData(t, result.err, map[string]any{"error": "unknown session", "field": "sessionId"})
			case "poison":
				var protocolErr *acp.RequestError
				require.ErrorAs(t, result.err, &protocolErr)
				require.Equal(t, -32603, protocolErr.Code)
				require.Equal(t, map[string]any{jsonFieldError: valSessionPoisoned, jsonFieldCause: poisonCauseNativeSessionDrift}, protocolErr.Data)
			case "delete cancelled":
				require.ErrorIs(t, result.err, context.Canceled)
			default:
				require.NoError(t, result.err)
				require.Equal(t, "read_failed", result.response.Reason)
			}
			require.Empty(t, result.response.Pools)
		})
	}
}

func TestRateLimitsFencesProviderAuthMutations(t *testing.T) {
	for _, mutation := range []string{"install", "disconnect"} {
		t.Run(mutation, func(t *testing.T) {
			fixture := newAuthFixture(t)
			fixture.runtime.providerCatalog = []opencode.ProviderCatalogEntry{{ID: "openrouter", Name: "OpenRouter"}}
			fixture.runtime.providerAuthMethods = map[string][]opencode.ProviderAuthMethod{"openrouter": {{Type: authMethodTypeAPI, Label: "Manually enter API Key"}}}
			fixture.refreshCatalog(t)
			flow := fixture.authorize(t, map[string]any{authFieldProviderID: "openrouter"})
			callback := mustJSON(t, map[string]any{
				authFieldSessionID: fixture.session.id, authFieldProviderID: "openrouter",
				authFieldMethod: "0", authFieldFlowID: flow.FlowID, authFieldInput: "replacement-key",
			})
			if mutation == "disconnect" {
				_, err := fixture.agent.HandleExtensionMethod(t.Context(), AuthCallbackMethod, callback)
				require.NoError(t, err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			fixture.runtime.providers.Raw = json.RawMessage(`{"providers":[{"id":"openrouter","key":"private-key","models":{"model":{"api":{"url":"https://openrouter.ai/api/v1","npm":"@openrouter/ai-sdk-provider"}}}}]}`)
			fixture.session.setModel("openrouter/model")
			fixture.agent.quotaHTTPClient = &http.Client{Transport: rateLimitsTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/api/v1/credits" {
					return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody, Header: make(http.Header), Request: request}, nil
				}
				close(started)
				<-release

				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"limit":null,"usage":0}}`)), Header: make(http.Header), Request: request}, nil
			})}
			params := mustJSON(t, RateLimitsRequest{SessionID: fixture.session.id})
			type callResult struct {
				value any
				err   error
			}
			done := make(chan callResult, 1)
			go func() {
				value, err := fixture.agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, params)
				done <- callResult{value: value, err: err}
			}()
			<-started
			if mutation == "install" {
				_, err := fixture.agent.HandleExtensionMethod(t.Context(), AuthCallbackMethod, callback)
				require.NoError(t, err)
			} else {
				_, err := fixture.agent.HandleExtensionMethod(t.Context(), AuthDisconnectMethod, mustJSON(t, map[string]any{
					authFieldSessionID: fixture.session.id, authFieldProviderID: "openrouter",
					authFieldConnectionID: "conn-1", authFieldBindingGeneration: 1,
				}))
				require.NoError(t, err)
			}
			close(release)
			result := <-done
			require.NoError(t, result.err)
			require.Equal(t, rateLimitsUnavailable("openrouter", "read_failed"), result.value)
		})
	}
}

func TestRateLimitsSelectorErrorsAndOnlyRequestedProviders(t *testing.T) {
	agent := newRateLimitsFixtureAgent(t)
	for _, provider := range []string{"xai", "openai", "anthropic", "opencode", "unknown"} {
		raw, err := json.Marshal(RateLimitsRequest{ProviderID: provider})
		require.NoError(t, err)
		value, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, raw)
		require.NoError(t, err)
		require.Equal(t, rateLimitsUnsupported(provider), value)
	}
	_, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, json.RawMessage(`{"providerId":"unknown","sessionId":"absent"}`))
	require.Error(t, err)
	var protocolErr *acp.RequestError
	require.ErrorAs(t, err, &protocolErr)
	require.Equal(t, map[string]any{"error": "unknown session", "field": "sessionId"}, protocolErr.Data)
	agent.sessions["other"] = &session{id: "other", providerID: "openrouter"}
	_, err = agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, nil)
	require.ErrorAs(t, err, &protocolErr)
	require.Equal(t, map[string]any{"error": "missing", "field": "providerId"}, protocolErr.Data)
}

func TestRateLimitsCallerCancellationAndDeadline(t *testing.T) {
	started := make(chan struct{})
	agent := quotaAgentWithSource(t, "openrouter", `{"data":{"limit":null,"usage":0}}`, func(request *http.Request) {
		deadline, ok := request.Context().Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
		close(started)
		<-request.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := agent.HandleExtensionMethod(ctx, RateLimitsMethod, json.RawMessage(`{"sessionId":"session-1"}`))
		done <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRateLimitsRetainsLifecycleFailures(t *testing.T) {
	agent := newRateLimitsFixtureAgent(t)
	agent.runtimeFatalErr = ErrContainmentIncomplete
	_, err := agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, json.RawMessage(`{"providerId":"opencode-go"}`))
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	agent.runtimeFatalErr = nil
	agent.sessions["session-1"].poisonCause = poisonCauseNativeSessionDrift
	_, err = agent.HandleExtensionMethod(t.Context(), RateLimitsMethod, json.RawMessage(`{"providerId":"unknown","sessionId":"session-1"}`))
	var protocolErr *acp.RequestError
	require.True(t, errors.As(err, &protocolErr))
	require.Equal(t, -32603, protocolErr.Code)
}
