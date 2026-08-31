package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newProviderAuthServer(t *testing.T, handler http.HandlerFunc) *openCodeServer {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
	}
}

func TestProviderCatalogDropsEveryKeyOutsideTheAllowlist(t *testing.T) {
	var path string

	client := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path

		writeJSON(t, w, map[string]any{
			"all": []map[string]any{{
				"id":       "zhipuai",
				"name":     "Zhipu AI",
				"source":   "custom",
				"env":      []string{"ZHIPU_API_KEY"},
				"options":  map[string]any{},
				"models":   map[string]any{"glm-5": map[string]any{"id": "glm-5"}},
				"key":      "sk-plaintext",
				"unknown":  true,
				"disabled": false,
			}},
			"default": map[string]any{},
		})
	})

	entries, err := client.ProviderCatalog(context.Background())
	if err != nil {
		t.Fatalf("ProviderCatalog: %v", err)
	}

	if path != routeProvider {
		t.Fatalf("catalog read %q, want %q", path, routeProvider)
	}

	if len(entries) != 1 || entries[0].ID != "zhipuai" || entries[0].Name != "Zhipu AI" || entries[0].Source != "custom" {
		t.Fatalf("unexpected catalog entry: %+v", entries)
	}

	if len(entries[0].Env) != 1 || len(entries[0].Models) != 1 {
		t.Fatalf("allowlisted fields were dropped: %+v", entries[0])
	}

	encoded, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}

	if strings.Contains(string(encoded), "sk-plaintext") {
		t.Fatal("the plaintext key survived the allowlist projection")
	}
}

func TestProviderCatalogFailures(t *testing.T) {
	client := newProviderAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := client.ProviderCatalog(context.Background()); err == nil {
		t.Fatal("expected a transport failure")
	}

	malformed := newProviderAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"all": []any{"not-an-object"}})
	})

	if _, err := malformed.ProviderCatalog(context.Background()); err == nil {
		t.Fatal("expected a decode failure")
	}
}

func TestDecodeProviderCatalogEntryFailures(t *testing.T) {
	if _, err := decodeProviderCatalogEntry(json.RawMessage(`{"id":[]}`)); err == nil {
		t.Fatal("expected a typed decode failure")
	}

	original := catalogMarshal
	catalogMarshal = func(any) ([]byte, error) { return nil, errors.New("encode") }

	t.Cleanup(func() { catalogMarshal = original })

	if _, err := decodeProviderCatalogEntry(json.RawMessage(`{"id":"xai"}`)); err == nil {
		t.Fatal("expected an encode failure")
	}
}

func TestProviderAuthMethodsDecodesPromptsStrictly(t *testing.T) {
	client := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != routeProviderAuth {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		writeJSON(t, w, map[string]any{
			"snowflake-cortex": []map[string]any{{
				"type":  "oauth",
				"label": "Login with Snowflake",
				"extra": "ignored",
				"prompts": []map[string]any{{
					"type":        "text",
					"key":         "account",
					"message":     "Account",
					"placeholder": "myorg",
				}},
			}},
		})
	})

	methods, err := client.ProviderAuthMethods(context.Background())
	if err != nil {
		t.Fatalf("ProviderAuthMethods: %v", err)
	}

	entry := methods["snowflake-cortex"][0]
	if entry.Type != "oauth" || entry.Label != "Login with Snowflake" || len(entry.Prompts) != 1 {
		t.Fatalf("unexpected method: %+v", entry)
	}

	if entry.Prompts[0].Placeholder != "myorg" {
		t.Fatalf("unexpected prompt: %+v", entry.Prompts[0])
	}
}

