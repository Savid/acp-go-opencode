package opencode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fixedTCPListener struct{ port int }

func (listener fixedTCPListener) Accept() (net.Conn, error) { return nil, errors.New("not supported") }
func (listener fixedTCPListener) Close() error              { return nil }
func (listener fixedTCPListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: listener.port}
}

func TestNativeVersionReportsHealthVersion(t *testing.T) {
	server := &openCodeServer{nativeVersion: "1.18.3"}
	require.Equal(t, "1.18.3", server.NativeVersion())
}

func TestStartServerHappyPathWithoutPrivilegedProcessLaunch(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)
	require.NotNil(t, openCodeHTTPClient())
	openCodeListen = func(string, string) (net.Listener, error) { return fixedTCPListener{port: 32123}, nil }
	openCodeApplyCredential = func(*exec.Cmd, *ProcessIsolation) error { return nil }
	openCodeCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/usr/bin/true")
	}
	openCodeStartProcess = startOpenCodeProcess
	supervisorReleaseIndependentWaiter = func(cmd *exec.Cmd, waiter *supervisorWaiter) (int, error) {
		waiter.start()

		return cmd.Process.Pid, nil
	}
	openCodeHTTPClient = func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case routeGlobalHealth:
				return performanceJSONResponse(map[string]any{"healthy": true, "version": "1.18.3"}), nil
			case routeDoc:
				return performanceJSONResponse(fullOpenCodeDoc()), nil
			case routeEvent:
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")),
				}, nil
			default:
				return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Header: make(http.Header), Body: http.NoBody}, nil
			}
		})}
	}

	shim, err := NewBrowserShim(testTraversableTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = shim.Remove() })
	client, err := StartServer(context.Background(), StartOptions{
		Root:             testGeneratedTempDir(t),
		ExecutablePath:   "/usr/bin/true",
		ProcessIsolation: testProcessIsolation(),
		skipSupervisor:   true,
		HandoffXDG:       true,
		BrowserShim:      shim,
		Pure:             true,
		QuestionTool:     true,
		LogLevel:         "DEBUG",
		ExtraPathDirs:    []string{"/usr/bin"},
		Logger:           slog.New(slog.DiscardHandler),
		MinVersion:       "1.18.3",
	})
	require.NoError(t, err)
	server, ok := client.(*openCodeServer)
	require.True(t, ok)
	require.NotNil(t, server.Events())
	require.NotNil(t, server.EventErrors())
	require.Equal(t, server.xdg, server.XDGDirs())
	require.NoError(t, server.Shutdown(context.Background()))
}

func TestStartServerEarlyContainmentAndCredentialFailures(t *testing.T) {
	base := func(t *testing.T) StartOptions {
		t.Helper()

		return StartOptions{ExistingXDG: testXDGDirs(t), ExecutablePath: "/usr/bin/true", ProcessIsolation: testProcessIsolation(), skipSupervisor: true}
	}

	t.Run("control root", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		options := base(t)
		options.ControlRoot = t.TempDir()
		require.NoError(t, os.Chmod(options.ControlRoot, 0o755))
		_, err := StartServer(context.Background(), options)
		require.ErrorContains(t, err, "control root")
	})

	t.Run("xdg handoff", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		options := base(t)
		options.HandoffXDG = true
		options.ExistingXDG = testUnhandoffableXDGDirs(t)
		options.ProcessIsolation = testForeignProcessIsolation()
		_, err := StartServer(context.Background(), options)
		require.ErrorContains(t, err, generatedTreeHandoffRefusal)
	})

	t.Run("browser handoff", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		options := base(t)
		shim, err := NewBrowserShim(t.TempDir())
		require.NoError(t, err)
		options.BrowserShim = shim
		options.ProcessIsolation = testForeignProcessIsolation()
		_, err = StartServer(context.Background(), options)
		require.ErrorContains(t, err, generatedTreeHandoffRefusal)
	})

	t.Run("credential", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		want := errors.New("credential")
		openCodeApplyCredential = func(*exec.Cmd, *ProcessIsolation) error { return want }
		_, err := StartServer(context.Background(), base(t))
		require.ErrorIs(t, err, want)
	})
}

