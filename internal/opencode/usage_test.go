package opencode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestUsageAccessVerifiesNativeRoute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, options, modelOptions, headers, plugin, key, reason string }{
		{name: "native key", key: "native-key"},
		{name: "effective override", options: `{"apiKey":"override"}`, key: "override"},
		{name: "disabled override", options: `{"apiKey":""}`, reason: wire.AccountUsageNotAuthenticated},
		{name: "proxy", options: `{"baseURL":"https://proxy.invalid/v1"}`, reason: wire.AccountUsageNotReported},
		{name: "auth header", headers: `{"Authorization":"Bearer different"}`, reason: wire.AccountUsageNotReported},
		{name: "model credential", modelOptions: `{"apiKey":"different"}`, reason: wire.AccountUsageNotReported},
		{name: "custom plugin", plugin: `["other-plugin"]`, reason: wire.AccountUsageNotReported},
		{name: "own plugin", plugin: `["file:///owned/environment.mjs"]`, key: "native-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options, modelOptions, headers, plugins := tc.options, tc.modelOptions, tc.headers, tc.plugin
			if options == "" {
				options = `{}`
			}
			if modelOptions == "" {
				modelOptions = `{}`
			}
			if headers == "" {
				headers = `{}`
			}
			if plugins == "" {
				plugins = `[]`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("directory") != "/workspace" {
					w.WriteHeader(http.StatusBadRequest)

					return
				}
				body := `{"plugin":` + plugins + `}`
				if r.URL.Path == "/config/providers" {
					body = `{"providers":[{"id":"openrouter","key":"native-key","options":` + options + `,"models":{"model":{"api":{"url":"https://openrouter.ai/api/v1","npm":"@openrouter/ai-sdk-provider"},"headers":` + headers + `,"options":` + modelOptions + `}}}]}`
				}
				_ = json.NewEncoder(w).Encode(json.RawMessage(body))
			}))
			defer server.Close()
			client := &Client{URL: server.URL, http: server.Client()}
			access, err := client.UsageAccess(t.Context(), "/workspace", "openrouter", "model", "file:///owned/environment.mjs")
			require.NoError(t, err)
			require.Equal(t, tc.reason, access.Reason)
			require.Equal(t, tc.key, access.APIKey)
		})
	}
}