func TestProviderAuthMethodsFailures(t *testing.T) {
	cases := []struct {
		name string
		body any
	}{
		{name: "transport"},
		{name: "method is not an object", body: map[string]any{"xai": []any{"oops"}}},
		{name: "prompt carries an unknown field", body: map[string]any{"xai": []map[string]any{{
			"type":    "oauth",
			"label":   "xAI",
			"prompts": []map[string]any{{"type": "text", "key": "a", "message": "m", "added": 1}},
		}}}},
		{name: "prompt has trailing content", body: nil},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := newProviderAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if testCase.body == nil {
					w.WriteHeader(http.StatusInternalServerError)

					return
				}

				writeJSON(t, w, testCase.body)
			})

			if _, err := client.ProviderAuthMethods(context.Background()); err == nil {
				t.Fatal("expected a failure")
			}
		})
	}
}

func TestStrictDecodeRejectsTrailingContent(t *testing.T) {
	var prompt ProviderAuthPrompt
	if err := strictDecode([]byte(`{"type":"text"} {}`), &prompt); err == nil {
		t.Fatal("expected trailing content to be rejected")
	}
}

func TestProviderAuthorizeSendsTheNativeIndexAndInputs(t *testing.T) {
	var body map[string]any

	var path string

	client := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}

		writeJSON(t, w, map[string]any{"url": "https://x", "method": "auto", "instructions": "go"})
	})

	authorization, err := client.ProviderAuthorize(context.Background(), "github-copilot", 1, map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("ProviderAuthorize: %v", err)
	}

	if path != "/provider/github-copilot/oauth/authorize" {
		t.Fatalf("unexpected path %q", path)
	}

	sentMethod, ok := body[providerAuthMethodField].(float64)
	if !ok || sentMethod != 1 || body["inputs"] == nil {
		t.Fatalf("unexpected body %v", body)
	}

	if authorization.Method != ProviderAuthNativeMethodAuto || authorization.URL != "https://x" {
		t.Fatalf("unexpected authorization %+v", authorization)
	}

	body = nil

	if _, err := client.ProviderAuthorize(context.Background(), "xai", 0, nil); err != nil {
		t.Fatalf("ProviderAuthorize without inputs: %v", err)
	}

	if body["inputs"] != nil {
		t.Fatalf("an empty inputs map was sent: %v", body)
	}
}

func TestProviderAuthCallbackCarriesTheCodeWhenPresent(t *testing.T) {
	var body map[string]any

	var path string

	client := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}

		writeJSON(t, w, true)
	})

	if err := client.ProviderAuthCallback(context.Background(), "xai", 0, "pasted"); err != nil {
		t.Fatalf("ProviderAuthCallback: %v", err)
	}

	if path != "/provider/xai/oauth/callback" || body["code"] != "pasted" {
		t.Fatalf("unexpected callback %q %v", path, body)
	}

	body = nil

	if err := client.ProviderAuthCallback(context.Background(), "xai", 0, ""); err != nil {
		t.Fatalf("ProviderAuthCallback without a code: %v", err)
	}

	if _, ok := body["code"]; ok {
		t.Fatalf("an empty code was sent: %v", body)
	}
}

// TestProviderAuthCallbackUsesADeadlineFreeClient drives the leg against a
// server that answers long after the configured client deadline. Callback
// blocks until the owner finishes at the provider, so any deadline carried into
// it aborts a login that was going to succeed — and the deadline belongs to the
// shared client, which must still have it afterwards.
func TestProviderAuthCallbackUsesADeadlineFreeClient(t *testing.T) {
	const deadline = 25 * time.Millisecond

	client := newProviderAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(4 * deadline)
		writeJSON(t, w, true)
	})
	client.httpClient.Timeout = deadline

	if err := client.ProviderAuthCallback(context.Background(), "xai", 0, ""); err != nil {
		t.Fatalf("ProviderAuthCallback: %v", err)
	}

	if client.httpClient.Timeout != deadline {
		t.Fatalf("the shared client's deadline is now %v", client.httpClient.Timeout)
	}

	client.httpClient = nil

	if err := client.ProviderAuthCallback(context.Background(), "xai", 0, ""); err != nil {
		t.Fatalf("ProviderAuthCallback without a configured client: %v", err)
	}
}

