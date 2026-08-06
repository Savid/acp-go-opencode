package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
)

const releaseGateRepetitions = 5

func testContainmentScratchReservation(context.Context) (func(), error) {
	return func() {}, nil
}

func platformStartOptions(t *testing.T, options StartOptions) StartOptions {
	t.Helper()
	options = withTestProcessIsolation(options)

	if runtime.GOOS == "darwin" {
		options.DarwinBestEffort = true
		options.ContainmentScratchParent = t.TempDir()
		options.ReserveContainmentScratch = testContainmentScratchReservation
	}

	return options
}

type openCodeMethodsRecorder struct {
	seen        []string
	messageBody MessageRequest
	commandBody CommandRequest
	forkBody    map[string]any
	createBody  map[string]any
	promptAsync bool
}

func openCodeMethodsRoutes(t *testing.T, rec *openCodeMethodsRecorder) map[string]http.HandlerFunc {
	t.Helper()

	return map[string]http.HandlerFunc{
		"POST /session": func(w http.ResponseWriter, r *http.Request) {
			rec.createBody = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&rec.createBody); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "created", "title": "Created"})
		},
		"GET /session": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("directory") != "/repo" {
				t.Errorf("directory query = %q", r.URL.RawQuery)
			}
			writeJSON(t, w, []map[string]any{{"id": "listed"}})
		},
		"GET /session/s%2F1": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"id": "s/1", "title": "Loaded"})
		},
		"DELETE /session/s%2F1": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"ok": true})
		},
		"POST /session/s%2F1/message": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&rec.messageBody); err != nil {
				t.Errorf("decode message body: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"info":  map[string]any{"id": "assistant", "sessionID": "s/1", "role": "assistant", "finish": "stop"},
				"parts": []map[string]any{{"id": "part-1", "sessionID": "s/1", "messageID": "assistant", "type": "text", "text": "ok"}},
			})
		},
		"GET /session/s%2F1/message": func(w http.ResponseWriter, _ *http.Request) {
			if rec.promptAsync {
				writeJSON(t, w, []map[string]any{{"info": map[string]any{"id": "assistant", "sessionID": "s/1", "role": "assistant", "finish": "stop"}, "parts": []map[string]any{{"id": "part-1", "sessionID": "s/1", "messageID": "assistant", "type": "text", "text": "ok"}}}})

				return
			}
			writeJSON(t, w, []map[string]any{{
				"info":  map[string]any{"id": "history", "sessionID": "s/1", "role": "assistant"},
				"parts": []map[string]any{{"id": "history-part", "sessionID": "s/1", "messageID": "history", "type": "text", "text": "ok"}},
			}})
		},
		"POST /session/s%2F1/prompt_async": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&rec.messageBody); err != nil {
				t.Errorf("decode async body: %v", err)
			}
			rec.promptAsync = true
			w.WriteHeader(http.StatusNoContent)
		},
		"GET /command": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, []map[string]any{{
				"name":        "review",
				"description": "Review",
				"agent":       "build",
				"model":       "openai/gpt-test",
				"source":      "command",
				"template":    "hidden",
				"subtask":     true,
				"hints":       []string{"$ARGUMENTS"},
			}})
		},
		"POST /session/s%2F1/command": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&rec.commandBody); err != nil {
				t.Errorf("decode command body: %v", err)
			}
			writeJSON(t, w, map[string]any{
				"info":  map[string]any{"id": "assistant-command", "sessionID": "s/1", "role": "assistant", "finish": "stop"},
				"parts": []map[string]any{{"id": "part-command", "sessionID": "s/1", "messageID": "assistant-command", "type": "text", "text": "ok"}},
			})
		},
		"GET /session/status": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"s/1": map[string]any{"type": "idle"}})
		},
		"POST /session/s%2F1/abort": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"ok": true})
		},
		"POST /session/s%2F1/fork": func(w http.ResponseWriter, r *http.Request) {
			rec.forkBody = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&rec.forkBody); err != nil {
				t.Errorf("decode fork body: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "forked"})
		},
		"GET /session/s%2F1/todo": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, []map[string]any{{"id": "todo", "content": "Do it"}})
		},
		"GET /config/providers": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, map[string]any{"providers": []map[string]any{{"id": "openai", "models": map[string]any{}}}})
		},
		"GET /agent": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, []map[string]any{{"name": "build"}})
		},
		"GET /empty": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"GET /invalid-json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{"))
		},
		"GET /status": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("short and stout"))
		},
	}
}

func newOpenCodeMethodsClient(t *testing.T) (*openCodeServer, *openCodeMethodsRecorder) {
	t.Helper()
	rec := &openCodeMethodsRecorder{}
	routes := openCodeMethodsRoutes(t, rec)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if username, password, ok := r.BasicAuth(); !ok || username != "opencode" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("bad auth"))

			return
		}
		rec.seen = append(rec.seen, r.Method+" "+r.URL.RequestURI())
		route, ok := routes[r.Method+" "+r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}
		route(w, r)
	}))
	t.Cleanup(server.Close)

	client := &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
		events:     make(chan Event),
		errs:       make(chan error),
		closed:     make(chan struct{}),
	}

	return client, rec
}

func TestOpenCodeServerSessionMethods(t *testing.T) {
	ctx := context.Background()
	client, rec := newOpenCodeMethodsClient(t)

	created, err := client.CreateSession(ctx, "Created")
	if err != nil || created.ID != "created" || rec.createBody["title"] != "Created" {
		t.Fatalf("CreateSession = %#v body=%#v err=%v", created, rec.createBody, err)
	}
	created, err = client.CreateSession(ctx, "")
	if err != nil || len(rec.createBody) != 0 {
		t.Fatalf("CreateSession empty = %#v body=%#v err=%v", created, rec.createBody, err)
	}
	got, err := client.GetSession(ctx, "s/1")
	if err != nil || got.ID != "s/1" {
		t.Fatalf("GetSession = %#v err=%v", got, err)
	}
	listed, err := client.ListSessions(ctx, "/repo")
	if err != nil || len(listed) != 1 || listed[0].ID != "listed" {
		t.Fatalf("ListSessions = %#v err=%v", listed, err)
	}
	if deleteErr := client.DeleteSession(ctx, "s/1"); deleteErr != nil {
		t.Fatalf("DeleteSession: %v", deleteErr)
	}
	if !containsString(rec.seen, "GET /session?directory=%2Frepo") {
		t.Fatalf("seen paths = %#v", rec.seen)
	}
}

func TestOpenCodeServerMessageAndCommandMethods(t *testing.T) {
	ctx := context.Background()
	client, rec := newOpenCodeMethodsClient(t)

	message, err := client.SendMessage(ctx, "s/1", MessageRequest{
		MessageID: "user-1",
		Model:     &ModelSelector{ProviderID: "openai", ModelID: "gpt-test"},
		Agent:     "build",
		Parts:     []map[string]any{{"type": "text", "text": "hello"}},
	})
	if err != nil || message.Info.ID != "assistant" || rec.messageBody.MessageID != "user-1" ||
		rec.messageBody.Model.ModelID != "gpt-test" || rec.messageBody.Agent != "build" || len(rec.messageBody.Parts) != 1 {
		t.Fatalf("SendMessage = %#v body=%#v err=%v", message, rec.messageBody, err)
	}
	messages, err := client.Messages(ctx, "s/1")
	if err != nil || len(messages) != 1 || messages[0].Info.ID != "assistant" {
		t.Fatalf("Messages = %#v err=%v", messages, err)
	}
	commands, err := client.Commands(ctx)
	if err != nil || len(commands) != 1 || commands[0].Name != "review" || commands[0].Template == nil || len(commands[0].Hints) != 1 {
		t.Fatalf("Commands = %#v err=%v", commands, err)
	}
	command, err := client.RunCommand(ctx, "s/1", CommandRequest{
		MessageID: "user-2",
		Agent:     "build",
		Model:     "openai/gpt-test",
		Command:   "review",
		Arguments: "args",
		Parts:     []map[string]any{{"type": "file", "mime": "image/png", "url": "data:image/png;base64,AA=="}},
	})
	if err != nil || command.Info.ID != "assistant-command" || rec.commandBody.MessageID != "user-2" ||
		rec.commandBody.Model != "openai/gpt-test" || rec.commandBody.Command != "review" || rec.commandBody.Arguments != "args" ||
		len(rec.commandBody.Parts) != 1 {
		t.Fatalf("RunCommand = %#v body=%#v err=%v", command, rec.commandBody, err)
	}
}

