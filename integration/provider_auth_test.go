//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
)

const (
	envRunAttended = "ACP_GO_OPENCODE_RUN_ATTENDED"
	envRunKeystore = "ACP_GO_OPENCODE_RUN_KEYSTORE"
)

// requireRunAttended gates the tier that needs a human at a provider. It fails
// rather than skips once the gate is set: a silently green attended suite is
// worse than a red one.
func requireRunAttended(t *testing.T) {
	t.Helper()
	requireRunIntegration(t)

	if os.Getenv(envRunAttended) != "1" {
		t.Skipf("set %s=1 to run attended provider-auth tests", envRunAttended)
	}
}

func requireRunKeystore(t *testing.T) {
	t.Helper()
	requireRunIntegration(t)

	if os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 to run credential-residence tests", envRunKeystore)
	}
}

// providerAuthAgent starts a live adapter with both provider-auth
// preconditions configured and returns the connection plus the ledger root.
func providerAuthAgent(t *testing.T, ctx context.Context) (*acp.ClientSideConnection, *liveAgent, string, string, string) {
	t.Helper()

	scratch := t.TempDir()
	home := t.TempDir()
	authRoot := t.TempDir()

	agent := startLiveAgent(t, ctx, scratch, "-home", home, "-provider-auth-root", authRoot)
	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)

	response, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}

	vendor, ok := response.AgentCapabilities.Meta["opencode"].(map[string]any)
	if !ok {
		t.Fatalf("initialize carried no vendor capability: %#v", response.AgentCapabilities.Meta)
	}

	if _, ok := vendor["providerAuth"]; !ok {
		t.Fatalf("provider auth was not advertised with a root and a home configured: %#v", vendor)
	}

	return conn, agent, home, authRoot, scratch
}

func newProviderAuthSession(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection) acp.SessionId {
	t.Helper()

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}

	return session.SessionId
}

func callAuthLeg(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection, method string, params any, out any) error {
	t.Helper()

	raw, err := conn.CallExtension(ctx, method, params)
	if err != nil {
		return err
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s result: %v", method, err)
	}

	return nil
}

type authMethodsWire struct {
	Providers map[string][]struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Label string `json:"label"`
	} `json:"providers"`
	Generation string `json:"generation"`
}

type authAuthorizeWire struct {
	Interaction   string `json:"interaction"`
	URL           string `json:"url"`
	Message       string `json:"message"`
	UserCode      string `json:"userCode"`
	CallbackInput string `json:"callbackInput"`
	FlowID        string `json:"flowId"`
	FlowExpiresAt int64  `json:"flowExpiresAt"`
}