func TestSetAndRemoveProviderAuth(t *testing.T) {
	var method, path string

	var body ProviderAuthCredential

	client := newProviderAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path

		if r.Method == http.MethodPut {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
		}

		writeJSON(t, w, true)
	})

	if err := client.SetProviderAuth(context.Background(), "xai", ProviderAuthCredential{Type: ProviderAuthTypeAPI, Key: "k"}); err != nil {
		t.Fatalf("SetProviderAuth: %v", err)
	}

	if method != http.MethodPut || path != "/auth/xai" || body.Key != "k" {
		t.Fatalf("unexpected write %s %s %+v", method, path, body)
	}

	if err := client.RemoveProviderAuth(context.Background(), "xai"); err != nil {
		t.Fatalf("RemoveProviderAuth: %v", err)
	}

	if method != http.MethodDelete || path != "/auth/xai" {
		t.Fatalf("unexpected removal %s %s", method, path)
	}

	if err := client.DisposeInstance(context.Background()); err != nil {
		t.Fatalf("DisposeInstance: %v", err)
	}

	if method != http.MethodPost || path != routeInstanceDispose {
		t.Fatalf("unexpected dispose %s %s", method, path)
	}
}

func TestStoredProviderAuthReadsTheOwnedStore(t *testing.T) {
	data := t.TempDir()
	client := &openCodeServer{xdg: XDGDirs{Data: data}}

	if _, ok, err := client.StoredProviderAuth(context.Background(), "xai"); err != nil || ok {
		t.Fatalf("absent store: ok=%v err=%v", ok, err)
	}

	storeDir := filepath.Join(data, authStoreDir)
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatalf("create store dir: %v", err)
	}

	write := func(contents string) {
		if err := os.WriteFile(filepath.Join(storeDir, authStoreFile), []byte(contents), 0o600); err != nil {
			t.Fatalf("write store: %v", err)
		}
	}

	write(`{"xai":{"type":"oauth","refresh":"r","access":"a","expires":42}}`)

	credential, ok, err := client.StoredProviderAuth(context.Background(), "xai")
	if err != nil || !ok || credential.Refresh != "r" || credential.Expires != 42 {
		t.Fatalf("unexpected credential ok=%v err=%v %+v", ok, err, credential)
	}

	if _, ok, err := client.StoredProviderAuth(context.Background(), "absent"); err != nil || ok {
		t.Fatalf("absent provider: ok=%v err=%v", ok, err)
	}

	for _, contents := range []string{
		`{`,
		`{"xai":{"type":"oauth","refresh":"r","access":"a","expires":1,"added":true}}`,
		`{"xai":{"type":"wellknown","key":"k","token":"t"}}`,
		`{"xai":{"type":"api"}}`,
		`{"xai":{"type":"oauth","refresh":"r"}}`,
	} {
		write(contents)

		if _, _, err := client.StoredProviderAuth(context.Background(), "xai"); err == nil {
			t.Fatalf("expected %q to be rejected", contents)
		}
	}
}

func TestReadProviderAuthStoreReportsReadFailures(t *testing.T) {
	original := authStoreReadFile
	authStoreReadFile = func(string) ([]byte, error) { return nil, errors.New("io") }

	t.Cleanup(func() { authStoreReadFile = original })

	if _, _, err := readProviderAuthStore(t.TempDir(), "xai"); err == nil {
		t.Fatal("expected a read failure")
	}
}

func TestURLPathSegmentEscapes(t *testing.T) {
	if got := urlPathSegment("a/b"); got != "a%2Fb" {
		t.Fatalf("urlPathSegment = %q", got)
	}
}

func TestIsRateLimited(t *testing.T) {
	if !IsRateLimited(&HTTPError{StatusCode: http.StatusTooManyRequests}) {
		t.Fatal("429 is a rate-limit refusal")
	}

	if IsRateLimited(errors.New("dial")) {
		t.Fatal("a transport error is not a rate-limit refusal")
	}
}