func TestOpenCodeServerControlAndInfoMethods(t *testing.T) {
	ctx := context.Background()
	client, rec := newOpenCodeMethodsClient(t)

	status, err := client.SessionStatus(ctx)
	if err != nil || status["s/1"].Type != "idle" {
		t.Fatalf("SessionStatus = %#v err=%v", status, err)
	}
	if abortErr := client.Abort(ctx, "s/1"); abortErr != nil {
		t.Fatalf("Abort: %v", abortErr)
	}
	forked, err := client.Fork(ctx, "s/1", "message-1")
	if err != nil || forked.ID != "forked" || rec.forkBody["messageID"] != "message-1" {
		t.Fatalf("Fork = %#v body=%#v err=%v", forked, rec.forkBody, err)
	}
	forked, err = client.Fork(ctx, "s/1", "")
	if err != nil || len(rec.forkBody) != 0 {
		t.Fatalf("Fork empty = %#v body=%#v err=%v", forked, rec.forkBody, err)
	}
	todos, err := client.Todos(ctx, "s/1")
	if err != nil || len(todos) != 1 || todos[0].ID != "todo" {
		t.Fatalf("Todos = %#v err=%v", todos, err)
	}
	providers, err := client.ConfigProviders(ctx)
	if err != nil || len(providers.Providers) != 1 || providers.Raw == nil {
		t.Fatalf("ConfigProviders = %#v err=%v", providers, err)
	}
	agents, err := client.Agents(ctx)
	if err != nil || len(agents) != 1 || agents[0].Name != "build" {
		t.Fatalf("Agents = %#v err=%v", agents, err)
	}
}

func TestOpenCodeServerRawJSONErrors(t *testing.T) {
	ctx := context.Background()
	client, _ := newOpenCodeMethodsClient(t)

	if err := client.getJSON(ctx, "/empty", nil, nil); err != nil {
		t.Fatalf("empty getJSON: %v", err)
	}
	if err := client.getJSON(ctx, "/invalid-json", nil, &map[string]any{}); err == nil {
		t.Fatal("invalid json unexpectedly succeeded")
	}
	if err := client.getJSON(ctx, "/status", nil, &map[string]any{}); err == nil || !strings.Contains(err.Error(), "short and stout") {
		t.Fatalf("status error = %v", err)
	}
	client.baseURL = ":// bad url"
	if err := client.getJSON(ctx, "/bad", nil, &map[string]any{}); err == nil {
		t.Fatal("bad request URL unexpectedly succeeded")
	}
}

func TestAssistantMessageErrorFields(t *testing.T) {
	finishErr := AssistantMessageError(NativeMessage{Info: NativeMessageInfo{Role: "assistant", Finish: "error"}})
	if finishErr == nil {
		t.Fatal("assistant finish error accepted")
	}
	if finishErr.Error() != "opencode assistant error" {
		t.Fatalf("empty assistant error string = %v", finishErr)
	}

	providerErr := AssistantMessageError(NativeMessage{Info: NativeMessageInfo{
		Role:  "assistant",
		Error: &NativeError{Message: "provider failed"},
	}})
	if providerErr == nil || !strings.Contains(providerErr.Error(), "provider failed") {
		t.Fatalf("assistant error = %v", providerErr)
	}
	emptyErr := AssistantMessageError(NativeMessage{Info: NativeMessageInfo{Error: &NativeError{}}})
	if emptyErr == nil {
		t.Fatal("empty assistant error accepted")
	}
	nonErrorErr := AssistantMessageError(NativeMessage{Info: NativeMessageInfo{Role: "assistant", Finish: "stop"}})
	if nonErrorErr != nil {
		t.Fatalf("non-error assistant rejected: %v", nonErrorErr)
	}

	apiErr := &NativeError{Message: "provider failed"}
	apiErr.Data.StatusCode = 429
	apiErr.Data.ResponseBody = `{"error":{"type":"rate_limit_exceeded"}}`
	structuredErr := AssistantMessageError(NativeMessage{Info: NativeMessageInfo{
		Role:   "assistant",
		Finish: "error",
		Error:  apiErr,
	}})
	var assistantErr *AssistantError
	if !errors.As(structuredErr, &assistantErr) {
		t.Fatalf("assistant error type = %T: %v", structuredErr, structuredErr)
	}
	if assistantErr.Error() != "opencode assistant error: provider failed" ||
		assistantErr.Detail() != "provider failed" ||
		assistantErr.StatusCode() != 429 ||
		assistantErr.ProviderCode() != "rate_limit_exceeded" {
		t.Fatalf("assistant error fields = %#v", assistantErr)
	}

	apiErr.Data.ResponseBody = "{bad json"
	malformedErr := AssistantMessageError(NativeMessage{Info: NativeMessageInfo{
		Role:   "assistant",
		Finish: "error",
		Error:  apiErr,
	}})
	if !errors.As(malformedErr, &assistantErr) {
		t.Fatalf("malformed provider body error type = %T: %v", malformedErr, malformedErr)
	}
	if assistantErr.ProviderCode() != "" {
		t.Fatalf("malformed provider code = %q", assistantErr.ProviderCode())
	}

	var nilAssistantErr *AssistantError
	if nilAssistantErr.Detail() != "" || nilAssistantErr.StatusCode() != 0 || nilAssistantErr.ProviderCode() != "" {
		t.Fatalf("nil assistant error accessors returned values")
	}
}

func TestOpenCodeSendMessageErrors(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name    string
		handler func(http.ResponseWriter, *http.Request)
		command bool
	}{
		{
			name: "post error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/session/s/message" && r.Method == http.MethodPost {
					w.WriteHeader(http.StatusInternalServerError)

					return
				}
				http.NotFound(w, r)
			},
		},
		{
			name:    "command post error",
			command: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/session/s/command" && r.Method == http.MethodPost {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte("unknown command"))

					return
				}
				http.NotFound(w, r)
			},
		},
		{
			name: "assistant error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/session/s/message" && r.Method == http.MethodPost {
					writeJSON(t, w, map[string]any{
						"info": map[string]any{
							"id":     "assistant",
							"role":   "assistant",
							"finish": "error",
							"error":  map[string]any{"message": "provider failed"},
						},
					})

					return
				}
				http.NotFound(w, r)
			},
		},
		{
			name:    "command assistant error",
			command: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/session/s/command" && r.Method == http.MethodPost {
					writeJSON(t, w, map[string]any{
						"info": map[string]any{
							"id":     "assistant",
							"role":   "assistant",
							"finish": "error",
							"error":  map[string]any{"message": "command failed"},
						},
					})

					return
				}
				http.NotFound(w, r)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if username, password, ok := r.BasicAuth(); !ok || username != "opencode" || password != "secret" {
					w.WriteHeader(http.StatusUnauthorized)

					return
				}
				tt.handler(w, r)
			}))
			defer server.Close()
			client := &openCodeServer{
				httpClient: server.Client(),
				baseURL:    server.URL,
				username:   "opencode",
				password:   "secret",
			}
			var err error
			if tt.command {
				_, err = client.RunCommand(ctx, "s", CommandRequest{Command: "review", Arguments: ""})
			} else {
				_, err = client.SendMessage(ctx, "s", MessageRequest{Parts: []map[string]any{{"type": "text", "text": "hello"}}})
			}
			if err == nil {
				t.Fatal("native send unexpectedly succeeded")
			}
		})
	}
}

