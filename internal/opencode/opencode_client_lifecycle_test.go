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
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
)

const releaseGateRepetitions = 5

type testWriteCloser struct {
	close func() error
}

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	<-r.closed

	return 0, io.EOF
}

func (r *blockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })

	return nil
}

func (w testWriteCloser) Write(value []byte) (int, error) { return len(value), nil }
func (w testWriteCloser) Close() error {
	if w.close != nil {
		return w.close()
	}

	return nil
}

func requireSignalClosed(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

type openCodeMethodsRecorder struct {
	seen        []string
	messageBody MessageRequest
	commandBody CommandRequest
	forkBody    map[string]any
	createBody  map[string]any
	carrierBody []map[string]any
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
			writeJSON(t, w, map[string]any{"id": "s/1", "title": "Loaded", "metadata": map[string]any{"native": "kept"}})
		},
		"PATCH /session/s%2F1": func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			rec.carrierBody = append(rec.carrierBody, body)
			writeJSON(t, w, map[string]any{"id": "s/1"})
		},
		"PATCH /session/forked": func(w http.ResponseWriter, r *http.Request) {
			body := map[string]any{}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			rec.carrierBody = append(rec.carrierBody, body)
			writeJSON(t, w, map[string]any{"id": "forked"})
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
			writeJSON(t, w, map[string]any{"id": "forked", "metadata": map[string]any{"native": "kept"}})
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
		httpClient:  server.Client(),
		baseURL:     server.URL,
		username:    "opencode",
		password:    "secret",
		eventStream: make(chan EventStreamItem),
		closed:      make(chan struct{}),
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

func TestOpenCodeServerCarriesSessionCarrierOnAddressedSessions(t *testing.T) {
	ctx := context.Background()
	client, rec := newOpenCodeMethodsClient(t)
	client.directory = "/repo"
	client.sessionCarrierReference = "opaque-reference"

	_, err := client.CreateSession(ctx, "Created")
	require.NoError(t, err)
	want := client.sessionCarrierMetadata(nil)
	wantJSON, err := json.Marshal(want)
	require.NoError(t, err)
	actualJSON, err := json.Marshal(rec.createBody["metadata"])
	require.NoError(t, err)
	require.JSONEq(t, string(wantJSON), string(actualJSON))
	require.NotContains(t, string(actualJSON), "env")
	require.NotContains(t, string(actualJSON), "extraPathDirs")

	_, err = client.GetSession(ctx, "s/1")
	require.NoError(t, err)
	_, err = client.Fork(ctx, "s/1", "")
	require.NoError(t, err)
	want = client.sessionCarrierMetadata(map[string]any{"native": "kept"})
	wantJSON, err = json.Marshal(want)
	require.NoError(t, err)
	require.Len(t, rec.carrierBody, 2)
	for _, body := range rec.carrierBody {
		actualJSON, err = json.Marshal(body["metadata"])
		require.NoError(t, err)
		require.JSONEq(t, string(wantJSON), string(actualJSON))
	}
}

func TestOpenCodeServerMessageAndCommandMethods(t *testing.T) {
	ctx := context.Background()
	client, rec := newOpenCodeMethodsClient(t)

	if err := client.DispatchMessage(ctx, "s/1", MessageRequest{
		MessageID: "user-1",
		Model:     &ModelSelector{ProviderID: "openai", ModelID: "gpt-test"},
		Agent:     "build",
		Parts:     []map[string]any{{"type": "text", "text": "hello"}},
	}); err != nil || rec.messageBody.MessageID != "user-1" ||
		rec.messageBody.Model.ModelID != "gpt-test" || rec.messageBody.Agent != "build" || len(rec.messageBody.Parts) != 1 {
		t.Fatalf("DispatchMessage body=%#v err=%v", rec.messageBody, err)
	}
	messages, err := client.Messages(ctx, "s/1")
	if err != nil || len(messages) != 1 || messages[0].Info.ID != "assistant" {
		t.Fatalf("Messages = %#v err=%v", messages, err)
	}
	commands, err := client.Commands(ctx)
	if err != nil || len(commands) != 1 || commands[0].Name != "review" || commands[0].Template == nil || len(commands[0].Hints) != 1 {
		t.Fatalf("Commands = %#v err=%v", commands, err)
	}
	commandErr := client.DispatchCommand(ctx, "s/1", CommandRequest{
		MessageID: "user-2",
		Agent:     "build",
		Model:     "openai/gpt-test",
		Command:   "review",
		Arguments: "args",
		Parts:     []map[string]any{{"type": "file", "mime": "image/png", "url": "data:image/png;base64,AA=="}},
	})
	if commandErr != nil || rec.commandBody.MessageID != "user-2" ||
		rec.commandBody.Model != "openai/gpt-test" || rec.commandBody.Command != "review" || rec.commandBody.Arguments != "args" ||
		len(rec.commandBody.Parts) != 1 {
		t.Fatalf("DispatchCommand body=%#v err=%v", rec.commandBody, commandErr)
	}
}

func TestOpenCodeServerControlAndInfoMethods(t *testing.T) {
	ctx := context.Background()
	client, rec := newOpenCodeMethodsClient(t)

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
	if err := client.getJSON(ctx, "/status", nil, &map[string]any{}); err == nil ||
		strings.Contains(err.Error(), "short and stout") || !strings.Contains(err.Error(), "418 I'm a teapot") {
		t.Fatalf("status error was not classified and redacted: %v", err)
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

func TestOpenCodeDispatchErrors(t *testing.T) {
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
				err = client.DispatchCommand(ctx, "s", CommandRequest{Command: "review", Arguments: ""})
			} else {
				err = client.DispatchMessage(ctx, "s", MessageRequest{Parts: []map[string]any{{"type": "text", "text": "hello"}}})
			}
			if err == nil {
				t.Fatal("native send unexpectedly succeeded")
			}
		})
	}
}