func TestStartServerRootAndSupervisorSetupFailures(t *testing.T) {
	_, err := StartServer(context.Background(), StartOptions{})
	require.ErrorContains(t, err, "runtime root is required")

	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)
	supervisorExecutable = func() (string, error) { return "", errors.New("supervisor lookup failed") }
	_, err = StartServer(context.Background(), StartOptions{
		Root: testGeneratedTempDir(t), ExecutablePath: "/usr/bin/true", ProcessIsolation: testProcessIsolation(),
	})
	require.ErrorContains(t, err, "supervisor lookup failed")

	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	openCodeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		command := exec.Command("/bin/true")
		command.Stdout = os.Stdout

		return command
	}
	_, err = StartServer(context.Background(), StartOptions{
		Root: root, ExecutablePath: "/usr/bin/true", skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
	})
	require.Error(t, err)

	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)
	root = t.TempDir()
	openCodeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		command := exec.Command(filepath.Join(root, "missing"))

		return command
	}
	_, err = StartServer(context.Background(), StartOptions{
		ScratchParent: testTraversableTempDir(t), ExecutablePath: "/usr/bin/true",
		skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
	})
	require.Error(t, err)
}

func TestNativeOwnedXDGIsNeverTouchedBeforeNativeLaunch(t *testing.T) {
	root := t.TempDir()
	control := t.TempDir()
	require.NoError(t, os.Chmod(control, 0o700))
	_, err := StartServer(context.Background(), StartOptions{
		ExistingXDG:      RuntimeXDGDirs(root),
		NativeOwnedXDG:   true,
		ControlRoot:      control,
		ExecutablePath:   filepath.Join(t.TempDir(), "missing-opencode"),
		ProcessIsolation: testProcessIsolation(),
		SeedFiles:        map[string]string{"opencode.json": `{"provider":{}}`},
		skipSupervisor:   true,
	})
	require.ErrorContains(t, err, "find OpenCode executable")
	entries, readErr := os.ReadDir(root)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestNativeOwnedXDGRejectsRedirectedLayoutAndSeeds(t *testing.T) {
	root := t.TempDir()
	dirs := RuntimeXDGDirs(root)
	dirs.Config = t.TempDir()
	_, err := StartServer(context.Background(), StartOptions{
		ExistingXDG:    dirs,
		NativeOwnedXDG: true,
	})
	require.ErrorContains(t, err, "must match their runtime root")

	control := t.TempDir()
	require.NoError(t, os.Chmod(control, 0o700))
	_, err = StartServer(context.Background(), StartOptions{
		ExistingXDG:    RuntimeXDGDirs(root),
		NativeOwnedXDG: true,
		ControlRoot:    control,
		SeedFiles:      map[string]string{"provider.json": `{}`},
	})
	require.ErrorContains(t, err, "seed file \"provider.json\" is unsupported")
}

func TestNativeOwnedXDGCanBeResolvedFromRootWithoutFilesystemWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	dirs, err := resolveRuntimeXDG(StartOptions{Root: root, NativeOwnedXDG: true})
	require.NoError(t, err)
	require.Equal(t, RuntimeXDGDirs(root), dirs)
	require.NoDirExists(t, root)
}

func TestOpenCodeServerAccessorsAndNilAssistantError(t *testing.T) {
	server := &openCodeServer{events: make(chan Event), errs: make(chan error), xdg: XDGDirs{Root: "/runtime"}}
	require.NotNil(t, server.Events())
	require.NotNil(t, server.EventErrors())
	require.Equal(t, server.xdg, server.XDGDirs())
	require.Equal(t, &AssistantError{}, AssistantErrorFromNativeError(nil))
}