func TestStartOpenCodeServerWithFakeExecutable(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	helper := fakeOpenCodeExecutable(t)
	root := testGeneratedTempDir(t)
	logger := slog.New(slog.DiscardHandler)
	client, err := StartServer(context.Background(), platformStartOptions(t, StartOptions{
		Root:            root,
		ExecutablePath:  helper,
		Env:             map[string]string{"BASE_ENV": "base"},
		Pure:            true,
		QuestionTool:    true,
		LogLevel:        "DEBUG",
		MinVersion:      "1.18.3",
		HealthTimeout:   5 * time.Second,
		Logger:          logger,
		SkipVersionGate: false,
	}))
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	server, ok := client.(*openCodeServer)
	if !ok {
		t.Fatalf("client type = %T, want *openCodeServer", client)
	}
	if server.xdg.Root != root {
		t.Fatalf("xdg dirs = %#v", server.xdg)
	}
	// The supervisor holds both locks in the control root beside the XDG root,
	// which is the home it was handed.
	controlRoot := ControlRootForXDG(server.xdg.Root)
	for _, name := range []string{homelock.ClaimFileName, homelock.LivenessFileName} {
		if _, statErr := os.Stat(filepath.Join(controlRoot, name)); statErr != nil {
			t.Fatalf("runtime lock %s was not retained: %v", name, statErr)
		}
	}
	configData, err := os.ReadFile(filepath.Join(server.xdg.Config, "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("permission config was not written: %v", err)
	}
	if strings.Contains(string(configData), `"permission"`) {
		t.Fatalf("runtime config contains session permission = %s", string(configData))
	}
	if server.Events() == nil || server.EventErrors() == nil || server.XDGDirs().Root == "" {
		t.Fatalf("server channels/dirs not initialized: %#v", server)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	for _, name := range []string{homelock.ClaimFileName, homelock.LivenessFileName} {
		if _, err := os.Stat(filepath.Join(controlRoot, name)); err != nil {
			t.Fatalf("runtime lock file %s was unlinked: %v", name, err)
		}
	}
}

func TestRuntimeShutdownIsBaseOwnedAndMemoizesOneContainmentResult(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	openCodeTerminateProcess = func(*os.Process, int) error { return nil }
	shutdownTimeout := make(chan time.Time)
	openCodeAfter = func(time.Duration) <-chan time.Time {
		return shutdownTimeout
	}

	var kills atomic.Int32
	openCodeKillProcess = func(*os.Process, int) error {
		kills.Add(1)

		return nil
	}

	root := t.TempDir()
	completion := filepath.Join(root, "complete")
	require.NoError(t, writeSupervisorMarker(completion))
	state := newRuntimeShutdownState()
	base := &openCodeServer{
		cmd: &exec.Cmd{Process: &os.Process{Pid: 123}}, supervisorControl: nopWriteCloser{},
		supervisor: &supervisorProof{completion: completion}, waitDone: make(chan error, 1),
		runtimeShutdown: state, runtimeClosed: make(chan struct{}),
	}
	scope := &openCodeServer{scopeCancel: func() {}, runtimeShutdown: state}
	require.ErrorIs(t, scope.Shutdown(context.Background()), ErrScopeRuntimeShutdown)
	require.Zero(t, kills.Load())

	results := make(chan error, 2)
	go func() { results <- base.Shutdown(context.Background()) }()
	go func() {
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		results <- base.Shutdown(cancelled)
	}()
	shutdownTimeout <- time.Now()
	base.waitDone <- nil
	first := <-results
	second := <-results
	require.ErrorContains(t, first, "did not exit after shutdown")
	require.True(t, first == second, "all shutdown callers must receive the exact memoized error")
	require.Zero(t, kills.Load(), "caller timeout must not kill a trusted supervisor that can own quarantine")
	require.True(t, first == base.Shutdown(context.Background()))
}

func TestStartOpenCodeServerFaultInjection(t *testing.T) {
	ctx := context.Background()

	t.Run("defaults and start error", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		xdg := testXDGDirs(t)
		binaryDir := t.TempDir()
		binary := filepath.Join(binaryDir, opencodeExecutableName)
		require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700))
		isolation := testProcessIsolation()
		isolation.BaseEnvironment["PATH"] = binaryDir + string(os.PathListSeparator) + isolation.BaseEnvironment["PATH"]
		var executable string
		openCodeCommandContext = func(ctx context.Context, name string, _ ...string) *exec.Cmd {
			executable = name

			return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-opencode"))
		}
		_, err := StartServer(ctx, StartOptions{
			ExistingXDG:      xdg,
			skipSupervisor:   true,
			ProcessIsolation: isolation,
		})
		if err == nil {
			t.Fatal("missing executable unexpectedly started")
		}
		if filepath.Base(executable) != "opencode" {
			t.Fatalf("default executable = %q", executable)
		}
	})

	t.Run("create xdg failure", func(t *testing.T) {
		_, err := StartServer(ctx, platformStartOptions(t, StartOptions{
			Root: filepath.Join(t.TempDir(), string([]byte{0})),
		}))
		if err == nil {
			t.Fatal("invalid session xdg path unexpectedly succeeded")
		}
	})

	t.Run("ensure existing xdg failure", func(t *testing.T) {
		_, err := StartServer(ctx, platformStartOptions(t, StartOptions{
			ExistingXDG: XDGDirs{Root: filepath.Join(t.TempDir(), "root")},
		}))
		if err == nil {
			t.Fatal("incomplete existing xdg unexpectedly succeeded")
		}
	})

	t.Run("runtime config failure", func(t *testing.T) {
		_, err := StartServer(ctx, StartOptions{
			ExistingXDG:      testXDGDirs(t),
			SeedFiles:        map[string]string{"../escape": "bad"},
			ProcessIsolation: testProcessIsolation(),
		})
		if err == nil {
			t.Fatal("invalid permission unexpectedly started")
		}
	})

	t.Run("allocate port failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeListen = func(string, string) (net.Listener, error) {
			return nil, errors.New("listen failed")
		}
		if _, err := StartServer(ctx, withTestProcessIsolation(StartOptions{ExistingXDG: testXDGDirs(t)})); err == nil {
			t.Fatal("listen error was ignored")
		}
	})

	t.Run("password entropy failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeRandReader = errorReader{err: errors.New("entropy failed")}
		if _, err := StartServer(ctx, withTestProcessIsolation(StartOptions{ExistingXDG: testXDGDirs(t)})); err == nil {
			t.Fatal("entropy error was ignored")
		}
	})

	t.Run("stdout pipe failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFakeOpenCodeServerProcessHelper")
			cmd.Stdout = io.Discard

			return cmd
		}
		if _, err := StartServer(ctx, withTestProcessIsolation(StartOptions{ExistingXDG: testXDGDirs(t)})); err == nil {
			t.Fatal("stdout pipe error was ignored")
		}
	})

	t.Run("stderr pipe failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestFakeOpenCodeServerProcessHelper")
			cmd.Stderr = io.Discard

			return cmd
		}
		if _, err := StartServer(ctx, withTestProcessIsolation(StartOptions{ExistingXDG: testXDGDirs(t)})); err == nil {
			t.Fatal("stderr pipe error was ignored")
		}
	})

	t.Run("readiness failure closes process", func(t *testing.T) {
		skipUnprivilegedDarwinIsolation(t)
		helper := fakeOpenCodeExecutable(t)
		_, err := StartServer(ctx, platformStartOptions(t, StartOptions{
			Root:            t.TempDir(),
			ExecutablePath:  helper,
			MinVersion:      "99.0.0",
			HealthTimeout:   5 * time.Second,
			SkipVersionGate: false,
			Logger:          slog.New(slog.DiscardHandler),
		}))
		if err == nil || !strings.Contains(err.Error(), "below minimum supported") {
			t.Fatalf("readiness error = %v", err)
		}
	})
}

