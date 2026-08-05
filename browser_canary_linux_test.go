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
	browserCanaryUID     = 4242
	browserCanaryGID     = 4242
	browserCanaryScratch = "/canary/scratch"
	browserCanaryNative  = "/usr/local/bin/opencode"
	browserCanaryState   = "/var/lib/acp-go-opencode-browser-canary"
)

// TestRealNativeBrowserContainment drives OpenCode's Snowflake external-browser
// login through the production ACP extension dispatcher and per-flow broker.
// The catalog comes from a real contained current-native server, which is
// fenced before authorize starts the independently contained broker server.
func TestRealNativeBrowserContainment(t *testing.T) {
	if os.Getenv("ACP_GO_OPENCODE_BROWSER_CANARY") != "1" {
		t.Fatal("real-native browser canary was selected without its required execution gate")
	}
	require.Equal(t, 0, os.Getuid(), "the canary must start as root so production isolation can enter uid 4242")
	require.FileExists(t, browserCanaryNative)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	require.NoError(t, os.MkdirAll(browserCanaryScratch, 0o711))
	require.NoError(t, os.Chmod(browserCanaryScratch, 0o711))
	for _, dir := range []string{
		"/home/canary/.cache",
		"/home/canary/.config",
		"/home/canary/.local/share",
		"/home/canary/.local/state",
		"/home/canary/.run",
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.Chown(dir, browserCanaryUID, browserCanaryGID))
	}

	authRoot := browserCanaryOwnedDir(t, filepath.Join(browserCanaryScratch, "auth-ledger"), 0o700, false)
	stateRoot := browserCanaryOwnedDir(t, browserCanaryState, 0o700, true)
	isolation := ProcessIsolation{
		UID:                 browserCanaryUID,
		GID:                 browserCanaryGID,
		StandaloneOwnerID:   "opencode-browser-canary",
		StandaloneStateRoot: stateRoot,
		BaseEnvironment: map[string]string{
			"BROWSER":             "/canary/decoys/open",
			"HOME":                "/home/canary",
			"LOGNAME":             "canary",
			"PATH":                "/canary/decoys:/usr/local/bin:/usr/bin:/bin",
			"USER":                "canary",
			"XDG_CACHE_HOME":      "/home/canary/.cache",
			"XDG_CONFIG_HOME":     "/home/canary/.config",
			managedEnvXDGDataHome: "/home/canary/.local/share",
			"XDG_RUNTIME_DIR":     "/home/canary/.run",
			"XDG_STATE_HOME":      "/home/canary/.local/state",
		},
	}

	agent := NewAgent(
		WithExecutablePath(browserCanaryNative),
		WithHome(stateRoot),
		WithScratchDir(browserCanaryScratch),
		WithProviderAuthRoot(authRoot),
		WithOpenCodePure(true),
		WithOpenCodeHealthCheckTimeout(60*time.Second),
		WithLogger(slog.New(slog.DiscardHandler)),
		WithProcessIsolation(isolation),
	)
	require.Equal(t, RuntimeContainmentAuthoritative, agent.ContainmentMode())
	require.NotNil(t, agent.providerAuth, "provider auth must be enabled for the canary")

	// Provider methods are runtime-discovered. Use a real short-lived contained
	// server for that production read, then release the UID fence before the
	// authorize leg starts its separately contained broker.
	catalogRoot := filepath.Join(browserCanaryScratch, "catalog-runtime")
	catalogClient, err := runtimeStartServer(ctx, opencode.StartOptions{
		Root:                     catalogRoot,
		ControlRoot:              filepath.Join(browserCanaryScratch, "catalog-control"),
		ScratchParent:            browserCanaryScratch,
		ContainmentScratchParent: browserCanaryScratch,
		ExecutablePath:           browserCanaryNative,
		ProcessIsolation:         openCodeProcessIsolation(&isolation),
		Pure:                     true,
		MinVersion:               minNativeVersion,
		HealthTimeout:            60 * time.Second,
		Logger:                   slog.New(slog.DiscardHandler),
		HandoffXDG:               true,
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

	// Current OpenCode launches the Snowflake authorization URL and then returns
	// a loopback completion variant. Production rejects that variant and fences
	// the broker; the container's external trace proves the preceding launch.
	require.Eventually(t, func() bool {
		matches, globErr := filepath.Glob(filepath.Join(browserCanaryScratch, "acp-go-opencode-browser-shim-*"))
		return globErr == nil && len(matches) == 0
	}, 15*time.Second, 25*time.Millisecond, "terminal provider-auth rejection left its browser shim behind")
}

func browserCanaryParams(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

func browserCanaryOwnedDir(t *testing.T, path string, mode os.FileMode, targetOwned bool) string {
	t.Helper()
	require.NoError(t, os.Mkdir(path, mode))
	if targetOwned {
		require.NoError(t, os.Chown(path, browserCanaryUID, browserCanaryGID))
	}
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(path)) })

	return path
}