func TestWaitForRuntimeShutdownTimeoutCancellationAndQuarantine(t *testing.T) {
	never := func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	ready := func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()

		return ch
	}

	done := make(chan error)
	go func() {
		time.Sleep(5 * time.Millisecond)
		done <- errors.New("exit")
	}()
	kills := 0
	waited, quarantined, err := waitForOpenCodeRuntimeShutdown(
		&openCodeServer{log: slog.New(slog.DiscardHandler)}, nil, done, context.Background(), time.Millisecond,
		func(*os.Process, int) error {
			kills++

			return nil
		}, ready,
	)
	require.True(t, waited)
	require.False(t, quarantined)
	require.ErrorContains(t, err, "did not exit")
	require.Equal(t, 1, kills)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waited, quarantined, err = waitForOpenCodeRuntimeShutdown(
		&openCodeServer{}, nil, make(chan error), ctx, time.Hour,
		func(*os.Process, int) error {
			kills++

			return nil
		}, never,
	)
	require.False(t, waited)
	require.False(t, quarantined)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Equal(t, 2, kills)

	quarantine := filepath.Join(t.TempDir(), "quarantine")
	require.NoError(t, os.WriteFile(quarantine, []byte("state"), 0o600))
	proofCtx, proofCancel := context.WithTimeout(context.Background(), time.Second)
	defer proofCancel()
	waited, quarantined, err = waitForOpenCodeRuntimeShutdown(
		&openCodeServer{supervisor: &supervisorProof{quarantine: quarantine}}, nil, make(chan error), proofCtx, time.Hour,
		func(*os.Process, int) error { return nil }, never,
	)
	require.False(t, waited)
	require.True(t, quarantined)
	require.ErrorContains(t, err, "quarantine")

	notDirectory := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	waited, quarantined, err = waitForOpenCodeRuntimeShutdown(
		&openCodeServer{supervisor: &supervisorProof{quarantine: filepath.Join(notDirectory, "child")}}, nil,
		make(chan error), proofCtx, time.Hour, func(*os.Process, int) error { return nil }, never,
	)
	require.False(t, waited)
	require.True(t, quarantined)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
}

func TestStartServerContainmentPreparationAndWaiterFailures(t *testing.T) {
	t.Run("prepare generation", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		want := errors.New("prepare generation failed")
		openCodePrepareRuntimeGeneration = func(context.Context, StartOptions) (string, func() error, error) {
			return "", nil, want
		}
		_, err := StartServer(context.Background(), StartOptions{
			ExistingXDG: testXDGDirs(t), ExecutablePath: "/usr/bin/true", ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, want)
	})

	t.Run("generation scratch reaches supervisor", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		generationRoot := t.TempDir()
		openCodePrepareRuntimeGeneration = func(context.Context, StartOptions) (string, func() error, error) {
			return generationRoot, func() error { return nil }, nil
		}
		want := errors.New("encode config failed")
		supervisorEncodeConfig = func(_ io.Writer, config supervisorConfig) error {
			require.Equal(t, generationRoot, config.Scratch)

			return want
		}
		_, err := StartServer(context.Background(), StartOptions{
			ExistingXDG: testXDGDirs(t), ExecutablePath: "/usr/bin/true", ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, want)
	})

	t.Run("release runtime waiter", func(t *testing.T) {
		skipUnprivilegedDarwinIsolation(t)
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		want := errors.New("release waiter failed")
		supervisorReleaseIndependentWaiter = func(*exec.Cmd, *supervisorWaiter) (int, error) {
			return 0, want
		}
		openCodeCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
		}
		_, err := StartServer(context.Background(), StartOptions{
			ExistingXDG: testXDGDirs(t), ExecutablePath: "/usr/bin/true",
			skipSupervisor: true, ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, want)
	})

	t.Run("release supervised runtime waiter", func(t *testing.T) {
		skipUnprivilegedDarwinIsolation(t)
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		want := errors.New("release supervised waiter failed")
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorReleaseIndependentWaiter = func(*exec.Cmd, *supervisorWaiter) (int, error) {
			return 0, want
		}
		openCodeCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
		}
		_, err := StartServer(context.Background(), StartOptions{
			ExistingXDG: testXDGDirs(t), ExecutablePath: "/usr/bin/true", ProcessIsolation: testProcessIsolation(),
		})
		require.ErrorIs(t, err, want)
	})
}

