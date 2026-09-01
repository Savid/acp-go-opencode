package opencode

import (
	"context"
	"errors"
	"net"
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

func TestNativeBoundaryMarkersAndRuntimeStateEdges(t *testing.T) {
	want := errors.New("native boundary")
	for _, marked := range []struct {
		err    error
		marker error
	}{
		{MarkPrepareOpaque(want), errPrepareOpaque},
		{MarkTreeReclaimPending(want), errTreeReclaimPending},
		{MarkProcessStartSettled(want), errProcessStartSettled},
	} {
		require.Equal(t, want.Error(), marked.err.Error())
		require.ErrorIs(t, marked.err, want)
		require.ErrorIs(t, marked.err, marked.marker)
	}
	require.True(t, NativeCleanupRetained(retainNativeCleanup(want)))
	require.False(t, NativeCleanupRetained(want))

	require.NoError(t, (&openCodeServer{}).Shutdown(t.Context()))
	require.False(t, (&openCodeServer{}).RuntimeRevoked())
	pending := &openCodeServer{settlement: &processSettlement{done: make(chan struct{})}}
	require.False(t, pending.RuntimeRevoked())
	done := make(chan struct{})
	close(done)
	require.True(t, (&openCodeServer{settlement: &processSettlement{
		done: done, result: ProcessOutcome{Revoked: true},
	}}).RuntimeRevoked())
}

func TestRuntimeXDGAndRetentionEdges(t *testing.T) {
	_, err := resolveRuntimeXDG(StartOptions{})
	require.ErrorContains(t, err, "runtime root is required")

	scratch := t.TempDir()
	dirs, err := resolveRuntimeXDG(StartOptions{ScratchParent: scratch})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(scratch, "acp-go-opencode"), dirs.Root)

	retainPreparedTrees([]preparedNativeTree{{path: "ignored"}}, nil)
	var retained string
	retainPreparedTrees([]preparedNativeTree{{path: "runtime", reclaimed: true}}, func(path string, reclaimed bool, _ func() error) {
		retained = path
		require.True(t, reclaimed)
	})
	require.Equal(t, "runtime", retained)
}