func TestStartOpenCodeServerWithFakeExecutable(t *testing.T) {
	helper := fakeOpenCodeExecutable(t)
	root := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	client, err := StartServer(context.Background(), StartOptions{
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
	})
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
	// The runtime holds both locks in the control root beside the XDG root.
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
	if server.EventStream() == nil || server.XDGDirs().Root == "" {
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

func TestRuntimeShutdownIsBaseOwnedAndMemoizesOneResult(t *testing.T) {
	want := errors.New("native wait failed")
	terminal := make(chan struct{})
	var revokeCalls atomic.Int32
	process := ProcessHandle{
		Input: testWriteCloser{close: func() error {
			return want
		}},
		Output: io.NopCloser(strings.NewReader("")),
		Errors: io.NopCloser(strings.NewReader("")),
		Stop: func(context.Context) error {
			revokeCalls.Add(1)
			select {
			case <-terminal:
			default:
				close(terminal)
			}

			return nil
		},
		Await: func(context.Context) (ProcessOutcome, error) {
			<-terminal

			return ProcessOutcome{Revoked: true}, nil
		},
	}
	settlement := newProcessSettlement(process)
	settlement.start()
	state := newRuntimeShutdownState()
	reclaimed := false
	cleaned := false
	base := &openCodeServer{
		process: process, settlement: settlement, runtimeShutdown: state,
		runtimeClosed: make(chan struct{}),
		preparedTrees: []preparedNativeTree{{path: "/prepared", cleanup: func() error {
			cleaned = true

			return nil
		}}},
		reclaimTree: func(context.Context, string) error {
			reclaimed = true

			return nil
		},
	}
	scope := &openCodeServer{scopeCancel: func() {}, runtimeShutdown: state}
	require.ErrorIs(t, scope.Shutdown(context.Background()), ErrScopeRuntimeShutdown)

	first := base.Shutdown(context.Background())
	second := base.Shutdown(context.Background())
	require.ErrorIs(t, first, want)
	require.True(t, first == second, "shutdown must publish one memoized result")
	require.Equal(t, int32(1), revokeCalls.Load())
	require.True(t, reclaimed)
	require.True(t, cleaned)
	require.Empty(t, base.preparedTrees)
}

func TestRuntimeShutdownRetriesBusyReclaimWithoutRemoval(t *testing.T) {
	process := ProcessHandle{
		Input: testWriteCloser{}, Output: io.NopCloser(strings.NewReader("")), Errors: io.NopCloser(strings.NewReader("")),
		Stop: func(context.Context) error { return nil },
		Await: func(context.Context) (ProcessOutcome, error) {
			return ProcessOutcome{}, nil
		},
	}
	settlement := newProcessSettlement(process)
	settlement.start()
	<-settlement.done
	events := make([]string, 0, 4)
	busy := errors.New("tree busy")
	carrierBusy := true
	server := &openCodeServer{
		process: process, settlement: settlement, runtimeShutdown: newRuntimeShutdownState(),
		runtimeClosed: make(chan struct{}),
		preparedTrees: []preparedNativeTree{
			{path: "runtime", cleanup: func() error {
				events = append(events, "remove:runtime")

				return nil
			}},
			{path: "carrier", cleanup: func() error {
				events = append(events, "remove:carrier")

				return nil
			}},
		},
		reclaimTree: func(_ context.Context, path string) error {
			events = append(events, "reclaim:"+path)
			if path == "carrier" && carrierBusy {
				return MarkTreeReclaimPending(busy)
			}

			return nil
		},
	}

	first := server.Shutdown(t.Context())
	require.ErrorIs(t, first, busy)
	require.Equal(t, []string{"reclaim:carrier", "reclaim:runtime", "remove:runtime"}, events)
	require.Len(t, server.preparedTrees, 1)
	carrierBusy = false
	require.NoError(t, server.Shutdown(t.Context()))
	require.Equal(t, []string{
		"reclaim:carrier", "reclaim:runtime", "remove:runtime", "reclaim:carrier", "remove:carrier",
	}, events)
	require.Empty(t, server.preparedTrees)
}

func TestRuntimeShutdownBoundsStopAndClearsItAfterTerminalRetry(t *testing.T) {
	originalTimeout := processContainmentTimeout
	processContainmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { processContainmentTimeout = originalTimeout })

	var waits atomic.Int32
	process := ProcessHandle{
		Input: testWriteCloser{}, Output: io.NopCloser(strings.NewReader("")), Errors: io.NopCloser(strings.NewReader("")),
		Stop: func(ctx context.Context) error {
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			<-ctx.Done()

			return ctx.Err()
		},
		Await: func(ctx context.Context) (ProcessOutcome, error) {
			if waits.Add(1) == 1 {
				<-ctx.Done()

				return ProcessOutcome{}, ctx.Err()
			}

			return ProcessOutcome{}, nil
		},
	}
	settlement := newProcessSettlement(process)
	settlement.start()
	server := &openCodeServer{
		process: process, settlement: settlement, runtimeShutdown: newRuntimeShutdownState(),
		runtimeClosed: make(chan struct{}),
	}

	err := server.Shutdown(context.Background())
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.NoError(t, server.Shutdown(context.Background()))
}

func TestRuntimeShutdownReportsGeneratedScratchCleanupFailure(t *testing.T) {
	want := errors.New("remove generated runtime failed")
	process := ProcessHandle{
		Input: testWriteCloser{}, Output: io.NopCloser(strings.NewReader("")), Errors: io.NopCloser(strings.NewReader("")),
		Stop: func(context.Context) error { return nil },
		Await: func(context.Context) (ProcessOutcome, error) {
			return ProcessOutcome{}, nil
		},
	}
	settlement := newProcessSettlement(process)
	settlement.start()
	<-settlement.done
	cleanupCalls := 0
	cleanupFails := true
	server := &openCodeServer{
		process: process, settlement: settlement, runtimeShutdown: newRuntimeShutdownState(),
		runtimeClosed: make(chan struct{}),
		preparedTrees: []preparedNativeTree{{path: "runtime", cleanup: func() error {
			cleanupCalls++
			if !cleanupFails {
				return nil
			}

			return errors.Join(ErrRuntimeScratchCleanup, want)
		}}},
	}

	err := server.Shutdown(t.Context())
	require.ErrorIs(t, err, ErrRuntimeScratchCleanup)
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, cleanupCalls)
	cleanupFails = false
	require.NoError(t, server.Shutdown(t.Context()))
	require.Equal(t, 2, cleanupCalls)
	require.NoError(t, server.Shutdown(t.Context()))
	require.Equal(t, 2, cleanupCalls)
}