func TestStartServerDarwinContainmentFailures(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin generation registry is platform-specific")
	}

	t.Run("generation root", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		parentFile := filepath.Join(t.TempDir(), "parent-file")
		require.NoError(t, os.WriteFile(parentFile, nil, 0o600))
		_, err := StartServer(context.Background(), StartOptions{
			ExistingXDG:               testXDGDirs(t),
			DarwinBestEffort:          true,
			ContainmentScratchParent:  parentFile,
			ReserveContainmentScratch: testContainmentScratchReservation,
			ProcessIsolation:          testProcessIsolation(),
		})
		require.ErrorContains(t, err, "scratch parent")
	})

	t.Run("untransferred generation removal", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		parent := t.TempDir()
		removeErr := errors.New("remove failed")
		openCodeRemoveAll = func(string) error { return removeErr }
		_, err := StartServer(context.Background(), StartOptions{
			ExistingXDG:               testXDGDirs(t),
			ExecutablePath:            "/usr/bin/false",
			skipSupervisor:            true,
			DarwinBestEffort:          true,
			ContainmentScratchParent:  parent,
			ReserveContainmentScratch: testContainmentScratchReservation,
			ProcessIsolation:          testProcessIsolation(),
		})
		require.ErrorIs(t, err, removeErr)
		require.ErrorIs(t, err, ErrRuntimeScratchCleanup)
	})

	t.Run("completed generation removal", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		want := errors.New("remove failed")
		openCodeRemoveAll = func(string) error { return want }
		_, cleanup, prepareErr := prepareDarwinRuntimeGeneration(context.Background(), StartOptions{
			DarwinBestEffort:          true,
			ContainmentScratchParent:  t.TempDir(),
			ReserveContainmentScratch: testContainmentScratchReservation,
		})
		require.NoError(t, prepareErr)
		server := &openCodeServer{containmentGenerationCleanup: cleanup}
		err := server.shutdownRuntime()
		require.ErrorIs(t, err, want)
		require.ErrorIs(t, err, ErrRuntimeScratchCleanup)
	})
}

func TestShutdownSupervisorWaitAndLoggingBranches(t *testing.T) {
	for name, setup := range map[string]func(*openCodeServer) context.Context{
		"wait error": func(server *openCodeServer) context.Context {
			go func() { server.waitDone <- errors.New("wait failed") }()

			return context.Background()
		},
		"context": func(server *openCodeServer) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			go func() {
				time.Sleep(time.Millisecond)
				server.waitDone <- nil
			}()

			return ctx
		},
		"timeout": func(server *openCodeServer) context.Context {
			openCodeAfter = func(time.Duration) <-chan time.Time {
				ready := make(chan time.Time, 1)
				ready <- time.Now()

				return ready
			}
			go func() {
				time.Sleep(time.Millisecond)
				server.waitDone <- nil
			}()

			return context.Background()
		},
	} {
		t.Run(name, func(t *testing.T) {
			restoreOpenCodeClientSeams(t)
			openCodeTerminateProcess = func(*os.Process, int) error { return nil }
			openCodeKillProcess = func(*os.Process, int) error { return nil }
			root := t.TempDir()
			completion := filepath.Join(root, "complete")
			require.NoError(t, writeSupervisorMarker(completion))
			server := &openCodeServer{
				cmd: &exec.Cmd{Process: &os.Process{Pid: 123}}, supervisorControl: nopWriteCloser{},
				supervisor: &supervisorProof{completion: completion}, waitDone: make(chan error),
				log: slog.New(slog.DiscardHandler),
			}
			ctx := setup(server)
			_ = server.Shutdown(ctx)
		})
	}
}