type authStatusWire struct {
	FlowID string `json:"flowId"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// TestProviderAuthCatalogOmitsXAILoopbackMethod pins the live OpenCode method
// split that the catalog filter depends on. The browser flow is native slot 0
// and must be absent; the headless and API-key flows keep native slots 1 and 2.
func TestProviderAuthCatalogOmitsXAILoopbackMethod(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, agent, _, _, _ := providerAuthAgent(t, ctx)
	defer agent.close()

	sessionID := newProviderAuthSession(t, ctx, conn)

	var methods authMethodsWire
	if err := callAuthLeg(t, ctx, conn, "_opencode/auth/methods", map[string]any{"sessionId": string(sessionID)}, &methods); err != nil {
		t.Fatalf("_opencode/auth/methods: %v", err)
	}

	entries, ok := methods.Providers["xai"]
	if !ok {
		t.Fatalf("catalog carried no %q provider: %#v", "xai", methods.Providers)
	}

	if len(entries) != 2 {
		t.Fatalf("xAI methods = %#v, want exactly the headless and API-key methods", entries)
	}

	if got := entries[0]; got.ID != "1" || got.Type != "oauth" || got.Label != "xAI Grok OAuth (Headless / Remote / VPS)" {
		t.Fatalf("xAI method 1 = %#v, want the headless OAuth method", got)
	}

	if got := entries[1]; got.ID != "2" || got.Type != "api" || got.Label != "Manually enter API Key" {
		t.Fatalf("xAI method 2 = %#v, want the API-key method", got)
	}
}

// TestAttendedProviderAuthDeviceFlowCompletes drives one real device login end
// to end. The operator names the provider and the method id, opens the relayed
// URL, and approves at the provider before the flow's deadline.
func TestAttendedProviderAuthDeviceFlowCompletes(t *testing.T) {
	requireRunAttended(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	conn, agent, home, authRoot, scratch := providerAuthAgent(t, ctx)
	defer agent.close()

	sessionID := newProviderAuthSession(t, ctx, conn)

	var methods authMethodsWire
	if err := callAuthLeg(t, ctx, conn, "_opencode/auth/methods", map[string]any{"sessionId": string(sessionID)}, &methods); err != nil {
		t.Fatalf("_opencode/auth/methods: %v", err)
	}

	providerID, methodID := oauthMethod(t, methods)
	t.Logf("driving the %s login method %s", providerID, methodID)

	var authorization authAuthorizeWire

	err := callAuthLeg(t, ctx, conn, "_opencode/auth/authorize", map[string]any{
		"sessionId":          string(sessionID),
		"providerId":         providerID,
		"connectionId":       "attended-connection",
		"methodsGeneration":  methods.Generation,
		"method":             methodID,
		"authorizeRequestId": "attended-request",
	}, &authorization)
	if err != nil {
		t.Fatalf("_opencode/auth/authorize: %v", err)
	}

	if !strings.HasPrefix(authorization.URL, "https://") {
		t.Fatalf("authorize relayed no https url: %+v", authorization)
	}
	if authorization.Interaction != "wait" {
		t.Fatalf("authorize interaction = %q, want wait", authorization.Interaction)
	}

	t.Logf("approve this login before %s:\n  url:  %s\n  code: %s\n  %s",
		time.UnixMilli(authorization.FlowExpiresAt).Format(time.RFC3339),
		authorization.URL, authorization.UserCode, authorization.Message)

	var status authStatusWire
	for {
		if err := callAuthLeg(t, ctx, conn, "_opencode/auth/status", map[string]any{
			"sessionId":  string(sessionID),
			"providerId": providerID,
			"flowId":     authorization.FlowID,
		}, &status); err != nil {
			t.Fatalf("_opencode/auth/status: %v", err)
		}

		if status.State != "pending" {
			break
		}

		select {
		case <-ctx.Done():
			t.Fatalf("wait for provider approval: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}

	if status.State != "authenticated" {
		t.Fatalf("state = %q (%q), want authenticated", status.State, status.Reason)
	}

	assertCredentialResident(t, home, providerID)
	assertLedgerIsValuesFree(t, authRoot, authorization)
	assertNoBrokerHomeSurvives(t, scratch)
}

// TestKeystoreProviderAuthResidence asserts where a completed login is
// resident. OpenCode has no keystore branch on any platform, so the assertion
// is that the durable runtime store is authoritative in every configuration.
func TestKeystoreProviderAuthResidence(t *testing.T) {
	requireRunKeystore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	conn, agent, home, authRoot, _ := providerAuthAgent(t, ctx)
	defer agent.close()

	sessionID := newProviderAuthSession(t, ctx, conn)

	var methods authMethodsWire
	if err := callAuthLeg(t, ctx, conn, "_opencode/auth/methods", map[string]any{"sessionId": string(sessionID)}, &methods); err != nil {
		t.Fatalf("_opencode/auth/methods: %v", err)
	}

	providerID, methodID := secretMethod(t, methods)

	var authorization authAuthorizeWire

	err := callAuthLeg(t, ctx, conn, "_opencode/auth/authorize", map[string]any{
		"sessionId":          string(sessionID),
		"providerId":         providerID,
		"connectionId":       "keystore-connection",
		"methodsGeneration":  methods.Generation,
		"method":             methodID,
		"authorizeRequestId": "keystore-request",
	}, &authorization)
	if err != nil {
		t.Fatalf("_opencode/auth/authorize: %v", err)
	}

	if authorization.Interaction != "secret" {
		t.Fatalf("interaction = %q, want secret", authorization.Interaction)
	}

	const canary = "canary-not-a-real-key"

	if err := callAuthLeg(t, ctx, conn, "_opencode/auth/callback", map[string]any{
		"sessionId":  string(sessionID),
		"providerId": providerID,
		"method":     methodID,
		"flowId":     authorization.FlowID,
		"input":      canary,
	}, nil); err != nil {
		t.Fatalf("_opencode/auth/callback: %v", err)
	}

	store := assertCredentialResident(t, home, providerID)
	if !strings.Contains(store, canary) {
		t.Fatal("the canary is not in the durable runtime store")
	}

	assertLedgerIsValuesFree(t, authRoot, authorization)

	if err := callAuthLeg(t, ctx, conn, "_opencode/auth/disconnect", map[string]any{
		"sessionId":         string(sessionID),
		"providerId":        providerID,
		"connectionId":      "keystore-connection",
		"bindingGeneration": 1,
	}, nil); err != nil {
		t.Fatalf("_opencode/auth/disconnect: %v", err)
	}

	if remaining := readAuthStore(t, home); strings.Contains(remaining, canary) {
		t.Fatal("disconnect left the canary resident")
	}
}

// oauthMethod picks the login the attended run drives. The catalog is native
// and its ten special-method providers are not a stable set, so the selection
// is made from what this pin actually enumerates.
func oauthMethod(t *testing.T, methods authMethodsWire) (string, string) {
	t.Helper()

	for providerID, entries := range methods.Providers {
		for _, entry := range entries {
			if entry.Type == "oauth" {
				return providerID, entry.ID
			}
		}
	}

	t.Fatal("the catalog offers no oauth method")

	return "", ""
}

func secretMethod(t *testing.T, methods authMethodsWire) (string, string) {
	t.Helper()

	for providerID, entries := range methods.Providers {
		for _, entry := range entries {
			if entry.Type == "api" {
				return providerID, entry.ID
			}
		}
	}

	t.Fatal("the catalog offers no api-key method")

	return "", ""
}

func readAuthStore(t *testing.T, home string) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(home, "data", "opencode", "auth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}

		t.Fatalf("read auth store: %v", err)
	}

	return string(contents)
}

func assertCredentialResident(t *testing.T, home string, providerID string) string {
	t.Helper()

	store := readAuthStore(t, home)
	if !strings.Contains(store, fmt.Sprintf("%q", providerID)) {
		t.Fatalf("provider %q is not resident in the durable runtime store", providerID)
	}

	return store
}

// assertLedgerIsValuesFree walks every ledger entry and fails on any presented
// value: the ledger records slot identity and provenance only.
func assertLedgerIsValuesFree(t *testing.T, authRoot string, authorization authAuthorizeWire) {
	t.Helper()

	banned := []string{authorization.URL, authorization.Message}
	if authorization.UserCode != "" {
		banned = append(banned, authorization.UserCode)
	}

	err := filepath.WalkDir(authRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, value := range banned {
			if value != "" && strings.Contains(string(contents), value) {
				t.Fatalf("ledger entry %s carries presented value %q", path, value)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk ledger: %v", err)
	}
}

func assertNoBrokerHomeSurvives(t *testing.T, scratchParent string) {
	t.Helper()

	entries, err := os.ReadDir(scratchParent)
	if err != nil {
		t.Fatalf("read scratch parent: %v", err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "acp-go-opencode-auth-broker-") {
			t.Fatalf("broker home %s survived its flow", entry.Name())
		}
	}
}