func TestStartFailureTransfersCleanupOnlyRetryAfterSuccessfulReclaim(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	want := errors.New("start refused")

	removeAll := openCodeRemoveAll
	failedRemoval := false
	openCodeRemoveAll = func(path string) error {
		if path == root && !failedRemoval {
			failedRemoval = true

			return errors.New("remove failed")
		}

		return removeAll(path)
	}
	t.Cleanup(func() { openCodeRemoveAll = removeAll })

	var (
		reclaims int
		retained reclaimedTreeCleanup
	)
	_, err := StartServer(t.Context(), StartOptions{
		Root: root, ControlRoot: ControlRootForXDG(root), RemoveRoot: true, Pure: true,
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		PrepareTree:       func(context.Context, string) error { return nil },
		ReclaimTree: func(context.Context, string) error {
			reclaims++

			return nil
		},
		RetainTree: func(path string, reclaimed bool, cleanup func() error) {
			retained = reclaimedTreeCleanup{path: path, reclaimed: reclaimed, cleanup: cleanup}
		},
		StartProcess: func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
			return ProcessHandle{}, MarkProcessStartSettled(want)
		},
	})
	require.ErrorIs(t, err, want)
	require.ErrorIs(t, err, ErrRuntimeScratchCleanup)
	require.Equal(t, 1, reclaims)
	require.Equal(t, root, retained.path)
	require.True(t, retained.reclaimed)
	require.NotNil(t, retained.cleanup)
	require.NoError(t, retained.cleanup())
	require.Equal(t, 1, reclaims, "cleanup-only retry reclaimed the same tree twice")
}

