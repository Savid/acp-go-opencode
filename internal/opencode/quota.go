package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	quotaUnsupported      = "unsupported"
	quotaUnavailable      = "unavailable"
	quotaAvailable        = "available"
	quotaReadFailed       = "read_failed"
	quotaNotAuthenticated = "not_authenticated"
	quotaSessionRequired  = "session_required"
	quotaCompatibleSDK    = "@ai-sdk/openai-compatible"
	quotaStatusOK         = "ok"
	quotaWeekly           = "weekly"
	quotaMonthly          = "monthly"
	quotaRolling          = "rolling"
	quotaGoProvider       = "opencode-go"
	quotaRouterProvider   = "openrouter"
	quotaGoBase           = "https://opencode.ai/zen/go/v1"
	quotaRouterBase       = "https://openrouter.ai/api/v1"
)

// ProviderQuota contains only structured observations received from the account
// endpoint. Credential material and native configuration never leave this file.
type ProviderQuota struct {
	Availability      string
	Reason            string
	ObservedAt        time.Time
	Go                *GoQuota
	OpenRouter        *OpenRouterQuota
	OpenRouterCredits *OpenRouterCredits
}

type GoQuota struct {
	Usage map[string]GoQuotaWindow `json:"usage"`
}

type GoQuotaWindow struct {
	Status   string   `json:"status"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

// OpenRouterQuota uses the native OpenRouter account-usage spelling.
//
//nolint:tagliatelle // Native response field names use snake_case.
type OpenRouterQuota struct {
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining"`
	LimitReset     *string  `json:"limit_reset"`
	Usage          *float64 `json:"usage"`
}

// OpenRouterCredits contains account USD usage and remaining credit, distinct
// from a key's optional spending cap. Its timestamp belongs to the credits read.
type OpenRouterCredits struct {
	Used       float64
	Remaining  float64
	ObservedAt time.Time
}

//nolint:tagliatelle // Native response field names use snake_case.
type openRouterCreditsWire struct {
	TotalCredits *float64 `json:"total_credits"`
	TotalUsage   *float64 `json:"total_usage"`
}

type quotaBinding struct {
	key          string
	endpoint     string
	availability string
	reason       string
}

// SupportsProviderQuota names the providers whose declared account endpoint
// accepts their ordinary inference credential.
func SupportsProviderQuota(providerID string) bool {
	return providerID == quotaGoProvider || providerID == quotaRouterProvider
}

// ReadProviderQuota resolves each addressed native scope before acquiring an
// observation. Multiple scopes must agree; an arbitrary session is never used
// as a provider-wide credential source. No observation is cached.
func ReadProviderQuota(ctx context.Context, clients []Client, providerID, modelID string, enabled bool, client *http.Client) ProviderQuota {
	if !SupportsProviderQuota(providerID) {
		return ProviderQuota{Availability: quotaUnsupported}
	}

	if !enabled {
		return ProviderQuota{Availability: quotaUnavailable, Reason: "disabled"}
	}

	if len(clients) == 0 {
		return ProviderQuota{Availability: quotaUnavailable, Reason: quotaSessionRequired}
	}

	binding := resolveQuotaBindings(ctx, clients, providerID, modelID)
	if binding.availability != quotaAvailable {
		return ProviderQuota{Availability: binding.availability, Reason: binding.reason}
	}

	result := readQuotaEndpoint(ctx, binding, providerID, client)
	if result.Availability != quotaAvailable {
		return result
	}

	if providerID == quotaRouterProvider {
		result.OpenRouterCredits = readOpenRouterCredits(ctx, binding, client)
	}
	// Native configuration may change independently of the adapter's auth API.
	// Re-resolve the same scopes before publishing an account observation.
	if current := resolveQuotaBindings(ctx, clients, providerID, modelID); current != binding {
		return ProviderQuota{Availability: quotaUnavailable, Reason: quotaReadFailed}
	}

	return currentProviderQuota(result, time.Now())
}

func currentProviderQuota(result ProviderQuota, now time.Time) ProviderQuota {
	if result.Go == nil {
		return result
	}

	for id, window := range result.Go.Usage {
		reset, err := time.Parse(time.RFC3339Nano, window.ResetsAt)
		if err != nil || !reset.After(now) {
			delete(result.Go.Usage, id)
		}
	}

	if len(result.Go.Usage) == 0 {
		return ProviderQuota{Availability: quotaUnavailable, Reason: "not_observed"}
	}

	return result
}

