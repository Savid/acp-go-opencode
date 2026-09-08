package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type quotaTransport func(*http.Request) (*http.Response, error)

func (f quotaTransport) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func quotaCatalog(t *testing.T, providerID, key, endpoint, npm string, options map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"providers": []any{map[string]any{
		"id": providerID, "key": key, "options": options,
		"models": map[string]any{"model": map[string]any{"api": map[string]any{"url": endpoint, "npm": npm}}},
	}}})
	require.NoError(t, err)

	return raw
}

func TestProviderQuotaReadsEffectiveNativeScope(t *testing.T) {
	for _, test := range []struct {
		name, providerID, base, npm, body string
	}{
		{"go", quotaGoProvider, quotaGoBase, "@ai-sdk/openai-compatible", fmt.Sprintf(`{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":%q},"weekly":{"status":"rate-limited","percent":103,"resetsAt":%q}}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339))},
		{"openrouter", quotaRouterProvider, quotaRouterBase, "@openrouter/ai-sdk-provider", `{"data":{"limit":20,"limit_remaining":-1,"limit_reset":"monthly","usage":99}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var catalogCalls atomic.Int64
			catalog := quotaCatalog(t, test.providerID, "stored-key", test.base, test.npm, map[string]any{"apiKey": "effective-key"})
			native := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/workspace", r.URL.Query().Get("directory"))
				if r.URL.Path == routeConfig {
					_, err := io.WriteString(w, `{"plugin":[]}`)
					require.NoError(t, err)

					return
				}
				require.Equal(t, routeConfigProviders, r.URL.Path)
				catalogCalls.Add(1)
				_, err := w.Write(catalog)
				require.NoError(t, err)
			})
			native.directory = "/workspace"
			httpClient := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer effective-key", request.Header.Get("Authorization"))
				require.True(t, strings.HasPrefix(request.URL.String(), test.base+"/"))
				require.Equal(t, http.MethodGet, request.Method)

				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header), Request: request}, nil
			})}
			before := time.Now()
			result := ReadProviderQuota(t.Context(), []Client{native}, test.providerID, "model", true, httpClient)
			require.Equal(t, "available", result.Availability)
			require.False(t, result.ObservedAt.Before(before))
			require.EqualValues(t, 2, catalogCalls.Load())
			if test.name == "go" {
				require.Equal(t, 0.0, *result.Go.Usage["rolling"].Percent)
				require.Equal(t, 103.0, *result.Go.Usage["weekly"].Percent)
			} else {
				require.Equal(t, -1.0, *result.OpenRouter.LimitRemaining)
			}
		})
	}
}

