package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScopedRuntimeMCPAndSyncMethods(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/mcp":
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			name, _ := body["name"].(string)
			writeJSON(t, writer, map[string]any{name: map[string]any{"status": "connected"}})
		case request.Method == http.MethodDelete && request.URL.Path[:5] == "/mcp/":
			mu.Lock()
			deleted = append(deleted, request.URL.Path)
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == routeEvent:
			require.Equal(t, "/repo", request.URL.Query().Get("directory"))
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n"))
		case request.Method == http.MethodPost && request.URL.Path == "/sync/history":
			writeJSON(t, writer, []map[string]any{{"id": "event-1", "sequence": 1, "type": "message.updated", "properties": map[string]any{}}})
		case request.Method == http.MethodPost && request.URL.Path == "/sync/replay":
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	runtimeExited := make(chan struct{})
	client := &openCodeServer{
		httpClient: server.Client(), baseURL: server.URL, events: make(chan Event, 1), errs: make(chan error, 1),
		closed: make(chan struct{}), runtimeOnce: &sync.Once{}, runtimeClosed: make(chan struct{}), runtimeExited: runtimeExited,
	}
	require.Equal(t, (<-chan struct{})(runtimeExited), client.RuntimeExited())
	require.NoError(t, client.Close(context.Background()), "root Close is intentionally a no-op")

	servers := []MCPServerConfig{
		{Name: "remote", URL: "https://mcp.test", Headers: map[string]string{"Authorization": "secret"}},
		{Name: "local", Command: []string{"tool", "serve"}, Env: map[string]string{"TOKEN": "secret"}},
	}
	scopedClient, err := client.Scope(context.Background(), ScopeOptions{Directory: "/repo", MCPServers: servers})
	require.NoError(t, err)
	scope, ok := scopedClient.(*openCodeServer)
	require.True(t, ok)
	require.Equal(t, "/repo", scope.directory)
	require.NoError(t, scope.Close(context.Background()))
	require.NoError(t, scope.Close(context.Background()))
	mu.Lock()
	require.Equal(t, []string{"/mcp/local", "/mcp/remote"}, deleted)
	mu.Unlock()

	events, err := client.SyncHistory(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Error(t, client.SyncReplay(context.Background(), "", nil))
	require.NoError(t, client.SyncReplay(context.Background(), "/repo", []SyncReplayEvent{{Type: "message.updated"}}))

	block := openCodeMCPConfigBlock(servers)
	remote, ok := block["remote"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "remote", remote["type"])
	local, ok := block["local"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "local", local["type"])
	require.Nil(t, openCodeMCPConfigBlock(nil))
}

func TestScopeAndMCPFailureShapes(t *testing.T) {
	client := &openCodeServer{}
	_, err := client.Scope(context.Background(), ScopeOptions{})
	require.ErrorContains(t, err, "directory is required")

	t.Run("register transport failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL, closed: make(chan struct{})}
		err := client.registerMCP(context.Background(), []MCPServerConfig{{Name: "broken", URL: "https://mcp.test"}})
		require.ErrorContains(t, err, "register directory MCP")
	})

	t.Run("register disconnected", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost {
				writeJSON(t, writer, map[string]any{"broken": map[string]any{"status": "failed"}})

				return
			}
			writer.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL, closed: make(chan struct{})}
		err := client.registerMCP(context.Background(), []MCPServerConfig{{Name: "broken", URL: "https://mcp.test"}})
		require.ErrorContains(t, err, "did not connect")
	})

	t.Run("unregister joins failures", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL, mcpNames: []string{"one", "two"}}
		require.Error(t, client.unregisterMCP(context.Background()))
		require.Empty(t, client.mcpNames)
	})

	t.Run("scope wrong first event", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == routeEvent {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = writer.Write([]byte("data: {\"type\":\"wrong\",\"properties\":{}}\n\n"))

				return
			}
			writer.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL, runtimeOnce: &sync.Once{}, runtimeClosed: make(chan struct{})}
		_, err := client.Scope(context.Background(), ScopeOptions{Directory: "/repo"})
		require.ErrorContains(t, err, "first directory-scoped event")
	})

	t.Run("scope stream error", func(t *testing.T) {
		client := &openCodeServer{
			httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			})},
			baseURL: "http://opencode.test", runtimeOnce: &sync.Once{}, runtimeClosed: make(chan struct{}),
		}
		_, err := client.Scope(context.Background(), ScopeOptions{Directory: "/repo"})
		require.ErrorContains(t, err, "directory event stream failed")
	})

	t.Run("scope context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := &openCodeServer{
			httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()

				return nil, request.Context().Err()
			})},
			baseURL: "http://opencode.test", runtimeOnce: &sync.Once{}, runtimeClosed: make(chan struct{}),
		}
		_, err := client.Scope(ctx, ScopeOptions{Directory: "/repo"})
		require.ErrorIs(t, err, context.Canceled)
	})
}
