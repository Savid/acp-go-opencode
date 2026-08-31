package opencode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
)

func TestNativeVersionReportsHealthVersion(t *testing.T) {
	server := &openCodeServer{nativeVersion: "1.18.3"}
	require.Equal(t, "1.18.3", server.NativeVersion())
}

func TestOpenCodeServerAccessorsAndNilAssistantError(t *testing.T) {
	server := &openCodeServer{
		eventStream:   make(chan EventStreamItem),
		runtimeExited: make(chan struct{}),
		xdg:           XDGDirs{Root: "/runtime"},
	}
	require.NotNil(t, server.EventStream())
	require.NotNil(t, server.RuntimeExited())
	require.Equal(t, server.xdg, server.XDGDirs())
	require.Equal(t, &AssistantError{}, AssistantErrorFromNativeError(nil))
}

func TestStartServerRejectsIncompleteTreeAuthorityBeforeMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	_, err := StartServer(context.Background(), StartOptions{
		Root:        root,
		PrepareTree: func(context.Context, string) error { return nil },
	})
	require.ErrorContains(t, err, "native tree authority is incomplete")
	require.NoDirExists(t, root)
}

func TestScopeRegisterFailureCreatePolicyAndDirectoryQueryClone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/mcp":
			writer.WriteHeader(http.StatusInternalServerError)
		case routeSession:
			writeJSON(t, writer, map[string]any{"id": "created"})
		case "/query":
			require.Equal(t, "/scope", request.URL.Query().Get("directory"))
			require.Equal(t, "preserved", request.URL.Query().Get("value"))
			writeJSON(t, writer, map[string]any{"ok": true})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := &openCodeServer{
		httpClient: server.Client(), baseURL: server.URL, closed: make(chan struct{}), directory: "/scope",
		sessionCarrierBroker: testSessionCarrierBroker(),
	}
	_, err := client.Scope(context.Background(), ScopeOptions{
		Directory: "/scope", MCPServers: []MCPServerConfig{{Name: "bad", URL: "https://bad"}},
	})
	require.ErrorContains(t, err, "register directory MCP")

	created, err := client.CreateSessionWithPolicy(context.Background(), "title", []PermissionRule{{
		Permission: "*", Pattern: "*", Action: "allow",
	}})
	require.NoError(t, err)
	require.Equal(t, "created", created.ID)

	query := url.Values{"value": []string{"preserved"}}
	var result map[string]any
	require.NoError(t, client.doJSON(context.Background(), http.MethodGet, "/query", query, nil, &result))
	require.Empty(t, query.Get("directory"), "caller query must not be mutated")
}

func TestRuntimeConfigForbiddenSeedsAndDeepMerge(t *testing.T) {
	for _, field := range []string{fieldPermission, fieldMCP} {
		t.Run(field, func(t *testing.T) {
			dirs, err := CreateRuntimeXDGDirs(t.TempDir())
			require.NoError(t, err)
			_, err = materializeOpenCodeRuntimeConfig(dirs, map[string]string{
				openCodeConfigFileName: `{"` + field + `":{}}`,
			}, "")
			require.ErrorContains(t, err, "session-scoped")
		})
	}

	merged := deepMergeJSON(
		map[string]any{"nested": map[string]any{"left": true}, "scalar": "base"},
		map[string]any{"nested": map[string]any{"right": true}, "scalar": map[string]any{"override": true}},
	)
	nested, ok := merged["nested"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, nested["left"])
	require.Equal(t, true, nested["right"])
	require.IsType(t, map[string]any{}, merged["scalar"])
}

func TestStartServerOrdinaryHomeLockFailurePrecedesLaunch(t *testing.T) {
	restoreOpenCodeClientSeams(t)

	want := errors.New("writable home is already claimed")
	openCodeAcquireHomeLock = func(string) (*homelock.Lock, error) { return nil, want }
	launched := false

	_, err := StartServer(context.Background(), StartOptions{
		Root: t.TempDir(), Pure: true,
		StartProcess: func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
			launched = true

			return ProcessHandle{}, errors.New("unexpected launch")
		},
	})
	require.ErrorIs(t, err, want)
	require.False(t, launched)
}

func TestStartServerRejectsAnUnbuildableNativeEnvironment(t *testing.T) {
	_, err := StartServer(context.Background(), StartOptions{
		Root: t.TempDir(), Pure: true,
		Env: map[string]string{"BAD=KEY": "x"},
	})
	require.ErrorContains(t, err, "invalid environment entry")
}

func TestReclaimPreparedTreesContinuesAfterFailure(t *testing.T) {
	want := errors.New("busy")
	var reclaimed, cleaned []string
	trees := []preparedNativeTree{
		{path: "runtime", cleanup: func() error {
			cleaned = append(cleaned, "runtime")

			return nil
		}},
		{path: "carrier", cleanup: func() error {
			cleaned = append(cleaned, "carrier")

			return nil
		}},
	}
	err := reclaimPreparedTrees(context.Background(), trees, func(_ context.Context, path string) error {
		reclaimed = append(reclaimed, path)
		if path == "runtime" {
			return want
		}

		return nil
	})
	require.ErrorIs(t, err, want)
	require.Equal(t, []string{"carrier", "runtime"}, reclaimed)
	require.Equal(t, []string{"carrier"}, cleaned)
}