func TestShutdownEmitsZeroOnlyForProvenDescendantQuiescence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		completion bool
		wantZero   bool
	}{
		{name: "proven", completion: true, wantZero: true},
		{name: "unproven", wantZero: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreOpenCodeClientSeams(t)
			openCodeContainmentTimeout = 10 * time.Millisecond
			root := t.TempDir()
			started := filepath.Join(root, "started")
			completion := filepath.Join(root, "complete")
			require.NoError(t, writeSupervisorMarker(started))
			if tc.completion {
				require.NoError(t, writeSupervisorMarker(completion))
			}

			var snapshots []int
			observation := &runtimeProcessObservation{observeSnapshot: func(_ context.Context, _ string, count int) {
				snapshots = append(snapshots, count)
			}}
			observation.markDescendantsReady(t.Context(), func() (int, bool) { return 3, true })
			waitDone := make(chan error, 1)
			waitDone <- nil
			server := &openCodeServer{
				cmd: &exec.Cmd{Process: &os.Process{Pid: 123}}, supervisorControl: nopWriteCloser{},
				supervisor:         &supervisorProof{started: started, completion: completion},
				processObservation: observation, waitDone: waitDone,
			}

			ctx := context.Background()
			if !tc.completion {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := server.Shutdown(ctx)
			if tc.wantZero {
				require.NoError(t, err)
				require.Equal(t, []int{3, 0}, snapshots)
			} else {
				require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
				require.Equal(t, []int{3}, snapshots)
			}
		})
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (nopWriteCloser) Close() error                    { return nil }

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
	client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL, closed: make(chan struct{}), directory: "/scope"}
	_, err := client.Scope(context.Background(), ScopeOptions{Directory: "/scope", MCPServers: []MCPServerConfig{{Name: "bad", URL: "https://bad"}}})
	require.ErrorContains(t, err, "register directory MCP")

	created, err := client.CreateSessionWithPolicy(context.Background(), "title", []PermissionRule{{Permission: "*", Pattern: "*", Action: "allow"}})
	require.NoError(t, err)
	require.Equal(t, "created", created.ID)

	query := url.Values{"value": []string{"preserved"}}
	var result map[string]any
	require.NoError(t, client.doJSON(context.Background(), http.MethodGet, "/query", query, nil, &result))
	require.Empty(t, query.Get("directory"), "caller query must not be mutated")
}

func TestSendMessagePollingSkipsAndAssistantFailure(t *testing.T) {
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/session/s/prompt_async":
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodGet && request.URL.Path == "/session/s/message":
			reads++
			switch reads {
			case 1:
				writeJSON(t, writer, []map[string]any{{"info": map[string]any{"id": "old", "role": "assistant"}}})
			case 2:
				writeJSON(t, writer, []map[string]any{{"info": map[string]any{"id": "user", "role": "user", "finish": "stop"}}})
			case 3:
				writeJSON(t, writer, []map[string]any{{"info": map[string]any{"id": "old", "role": "assistant", "finish": "stop"}}})
			case 4:
				writeJSON(t, writer, []map[string]any{{"info": map[string]any{"id": "new", "role": "assistant"}}})
			default:
				writeJSON(t, writer, []map[string]any{{"info": map[string]any{
					"id": "new", "role": "assistant", "finish": "error", "error": map[string]any{"message": "provider failed"},
				}}})
			}
		case request.Method == http.MethodGet && request.URL.Path == "/session/status":
			writeJSON(t, writer, map[string]any{"s": map[string]any{"type": "idle"}})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL}
	_, err := client.SendMessage(context.Background(), "s", MessageRequest{})
	require.ErrorContains(t, err, "provider failed")
	require.GreaterOrEqual(t, reads, 5)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.SendMessage(ctx, "s", MessageRequest{})
	require.Error(t, err)
}

func TestRuntimeConfigForbiddenSeedsAndDeepMerge(t *testing.T) {
	for _, field := range []string{fieldPermission, fieldMCP} {
		t.Run(field, func(t *testing.T) {
			dirs, err := CreateRuntimeXDGDirs(t.TempDir())
			require.NoError(t, err)
			_, err = materializeOpenCodeRuntimeConfig(dirs, map[string]string{
				openCodeConfigFileName: `{"` + field + `":{}}`,
			})
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

func TestStartServerSupervisorControlAndStartFailures(t *testing.T) {
	t.Run("supervisor control pipe", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		openCodeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
			command := exec.Command("/bin/true")
			command.Stdin = os.Stdin

			return command
		}
		_, err := StartServer(context.Background(), StartOptions{Root: root})
		require.Error(t, err)
	})

	t.Run("start closes supervisor control", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		openCodeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
			return exec.Command(filepath.Join(root, "missing"))
		}
		_, err := StartServer(context.Background(), StartOptions{Root: root})
		require.Error(t, err)
	})
}