// OpenCode's Go catalog mixes Chat Completions, Messages, and Responses SDKs
// under one official endpoint and API key (opencode.ai/docs/go/#endpoints).
func TestProviderQuotaMixedGoSDKsShareTheSameAccount(t *testing.T) {
	for _, test := range []struct {
		name, modelID, responsesSDK, responsesBase string
		availability, reason                       string
		reads                                      int64
	}{
		{name: "whole provider", availability: quotaAvailable, reads: 1},
		{name: "selected responses model", modelID: "responses", availability: quotaAvailable, reads: 1},
		{name: "mixed custom SDK", responsesSDK: "custom-sdk", availability: quotaUnavailable, reason: quotaSessionRequired},
		{name: "selected custom SDK", modelID: "responses", responsesSDK: "custom-sdk", availability: quotaUnsupported},
		{name: "mixed custom endpoint", responsesBase: "https://gateway.example/v1", availability: quotaUnavailable, reason: quotaSessionRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			models := make(map[string]any)
			for id, npm := range map[string]string{
				"chat": quotaCompatibleSDK, "messages": "@ai-sdk/anthropic", "responses": "@ai-sdk/openai",
			} {
				base := quotaGoBase
				if id == "responses" {
					if test.responsesSDK != "" {
						npm = test.responsesSDK
					}
					if test.responsesBase != "" {
						base = test.responsesBase
					}
				}
				models[id] = map[string]any{"api": map[string]any{"url": base, "npm": npm}}
			}
			catalog, err := json.Marshal(map[string]any{"providers": []any{map[string]any{
				"id": quotaGoProvider, "key": "same-go-key", "models": models,
			}}})
			require.NoError(t, err)
			native := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == routeConfig {
					_, writeErr := io.WriteString(w, `{}`)
					require.NoError(t, writeErr)

					return
				}
				require.Equal(t, routeConfigProviders, r.URL.Path)
				_, writeErr := w.Write(catalog)
				require.NoError(t, writeErr)
			})
			var reads atomic.Int64
			client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
				reads.Add(1)
				require.Equal(t, quotaGoBase+"/usage", request.URL.String())
				require.Equal(t, "Bearer same-go-key", request.Header.Get("Authorization"))
				body := fmt.Sprintf(`{"usage":{"rolling":{"status":"ok","percent":12,"resetsAt":%q}}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))

				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
			})}
			result := ReadProviderQuota(t.Context(), []Client{native}, quotaGoProvider, test.modelID, true, client)
			require.Equal(t, test.availability, result.Availability)
			require.Equal(t, test.reason, result.Reason)
			require.Equal(t, test.reads, reads.Load())
			if test.reads != 0 {
				require.Equal(t, 12.0, *result.Go.Usage["rolling"].Percent)
			}
		})
	}
}

func TestOpenRouterCreditsPreserveIndependentKeyQuota(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		readError  error
		remaining  *float64
		used       float64
	}{
		{name: "account balance", status: 200, body: `{"data":{"total_credits":50,"total_usage":40.812047315}}`, remaining: new(9.187952685), used: 40.812047315},
		{name: "zero is observed", status: 200, body: `{"data":{"total_credits":0,"total_usage":0}}`, remaining: new(float64(0))},
		{name: "overdrawn account", status: 200, body: `{"data":{"total_credits":5,"total_usage":7.5}}`, remaining: new(-2.5), used: 7.5},
		{name: "denied", status: 403, body: `{"error":{"message":"Only management keys can perform this operation"}}`},
		{name: "account unauthorized", status: 401},
		{name: "server failure", status: 500},
		{name: "throttled", status: 429},
		{name: "redirect", status: 302},
		{name: "malformed JSON", status: 200, body: `{`},
		{name: "missing total", status: 200, body: `{"data":{"total_usage":0}}`},
		{name: "null total", status: 200, body: `{"data":{"total_credits":50,"total_usage":null}}`},
		{name: "negative usage", status: 200, body: `{"data":{"total_credits":50,"total_usage":-1}}`},
		{name: "nonfinite amount", status: 200, body: `{"data":{"total_credits":1e999,"total_usage":0}}`},
		{name: "oversize body", status: 200, body: strings.Repeat(" ", (1<<20)+1)},
		{name: "transport failure", readError: io.ErrUnexpectedEOF},
		{name: "source timeout", readError: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := quotaCatalog(t, quotaRouterProvider, "selected-key", quotaRouterBase, "@openrouter/ai-sdk-provider", nil)
			native := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
				body := catalog
				if r.URL.Path == routeConfig {
					body = json.RawMessage(`{}`)
				}
				_, err := w.Write(body)
				require.NoError(t, err)
			})
			var paths []string
			client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				require.Equal(t, "openrouter.ai", request.URL.Host)
				require.Equal(t, "Bearer selected-key", request.Header.Get("Authorization"))
				if request.URL.Path == "/api/v1/key" {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"limit":null,"usage":0}}`)), Header: make(http.Header), Request: request}, nil
				}
				require.Equal(t, "/api/v1/credits", request.URL.Path)
				deadline, ok := request.Context().Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
				if test.readError != nil {
					return nil, test.readError
				}

				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: http.Header{"Location": {"https://other.example/credits"}}, Request: request}, nil
			})}
			before := time.Now().UTC()
			result := ReadProviderQuota(t.Context(), []Client{native}, quotaRouterProvider, "", true, client)
			require.Equal(t, quotaAvailable, result.Availability)
			require.Empty(t, result.Reason)
			require.Equal(t, &OpenRouterQuota{Usage: new(float64(0))}, result.OpenRouter)
			require.Equal(t, []string{"/api/v1/key", "/api/v1/credits"}, paths)
			if test.remaining == nil {
				require.Nil(t, result.OpenRouterCredits)
			} else {
				require.NotNil(t, result.OpenRouterCredits)
				require.InDelta(t, *test.remaining, result.OpenRouterCredits.Remaining, 0.000000001)
				require.Equal(t, test.used, result.OpenRouterCredits.Used)
				require.False(t, result.ObservedAt.Before(before))
				require.False(t, result.OpenRouterCredits.ObservedAt.Before(result.ObservedAt))
			}
		})
	}
}

