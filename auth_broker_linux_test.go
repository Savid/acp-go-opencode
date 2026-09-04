//go:build linux && browsercanary

package opencodeacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

const (
	browserCanaryScratch = "/canary/scratch"
	browserCanaryNative  = "/usr/local/bin/opencode"
	browserCanaryState   = "/var/lib/acp-go-opencode-browser-canary"

	// browserCanaryMethodLabel is the native Snowflake browser method in the
	// pinned release. Its mint execs the platform browser launcher and returns
	// a loopback completion variant, which is what the canary traces.
	browserCanaryMethodLabel = "Login with Snowflake (External Browser)"
)

func TestRealNativeBrowserLaunchIsNeutralized(t *testing.T) {
	if os.Getenv("ACP_GO_OPENCODE_BROWSER_CANARY") != "1" {
		t.Fatal("real-native browser canary was selected without its required execution gate")
	}
	require.FileExists(t, browserCanaryNative)

	// The reviewed registry publishes no browser method, so the canary admits
	// the pinned one itself. The proof is about what lies past the catalog: a
	// real browser launch resolves inside the production shim, and the broker
	// still refuses the loopback completion variant and cleans up.
	admitOAuthMethodForTest(t, authProviderSnowflake, browserCanaryMethodLabel)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	require.NoError(t, os.MkdirAll(browserCanaryScratch, 0o700))
	authRoot := browserCanaryDir(t, filepath.Join(browserCanaryScratch, "auth-ledger"))
	stateRoot := browserCanaryDir(t, browserCanaryState)

	agent := NewAgent(
		WithExecutablePath(browserCanaryNative),
		WithHome(stateRoot),
		WithScratchDir(browserCanaryScratch),
		WithProviderAuthRoot(authRoot),
		WithOpenCodePure(true),
		WithOpenCodeHealthCheckTimeout(60*time.Second),
		WithLogger(slog.New(slog.DiscardHandler)),
	)
	require.NotNil(t, agent.providerAuth, "ordinary provider auth must be enabled for the canary")

	catalogClient, err := runtimeStartServer(ctx, opencode.StartOptions{
		Root:           filepath.Join(browserCanaryScratch, "catalog-runtime"),
		ControlRoot:    filepath.Join(browserCanaryScratch, "catalog-control"),
		ScratchParent:  browserCanaryScratch,
		ExecutablePath: browserCanaryNative,
		Pure:           true,
		MinVersion:     minNativeVersion,
		HealthTimeout:  60 * time.Second,
		Logger:         slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)

	sessionID := acp.SessionId("browser-canary-session")
	agent.mu.Lock()
	agent.sessions[sessionID] = &session{agent: agent, id: sessionID, client: catalogClient}
	agent.mu.Unlock()

	enumerated, err := agent.HandleExtensionMethod(ctx, AuthMethodsMethod, browserCanaryParams(t, map[string]any{
		"sessionId": string(sessionID),
	}))
	require.NoError(t, err)
	methods, ok := enumerated.(authMethodsResult)
	require.True(t, ok)
	entries := methods.Providers[authProviderSnowflake]
	require.NotEmpty(t, entries, "current native Snowflake browser method is absent")
	require.Equal(t, "0", entries[0].ID)
	require.Equal(t, authMethodTypeOAuth, entries[0].Type)
	require.NoError(t, catalogClient.Shutdown(ctx))

	_, err = agent.HandleExtensionMethod(ctx, AuthAuthorizeMethod, browserCanaryParams(t, map[string]any{
		"sessionId":          string(sessionID),
		"providerId":         authProviderSnowflake,
		"connectionId":       "browser-canary-connection",
		"methodsGeneration":  methods.Generation,
		authFieldMethod:      entries[0].ID,
		"authorizeRequestId": "browser-canary-request",
		"inputs": map[string]string{
			"account": "canary",
			"role":    "PUBLIC",
		},
	}))
	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32000, requestErr.Code)
	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, authCauseUnsupportedVariant, data[jsonFieldCause])
	require.Equal(t, authProviderSnowflake, data[authFieldProviderID])
	require.Equal(t, entries[0].ID, data[authFieldMethod])
	require.NotEmpty(t, data[authFieldFlowID])

	require.Eventually(t, func() bool {
		matches, globErr := filepath.Glob(filepath.Join(browserCanaryScratch, "acp-go-opencode-browser-shim-*"))
		return globErr == nil && len(matches) == 0
	}, 15*time.Second, 25*time.Millisecond, "terminal provider-auth rejection left its browser shim behind")
}

// admitOAuthMethodForTest publishes one native OAuth method for the duration
// of the test, so a flow past the catalog can be driven against a method the
// reviewed registry does not carry. The canary is the only caller and runs
// alone in its container, so the package-level lookup is safe to edit.
func admitOAuthMethodForTest(t *testing.T, providerID, label string) {
	t.Helper()

	entries, existed := authReviewedOAuthMethods[providerID]
	if !existed {
		entries = map[string]struct{}{}
		authReviewedOAuthMethods[providerID] = entries
	}

	_, admitted := entries[label]
	entries[label] = struct{}{}

	t.Cleanup(func() {
		if admitted {
			return
		}

		delete(entries, label)

		if !existed {
			delete(authReviewedOAuthMethods, providerID)
		}
	})
}

func browserCanaryParams(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

func browserCanaryDir(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.Mkdir(path, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(path)) })

	return path
}