func TestDetachedReclaimIsFiniteAndRetainsTreeForRetry(t *testing.T) {
	originalTimeout := processContainmentTimeout
	processContainmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { processContainmentTimeout = originalTimeout })

	cleaned := false
	trees := []preparedNativeTree{{path: "runtime", cleanup: func() error {
		cleaned = true

		return nil
	}}}
	remaining, err := reclaimPreparedTreesDetached(trees, func(ctx context.Context, _ string) error {
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		<-ctx.Done()

		return ctx.Err()
	})
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.False(t, cleaned)
	require.Len(t, remaining, 1)

	want := errors.New("ordinary reclaim refusal")
	remaining, err = reclaimPreparedTreesDetached(remaining, func(context.Context, string) error { return want })
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.ErrorIs(t, err, want)
	require.False(t, cleaned)
	require.Len(t, remaining, 1)

	remaining, err = reclaimPreparedTreesDetached(remaining, func(context.Context, string) error { return nil })
	require.NoError(t, err)
	require.True(t, cleaned)
	require.Empty(t, remaining)
}

func TestFailedPublishedServerStartTransfersRemainingPreparedTrees(t *testing.T) {
	done := make(chan struct{})
	close(done)
	want := errors.Join(ErrProcessContainmentIncomplete, errors.New("terminal proof unavailable"))
	server := &openCodeServer{
		runtimeShutdown: &runtimeShutdownState{terminal: &runtimeShutdownAttempt{done: done, err: want}},
		preparedTrees:   []preparedNativeTree{{path: "runtime"}},
	}
	var retained []preparedNativeTree
	err := settleFailedServerStart(server, func(path string, reclaimed bool, cleanup func() error) {
		retained = append(retained, preparedNativeTree{path: path, reclaimed: reclaimed, cleanup: cleanup})
	}, errors.New("readiness failed"))
	require.ErrorIs(t, err, want)
	require.True(t, NativeCleanupRetained(err))
	require.Equal(t, []preparedNativeTree{{path: "runtime"}}, retained)
	require.Empty(t, server.preparedTrees)
}

type reclaimedTreeCleanup struct {
	path      string
	reclaimed bool
	cleanup   func() error
}

func TestScopesCreateNoNativeTreesOrRetireSharedRuntime(t *testing.T) {
	nativeRoot := t.TempDir()
	runtimeRoot := filepath.Join(nativeRoot, "runtime")
	carrierRoot := filepath.Join(runtimeRoot, "carrier")
	require.NoError(t, os.Mkdir(runtimeRoot, 0o700))
	require.NoError(t, os.Mkdir(carrierRoot, 0o700))

	runtimeClosed := make(chan struct{})
	carrierBroker := &sessionCarrierBroker{carriers: map[string]sessionCarrierPayload{}}
	base := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")),
			}, nil
		})},
		baseURL:              "http://opencode.test",
		runtimeShutdown:      newRuntimeShutdownState(),
		runtimeClosed:        runtimeClosed,
		sessionCarrierBroker: carrierBroker,
		preparedTrees:        []preparedNativeTree{{path: runtimeRoot}},
	}

	scopeClient, err := base.Scope(context.Background(), ScopeOptions{Directory: t.TempDir()})
	require.NoError(t, err)
	scope, ok := scopeClient.(*openCodeServer)
	require.True(t, ok)
	require.Empty(t, scope.preparedTrees)
	require.Equal(t, []preparedNativeTree{{path: runtimeRoot}}, base.preparedTrees)
	entries, err := os.ReadDir(nativeRoot)
	require.NoError(t, err)
	require.Equal(t, []string{"runtime"}, []string{entries[0].Name()})

	require.NoError(t, scope.Close(context.Background()))
	require.Empty(t, carrierBroker.carriers)
	select {
	case <-runtimeClosed:
		t.Fatal("closing one scope retired the shared runtime")
	default:
	}
	require.Equal(t, []preparedNativeTree{{path: runtimeRoot}}, base.preparedTrees)
}