func TestOpenRouterCreditsRemainBehindCredentialFence(t *testing.T) {
	var changed atomic.Bool
	native := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == routeConfig {
			_, err := io.WriteString(w, `{}`)
			require.NoError(t, err)

			return
		}
		key := "selected-key"
		if changed.Load() {
			key = "replacement-key"
		}
		_, err := w.Write(quotaCatalog(t, quotaRouterProvider, key, quotaRouterBase, "@openrouter/ai-sdk-provider", nil))
		require.NoError(t, err)
	})
	var paths []string
	client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		require.Equal(t, "Bearer selected-key", request.Header.Get("Authorization"))
		body := `{"data":{"limit":null,"usage":0}}`
		if request.URL.Path == "/api/v1/credits" {
			changed.Store(true)
			body = `{"data":{"total_credits":50,"total_usage":40}}`
		}

		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	result := ReadProviderQuota(t.Context(), []Client{native}, quotaRouterProvider, "", true, client)
	require.Equal(t, []string{"/api/v1/key", "/api/v1/credits"}, paths)
	require.Equal(t, ProviderQuota{Availability: quotaUnavailable, Reason: quotaReadFailed}, result)
}

func TestProviderQuotaRejectsUnsupportedCredentialEndpoints(t *testing.T) {
	for _, test := range []struct {
		name          string
		endpoint, npm string
		options       map[string]any
	}{
		{"gateway", "https://gateway.example/v1", "@ai-sdk/openai-compatible", nil},
		{"configured gateway", quotaGoBase, "@ai-sdk/openai-compatible", map[string]any{"baseURL": "https://gateway.example/v1"}},
		{"authorization header", quotaGoBase, "@ai-sdk/openai-compatible", map[string]any{"headers": map[string]string{"Authorization": "another"}}},
		{"custom SDK", quotaGoBase, "custom-sdk", nil},
		{"null key", quotaGoBase, "@ai-sdk/openai-compatible", map[string]any{"apiKey": nil}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := quotaCatalog(t, quotaGoProvider, "key", test.endpoint, test.npm, test.options)
			require.Equal(t, "unsupported", resolveQuotaBinding(raw, quotaGoProvider, "").availability)
		})
	}
}

