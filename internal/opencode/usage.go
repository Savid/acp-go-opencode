package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/savid/acp-go-core/usage"
	"github.com/savid/acp-go-core/usage/gateway"
	"github.com/savid/acp-go-core/usage/opencodego"
	"github.com/savid/acp-go-core/usage/openrouter"
	"github.com/savid/acp-go-core/wire"
)

const usageSDKAnthropic = "@ai-sdk/anthropic"

type usageProvider struct {
	ID      string                     `json:"id"`
	Key     string                     `json:"key"`
	Options map[string]json.RawMessage `json:"options"`
	Models  map[string]struct {
		API struct {
			URL string `json:"url"`
			NPM string `json:"npm"`
		} `json:"api"`
		Headers map[string]string          `json:"headers"`
		Options map[string]json.RawMessage `json:"options"`
	} `json:"models"`
}

// UsageAccess resolves the addressed directory's effective credentials and model route.
func (c *Client) UsageAccess(ctx context.Context, directory, providerID, modelID string) (usage.Access, error) {
	var auth map[string]json.RawMessage
	if err := c.Do(ctx, directory, http.MethodGet, "/provider/auth", nil, &auth); err != nil {
		return usage.Access{}, err
	}

	if auth == nil {
		return usage.Access{}, errors.New("native authentication methods missing")
	}

	// Native auth methods identify providers whose credentials are controlled by plugins.
	if _, hooked := auth[providerID]; hooked {
		return usage.Access{Reason: wire.AccountUsageNotReported}, nil
	}

	var catalog struct {
		Providers []usageProvider `json:"providers"`
	}
	if err := c.Do(ctx, directory, http.MethodGet, "/config/providers", nil, &catalog); err != nil {
		return usage.Access{}, err
	}

	if catalog.Providers == nil {
		return usage.Access{}, errors.New("native provider catalog missing")
	}

	for _, provider := range catalog.Providers {
		if provider.ID != providerID {
			continue
		}

		return provider.usageAccess(providerID, modelID)
	}

	return usage.Access{Reason: wire.AccountUsageNotAuthenticated}, nil
}

func (p usageProvider) usageAccess(providerID, modelID string) (usage.Access, error) {
	unsupported := usage.Access{Reason: wire.AccountUsageNotReported}

	key := p.Key
	if raw, present := p.Options["apiKey"]; present {
		if string(raw) == "null" || json.Unmarshal(raw, &key) != nil {
			return unsupported, nil //nolint:nilerr // An unrecognized credential shape cannot establish the account.
		}
	}

	var base string
	if raw, present := p.Options["baseURL"]; present && json.Unmarshal(raw, &base) != nil {
		return unsupported, nil //nolint:nilerr // An unrecognized endpoint cannot establish the route.
	}

	var headers map[string]string
	if raw, present := p.Options["headers"]; present && json.Unmarshal(raw, &headers) != nil {
		return unsupported, nil //nolint:nilerr // Unrecognized headers cannot establish authentication.
	}

	if !usageHeaders(headers) {
		return unsupported, nil
	}

	for _, name := range []string{"fetch", "auth", "token", "accessToken"} {
		if _, present := p.Options[name]; present {
			return unsupported, nil
		}
	}

	found := false

	for id, model := range p.Models {
		if modelID != "" && id != modelID {
			continue
		}

		found = true

		endpoint := base
		if endpoint == "" {
			endpoint = model.API.URL
		}

		if !usageRoute(providerID, endpoint, model.API.NPM) || !usageHeaders(model.Headers) {
			return unsupported, nil
		}

		for _, name := range []string{"apiKey", "baseURL", "headers", "fetch", "auth", "token", "accessToken"} {
			if _, present := model.Options[name]; present {
				return unsupported, nil
			}
		}
	}

	if !found {
		return unsupported, nil
	}

	if strings.TrimSpace(key) == "" {
		return usage.Access{Reason: wire.AccountUsageNotAuthenticated}, nil
	}

	raw, err := json.Marshal(p)
	if err != nil {
		return usage.Access{}, errors.New("native provider configuration invalid")
	}

	return usage.Access{APIKey: key, Fingerprint: sha256.Sum256(raw)}, nil
}

func usageHeaders(headers map[string]string) bool {
	for name := range headers {
		switch strings.ToLower(name) {
		case "http-referer", "x-title", "x-source":
		default:
			return false
		}
	}

	return true
}

func usageRoute(providerID, endpoint, npm string) bool {
	endpoint = strings.TrimSuffix(endpoint, "/")

	switch providerID {
	case openrouter.ProviderID:
		return endpoint == strings.TrimSuffix(openrouter.Endpoint, "/key") && npm == "@openrouter/ai-sdk-provider"
	case opencodego.ProviderID:
		return endpoint == strings.TrimSuffix(opencodego.Endpoint, "/usage") && (npm == "@ai-sdk/openai-compatible" || npm == usageSDKAnthropic || npm == "@ai-sdk/openai")
	default:
		return false
	}
}

// UsageGateways lists the catalog providers configured with their own base
// URL: the routes opencode sends requests through, each with the key the
// catalog holds for it, an {env:NAME} key resolved through lookup.
func (c *Client) UsageGateways(ctx context.Context, directory string, lookup func(string) (string, bool)) ([]gateway.Route, error) {
	var catalog struct {
		Providers []usageProvider `json:"providers"`
	}
	if err := c.Do(ctx, directory, http.MethodGet, "/config/providers", nil, &catalog); err != nil {
		return nil, err
	}

	routes := make([]gateway.Route, 0, len(catalog.Providers))

	for _, provider := range catalog.Providers {
		var base string
		if raw, present := provider.Options["baseURL"]; !present || json.Unmarshal(raw, &base) != nil || base == "" {
			continue
		}

		key := provider.Key
		if raw, present := provider.Options["apiKey"]; present {
			var configured string
			if json.Unmarshal(raw, &configured) == nil {
				key = configured
			}
		}

		if name, ok := strings.CutPrefix(key, "{env:"); ok && strings.HasSuffix(name, "}") {
			key, _ = lookup(strings.TrimSuffix(name, "}"))
		}

		routes = append(routes, gateway.Route{Provider: provider.ID, BaseURL: base, Token: key})
	}

	return routes, nil
}