func TestStartOpenCodeServerFaultInjection(t *testing.T) {
	ctx := context.Background()

	t.Run("create xdg", func(t *testing.T) {
		_, err := StartServer(ctx, StartOptions{Root: filepath.Join(t.TempDir(), string([]byte{0}))})
		require.Error(t, err)
	})

	t.Run("incomplete existing xdg", func(t *testing.T) {
		_, err := StartServer(ctx, StartOptions{ExistingXDG: XDGDirs{Root: filepath.Join(t.TempDir(), "root")}})
		require.Error(t, err)
	})

	t.Run("runtime config", func(t *testing.T) {
		_, err := StartServer(ctx, StartOptions{
			ExistingXDG: testXDGDirs(t), SeedFiles: map[string]string{"../escape": "bad"},
		})
		require.Error(t, err)
	})

	t.Run("allocate port", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeListen = func(string, string) (net.Listener, error) {
			return nil, errors.New("listen failed")
		}
		_, err := StartServer(ctx, StartOptions{ExistingXDG: testXDGDirs(t), Pure: true})
		require.ErrorContains(t, err, "listen failed")
	})

	t.Run("entropy", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeRandReader = errorReader{err: errors.New("entropy failed")}
		_, err := StartServer(ctx, StartOptions{ExistingXDG: testXDGDirs(t)})
		require.ErrorContains(t, err, "entropy failed")
	})

	t.Run("start process", func(t *testing.T) {
		want := errors.New("native spawn refused")
		_, err := StartServer(ctx, StartOptions{
			ExistingXDG: testXDGDirs(t), Pure: true,
			StartProcess: func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
				return ProcessHandle{}, want
			},
		})
		require.ErrorIs(t, err, want)
	})

	t.Run("incomplete process", func(t *testing.T) {
		_, err := StartServer(ctx, StartOptions{
			ExistingXDG: testXDGDirs(t), Pure: true,
			StartProcess: func(context.Context, string, []string, []string, string) (ProcessHandle, error) {
				return ProcessHandle{}, nil
			},
		})
		require.ErrorContains(t, err, "native process handle is incomplete")
	})

	t.Run("readiness", func(t *testing.T) {
		helper := fakeOpenCodeExecutable(t)
		_, err := StartServer(ctx, StartOptions{
			Root: t.TempDir(), ExecutablePath: helper, MinVersion: "99.0.0",
			HealthTimeout: 5 * time.Second, Logger: slog.New(slog.DiscardHandler),
		})
		require.ErrorContains(t, err, "below minimum supported")
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
		if err := client.readEventStream(ctx); err == nil || strings.Contains(err.Error(), "bad stream") {
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
		baseURL:     "http://opencode.test",
		username:    "opencode",
		password:    "secret",
		eventStream: make(chan EventStreamItem),
		closed:      make(chan struct{}),
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
	client.eventStream = make(chan EventStreamItem, 1)
	if err := client.readEventStream(ctx); !errors.Is(err, ErrSSEDisconnect) {
		t.Fatalf("multi-line clean EOF readEventStream error = %v", err)
	}
	if item := <-client.eventStream; item.Event == nil || item.Event.Type != "server.connected" {
		t.Fatalf("event item = %#v", item)
	}

	unterminatedStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n"))
	}))
	defer unterminatedStream.Close()
	client.httpClient = unterminatedStream.Client()
	client.baseURL = unterminatedStream.URL
	client.eventStream = make(chan EventStreamItem, 1)
	if err := client.readEventStream(ctx); !errors.Is(err, ErrSSEDisconnect) {
		t.Fatalf("unterminated clean EOF readEventStream error = %v", err)
	}
	if item := <-client.eventStream; item.Event == nil || item.Event.Type != "server.connected" {
		t.Fatalf("unterminated event item = %#v", item)
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
	client.eventStream = make(chan EventStreamItem, 1)
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
	client.eventStream = make(chan EventStreamItem)
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
		httpClient:  cancelledStream.Client(),
		baseURL:     cancelledStream.URL,
		username:    "opencode",
		password:    "secret",
		eventStream: make(chan EventStreamItem),
		closed:      make(chan struct{}),
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

// TestOpenCodeReadEventStreamReportsAClosedScopeAtCleanEnd proves a stream that
// ends cleanly on a scope that is already closed reports the close rather than a
// disconnect: the scope is gone, so there is nothing to reconnect to.
func TestOpenCodeReadEventStreamReportsAClosedScopeAtCleanEnd(t *testing.T) {
	restoreOpenCodeClientSeams(t)

	emptyStream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer emptyStream.Close()

	client := &openCodeServer{
		httpClient:  emptyStream.Client(),
		baseURL:     emptyStream.URL,
		username:    "opencode",
		password:    "secret",
		eventStream: make(chan EventStreamItem),
		closed:      make(chan struct{}),
	}
	close(client.closed)

	if err := client.readEventStream(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("closed scope clean end error = %v", err)
	}
}

// TestOpenCodeReadEventsStopsWhenItsSubscriptionIsCancelled proves an
// intentionally cancelled subscription publishes no terminal gap. Cancellation
// is the scope's own containment boundary, not evidence that a live transport
// lost an event.
func TestOpenCodeReadEventsStopsWhenItsSubscriptionIsCancelled(t *testing.T) {
	restoreOpenCodeClientSeams(t)

	requests := 0
	client := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
		baseURL:     "http://opencode.test",
		username:    "opencode",
		password:    "secret",
		eventStream: make(chan EventStreamItem, 1),
		closed:      make(chan struct{}),
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	client.readEvents(cancelled)

	if requests > 1 {
		t.Fatalf("requests = %d, want at most 1", requests)
	}

	select {
	case item := <-client.eventStream:
		t.Fatalf("cancelled stream published %#v", item)
	default:
	}
}

// TestOpenCodeReadEventsKeepsFinalEventsBeforeTheTerminalOnAFullChannel proves
// the single bounded channel is ordered and lossless at EOF. With only one
// retained item, the producer blocks instead of dropping either the final event
// or the terminal marker behind it.
func TestOpenCodeReadEventsKeepsFinalEventsBeforeTheTerminalOnAFullChannel(t *testing.T) {
	requests := 0
	client := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"first\",\"properties\":{}}\n\n" +
						"data: {\"type\":\"final\",\"properties\":{}}\n\n",
				)),
			}, nil
		})},
		baseURL:     "http://opencode.test",
		eventStream: make(chan EventStreamItem, 1),
		closed:      make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		client.readEvents(context.Background())
		close(done)
	}()

	first := <-client.eventStream
	require.NotNil(t, first.Event)
	require.Equal(t, "first", first.Event.Type)
	require.NoError(t, first.Terminal)

	final := <-client.eventStream
	require.NotNil(t, final.Event)
	require.Equal(t, "final", final.Event.Type)
	require.NoError(t, final.Terminal)

	terminal := <-client.eventStream
	require.Nil(t, terminal.Event)
	require.ErrorIs(t, terminal.Terminal, ErrSSEDisconnect)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event reader did not finish after publishing its terminal marker")
	}
	require.Equal(t, 1, requests, "a terminal gap opened another stream for the fenced generation")
}