func resolveQuotaBindings(ctx context.Context, clients []Client, providerID, modelID string) quotaBinding {
	var selected quotaBinding

	for i, client := range clients {
		allowed, err := client.ProviderQuotaAllowed(ctx)
		if err != nil {
			return quotaBinding{availability: quotaUnavailable, reason: quotaReadFailed}
		}

		binding := quotaBinding{availability: quotaUnsupported}

		if allowed {
			response, readErr := client.ConfigProviders(ctx)
			if readErr != nil {
				return quotaBinding{availability: quotaUnavailable, reason: quotaReadFailed}
			}

			binding = resolveQuotaBinding(response.Raw, providerID, modelID)
		}

		if i != 0 && binding != selected {
			return quotaBinding{availability: quotaUnavailable, reason: quotaSessionRequired}
		}

		selected = binding
	}

	return selected
}

func resolveQuotaBinding(raw json.RawMessage, providerID, modelID string) quotaBinding {
	var response struct {
		Providers []struct {
			ID      string                     `json:"id"`
			Key     string                     `json:"key"`
			Options map[string]json.RawMessage `json:"options"`
			Models  map[string]struct {
				API struct {
					URL string `json:"url"`
					NPM string `json:"npm"`
				} `json:"api"`
				Headers map[string]string `json:"headers"`
			} `json:"models"`
		} `json:"providers"`
	}

	failed := quotaBinding{availability: quotaUnavailable, reason: quotaReadFailed}
	unsupported := quotaBinding{availability: quotaUnsupported}

	if json.Unmarshal(raw, &response) != nil {
		return failed
	}

	for _, provider := range response.Providers {
		if provider.ID != providerID {
			continue
		}

		key := provider.Key
		if raw, ok := provider.Options["apiKey"]; ok {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &key) != nil {
				return unsupported
			}
		}

		var endpoint string
		if raw, ok := provider.Options["baseURL"]; ok && json.Unmarshal(raw, &endpoint) != nil {
			return unsupported
		}

		var headers map[string]string
		if raw, ok := provider.Options["headers"]; ok && json.Unmarshal(raw, &headers) != nil {
			return unsupported
		}

		if !quotaHeadersSupported(headers) {
			return unsupported
		}
		// JSON cannot represent plugin fetch functions. Only the reviewed native
		// API-key modes and SDK identities are accepted below.
		for _, field := range []string{"fetch", "auth", "token", "accessToken"} {
			if _, ok := provider.Options[field]; ok {
				return unsupported
			}
		}

		found, supported := 0, 0

		for id, model := range provider.Models {
			if modelID != "" && id != modelID {
				continue
			}

			found++

			base := endpoint
			if base == "" {
				base = model.API.URL
			}

			if quotaHeadersSupported(model.Headers) && quotaSDKSupported(providerID, model.API.NPM) && quotaEndpointSupported(providerID, base) {
				supported++
			}
		}

		if supported == 0 {
			return unsupported
		}

		if supported != found {
			return quotaBinding{availability: quotaUnavailable, reason: quotaSessionRequired}
		}

		if strings.TrimSpace(key) == "" {
			return quotaBinding{availability: quotaUnavailable, reason: quotaNotAuthenticated}
		}

		endpoint = quotaGoBase + "/usage"
		if providerID == quotaRouterProvider {
			endpoint = quotaRouterBase + "/key"
		}

		return quotaBinding{key: key, endpoint: endpoint, availability: quotaAvailable}
	}

	return quotaBinding{availability: quotaUnavailable, reason: quotaNotAuthenticated}
}

func quotaHeadersSupported(headers map[string]string) bool {
	for name := range headers {
		switch strings.ToLower(name) {
		case "http-referer", "x-title", "x-source":
		default:
			return false
		}
	}

	return true
}

func quotaSDKSupported(providerID, npm string) bool {
	if providerID == quotaRouterProvider {
		return npm == "@openrouter/ai-sdk-provider"
	}

	// Go's official catalog serves one account through Chat Completions,
	// Messages, and Responses SDKs; the endpoint check keeps all three on Go.
	return npm == quotaCompatibleSDK || npm == "@ai-sdk/anthropic" || npm == "@ai-sdk/openai"
}

func quotaEndpointSupported(providerID, endpoint string) bool {
	expected := quotaGoBase
	if providerID == quotaRouterProvider {
		expected = quotaRouterBase
	}

	return strings.TrimSuffix(endpoint, "/") == expected
}

func readQuotaEndpoint(ctx context.Context, binding quotaBinding, providerID string, client *http.Client) ProviderQuota {
	failed := ProviderQuota{Availability: quotaUnavailable, Reason: quotaReadFailed}

	body, observedAt, reason := readQuotaBody(ctx, binding, client)
	if reason != "" {
		return ProviderQuota{Availability: quotaUnavailable, Reason: reason}
	}

	result := ProviderQuota{Availability: quotaAvailable, ObservedAt: observedAt}

	if providerID == quotaGoProvider {
		var usage GoQuota
		if json.Unmarshal(body, &usage) != nil || !validGoQuota(usage) {
			return failed
		}

		result.Go = &usage
	} else {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			return failed
		}

		var (
			usage  OpenRouterQuota
			fields map[string]json.RawMessage
		)
		if json.Unmarshal(envelope.Data, &usage) != nil || json.Unmarshal(envelope.Data, &fields) != nil || fields["limit"] == nil || !validOpenRouterQuota(usage) {
			return failed
		}

		result.OpenRouter = &usage
	}

	return result
}