func TestOpenCodeServerReadinessGateAndStreamFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	t.Run("version gate", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/global/health":
				writeJSON(t, w, map[string]any{"healthy": true, "version": "1.0.0"})
			case "/doc":
				writeJSON(t, w, fullOpenCodeDoc())
			case "/event":
				writeSSE(t, w, `{"type":"server.connected","properties":{}}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer closeServer()
		if err := client.waitReady(ctx, ctx, StartOptions{MinVersion: "9.0.0"}); err == nil {
			t.Fatal("old version unexpectedly passed readiness")
		}
	})

	t.Run("wrong first event", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/global/health":
				writeJSON(t, w, map[string]any{"healthy": true, "version": "9.0.0"})
			case "/doc":
				writeJSON(t, w, fullOpenCodeDoc())
			case "/event":
				writeSSE(t, w, `{"type":"other","properties":{}}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer closeServer()
		if err := client.waitReady(ctx, ctx, StartOptions{SkipVersionGate: true}); err == nil {
			t.Fatal("wrong first event unexpectedly passed readiness")
		}
	})

	t.Run("event stream status and malformed data", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/event":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("bad stream"))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer closeServer()
		if err := client.readEventStream(ctx); err == nil || !strings.Contains(err.Error(), "bad stream") {
			t.Fatalf("stream status error = %v", err)
		}

		client, closeServer = readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/event" {
				writeSSE(t, w, `{`)

				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		defer closeServer()
		if err := client.readEventStream(ctx); err == nil {
			t.Fatal("malformed event stream unexpectedly succeeded")
		}
	})
}

func TestOpenCodeServerReadinessHealthDocAndEventFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	t.Run("health timeout with last error", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/global/health" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("bad health"))

				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		defer closeServer()
		shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := client.waitReady(shortCtx, shortCtx, StartOptions{SkipVersionGate: true}); err == nil ||
			!strings.Contains(err.Error(), "health check failed") {
			t.Fatalf("health error = %v", err)
		}
	})

	t.Run("health timeout while unhealthy", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/global/health" {
				writeJSON(t, w, map[string]any{"healthy": false, "version": "9.0.0"})

				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		defer closeServer()
		shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := client.waitReady(shortCtx, shortCtx, StartOptions{SkipVersionGate: true}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unhealthy timeout error = %v", err)
		}
	})

	t.Run("doc load and validation errors", func(t *testing.T) {
		tests := map[string]func(http.ResponseWriter){
			"load": func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("doc failed"))
			},
			"validate": func(w http.ResponseWriter) { writeJSON(t, w, map[string]any{"paths": map[string]any{}}) },
		}
		for name, docHandler := range tests {
			t.Run(name, func(t *testing.T) {
				client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/global/health":
						writeJSON(t, w, map[string]any{"healthy": true, "version": "9.0.0"})
					case "/doc":
						docHandler(w)
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				})
				defer closeServer()
				if err := client.waitReady(ctx, ctx, StartOptions{SkipVersionGate: true}); err == nil {
					t.Fatal("doc readiness error was ignored")
				}
			})
		}
	})

	t.Run("event stream readiness error", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/global/health":
				writeJSON(t, w, map[string]any{"healthy": true, "version": "9.0.0"})
			case "/doc":
				writeJSON(t, w, fullOpenCodeDoc())
			case "/event":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("event failed"))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer closeServer()
		if err := client.waitReady(ctx, ctx, StartOptions{SkipVersionGate: true}); err == nil ||
			!strings.Contains(err.Error(), "event stream failed") {
			t.Fatalf("event readiness error = %v", err)
		}
	})

	t.Run("event wait context deadline", func(t *testing.T) {
		client, closeServer := readinessClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/global/health":
				writeJSON(t, w, map[string]any{"healthy": true, "version": "9.0.0"})
			case "/doc":
				writeJSON(t, w, fullOpenCodeDoc())
			case "/event":
				time.Sleep(500 * time.Millisecond)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer closeServer()
		// The deadline is generous enough that the health and /doc probes always
		// complete first, so the deadline reliably fires while waiting on the
		// readiness event rather than during an earlier probe.
		shortCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := client.waitReady(shortCtx, context.Background(), StartOptions{SkipVersionGate: true}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("event wait error = %v", err)
		}
	})
}

func TestOpenCodeHTTPAndSSEFaultBranches(t *testing.T) {
	ctx := context.Background()
	client := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       errorReadCloser{err: errors.New("body read failed")},
			}, nil
		})},
		baseURL:  "http://opencode.test",
		username: "opencode",
		password: "secret",
		events:   make(chan Event),
		errs:     make(chan error, 1),
		closed:   make(chan struct{}),
	}
	client.httpClient.Timeout = 30 * time.Second
	if eventClient := client.eventHTTPClient(); eventClient.Timeout != 0 || eventClient == client.httpClient || eventClient.Transport == nil {
		t.Fatalf("eventHTTPClient = %#v", eventClient)
	}
	client.httpClient.Timeout = 0
	if httpClient := (&openCodeServer{}).eventHTTPClient(); httpClient != http.DefaultClient {
		t.Fatalf("nil eventHTTPClient = %#v", httpClient)
	}
	if err := client.doJSON(ctx, http.MethodPost, "/bad-body", nil, map[string]any{"bad": func() {}}, nil); err == nil {
		t.Fatal("doJSON accepted unmarshalable request body")
	}
	if err := client.doJSON(ctx, http.MethodGet, "/copy-error", nil, nil, nil); err == nil {
		t.Fatal("doJSON ignored response body copy error")
	}

	client.baseURL = ":// bad url"
	if err := client.readEventStream(ctx); err == nil {
		t.Fatal("readEventStream accepted bad URL")
	}

	client.baseURL = "http://opencode.test"
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("event dial failed")
	})}
	if err := client.readEventStream(ctx); err == nil {
		t.Fatal("readEventStream ignored HTTP client error")
	}

	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\n"))
		_, _ = w.Write([]byte("data: \"server.connected\",\"properties\":{}}\n\n"))
	}))
	defer stream.Close()
	client.httpClient = stream.Client()
	client.baseURL = stream.URL
	client.events = make(chan Event, 1)
	if err := client.readEventStream(ctx); !errors.Is(err, ErrSSEDisconnect) {
		t.Fatalf("multi-line clean EOF readEventStream error = %v", err)
	}
	if event := <-client.events; event.Type != "server.connected" {
		t.Fatalf("event = %#v", event)
	}

	unterminatedStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n"))
	}))
	defer unterminatedStream.Close()
	client.httpClient = unterminatedStream.Client()
	client.baseURL = unterminatedStream.URL
	client.events = make(chan Event, 1)
	if err := client.readEventStream(ctx); !errors.Is(err, ErrSSEDisconnect) {
		t.Fatalf("unterminated clean EOF readEventStream error = %v", err)
	}
	if event := <-client.events; event.Type != "server.connected" {
		t.Fatalf("unterminated event = %#v", event)
	}

	cancelOnEOF, cancelEOF := context.WithCancel(context.Background())
	client.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       eofCancelReadCloser{cancel: cancelEOF},
		}, nil
	})}
	client.baseURL = "http://opencode.test"
	client.events = make(chan Event, 1)
	if err := client.readEventStream(cancelOnEOF); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-EOF cancelled stream error = %v", err)
	}

	closedFlushStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n"))
	}))
	defer closedFlushStream.Close()
	client.httpClient = closedFlushStream.Client()
	client.baseURL = closedFlushStream.URL
	client.events = make(chan Event)
	close(client.closed)
	if err := client.readEventStream(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("closed stream error = %v", err)
	}

	cancelledStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer cancelledStream.Close()
	client = &openCodeServer{
		httpClient: cancelledStream.Client(),
		baseURL:    cancelledStream.URL,
		username:   "opencode",
		password:   "secret",
		events:     make(chan Event),
		errs:       make(chan error, 1),
		closed:     make(chan struct{}),
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.readEventStream(streamCtx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled stream error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled stream did not return")
	}
}