func TestOpenCodeReadEventsKeepsFinalEventBeforeBodyErrorOnAFullChannel(t *testing.T) {
	requests := 0
	bodyErr := errors.New("SSE body failed")
	client := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++

			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(io.MultiReader(
					strings.NewReader("data: {\"type\":\"final\",\"properties\":{}}\n\n"),
					errorReadCloser{err: bodyErr},
				)),
			}, nil
		})},
		baseURL:     "http://opencode.test",
		eventStream: make(chan EventStreamItem, 1),
		closed:      make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		client.readEvents(context.Background())
		close(done)
	}()

	final := <-client.eventStream
	require.NotNil(t, final.Event)
	require.Equal(t, "final", final.Event.Type)
	require.NoError(t, final.Terminal)

	terminal := <-client.eventStream
	require.Nil(t, terminal.Event)
	require.ErrorIs(t, terminal.Terminal, bodyErr)
	requireSignalClosed(t, done, "event reader did not finish after its body error")
	require.Equal(t, 1, requests)
}

func TestOpenCodeReadEventsContainsBodyReaderPanicAsTerminal(t *testing.T) {
	client := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       panicReadCloser{},
			}, nil
		})},
		baseURL:     "http://opencode.test",
		eventStream: make(chan EventStreamItem, 1),
		closed:      make(chan struct{}),
	}

	client.readEvents(context.Background())
	item := <-client.eventStream
	require.Nil(t, item.Event)
	require.ErrorContains(t, item.Terminal, "SSE reader panicked")
	require.NotContains(t, item.Terminal.Error(), "SECRET_BODY_READER_PANIC")
}

func TestOpenCodeReadEventsContainsProducerPanicAndCancelledPublication(t *testing.T) {
	client := &openCodeServer{
		eventStream: make(chan EventStreamItem, 1),
		closed:      make(chan struct{}),
		eventReader: func(context.Context) error { panic("SECRET_SSE_PRODUCER_PANIC") },
	}

	client.readEvents(context.Background())
	item := <-client.eventStream
	require.ErrorContains(t, item.Terminal, "SSE producer panicked")
	require.NotContains(t, item.Terminal.Error(), "SECRET_SSE_PRODUCER_PANIC")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.publishEventStreamTerminal(ctx, errors.New("ignored"))
	require.Empty(t, client.eventStream)
	client.publishEventStreamTerminal(context.Background(), nil)
}

func TestSSEAggregateLimitBoundsMultilineMalformedAndUnterminatedEvents(t *testing.T) {
	for name, body := range map[string]string{
		"multiline":    "data: 12345678901234567890\ndata: 12345678901234567890\n\n",
		"malformed":    "data: {{{{{{{{{{{{{{{{{{{{\ndata: ]]]]]]]]]]]]]]]]]]]]\n\n",
		"unterminated": "data: 12345678901234567890\ndata: 12345678901234567890\n",
	} {
		t.Run(name, func(t *testing.T) {
			client := &openCodeServer{
				httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Status:     "200 OK",
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader(body)),
					}, nil
				})},
				baseURL:     "http://opencode.test",
				eventStream: make(chan EventStreamItem, 1),
				closed:      make(chan struct{}),
			}

			require.ErrorIs(t, client.readEventStreamWithLimit(context.Background(), 32), ErrSSEEventTooLarge)
		})
	}
}

func TestOpenCodeReadEventsPublishesNoTerminalForAnIntentionalClose(t *testing.T) {
	client := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"blocked\",\"properties\":{}}\n\n",
				)),
			}, nil
		})},
		baseURL:     "http://opencode.test",
		eventStream: make(chan EventStreamItem),
		closed:      make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		client.readEvents(context.Background())
		close(done)
	}()
	close(client.closed)
	requireSignalClosed(t, done, "event reader did not stop with its scope")

	select {
	case item := <-client.eventStream:
		t.Fatalf("intentional close published stream item %#v", item)
	default:
	}
}