func readOpenRouterCredits(ctx context.Context, binding quotaBinding, client *http.Client) *OpenRouterCredits {
	// Account credits are optional: a slow or denied account lookup must not
	// consume the whole acquisition budget for a valid key observation.
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	binding.endpoint = quotaRouterBase + "/credits"

	body, observedAt, reason := readQuotaBody(readCtx, binding, client)
	if reason != "" {
		return nil
	}

	var envelope struct {
		Data openRouterCreditsWire `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}

	credits, used := envelope.Data.TotalCredits, envelope.Data.TotalUsage
	if credits == nil || used == nil || !quotaFinite(*credits) || !quotaFinite(*used) || *credits < 0 || *used < 0 {
		return nil
	}

	return &OpenRouterCredits{Used: *used, Remaining: *credits - *used, ObservedAt: observedAt}
}

// readQuotaBody shares the credential, redirect, response-size, and receipt
// timestamp boundary between required and optional quota endpoints.
func readQuotaBody(ctx context.Context, binding quotaBinding, client *http.Client) ([]byte, time.Time, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binding.endpoint, http.NoBody)
	if err != nil {
		return nil, time.Time{}, quotaReadFailed
	}

	req.Header.Set("Authorization", "Bearer "+binding.key)
	req.Header.Set("Accept", "application/json")
	// Copy the client to enforce this boundary even when tests substitute its
	// transport. No redirect may forward or reuse the inference credential.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	response, err := bounded.Do(req)
	if err != nil {
		return nil, time.Time{}, quotaReadFailed
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusUnauthorized {
		return nil, time.Time{}, quotaNotAuthenticated
	}

	if response.StatusCode != http.StatusOK {
		return nil, time.Time{}, quotaReadFailed
	}

	const bodyLimit = 1 << 20

	body, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit+1))
	observedAt := time.Now().UTC()

	if err != nil || len(body) > bodyLimit {
		return nil, time.Time{}, quotaReadFailed
	}

	return body, observedAt, ""
}

func validGoQuota(usage GoQuota) bool {
	if len(usage.Usage) == 0 {
		return false
	}

	for id, window := range usage.Usage {
		if id != quotaRolling && id != quotaWeekly && id != quotaMonthly {
			return false
		}

		if window.Status != quotaStatusOK && window.Status != "rate-limited" {
			return false
		}

		if window.Percent == nil || !quotaFinite(*window.Percent) || *window.Percent < 0 {
			return false
		}

		if _, err := time.Parse(time.RFC3339Nano, window.ResetsAt); err != nil || !strings.HasSuffix(window.ResetsAt, "Z") {
			return false
		}
	}

	return true
}

func validOpenRouterQuota(usage OpenRouterQuota) bool {
	if usage.Usage == nil || !quotaFinite(*usage.Usage) || *usage.Usage < 0 {
		return false
	}

	if usage.Limit != nil && (!quotaFinite(*usage.Limit) || *usage.Limit < 0 || usage.LimitRemaining == nil) {
		return false
	}

	if usage.LimitRemaining != nil && !quotaFinite(*usage.LimitRemaining) {
		return false
	}

	if usage.LimitReset != nil && *usage.LimitReset != "daily" && *usage.LimitReset != quotaWeekly && *usage.LimitReset != quotaMonthly {
		return false
	}

	return true
}

func quotaFinite(value float64) bool { return !math.IsInf(value, 0) && !math.IsNaN(value) }

// ProviderQuotaAllowed rejects custom plugins, whose non-JSON fetch functions
// may replace authentication without appearing in the provider catalog.
func (s *openCodeServer) ProviderQuotaAllowed(ctx context.Context) (bool, error) {
	var config *struct {
		Plugin []json.RawMessage `json:"plugin"`
	}
	if err := s.getJSON(ctx, routeConfig, nil, &config); err != nil {
		return false, err
	}

	if config == nil {
		return false, errors.New("missing native quota configuration")
	}

	for _, raw := range config.Plugin {
		var plugin string
		if err := json.Unmarshal(raw, &plugin); err != nil {
			return false, err
		}

		if plugin == "" || plugin != s.quotaCarrierPlugin {
			return false, nil
		}
	}

	return true, nil
}