func TestOpenCodeReadEventsDropsErrorWhenChannelFullAndReconnects(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	requests := 0
	var client *openCodeServer
	client = &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			if requests == 2 {
				close(client.closed)
			}

			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Status:     "500 Internal Server Error",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("bad stream")),
			}, nil
		})},
		baseURL:  "http://opencode.test",
		username: "opencode",
		password: "secret",
		events:   make(chan Event, 1),
		errs:     make(chan error, 1),
		closed:   make(chan struct{}),
	}
	client.errs <- errors.New("already full")
	delayCalls := 0
	openCodeAfter = func(time.Duration) <-chan time.Time {
		delayCalls++
		if delayCalls > 1 {
			return make(chan time.Time)
		}
		ch := make(chan time.Time, 1)
		ch <- time.Now()

		return ch
	}
	client.readEvents(context.Background())
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestOpenCodeServerCloseTimeoutAndContext(t *testing.T) {
	for name, cancelled := range map[string]bool{"timeout": false, "context": true} {
		t.Run(name, func(t *testing.T) {
			restoreOpenCodeClientSeams(t)
			openCodeContainmentTimeout = 10 * time.Millisecond
			cmd := &exec.Cmd{Process: &os.Process{Pid: 1234}}
			openCodeTerminateProcess = func(*os.Process, int) error { return nil }
			openCodeKillProcess = func(*os.Process, int) error { return nil }
			openCodeWaitCommand = func(*exec.Cmd) error {
				select {}
			}
			server := &openCodeServer{
				cmd:    cmd,
				cancel: func() {},
				closed: make(chan struct{}),
				log:    slog.New(slog.DiscardHandler),
			}
			ctx := context.Background()
			if cancelled {
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
				openCodeAfter = func(time.Duration) <-chan time.Time {
					return make(chan time.Time)
				}
			} else {
				openCodeShutdownTimeout = time.Millisecond
			}
			if err := server.Shutdown(ctx); err == nil {
				t.Fatal("Shutdown unexpectedly succeeded")
			}
		})
	}
}