func TestStartServerSupervisedRollbackAndReadinessBranches(t *testing.T) {
	options := func(t *testing.T) StartOptions {
		t.Helper()

		return StartOptions{
			ExistingXDG:      testXDGDirs(t),
			ExecutablePath:   "/usr/bin/true",
			ProcessIsolation: testProcessIsolation(),
		}
	}
	command := func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
	}

	t.Run("control pipe", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		openCodeSupervisorCommand = func(ctx context.Context, _ supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			cmd := command(ctx)
			cmd.Stdin = strings.NewReader("")

			return cmd, &supervisorProof{}, nil
		}
		_, err := StartServer(context.Background(), options(t))
		require.Error(t, err)
	})

	t.Run("start", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		openCodeSupervisorCommand = func(ctx context.Context, _ supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return command(ctx), &supervisorProof{}, nil
		}
		want := errors.New("start failed")
		openCodeStartProcess = func(*exec.Cmd) (*supervisorWaiter, error) { return nil, want }
		_, err := StartServer(context.Background(), options(t))
		require.ErrorIs(t, err, want)
	})

	t.Run("close inherited", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		closed, err := os.CreateTemp(t.TempDir(), "inherited")
		require.NoError(t, err)
		require.NoError(t, closed.Close())
		openCodeSupervisorCommand = func(ctx context.Context, _ supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return command(ctx), &supervisorProof{inherited: []*os.File{closed}}, nil
		}
		_, err = StartServer(context.Background(), options(t))
		require.ErrorContains(t, err, "close inherited supervisor config")
	})

	t.Run("release", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		openCodeSupervisorCommand = func(ctx context.Context, _ supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
			return command(ctx), &supervisorProof{}, nil
		}
		want := errors.New("release failed")
		supervisorReleaseIndependentWaiter = func(*exec.Cmd, *supervisorWaiter) (int, error) { return 0, want }
		_, err := StartServer(context.Background(), options(t))
		require.ErrorIs(t, err, want)
	})

	t.Run("readiness", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		preserveSupervisorGlobals(t)
		openCodeCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd { return command(ctx) }
		openCodeApplyCredential = func(*exec.Cmd, *ProcessIsolation) error { return nil }
		openCodeReadyPollInterval = time.Millisecond
		openCodeHTTPClient = func() *http.Client {
			return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("health unavailable")
			})}
		}
		startOptions := options(t)
		startOptions.skipSupervisor = true
		startOptions.HealthTimeout = 5 * time.Millisecond
		_, err := StartServer(context.Background(), startOptions)
		require.ErrorContains(t, err, "health unavailable")
	})
}

func TestStartServerSupervisedReadinessSuccess(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)
	completion := filepath.Join(t.TempDir(), "complete")
	require.NoError(t, writeSupervisorMarker(completion))
	openCodeSupervisorCommand = func(ctx context.Context, _ supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
		return exec.CommandContext(ctx, "/bin/cat"), &supervisorProof{completion: completion}, nil
	}
	supervisorReleaseIndependentWaiter = func(cmd *exec.Cmd, waiter *supervisorWaiter) (int, error) {
		waiter.start()

		return cmd.Process.Pid, nil
	}
	openCodeHTTPClient = func() *http.Client {
		return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case routeGlobalHealth:
				return performanceJSONResponse(map[string]any{"healthy": true, "version": "1.18.3"}), nil
			case routeDoc:
				return performanceJSONResponse(fullOpenCodeDoc()), nil
			case routeEvent:
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")),
				}, nil
			default:
				return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Header: make(http.Header), Body: http.NoBody}, nil
			}
		})}
	}

	client, err := StartServer(context.Background(), StartOptions{
		ExistingXDG:      testXDGDirs(t),
		ExecutablePath:   "/usr/bin/true",
		ProcessIsolation: testProcessIsolation(),
		ObserveProcess:   func(context.Context, string, int64) {},
	})
	require.NoError(t, err)
	server, ok := client.(*openCodeServer)
	require.True(t, ok)
	require.NoError(t, server.Shutdown(context.Background()))
}

func TestSendMessagePollContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			writeJSON(t, writer, []map[string]any{})
		case http.MethodPost:
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	client := &openCodeServer{httpClient: server.Client(), baseURL: server.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := client.SendMessage(ctx, "s", MessageRequest{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
