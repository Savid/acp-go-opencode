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
	for _, tc := range []struct{ name, options, modelOptions, headers, auth, key, reason string }{
		{name: "native key", key: "native-key"},
		{name: "effective override", options: `{"apiKey":"override"}`, key: "override"},
		{name: "disabled override", options: `{"apiKey":""}`, reason: wire.AccountUsageNotAuthenticated},
		{name: "proxy", options: `{"baseURL":"https://proxy.invalid/v1"}`, reason: wire.AccountUsageNotReported},
		{name: "auth header", headers: `{"Authorization":"Bearer different"}`, reason: wire.AccountUsageNotReported},
		{name: "model credential", modelOptions: `{"apiKey":"different"}`, reason: wire.AccountUsageNotReported},
		{name: "provider auth hook", auth: `{"openrouter":[{"type":"api","label":"Custom key"}]}`, reason: wire.AccountUsageNotReported},
		{name: "provider auth loader without login methods", auth: `{"openrouter":[]}`, reason: wire.AccountUsageNotReported},
		{name: "unrelated provider auth hook", auth: `{"kimi-for-coding-oauth":[{"type":"oauth","label":"Kimi"}]}`, key: "native-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			options, modelOptions, headers, auth := tc.options, tc.modelOptions, tc.headers, tc.auth
			if options == "" {
				options = `{}`
			}
			if modelOptions == "" {
				modelOptions = `{}`
			}
			if headers == "" {
				headers = `{}`
			}
			if auth == "" {
				auth = `{}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("directory") != "/workspace" {
					w.WriteHeader(http.StatusBadRequest)

					return
				}
				var body string
				switch r.URL.Path {
				case "/provider/auth":
					body = auth
				case "/config/providers":
					body = `{"providers":[{"id":"openrouter","key":"native-key","options":` + options + `,"models":{"model":{"api":{"url":"https://openrouter.ai/api/v1","npm":"@openrouter/ai-sdk-provider"},"headers":` + headers + `,"options":` + modelOptions + `}}}]}`
				default:
					w.WriteHeader(http.StatusNotFound)

					return
				}
				_ = json.NewEncoder(w).Encode(json.RawMessage(body))
			}))
			defer server.Close()
			client := &Client{URL: server.URL, http: server.Client()}
			access, err := client.UsageAccess(t.Context(), "/workspace", "openrouter", "model")
			require.NoError(t, err)
			require.Equal(t, tc.reason, access.Reason)
			require.Equal(t, tc.key, access.APIKey)
		})
	}
}