func TestXDGEnvAndPipeHelpers(t *testing.T) {
	root := t.TempDir()
	xdg, err := CreateRuntimeXDGDirs(root)
	if err != nil {
		t.Fatalf("CreateRuntimeXDGDirs: %v", err)
	}
	if xdg.Root != root {
		t.Fatalf("default xdg root = %#v", xdg)
	}
	if ensureErr := ensureXDGDirs(XDGDirs{Root: "", Data: "x", Config: "x", Cache: "x", State: "x"}); ensureErr == nil {
		t.Fatal("ensureXDGDirs accepted empty root")
	}
	permissionConfig, err := materializeOpenCodeRuntimeConfig(xdg, nil)
	if err != nil || strings.Contains(permissionConfig, `"permission"`) {
		t.Fatalf("runtime config = %q err=%v", permissionConfig, err)
	}
	permissionFile := filepath.Join(xdg.Config, "opencode", "opencode.json")
	info, err := os.Stat(permissionFile)
	if err != nil {
		t.Fatalf("permission config file stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permission config file mode = %v", info.Mode().Perm())
	}
	configRootFile := filepath.Join(t.TempDir(), "config-file")
	if writeErr := os.WriteFile(configRootFile, []byte("file"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, mkdirErr := materializeOpenCodePermissionConfig(XDGDirs{Config: configRootFile}, "ask", nil, nil); mkdirErr == nil {
		t.Fatal("permission config mkdir failure ignored")
	}
	configRoot := filepath.Join(t.TempDir(), "config")
	if mkErr := os.MkdirAll(filepath.Join(configRoot, "opencode", "opencode.json"), 0o700); mkErr != nil {
		t.Fatal(mkErr)
	}
	if _, writeFailErr := materializeOpenCodePermissionConfig(XDGDirs{Config: configRoot}, "ask", nil, nil); writeFailErr == nil {
		t.Fatal("permission config write failure ignored")
	}
	port, err := allocatePort()
	if err != nil || port <= 0 {
		t.Fatalf("allocatePort = %d err=%v", port, err)
	}
	password, err := randomPassword()
	if err != nil || password == "" {
		t.Fatalf("randomPassword = %q err=%v", password, err)
	}
	env, err := buildProcessEnvironment(&ProcessIsolation{
		UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}, map[string]string{"A": "1"}, map[string]string{"A": "2", "B": "3"})
	if err != nil || env["A"] != "2" || env["B"] != "3" {
		t.Fatalf("merged env = %#v, err = %v", env, err)
	}
	drainProcessPipe(slog.New(slog.DiscardHandler), "test", strings.NewReader("one\ntwo\n"))
	for _, value := range []any{float64(-1), int(-1), json.Number("bad")} {
		if got, ok := IntFromNumber(value); ok || got != 0 {
			t.Fatalf("IntFromNumber(%#v) = %d, %v", value, got, ok)
		}
	}
	if got, ok := IntFromNumber(json.Number("12")); !ok || got != 12 {
		t.Fatalf("IntFromNumber json number = %d, %v", got, ok)
	}
}

// Seed-guard tests below exercise the shared runtime seed writer. Their old
// helper shape is retained only inside this test file to keep fault tables
// focused on filesystem behavior; permission/MCP assertions have dedicated
// multiplexing tests.
func materializeOpenCodePermissionConfig(dirs XDGDirs, _ string, seeds map[string]string, _ []MCPServerConfig) (string, error) {
	return materializeOpenCodeRuntimeConfig(dirs, seeds)
}

func TestOpenCodeSeedFilesMergeAndConfinement(t *testing.T) {
	xdg := testXDGDirs(t)
	seed := map[string]string{
		"opencode.json": `{
  "provider": {
    "litellm": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"baseURL": "https://proxy.example/v1"}
    }
  }
}`,
		"themes/custom.json":        `{"name":"custom"}`,
		"agents/subdir/reviewer.md": "seeded agent",
	}

	config, err := materializeOpenCodePermissionConfig(xdg, "ask", seed, nil)
	if err != nil {
		t.Fatalf("materializeOpenCodePermissionConfig: %v", err)
	}

	// The returned OPENCODE_CONFIG_CONTENT must carry the wrapper-managed keys
	// merged on top of the seeded provider block.
	var merged map[string]any
	if unmarshalErr := json.Unmarshal([]byte(config), &merged); unmarshalErr != nil {
		t.Fatalf("returned config is not JSON: %v", unmarshalErr)
	}
	if merged["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("merged config missing wrapper $schema: %#v", merged)
	}
	if _, ok := merged["permission"]; ok {
		t.Fatalf("shared runtime config contains permission: %#v", merged)
	}
	provider, ok := merged["provider"].(map[string]any)
	if !ok || provider["litellm"] == nil {
		t.Fatalf("seeded provider block was dropped: %#v", merged["provider"])
	}

	// The on-disk opencode.json must equal the returned content exactly.
	onDisk, err := os.ReadFile(filepath.Join(xdg.Config, "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	if string(onDisk) != config {
		t.Fatalf("on-disk opencode.json != OPENCODE_CONFIG_CONTENT\n disk=%q\n env =%q", onDisk, config)
	}

	// Other seeded files land verbatim under the config root, including nested dirs.
	for rel, want := range map[string]string{
		"themes/custom.json":        `{"name":"custom"}`,
		"agents/subdir/reviewer.md": "seeded agent",
	} {
		got, err := os.ReadFile(filepath.Join(xdg.Config, "opencode", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read seeded %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("seeded %s = %q, want %q", rel, got, want)
		}
	}

	// Path confinement: absolute, parent escapes, dot, empty, and
	// whitespace-only keys fail closed.
	for _, bad := range []string{"/etc/passwd", "../escape.json", "a/../../escape", ".", "", "   ", "\t"} {
		if _, err := materializeOpenCodePermissionConfig(testXDGDirs(t), "ask", map[string]string{bad: "x"}, nil); err == nil {
			t.Fatalf("seed path %q was not rejected", bad)
		}
	}

	// A malformed seeded opencode.json also fails closed.
	if _, err := materializeOpenCodePermissionConfig(testXDGDirs(t), "ask", map[string]string{"opencode.json": "not json"}, nil); err == nil {
		t.Fatal("malformed seeded opencode.json was accepted")
	}

	// Filesystem faults while writing a verbatim seed surface as errors.
	faultXDG := testXDGDirs(t)
	configDir := filepath.Join(faultXDG.Config, "opencode")
	if err := os.MkdirAll(filepath.Join(configDir, "isdir"), 0o700); err != nil {
		t.Fatalf("prepare seed dir clash: %v", err)
	}
	if _, err := materializeOpenCodePermissionConfig(faultXDG, "ask", map[string]string{"isdir": "x"}, nil); err == nil {
		t.Fatal("seed write over existing directory was accepted")
	}
	if err := os.WriteFile(filepath.Join(configDir, "afile"), []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare seed parent clash: %v", err)
	}
	if _, err := materializeOpenCodePermissionConfig(faultXDG, "ask", map[string]string{"afile/child.json": "x"}, nil); err == nil {
		t.Fatal("seed mkdir over existing file was accepted")
	}
}

// readOpenCodeSeedManifestForTest returns the sorted managed relpaths recorded
// in the seed-root ownership manifest.
func readOpenCodeSeedManifestForTest(t *testing.T, configDir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, openCodeSeedManifestName))
	if err != nil {
		t.Fatalf("read seed manifest: %v", err)
	}
	var manifest []string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("seed manifest is not a JSON array: %v", err)
	}

	return manifest
}

func TestOpenCodeSeedGuardManifestAndBackups(t *testing.T) {
	t.Run("empty root records every write in the manifest", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		seed := map[string]string{
			"opencode.json":      `{"provider":{"litellm":{"npm":"@ai-sdk/openai-compatible"}}}`,
			"themes/custom.json": `{"name":"custom"}`,
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", seed, nil); err != nil {
			t.Fatalf("materializeOpenCodePermissionConfig: %v", err)
		}

		want := []string{"opencode.json", "themes/custom.json"}
		if got := readOpenCodeSeedManifestForTest(t, configDir); !slicesEqualForTest(got, want) {
			t.Fatalf("manifest = %v, want %v", got, want)
		}
	})

	t.Run("re-seeding identical content creates no backup", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		seed := map[string]string{
			"opencode.json":      `{"provider":{"litellm":{"npm":"@ai-sdk/openai-compatible"}}}`,
			"themes/custom.json": `{"name":"custom"}`,
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", seed, nil); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", seed, nil); err != nil {
			t.Fatalf("second seed: %v", err)
		}

		for _, rel := range []string{"opencode.json", "themes/custom.json"} {
			backup := filepath.Join(configDir, filepath.FromSlash(rel)+openCodeSeedBackupSuffix)
			if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("idempotent re-seed created backup for %s (err=%v)", rel, err)
			}
		}
	})

	t.Run("changed managed content backs up prior bytes", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"first"}`,
		}, nil); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		firstOnDisk, err := os.ReadFile(filepath.Join(configDir, "themes", "custom.json"))
		if err != nil {
			t.Fatalf("read first seed: %v", err)
		}

		if _, seedErr := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"second"}`,
		}, nil); seedErr != nil {
			t.Fatalf("second seed: %v", seedErr)
		}

		backup, err := os.ReadFile(filepath.Join(configDir, "themes", "custom.json"+openCodeSeedBackupSuffix))
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		if string(backup) != string(firstOnDisk) {
			t.Fatalf("backup = %q, want prior bytes %q", backup, firstOnDisk)
		}
		updated, err := os.ReadFile(filepath.Join(configDir, "themes", "custom.json"))
		if err != nil {
			t.Fatalf("read updated seed: %v", err)
		}
		if string(updated) != `{"name":"second"}` {
			t.Fatalf("updated seed = %q, want second content", updated)
		}
	})

	t.Run("merged opencode.json is guarded and backed up on change", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		first, err := materializeOpenCodePermissionConfig(xdg, "ask", nil, nil)
		if err != nil {
			t.Fatalf("first seed: %v", err)
		}
		// A different immutable seed changes the merged bytes, so the guard must
		// back up the prior merged opencode.json.
		if _, seedErr := materializeOpenCodePermissionConfig(xdg, "allow", map[string]string{"opencode.json": `{"provider":{"x":{}}}`}, nil); seedErr != nil {
			t.Fatalf("second seed: %v", seedErr)
		}
		backup, err := os.ReadFile(filepath.Join(configDir, openCodeConfigFileName+openCodeSeedBackupSuffix))
		if err != nil {
			t.Fatalf("read merged backup: %v", err)
		}
		if string(backup) != first {
			t.Fatalf("merged backup = %q, want prior merged bytes %q", backup, first)
		}
	})
}