func TestProviderQuotaFencesChangedCredentialAndAmbiguousScopes(t *testing.T) {
	var changed atomic.Bool
	var polls atomic.Int64
	native := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == routeConfig {
			_, err := io.WriteString(w, `{}`)
			require.NoError(t, err)

			return
		}
		key := "first"
		if changed.Load() {
			key = "second"
		}
		_, err := w.Write(quotaCatalog(t, quotaGoProvider, key, quotaGoBase, "@ai-sdk/openai-compatible", nil))
		require.NoError(t, err)
	})
	client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
		polls.Add(1)
		changed.Store(true)

		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"2026-09-09T00:00:00Z"}}}`)), Header: make(http.Header), Request: request}, nil
	})}
	result := ReadProviderQuota(t.Context(), []Client{native}, quotaGoProvider, "", true, client)
	require.Equal(t, "read_failed", result.Reason)
	require.Nil(t, result.Go)
	other := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == routeConfig {
			_, err := io.WriteString(w, `{}`)
			require.NoError(t, err)

			return
		}
		_, err := w.Write(quotaCatalog(t, quotaGoProvider, "third", quotaGoBase, "@ai-sdk/openai-compatible", nil))
		require.NoError(t, err)
	})
	result = ReadProviderQuota(t.Context(), []Client{native, other}, quotaGoProvider, "", true, client)
	require.Equal(t, "session_required", result.Reason)
	require.EqualValues(t, 1, polls.Load())
}

func TestQuotaEndpointFailuresAndRedirects(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"unauthorized", 401, "secret detail", "not_authenticated"},
		{"forbidden", 403, "secret detail", "read_failed"},
		{"throttled", 429, "secret detail", "read_failed"},
		{"malformed", 200, `{`, "read_failed"},
		{"missing limit", 200, `{"data":{"usage":0}}`, "read_failed"},
		{"missing amount", 200, `{"data":{"limit":2,"usage":0}}`, "read_failed"},
		{"redirect", 302, "", "read_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
				calls++

				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: http.Header{"Location": {"https://other.example/"}}, Request: request}, nil
			})}
			result := readQuotaEndpoint(t.Context(), quotaBinding{key: "private", endpoint: quotaRouterBase + "/key"}, quotaRouterProvider, client)
			require.Equal(t, test.want, result.Reason)
			require.Equal(t, 1, calls)
		})
	}
}

func TestGoQuotaEndpointDistinguishesAuthenticationAndEntitlement(t *testing.T) {
	for _, test := range []struct {
		name, body, reason string
		status             int
	}{
		{"authentication", `{"type":"error","error":{"type":"AuthError","message":"Unauthorized"}}`, "not_authenticated", http.StatusUnauthorized},
		{"entitlement", `{"type":"error","error":{"type":"EntitlementError","message":"OpenCode Go subscription required."}}`, "read_failed", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
				require.Equal(t, quotaGoBase+"/usage", request.URL.String())
				require.Equal(t, "Bearer private", request.Header.Get("Authorization"))

				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header), Request: request}, nil
			})}
			result := readQuotaEndpoint(t.Context(), quotaBinding{key: "private", endpoint: quotaGoBase + "/usage"}, quotaGoProvider, client)
			require.Equal(t, ProviderQuota{Availability: "unavailable", Reason: test.reason}, result)
			encoded, err := json.Marshal(result)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "Unauthorized")
			require.NotContains(t, string(encoded), "OpenCode Go subscription required.")
			require.NotContains(t, string(encoded), "private")
		})
	}
}

func TestProviderQuotaCustomPluginsAndDisable(t *testing.T) {
	native := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, routeConfig, r.URL.Path)
		_, err := io.WriteString(w, `{"plugin":["third-party-auth"]}`)
		require.NoError(t, err)
	})
	result := ReadProviderQuota(t.Context(), []Client{native}, quotaGoProvider, "", true, &http.Client{})
	require.Equal(t, "unsupported", result.Availability)
	result = ReadProviderQuota(t.Context(), []Client{native}, quotaGoProvider, "", false, &http.Client{})
	require.Equal(t, "disabled", result.Reason)
}

func TestProviderQuotaOmitsExpiredWindows(t *testing.T) {
	now := time.Now().UTC()
	percent := 0.0
	for _, test := range []struct {
		name      string
		nextReset time.Time
		want      string
	}{
		{"one remains", now.Add(time.Hour), "available"},
		{"none remain", now, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := currentProviderQuota(ProviderQuota{Availability: "available", Go: &GoQuota{Usage: map[string]GoQuotaWindow{
				"rolling": {Percent: &percent, Status: "ok", ResetsAt: now.Add(-time.Second).Format(time.RFC3339Nano)},
				"weekly":  {Percent: &percent, Status: "ok", ResetsAt: test.nextReset.Format(time.RFC3339Nano)},
			}}}, now)
			require.Equal(t, test.want, result.Availability)
			if result.Go != nil {
				require.Len(t, result.Go.Usage, 1)
			} else {
				require.Equal(t, "not_observed", result.Reason)
			}
		})
	}
}

func TestQuotaEndpointHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	client := &http.Client{Transport: quotaTransport(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()

		return nil, request.Context().Err()
	})}
	done := make(chan ProviderQuota, 1)
	go func() {
		done <- readQuotaEndpoint(ctx, quotaBinding{key: "key", endpoint: quotaGoBase + "/usage"}, quotaGoProvider, client)
	}()
	<-started
	cancel()
	require.Equal(t, "read_failed", (<-done).Reason)
}