func TestOpenCodeServerCloseDetachesAndRetryProvesTerminalAfterJoiningWorkers(t *testing.T) {
	originalTimeout := processContainmentTimeout
	processContainmentTimeout = 20 * time.Millisecond
	t.Cleanup(func() { processContainmentTimeout = originalTimeout })

	observerStarted := make(chan struct{})
	observerDone := make(chan struct{})
	terminalWaitDone := make(chan struct{})
	var waitCalls atomic.Int32
	var terminal atomic.Bool
	stdout := newBlockingReadCloser()
	stderr := newBlockingReadCloser()
	stopped := make(chan struct{}, 2)
	stopRelease := make(chan struct{})
	process := ProcessHandle{
		Input: testWriteCloser{}, Output: stdout, Errors: stderr,
		Stop: func(ctx context.Context) error {
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			stopped <- struct{}{}
			<-stopRelease

			return nil
		},
		Await: func(ctx context.Context) (ProcessOutcome, error) {
			call := waitCalls.Add(1)
			_, hasDeadline := ctx.Deadline()

			switch call {
			case 1:
				close(observerStarted)
			default:
				require.True(t, hasDeadline)
			}

			if terminal.Load() {
				return ProcessOutcome{Revoked: true}, nil
			}

			<-ctx.Done()

			switch call {
			case 1:
				close(observerDone)
			case 2:
				close(terminalWaitDone)
			default:
			}

			return ProcessOutcome{}, ctx.Err()
		},
	}
	settlement := newProcessSettlement(process)
	settlement.start()
	<-observerStarted

	runtimeExited := make(chan struct{})
	runtimeWatchDone := make(chan struct{})
	go observeProcessSettlement(settlement, runtimeExited, runtimeWatchDone)
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() { defer close(stdoutDone); drainProcessPipe(nil, "stdout", stdout) }()
	go func() { defer close(stderrDone); drainProcessPipe(nil, "stderr", stderr) }()

	reclaimCalls := atomic.Int32{}
	server := &openCodeServer{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusMethodNotAllowed,
				Status:     "405 Method Not Allowed",
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
		baseURL: "http://opencode.test",
		process: process, settlement: settlement,
		runtimeShutdown: newRuntimeShutdownState(), runtimeClosed: make(chan struct{}), runtimeExited: runtimeExited,
		runtimeWatchDone: runtimeWatchDone, stdoutDone: stdoutDone, stderrDone: stderrDone,
		preparedTrees: []preparedNativeTree{{path: "runtime"}},
		reclaimTree: func(ctx context.Context, _ string) error {
			_, hasDeadline := ctx.Deadline()
			require.True(t, hasDeadline)
			reclaimCalls.Add(1)

			return nil
		},
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, server.Shutdown(cancelled), context.Canceled)
	requireSignalClosed(t, stopped, "shutdown never initiated revoke")
	server.runtimeShutdown.mu.Lock()
	firstAttempt := server.runtimeShutdown.current
	server.runtimeShutdown.mu.Unlock()
	require.NotNil(t, firstAttempt)
	close(stopRelease)

	requireSignalClosed(t, observerDone, "shutdown did not join the owned observer")
	requireSignalClosed(t, terminalWaitDone, "shutdown did not bound terminal wait")
	requireSignalClosed(t, stdoutDone, "shutdown did not join stdout drain")
	requireSignalClosed(t, stderrDone, "shutdown did not join stderr drain")
	requireSignalClosed(t, runtimeWatchDone, "shutdown did not join runtime watcher")

	<-firstAttempt.done
	require.ErrorIs(t, firstAttempt.err, ErrProcessContainmentIncomplete)
	require.Len(t, server.preparedTrees, 1)
	require.Zero(t, reclaimCalls.Load())

	terminal.Store(true)
	require.NoError(t, server.Shutdown(context.Background()))
	require.True(t, server.RuntimeRevoked())
	require.Empty(t, server.preparedTrees)
	require.EqualValues(t, 1, reclaimCalls.Load())
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
	permissionConfig, err := materializeOpenCodeRuntimeConfig(xdg, nil, "")
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
	env, err := buildProcessEnvironmentFrom(
		map[string]string{"PATH": "/usr/bin:/bin"},
		map[string]string{"A": "1"}, map[string]string{"A": "2", "B": "3"},
	)
	if err != nil || env["A"] != "2" || env["B"] != "3" {
		t.Fatalf("merged env = %#v, err = %v", env, err)
	}
	var processLog bytes.Buffer
	drainProcessPipe(slog.New(slog.NewTextHandler(&processLog, nil)), "test",
		strings.NewReader("SECRET_SENTINEL\n"))
	if strings.Contains(processLog.String(), "SECRET_SENTINEL") {
		t.Fatalf("child output reached logs: %q", processLog.String())
	}
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
	return materializeOpenCodeRuntimeConfig(dirs, seeds, "")
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
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	script := filepath.Join(directory, "fake-opencode")
	body := fmt.Sprintf(
		"#!/bin/sh\nACP_GO_OPENCODE_FAKE_SERVER_HELPER=1 exec %q -test.run=TestFakeOpenCodeServerProcessHelper -- \"$@\"\n",
		executable,
	)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}

	return script
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
		if r.URL.Query().Get("directory") != "" {
			instantiateFakeSessionCarrierPlugin()
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

// publishFakeSessionCarrierProof performs the startup proof the generated
// plugin makes on its first load: one authorized POST to the broker's ready
// route, using the endpoint and bearer token the runtime wrote into the module.
func publishFakeSessionCarrierProof(source string) {
	endpoint, endpointOK := fakePluginConstant(source, "BROKER_ENDPOINT")
	token, tokenOK := fakePluginConstant(source, "BROKER_TOKEN")

	if !endpointOK || !tokenOK {
		return
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint+"/ready", nil)
	if err != nil {
		return
	}

	request.Header.Set("Authorization", "Bearer "+token)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return
	}

	_ = response.Body.Close()
}