func TestOpenCodeSeedGuardOwnershipAndManifest(t *testing.T) {
	t.Run("pre-existing unmanaged file fails closed", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if err := os.MkdirAll(filepath.Join(configDir, "themes"), 0o700); err != nil {
			t.Fatalf("prepare operator dir: %v", err)
		}
		operator := filepath.Join(configDir, "themes", "custom.json")
		operatorBytes := []byte(`{"name":"operator-authored"}`)
		if err := os.WriteFile(operator, operatorBytes, 0o600); err != nil {
			t.Fatalf("write operator file: %v", err)
		}

		_, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"gateway"}`,
		}, nil)
		if err == nil {
			t.Fatal("seed over unmanaged operator file was accepted")
		}
		if !strings.Contains(err.Error(), "themes/custom.json") {
			t.Fatalf("error does not name offending relpath: %v", err)
		}

		// Nothing was written or changed: the operator file is intact, no merged
		// opencode.json was authored, and no manifest was created.
		if got, err := os.ReadFile(operator); err != nil || string(got) != string(operatorBytes) {
			t.Fatalf("operator file changed: got=%q err=%v", got, err)
		}
		if _, err := os.Stat(filepath.Join(configDir, openCodeConfigFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("merged opencode.json was written on fail-closed (err=%v)", err)
		}
		if _, err := os.Stat(filepath.Join(configDir, openCodeSeedManifestName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("manifest was created on fail-closed (err=%v)", err)
		}
	})

	t.Run("manifest survives across passes so managed files reseed", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"first"}`,
		}, nil); err != nil {
			t.Fatalf("first pass: %v", err)
		}
		// Second pass rewrites the same managed relpath: because the manifest
		// persisted, it is treated as owned rather than fail-closed.
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"second"}`,
		}, nil); err != nil {
			t.Fatalf("second pass rejected an owned file: %v", err)
		}
		manifest := readOpenCodeSeedManifestForTest(t, configDir)
		if !slicesContainsForTest(manifest, "themes/custom.json") {
			t.Fatalf("manifest lost managed relpath: %v", manifest)
		}
	})

	t.Run("corrupt manifest fails closed", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatalf("prepare config dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(configDir, openCodeSeedManifestName), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write corrupt manifest: %v", err)
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", nil, nil); err == nil {
			t.Fatal("corrupt manifest was accepted")
		}
	})
}

func slicesEqualForTest(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

func slicesContainsForTest(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}

	return false
}

func TestOpenCodeSeedGuardWriteFaults(t *testing.T) {
	t.Run("backup write failure surfaces", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"first"}`,
		}, nil); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		// Occupy the backup path with a directory so the .seed.bak write fails
		// when the managed file's content changes.
		backup := filepath.Join(configDir, "themes", "custom.json"+openCodeSeedBackupSuffix)
		if err := os.MkdirAll(backup, 0o700); err != nil {
			t.Fatalf("prepare backup clash: %v", err)
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"themes/custom.json": `{"name":"second"}`,
		}, nil); err == nil {
			t.Fatal("backup write failure was ignored")
		}
	})

	t.Run("mkdir failure surfaces", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatalf("prepare config dir: %v", err)
		}
		mkdirFailure := errors.New("mkdir failed")
		openCodeSeedMkdirAll = func(path string, mode os.FileMode) error {
			if path == filepath.Join(configDir, "ro", "sub") {
				return mkdirFailure
			}

			return os.MkdirAll(path, mode)
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", map[string]string{
			"ro/sub/child.json": "x",
		}, nil); !errors.Is(err, mkdirFailure) {
			t.Fatalf("mkdir failure = %v, want injected error", err)
		}
	})

	t.Run("target write failure surfaces", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			t.Fatalf("prepare config dir: %v", err)
		}
		writeFailure := errors.New("write failed")
		openCodeSeedWriteFile = func(path string, payload []byte, mode os.FileMode) error {
			if path == filepath.Join(configDir, openCodeConfigFileName) {
				return writeFailure
			}

			return os.WriteFile(path, payload, mode)
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", nil, nil); !errors.Is(err, writeFailure) {
			t.Fatalf("target write failure = %v, want injected error", err)
		}
	})

	t.Run("manifest marshal failure surfaces", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeMarshalIndent = func(value any, prefix, indent string) ([]byte, error) {
			if _, ok := value.([]string); ok {
				return nil, errors.New("manifest marshal failed")
			}

			return json.MarshalIndent(value, prefix, indent)
		}
		if _, err := materializeOpenCodePermissionConfig(testXDGDirs(t), "ask", map[string]string{
			"themes/custom.json": `{"name":"x"}`,
		}, nil); err == nil {
			t.Fatal("manifest marshal failure was ignored")
		}
	})

	t.Run("manifest read failure surfaces", func(t *testing.T) {
		xdg := testXDGDirs(t)
		configDir := filepath.Join(xdg.Config, "opencode")
		// A directory at the manifest path makes ReadFile fail with a
		// non-NotExist error, which must surface rather than be treated as absent.
		if err := os.MkdirAll(filepath.Join(configDir, openCodeSeedManifestName), 0o700); err != nil {
			t.Fatalf("prepare manifest clash: %v", err)
		}
		if _, err := materializeOpenCodePermissionConfig(xdg, "ask", nil, nil); err == nil {
			t.Fatal("manifest read failure was ignored")
		}
	})
}

func TestPortPasswordAndConfigFaultInjection(t *testing.T) {
	restoreOpenCodeClientSeams(t)

	openCodeListen = func(string, string) (net.Listener, error) {
		return fakeListener{addr: stringAddr("not tcp")}, nil
	}
	if _, err := allocatePort(); err == nil {
		t.Fatal("allocatePort accepted non-TCP listener")
	}
	openCodeListen = func(string, string) (net.Listener, error) {
		return nil, errors.New("listen failed")
	}
	if _, err := allocatePort(); err == nil {
		t.Fatal("allocatePort ignored listen error")
	}

	openCodeRandReader = errorReader{err: errors.New("entropy failed")}
	if _, err := randomPassword(); err == nil {
		t.Fatal("randomPassword ignored entropy error")
	}

	openCodeMarshalIndent = func(any, string, string) ([]byte, error) {
		return nil, errors.New("marshal failed")
	}
	if _, err := materializeOpenCodePermissionConfig(testXDGDirs(t), "ask", nil, nil); err == nil {
		t.Fatal("permission config ignored marshal error")
	}
}

func TestNativeUnmarshalErrors(t *testing.T) {
	var part NativePart
	if err := part.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("NativePart accepted malformed JSON")
	}
	var event Event
	if err := event.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("Event accepted malformed JSON")
	}
	var providers ProvidersResponse
	if err := providers.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("ProvidersResponse accepted malformed JSON")
	}
}

func TestFakeOpenCodeServerProcessHelper(t *testing.T) {
	if os.Getenv("ACP_GO_OPENCODE_FAKE_SERVER_HELPER") != "1" {
		return
	}
	runFakeOpenCodeServerProcess()
	os.Exit(0)
}

func fakeOpenCodeExecutable(t *testing.T) string {
	t.Helper()
	directory := testTraversableTempDir(t)
	script := filepath.Join(directory, "fake-opencode")
	body := fmt.Sprintf(
		"#!/bin/sh\nACP_GO_OPENCODE_FAKE_SERVER_HELPER=1 exec %q -test.run=TestFakeOpenCodeServerProcessHelper -- \"$@\"\n",
		reachableTestBinary(t, directory),
	)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}

	return script
}

// reachableTestBinary copies the test binary somewhere the isolated native
// identity can reach it. The product launches the fake executable as that
// identity, and the binary the go tool builds is a 0700 root-owned file inside
// a 0700 build directory, so exec'ing it in place fails for anyone but the
// runner and the launch dies before the server ever listens.
func reachableTestBinary(t *testing.T, directory string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	reachable := filepath.Join(directory, "fake-opencode-helper")
	if err = os.WriteFile(reachable, payload, 0o755); err != nil {
		t.Fatalf("publish test executable: %v", err)
	}

	return reachable
}