func TestProcessEnvironmentAndSettlementEdges(t *testing.T) {
	originalEnviron := processEnviron
	processEnviron = func() []string {
		return []string{"PATH=/captured", privateEnvironmentPrefix + "TOKEN=secret"}
	}
	t.Cleanup(func() { processEnviron = originalEnviron })

	environment, err := buildProcessEnvironmentFrom(nil)
	require.NoError(t, err)
	require.Equal(t, "/captured", environment[pathEnv])
	require.NotContains(t, environment, privateEnvironmentPrefix+"TOKEN")
	_, err = buildProcessEnvironmentFrom(map[string]string{"BAD=KEY": "value"})
	require.Error(t, err)
	_, err = buildProcessEnvironmentFrom(map[string]string{}, map[string]string{"BAD=KEY": "value"})
	require.Error(t, err)
	require.NotContains(t, withoutManagedRootOverrides(map[string]string{"home": "/private", "KEEP": "yes"}), "home")

	var absent *processSettlement
	_, err = absent.wait(t.Context())
	require.ErrorContains(t, err, "settlement is unavailable")

	release := make(chan struct{})
	settlement := newProcessSettlement(ProcessHandle{Await: func(context.Context) (ProcessOutcome, error) {
		<-release

		return ProcessOutcome{ExitCode: 7}, nil
	}})
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = settlement.wait(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	<-settlement.done
	result, err := settlement.wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
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

func TestStartServerOwnershipFailureEdges(t *testing.T) {
	want := errors.New("native start failed")
	authority := func(options StartOptions) StartOptions {
		options.PrepareTree = func(context.Context, string) error { return nil }
		options.ReclaimTree = func(context.Context, string) error { return nil }

		return options
	}

	t.Run("opaque prepare retained", func(t *testing.T) {
		var retained string
		options := authority(StartOptions{ExistingXDG: testXDGDirs(t), Pure: true})
		options.PrepareTree = func(context.Context, string) error { return MarkPrepareOpaque(want) }
		options.RetainTree = func(path string, _ bool, _ func() error) { retained = path }
		_, err := StartServer(t.Context(), options)
		require.ErrorIs(t, err, want)
		require.True(t, NativeCleanupRetained(err))
		require.Empty(t, retained, "opaque preparation never transferred the tree into the prepared set")
	})

	t.Run("ordinary prepare cleans carrier", func(t *testing.T) {
		options := authority(StartOptions{ExistingXDG: testXDGDirs(t)})
		options.PrepareTree = func(context.Context, string) error { return want }
		_, err := StartServer(t.Context(), options)
		require.ErrorIs(t, err, want)
		require.False(t, NativeCleanupRetained(err))
	})

	t.Run("unsettled start retained", func(t *testing.T) {
		var retained string
		options := authority(StartOptions{ExistingXDG: testXDGDirs(t), Pure: true})
		options.StartProcess = func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
			return ProcessHandle{}, want
		}
		options.RetainTree = func(path string, _ bool, _ func() error) { retained = path }
		_, err := StartServer(t.Context(), options)
		require.ErrorIs(t, err, want)
		require.True(t, NativeCleanupRetained(err))
		require.NotEmpty(t, retained)
	})

	t.Run("settled start reclaimed", func(t *testing.T) {
		options := authority(StartOptions{ExistingXDG: testXDGDirs(t), Pure: true})
		options.StartProcess = func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
			return ProcessHandle{}, MarkProcessStartSettled(want)
		}
		_, err := StartServer(t.Context(), options)
		require.ErrorIs(t, err, want)
		require.False(t, NativeCleanupRetained(err))
	})

	t.Run("incomplete process retained", func(t *testing.T) {
		options := authority(StartOptions{ExistingXDG: testXDGDirs(t), Pure: true})
		options.StartProcess = func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
			return ProcessHandle{}, nil
		}
		_, err := StartServer(t.Context(), options)
		require.ErrorContains(t, err, "process handle is incomplete")
		require.True(t, NativeCleanupRetained(err))
	})
}

func TestStartServerMaterializationFailureEdges(t *testing.T) {
	t.Run("generated root cleanup includes carrier", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeListen = func(string, string) (net.Listener, error) { return nil, errors.New("listen failed") }
		root := filepath.Join(t.TempDir(), "generated")
		_, err := StartServer(t.Context(), StartOptions{Root: root, RemoveRoot: true})
		require.ErrorContains(t, err, "listen failed")
		require.NoDirExists(t, root)
	})

	t.Run("prepared carrier cleanup", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeListen = func(string, string) (net.Listener, error) { return nil, errors.New("listen failed") }
		_, err := StartServer(t.Context(), StartOptions{ExistingXDG: testXDGDirs(t)})
		require.ErrorContains(t, err, "listen failed")
	})

	t.Run("control root", func(t *testing.T) {
		_, err := StartServer(t.Context(), StartOptions{
			ExistingXDG: testXDGDirs(t), Pure: true,
			ControlRoot: filepath.Join(t.TempDir(), string([]byte{0})),
		})
		require.Error(t, err)
	})

	t.Run("password entropy", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeRandReader = errorReader{err: errors.New("entropy failed")}
		_, err := StartServer(t.Context(), StartOptions{ExistingXDG: testXDGDirs(t), Pure: true})
		require.ErrorContains(t, err, "entropy failed")
	})

	t.Run("native environment unavailable", func(t *testing.T) {
		_, err := StartServer(t.Context(), StartOptions{
			ExistingXDG: testXDGDirs(t), Pure: true,
			NativeEnvironment: func() map[string]string { return nil },
		})
		require.ErrorContains(t, err, "native environment is unavailable")
	})
}