// instantiateFakeSessionCarrierPlugin does for the fake native process what a
// real OpenCode does when a directory-scoped request first reaches it: it loads
// the generated plugin module, which publishes the marker the runtime demands
// before it will serve a session. The fake reads the module the product
// actually wrote, so a plugin the runtime never registered stays unproven here
// exactly as it would natively.
func instantiateFakeSessionCarrierPlugin() {
	var config struct {
		Plugin []string `json:"plugin"`
	}
	if json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &config) != nil {
		return
	}
	for _, plugin := range config.Plugin {
		parsed, err := url.Parse(plugin)
		if err != nil || parsed.Scheme != "file" {
			continue
		}
		source, err := os.ReadFile(parsed.Path)
		if err != nil {
			continue
		}
		publishFakeSessionCarrierProof(string(source))
	}
}

func fakePluginConstant(source string, name string) (string, bool) {
	_, rest, ok := strings.Cut(source, "const "+name+" = ")
	if !ok {
		return "", false
	}
	literal, _, ok := strings.Cut(rest, "\n")
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal([]byte(literal), &value) != nil {
		return "", false
	}

	return value, true
}

func restoreOpenCodeClientSeams(t *testing.T) {
	t.Helper()
	acquireHomeLock := openCodeAcquireHomeLock
	httpClient := openCodeHTTPClient
	listen := openCodeListen
	randReader := openCodeRandReader
	marshalIndent := openCodeMarshalIndent
	seedMkdirAll := openCodeSeedMkdirAll
	seedWriteFile := openCodeSeedWriteFile
	removeAll := openCodeRemoveAll
	after := openCodeAfter
	readyPoll := openCodeReadyPollInterval
	shutdownTimeout := openCodeShutdownTimeout
	t.Cleanup(func() {
		openCodeAcquireHomeLock = acquireHomeLock
		openCodeHTTPClient = httpClient
		openCodeListen = listen
		openCodeRandReader = randReader
		openCodeMarshalIndent = marshalIndent
		openCodeSeedMkdirAll = seedMkdirAll
		openCodeSeedWriteFile = seedWriteFile
		openCodeRemoveAll = removeAll
		openCodeAfter = after
		openCodeReadyPollInterval = readyPoll
		openCodeShutdownTimeout = shutdownTimeout
	})
}

func testXDGDirs(t *testing.T) XDGDirs {
	t.Helper()
	root := t.TempDir()

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

type panicReadCloser struct{}

func (panicReadCloser) Read([]byte) (int, error) { panic("SECRET_BODY_READER_PANIC") }
func (panicReadCloser) Close() error             { return nil }

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
		httpClient:  server.Client(),
		baseURL:     server.URL,
		username:    "opencode",
		password:    "secret",
		eventStream: make(chan EventStreamItem, 8),
		closed:      make(chan struct{}),
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
			baseURL:     "http://opencode.release-gate",
			eventStream: make(chan EventStreamItem, 8),
			closed:      make(chan struct{}),
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
	executable := fakeOpenCodeExecutable(t)
	durations := make([]time.Duration, 0, releaseGateRepetitions)

	for range releaseGateRepetitions {
		started := time.Now()
		client, err := StartServer(context.Background(), StartOptions{
			Root:            t.TempDir(),
			ExecutablePath:  executable,
			MinVersion:      "1.18.3",
			HealthTimeout:   5 * time.Second,
			SkipVersionGate: false,
			Pure:            true,
		})
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