func runFakeOpenCodeServerProcess() {
	args := os.Args
	sep := slicesIndex(args, "--")
	if sep >= 0 {
		args = args[sep+1:]
	}
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintf(os.Stderr, "unexpected args: %q\n", strings.Join(args, " "))
		os.Exit(2)
	}
	port := ""
	for i := 1; i < len(args)-1; i++ {
		if args[i] == "--port" {
			port = args[i+1]
		}
	}
	if port == "" {
		fmt.Fprintln(os.Stderr, "missing --port")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "native stdout noise")
	fmt.Fprintln(os.Stderr, "native stderr noise")
	username := os.Getenv("OPENCODE_SERVER_USERNAME")
	password := os.Getenv("OPENCODE_SERVER_PASSWORD")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotUser, gotPassword, ok := r.BasicAuth(); !ok || gotUser != username || gotPassword != password {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		switch r.URL.Path {
		case "/global/health":
			writeJSONNoTest(w, map[string]any{"healthy": true, "version": "1.18.3"})
		case "/doc":
			writeJSONNoTest(w, fullOpenCodeDoc())
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n"))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {}
		default:
			writeJSONNoTest(w, map[string]any{"ok": true})
		}
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, handler); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func restoreOpenCodeClientSeams(t *testing.T) {
	t.Helper()
	commandContext := openCodeCommandContext
	startProcess := openCodeStartProcess
	applyCredential := openCodeApplyCredential
	supervisorCommandFn := openCodeSupervisorCommand
	httpClient := openCodeHTTPClient
	listen := openCodeListen
	randReader := openCodeRandReader
	marshalIndent := openCodeMarshalIndent
	seedMkdirAll := openCodeSeedMkdirAll
	seedWriteFile := openCodeSeedWriteFile
	terminateProcess := openCodeTerminateProcess
	killProcess := openCodeKillProcess
	waitCommand := openCodeWaitCommand
	removeAll := openCodeRemoveAll
	prepareRuntimeGeneration := openCodePrepareRuntimeGeneration
	after := openCodeAfter
	readyPoll := openCodeReadyPollInterval
	reconnectDelay := openCodeEventReconnectDelay
	shutdownTimeout := openCodeShutdownTimeout
	containmentTimeout := openCodeContainmentTimeout
	t.Cleanup(func() {
		openCodeCommandContext = commandContext
		openCodeStartProcess = startProcess
		openCodeApplyCredential = applyCredential
		openCodeSupervisorCommand = supervisorCommandFn
		openCodeHTTPClient = httpClient
		openCodeListen = listen
		openCodeRandReader = randReader
		openCodeMarshalIndent = marshalIndent
		openCodeSeedMkdirAll = seedMkdirAll
		openCodeSeedWriteFile = seedWriteFile
		openCodeTerminateProcess = terminateProcess
		openCodeKillProcess = killProcess
		openCodeWaitCommand = waitCommand
		openCodeRemoveAll = removeAll
		openCodePrepareRuntimeGeneration = prepareRuntimeGeneration
		openCodeAfter = after
		openCodeReadyPollInterval = readyPoll
		openCodeEventReconnectDelay = reconnectDelay
		openCodeShutdownTimeout = shutdownTimeout
		openCodeContainmentTimeout = containmentTimeout
	})
}

func testXDGDirs(t *testing.T) XDGDirs {
	t.Helper()
	root := testGeneratedTempDir(t)

	return XDGDirs{
		Root:   root,
		Data:   filepath.Join(root, "data"),
		Config: filepath.Join(root, "config"),
		Cache:  filepath.Join(root, "cache"),
		State:  filepath.Join(root, "state"),
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r errorReadCloser) Close() error {
	return nil
}

type eofCancelReadCloser struct {
	cancel context.CancelFunc
}

func (r eofCancelReadCloser) Read([]byte) (int, error) {
	r.cancel()

	return 0, io.EOF
}

func (r eofCancelReadCloser) Close() error {
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeListener struct {
	addr net.Addr
}

func (l fakeListener) Accept() (net.Conn, error) {
	return nil, errors.New("unused fake listener")
}

func (l fakeListener) Close() error {
	return nil
}

func (l fakeListener) Addr() net.Addr {
	return l.addr
}

type stringAddr string

func (a stringAddr) Network() string {
	return "string"
}

func (a stringAddr) String() string {
	return string(a)
}

func readinessClient(t *testing.T, handler http.HandlerFunc) (*openCodeServer, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	client := &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
		events:     make(chan Event, 8),
		errs:       make(chan error, 8),
		closed:     make(chan struct{}),
	}

	return client, func() {
		close(client.closed)
		server.Close()
	}
}

func writeSSE(t *testing.T, w http.ResponseWriter, payload string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeJSONNoTest(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func slicesIndex(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}

	return -1
}

// TestHealthAttemptDeadlineReleaseGate proves the readiness loop cannot inherit
// the shared HTTP client's 30-second timeout. The timed interval starts when a
// deliberately blocked /global/health RoundTrip begins and stops when its
// request context cancels. The local in-memory transport performs no network or
// provider work. With five samples, nearest-rank p95 is the slowest sample.
func TestHealthAttemptDeadlineReleaseGate(t *testing.T) {
	budgets := make([]time.Duration, 0, releaseGateRepetitions)
	elapsed := make([]time.Duration, 0, releaseGateRepetitions)

	for range releaseGateRepetitions {
		var healthCalls atomic.Int32
		eventCtx, cancelEvents := context.WithCancel(context.Background())
		client := &openCodeServer{
			httpClient: &http.Client{Timeout: 30 * time.Second, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case routeGlobalHealth:
					if healthCalls.Add(1) == 1 {
						deadline, ok := req.Context().Deadline()
						require.True(t, ok, "health attempt must carry its own deadline")
						started := time.Now()
						budgets = append(budgets, time.Until(deadline))
						<-req.Context().Done()
						elapsed = append(elapsed, time.Since(started))

						return nil, req.Context().Err()
					}

					return performanceJSONResponse(map[string]any{"healthy": true, "version": "1.18.3"}), nil
				case routeDoc:
					return performanceJSONResponse(fullOpenCodeDoc()), nil
				case routeEvent:
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(
							"data: {\"type\":\"server.connected\",\"properties\":{}}\n\n",
						)),
					}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Status:     "404 Not Found",
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				}
			})},
			baseURL: "http://opencode.release-gate",
			events:  make(chan Event, 8),
			errs:    make(chan error, 8),
			closed:  make(chan struct{}),
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := client.waitReady(ctx, eventCtx, StartOptions{MinVersion: "1.18.3"})
		cancel()
		cancelEvents()
		close(client.closed)
		require.NoError(t, err)
		require.EqualValues(t, 2, healthCalls.Load())
	}

	require.Len(t, budgets, releaseGateRepetitions)
	require.Len(t, elapsed, releaseGateRepetitions)
	slices.Sort(budgets)
	slices.Sort(elapsed)
	p95Budget := budgets[len(budgets)-1]
	p95Elapsed := elapsed[len(elapsed)-1]
	t.Logf("health-attempt release gate: repetitions=%d p95_budget=%s p95_elapsed=%s", releaseGateRepetitions, p95Budget, p95Elapsed)
	require.LessOrEqual(t, p95Budget, 500*time.Millisecond)
	require.Greater(t, p95Budget, 400*time.Millisecond)
	require.Less(t, p95Elapsed, time.Second, "health-attempt p95 must reject the fixed 30-second penalty")
}

// TestColdStartupReleaseGate is the deterministic provider-free adapter gate.
// It times StartServer from immediately before synthetic local process launch
// through health, /doc validation, and the first server.connected event.
// Fixture construction and shutdown are outside the interval. Each repetition
// uses a fresh local XDG root and a fake HTTP/SSE process that implements the
// version contract; this is not a physical OpenCode p95 claim.
// With five samples, nearest-rank p95 is the slowest sample.
func TestColdStartupReleaseGate(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	executable := fakeOpenCodeExecutable(t)
	durations := make([]time.Duration, 0, releaseGateRepetitions)

	for range releaseGateRepetitions {
		started := time.Now()
		client, err := StartServer(context.Background(), platformStartOptions(t, StartOptions{
			Root:            testGeneratedTempDir(t),
			ExecutablePath:  executable,
			MinVersion:      "1.18.3",
			HealthTimeout:   5 * time.Second,
			SkipVersionGate: false,
		}))
		durations = append(durations, time.Since(started))
		require.NoError(t, err)
		require.NoError(t, client.Shutdown(context.Background()))
	}

	slices.Sort(durations)
	p95 := durations[len(durations)-1]
	t.Logf("deterministic-adapter cold-start gate: repetitions=%d p95=%s", releaseGateRepetitions, p95)
	require.Less(t, p95, 5*time.Second)
}

func performanceJSONResponse(value any) *http.Response {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)),
	}
}
