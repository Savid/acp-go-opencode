package opencodeacp

import (
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
	"strings"
	"testing"
	"time"
)

func TestOpenCodeServerHTTPMethodsAndErrors(t *testing.T) {
	ctx := context.Background()
	var seen []string
	var messageBody openCodeMessageRequest
	var forkBody map[string]any
	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if username, password, ok := r.BasicAuth(); !ok || username != "opencode" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("bad auth"))
			return
		}
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		path := r.URL.EscapedPath()
		switch {
		case path == "/session" && r.Method == http.MethodPost:
			createBody = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "created", "title": "Created"})
		case path == "/session" && r.Method == http.MethodGet:
			if r.URL.Query().Get("directory") != "/repo" {
				t.Errorf("directory query = %q", r.URL.RawQuery)
			}
			writeJSON(t, w, []map[string]any{{"id": "listed"}})
		case path == "/session/s%2F1" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"id": "s/1", "title": "Loaded"})
		case path == "/session/s%2F1" && r.Method == http.MethodDelete:
			writeJSON(t, w, map[string]any{"ok": true})
		case path == "/session/s%2F1/message" && r.Method == http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&messageBody); err != nil {
				t.Errorf("decode message body: %v", err)
			}
			writeJSON(t, w, map[string]any{"info": map[string]any{"id": "assistant", "sessionID": "s/1"}})
		case path == "/session/s%2F1/message" && r.Method == http.MethodGet:
			writeJSON(t, w, []map[string]any{{"info": map[string]any{"id": "message"}}})
		case path == "/session/s%2F1/abort" && r.Method == http.MethodPost:
			writeJSON(t, w, map[string]any{"ok": true})
		case path == "/session/s%2F1/fork" && r.Method == http.MethodPost:
			forkBody = map[string]any{}
			if err := json.NewDecoder(r.Body).Decode(&forkBody); err != nil {
				t.Errorf("decode fork body: %v", err)
			}
			writeJSON(t, w, map[string]any{"id": "forked"})
		case path == "/session/s%2F1/todo" && r.Method == http.MethodGet:
			writeJSON(t, w, []map[string]any{{"id": "todo", "content": "Do it"}})
		case path == "/config/providers" && r.Method == http.MethodGet:
			writeJSON(t, w, map[string]any{"providers": []map[string]any{{"id": "openai", "models": map[string]any{}}}})
		case path == "/agent" && r.Method == http.MethodGet:
			writeJSON(t, w, []map[string]any{{"name": "build"}})
		case path == "/empty" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNoContent)
		case path == "/invalid-json" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte("{"))
		case path == "/status" && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("short and stout"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
		events:     make(chan openCodeEvent),
		errs:       make(chan error),
		closed:     make(chan struct{}),
	}

	created, err := client.CreateSession(ctx, "Created")
	if err != nil || created.ID != "created" || createBody["title"] != "Created" {
		t.Fatalf("CreateSession = %#v body=%#v err=%v", created, createBody, err)
	}
	created, err = client.CreateSession(ctx, "")
	if err != nil || len(createBody) != 0 {
		t.Fatalf("CreateSession empty = %#v body=%#v err=%v", created, createBody, err)
	}
	got, err := client.GetSession(ctx, "s/1")
	if err != nil || got.ID != "s/1" {
		t.Fatalf("GetSession = %#v err=%v", got, err)
	}
	listed, err := client.ListSessions(ctx, "/repo")
	if err != nil || len(listed) != 1 || listed[0].ID != "listed" {
		t.Fatalf("ListSessions = %#v err=%v", listed, err)
	}
	if err := client.DeleteSession(ctx, "s/1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	message, err := client.SendMessage(ctx, "s/1", openCodeMessageRequest{NoReply: true})
	if err != nil || message.Info.ID != "assistant" || !messageBody.NoReply {
		t.Fatalf("SendMessage = %#v body=%#v err=%v", message, messageBody, err)
	}
	messages, err := client.Messages(ctx, "s/1")
	if err != nil || len(messages) != 1 || messages[0].Info.ID != "message" {
		t.Fatalf("Messages = %#v err=%v", messages, err)
	}
	if err := client.Abort(ctx, "s/1"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	forked, err := client.Fork(ctx, "s/1", "message-1")
	if err != nil || forked.ID != "forked" || forkBody["messageID"] != "message-1" {
		t.Fatalf("Fork = %#v body=%#v err=%v", forked, forkBody, err)
	}
	forked, err = client.Fork(ctx, "s/1", "")
	if err != nil || len(forkBody) != 0 {
		t.Fatalf("Fork empty = %#v body=%#v err=%v", forked, forkBody, err)
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
	if !containsString(seen, "GET /session?directory=%2Frepo") {
		t.Fatalf("seen paths = %#v", seen)
	}
}

func TestStartOpenCodeServerWithFakeExecutable(t *testing.T) {
	helper := fakeOpenCodeExecutable(t)
	root := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	client, err := startOpenCodeServer(context.Background(), openCodeStartOptions{
		ACPSessionID:     "session/one",
		Root:             root,
		Cwd:              t.TempDir(),
		ExecutablePath:   helper,
		DefaultModel:     "openai/gpt-test",
		Env:              map[string]string{"BASE_ENV": "base"},
		AdditionalEnv:    map[string]string{"EXTRA_ENV": "extra"},
		Pure:             true,
		QuestionTool:     true,
		LogLevel:         "DEBUG",
		MinimumVersion:   "1.0.0",
		HealthTimeout:    5 * time.Second,
		Logger:           logger,
		SkipVersionGate:  false,
		ExpectedNativeID: "native",
	})
	if err != nil {
		t.Fatalf("startOpenCodeServer: %v", err)
	}
	server := client.(*openCodeServer)
	if server.xdg.Root == "" || !strings.Contains(filepath.Base(server.xdg.Root), "session_one") {
		t.Fatalf("xdg dirs = %#v", server.xdg)
	}
	if _, err := os.Stat(filepath.Join(server.xdg.State, leaseFileName)); err != nil {
		t.Fatalf("lease was not written: %v", err)
	}
	if server.Events() == nil || server.EventErrors() == nil || server.XDGDirs().Root == "" {
		t.Fatalf("server channels/dirs not initialized: %#v", server)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(server.xdg.State, leaseFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease after close err = %v", err)
	}
}

func TestStartOpenCodeServerFaultInjection(t *testing.T) {
	ctx := context.Background()

	t.Run("defaults and start error", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		xdg := testXDGDirs(t)
		var executable string
		openCodeCommandContext = func(ctx context.Context, name string, _ ...string) *exec.Cmd {
			executable = name
			return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-opencode"))
		}
		_, err := startOpenCodeServer(ctx, openCodeStartOptions{ExistingXDG: xdg})
		if err == nil {
			t.Fatal("missing executable unexpectedly started")
		}
		if executable != "opencode" {
			t.Fatalf("default executable = %q", executable)
		}
	})

	t.Run("reap failure", func(t *testing.T) {
		if _, err := startOpenCodeServer(ctx, openCodeStartOptions{Root: "["}); err == nil {
			t.Fatal("invalid reap glob unexpectedly succeeded")
		}
	})

	t.Run("create xdg failure", func(t *testing.T) {
		_, err := startOpenCodeServer(ctx, openCodeStartOptions{
			Root:         t.TempDir(),
			ACPSessionID: acpSessionIDString(string([]byte{0})),
		})
		if err == nil {
			t.Fatal("invalid session xdg path unexpectedly succeeded")
		}
	})

	t.Run("ensure existing xdg failure", func(t *testing.T) {
		_, err := startOpenCodeServer(ctx, openCodeStartOptions{
			ExistingXDG: xdgDirs{Root: filepath.Join(t.TempDir(), "root")},
		})
		if err == nil {
			t.Fatal("incomplete existing xdg unexpectedly succeeded")
		}
	})

	t.Run("allocate port failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeListen = func(string, string) (net.Listener, error) {
			return nil, errors.New("listen failed")
		}
		if _, err := startOpenCodeServer(ctx, openCodeStartOptions{ExistingXDG: testXDGDirs(t)}); err == nil {
			t.Fatal("listen error was ignored")
		}
	})

	t.Run("password entropy failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeRandReader = errorReader{err: errors.New("entropy failed")}
		if _, err := startOpenCodeServer(ctx, openCodeStartOptions{ExistingXDG: testXDGDirs(t)}); err == nil {
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
		if _, err := startOpenCodeServer(ctx, openCodeStartOptions{ExistingXDG: testXDGDirs(t)}); err == nil {
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
		if _, err := startOpenCodeServer(ctx, openCodeStartOptions{ExistingXDG: testXDGDirs(t)}); err == nil {
			t.Fatal("stderr pipe error was ignored")
		}
	})

	t.Run("lease write failure", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		openCodeWriteLease = func(string, serverLease) error {
			return errors.New("lease failed")
		}
		if _, err := startOpenCodeServer(ctx, openCodeStartOptions{ExistingXDG: testXDGDirs(t)}); err == nil {
			t.Fatal("lease error was ignored")
		}
	})

	t.Run("post-start lease write failure kills process", func(t *testing.T) {
		restoreOpenCodeClientSeams(t)
		helper := fakeOpenCodeExecutable(t)
		writeCount := 0
		killed := false
		openCodeWriteLease = func(string, serverLease) error {
			writeCount++
			if writeCount == 2 {
				return errors.New("post-start lease failed")
			}
			return nil
		}
		openCodeKillProcess = func(*exec.Cmd) error {
			killed = true
			return nil
		}
		_, err := startOpenCodeServer(ctx, openCodeStartOptions{
			Root:           t.TempDir(),
			ExecutablePath: helper,
			HealthTimeout:  5 * time.Second,
		})
		if err == nil || !strings.Contains(err.Error(), "post-start lease failed") {
			t.Fatalf("post-start lease err = %v", err)
		}
		if !killed {
			t.Fatal("post-start lease failure did not kill process")
		}
	})

	t.Run("readiness failure closes process", func(t *testing.T) {
		helper := fakeOpenCodeExecutable(t)
		_, err := startOpenCodeServer(ctx, openCodeStartOptions{
			Root:            t.TempDir(),
			ExecutablePath:  helper,
			MinimumVersion:  "99.0.0",
			HealthTimeout:   2 * time.Second,
			SkipVersionGate: false,
			Logger:          slog.New(slog.DiscardHandler),
		})
		if err == nil || !strings.Contains(err.Error(), "below minimum") {
			t.Fatalf("readiness error = %v", err)
		}
	})
}

func TestOpenCodeServerReadinessFailuresAndStreams(t *testing.T) {
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
		if err := client.waitReady(ctx, ctx, openCodeStartOptions{MinimumVersion: "9.0.0"}); err == nil {
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
		if err := client.waitReady(ctx, ctx, openCodeStartOptions{SkipVersionGate: true}); err == nil {
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
		if err := client.waitReady(shortCtx, shortCtx, openCodeStartOptions{SkipVersionGate: true}); err == nil ||
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
		if err := client.waitReady(shortCtx, shortCtx, openCodeStartOptions{SkipVersionGate: true}); !errors.Is(err, context.DeadlineExceeded) {
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
				if err := client.waitReady(ctx, ctx, openCodeStartOptions{SkipVersionGate: true}); err == nil {
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
		if err := client.waitReady(ctx, ctx, openCodeStartOptions{SkipVersionGate: true}); err == nil ||
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
				time.Sleep(50 * time.Millisecond)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
		defer closeServer()
		shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := client.waitReady(shortCtx, context.Background(), openCodeStartOptions{SkipVersionGate: true}); !errors.Is(err, context.DeadlineExceeded) {
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
		events:   make(chan openCodeEvent),
		errs:     make(chan error, 1),
		closed:   make(chan struct{}),
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
	client.events = make(chan openCodeEvent, 1)
	if err := client.readEventStream(ctx); err != nil {
		t.Fatalf("multi-line readEventStream: %v", err)
	}
	if event := <-client.events; event.Type != "server.connected" {
		t.Fatalf("event = %#v", event)
	}

	close(client.closed)
	client.events = make(chan openCodeEvent)
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
		events:     make(chan openCodeEvent),
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

func TestOpenCodeServerCloseTimeoutAndContext(t *testing.T) {
	for name, cancelled := range map[string]bool{"timeout": false, "context": true} {
		t.Run(name, func(t *testing.T) {
			restoreOpenCodeClientSeams(t)
			cmd := &exec.Cmd{Process: &os.Process{Pid: 1234}}
			openCodeTerminateProcess = func(*exec.Cmd) error { return nil }
			openCodeKillProcess = func(*exec.Cmd) error { return nil }
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
			if err := server.Close(ctx); err == nil {
				t.Fatal("Close unexpectedly succeeded")
			}
		})
	}
}

func TestXDGLeaseEnvAndPipeHelpers(t *testing.T) {
	root := t.TempDir()
	xdg, err := createXDGDirs(root, "")
	if err != nil {
		t.Fatalf("createXDGDirs: %v", err)
	}
	if filepath.Base(xdg.Root) != "session" {
		t.Fatalf("default xdg root = %#v", xdg)
	}
	if err := ensureXDGDirs(xdgDirs{Root: "", Data: "x", Config: "x", Cache: "x", State: "x"}); err == nil {
		t.Fatal("ensureXDGDirs accepted empty root")
	}
	port, err := allocatePort()
	if err != nil || port <= 0 {
		t.Fatalf("allocatePort = %d err=%v", port, err)
	}
	password, err := randomPassword()
	if err != nil || password == "" {
		t.Fatalf("randomPassword = %q err=%v", password, err)
	}
	if err := writeLease(xdg.State, serverLease{PID: 0, Port: port, PasswordHash: passwordHash(password)}); err != nil {
		t.Fatalf("writeLease: %v", err)
	}
	badRoot := string([]byte{0})
	if err := writeLease(badRoot, serverLease{}); err == nil {
		t.Fatal("writeLease accepted invalid path")
	}
	if err := os.MkdirAll(filepath.Join(root, "bad", "state"), 0o700); err != nil {
		t.Fatalf("mkdir bad lease: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad", "state", leaseFileName), []byte("{"), 0o600); err != nil {
		t.Fatalf("write bad lease: %v", err)
	}
	if err := reapStaleLeases(root, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("reapStaleLeases: %v", err)
	}
	if err := reapStaleLeases("", nil); err != nil {
		t.Fatalf("empty reapStaleLeases: %v", err)
	}
	env := mergeProcessEnv(map[string]string{"": "skip", "A": "1"}, map[string]string{"A": "2", "B": "3"})
	if env["A"] != "2" || env["B"] != "3" {
		t.Fatalf("merged env = %#v", env)
	}
	drainProcessPipe(slog.New(slog.DiscardHandler), "test", strings.NewReader("one\ntwo\n"))
	for _, value := range []any{float64(-1), int(-1), json.Number("bad")} {
		if got, ok := intFromNumber(value); ok || got != 0 {
			t.Fatalf("intFromNumber(%#v) = %d, %v", value, got, ok)
		}
	}
	if got, ok := intFromNumber(json.Number("12")); !ok || got != 12 {
		t.Fatalf("intFromNumber json number = %d, %v", got, ok)
	}
}

func TestPortPasswordLeaseAndReaperFaultInjection(t *testing.T) {
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
	if err := writeLease(t.TempDir(), serverLease{}); err == nil {
		t.Fatal("writeLease ignored marshal error")
	}

	if err := reapStaleLeases("[", slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("reapStaleLeases accepted malformed glob")
	}
	root := t.TempDir()
	stateDir := filepath.Join(root, "session", "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(stateDir, leaseFileName)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := reapStaleLeases(root, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("reap stale read error: %v", err)
	}
}

func TestLeaseReaperVerifiesProcessIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process identity is platform-specific")
	}
	t.Run("unrelated process survives", func(t *testing.T) {
		root := t.TempDir()
		xdg, err := createXDGDirs(root, "unrelated")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sleep", "30")
		cmd.Env = append(os.Environ(),
			"XDG_STATE_HOME="+xdg.State,
			"OPENCODE_SERVER_PASSWORD=secret",
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start sleep: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				<-done
			}
		})
		identity, err := openCodeInspectProcess(cmd.Process.Pid)
		if err != nil {
			t.Skipf("process identity unavailable: %v", err)
		}
		if err := writeLease(xdg.State, serverLease{
			PID:              cmd.Process.Pid,
			PasswordHash:     passwordHash("secret"),
			XDGRoot:          xdg.Root,
			ProcessStartTime: identity.StartTime,
		}); err != nil {
			t.Fatal(err)
		}
		if err := reapStaleLeases(root, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("reapStaleLeases: %v", err)
		}
		select {
		case err := <-done:
			t.Fatalf("unrelated process was killed: %v", err)
		default:
		}
		if _, err := os.Stat(filepath.Join(xdg.State, leaseFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lease after unrelated reap = %v", err)
		}
	})

	t.Run("verified fake opencode is reaped", func(t *testing.T) {
		root := t.TempDir()
		xdg, err := createXDGDirs(root, "orphan")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=TestFakeOpenCodeServerProcessHelper", "--", "serve", "--port", "0")
		configureOpenCodeProcess(cmd)
		cmd.Env = append(os.Environ(),
			"ACP_GO_OPENCODE_FAKE_SERVER_HELPER=1",
			"XDG_STATE_HOME="+xdg.State,
			"OPENCODE_SERVER_USERNAME=opencode",
			"OPENCODE_SERVER_PASSWORD=secret",
		)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start fake opencode: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				<-done
			}
		})
		var identity processIdentity
		for i := 0; i < 50; i++ {
			identity, err = openCodeInspectProcess(cmd.Process.Pid)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Skipf("process identity unavailable: %v", err)
		}
		if err := writeLease(xdg.State, serverLease{
			PID:              cmd.Process.Pid,
			PasswordHash:     passwordHash("secret"),
			XDGRoot:          xdg.Root,
			ProcessStartTime: identity.StartTime,
		}); err != nil {
			t.Fatal(err)
		}
		if err := reapStaleLeases(root, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatalf("reapStaleLeases: %v", err)
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("verified fake opencode was not reaped")
		}
		if _, err := os.Stat(filepath.Join(xdg.State, leaseFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lease after verified reap = %v", err)
		}
	})
}

func TestLeaseIdentityBranchCoverage(t *testing.T) {
	root := t.TempDir()
	xdg, err := createXDGDirs(root, "lease")
	if err != nil {
		t.Fatal(err)
	}
	leasePath := filepath.Join(xdg.State, leaseFileName)
	baseIdentity := processIdentity{
		StartTime: "start",
		Cmdline:   []string{"/usr/bin/opencode", "serve"},
		Env: map[string]string{
			"XDG_STATE_HOME":           xdg.State,
			"OPENCODE_SERVER_PASSWORD": "secret",
		},
	}
	baseLease := serverLease{
		PID:              999999,
		PasswordHash:     passwordHash("secret"),
		XDGRoot:          xdg.Root,
		ProcessStartTime: "start",
	}

	restoreOpenCodeClientSeams(t)
	openCodeInspectProcess = func(int) (processIdentity, error) {
		return baseIdentity, nil
	}
	if !leaseMatchesProcess(leasePath, baseLease) {
		t.Fatal("matching lease did not match")
	}
	if cmdlineLooksLikeOpenCodeServe([]string{"/tmp/opencode"}) != true || cmdlineLooksLikeOpenCodeServe([]string{"node"}) {
		t.Fatal("cmdline OpenCode detection mismatch")
	}
	if leaseMatchesProcess(leasePath, serverLease{PID: 0, ProcessStartTime: "start"}) {
		t.Fatal("zero pid lease matched")
	}
	if leaseMatchesProcess(leasePath, serverLease{PID: 1}) {
		t.Fatal("missing start time lease matched")
	}

	for _, tt := range []struct {
		name     string
		identity processIdentity
		lease    serverLease
		err      error
	}{
		{name: "inspect error", identity: baseIdentity, lease: baseLease, err: errors.New("inspect failed")},
		{name: "start mismatch", identity: processIdentity{StartTime: "other", Cmdline: baseIdentity.Cmdline, Env: baseIdentity.Env}, lease: baseLease},
		{name: "state mismatch", identity: processIdentity{StartTime: "start", Cmdline: baseIdentity.Cmdline, Env: map[string]string{"XDG_STATE_HOME": t.TempDir(), "OPENCODE_SERVER_PASSWORD": "secret"}}, lease: baseLease},
		{name: "password mismatch", identity: processIdentity{StartTime: "start", Cmdline: baseIdentity.Cmdline, Env: map[string]string{"XDG_STATE_HOME": xdg.State, "OPENCODE_SERVER_PASSWORD": "wrong"}}, lease: baseLease},
		{name: "root mismatch", identity: baseIdentity, lease: serverLease{PID: baseLease.PID, PasswordHash: baseLease.PasswordHash, XDGRoot: t.TempDir(), ProcessStartTime: baseLease.ProcessStartTime}},
		{name: "cmdline mismatch", identity: processIdentity{StartTime: "start", Cmdline: []string{"node"}, Env: baseIdentity.Env}, lease: baseLease},
	} {
		t.Run(tt.name, func(t *testing.T) {
			openCodeInspectProcess = func(int) (processIdentity, error) {
				return tt.identity, tt.err
			}
			if leaseMatchesProcess(leasePath, tt.lease) {
				t.Fatal("mismatched lease matched")
			}
		})
	}

	openCodeInspectProcess = func(int) (processIdentity, error) {
		return baseIdentity, nil
	}
	if err := writeLease(xdg.State, baseLease); err != nil {
		t.Fatal(err)
	}
	reapLeaseFile(leasePath, slog.New(slog.DiscardHandler))
	if _, err := os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease after reap = %v", err)
	}
	reapLeaseFile(t.TempDir(), nil)
}

func TestNativeUnmarshalErrors(t *testing.T) {
	var part nativePart
	if err := part.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("nativePart accepted malformed JSON")
	}
	var event openCodeEvent
	if err := event.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("openCodeEvent accepted malformed JSON")
	}
	var providers providersResponse
	if err := providers.UnmarshalJSON([]byte("{")); err == nil {
		t.Fatal("providersResponse accepted malformed JSON")
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
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	script := filepath.Join(t.TempDir(), "fake-opencode")
	body := fmt.Sprintf("#!/bin/sh\nACP_GO_OPENCODE_FAKE_SERVER_HELPER=1 exec %q -test.run=TestFakeOpenCodeServerProcessHelper -- \"$@\"\n", testBinary)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
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
		switch r.URL.Path {
		case "/global/health":
			writeJSONNoTest(w, map[string]any{"healthy": true, "version": "9.9.9"})
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
	listen := openCodeListen
	randReader := openCodeRandReader
	marshalIndent := openCodeMarshalIndent
	writeLease := openCodeWriteLease
	terminateProcess := openCodeTerminateProcess
	killProcess := openCodeKillProcess
	inspectProcess := openCodeInspectProcess
	procReader := procReadFile
	waitCommand := openCodeWaitCommand
	after := openCodeAfter
	readyPoll := openCodeReadyPollInterval
	reconnectDelay := openCodeEventReconnectDelay
	shutdownTimeout := openCodeShutdownTimeout
	t.Cleanup(func() {
		openCodeCommandContext = commandContext
		openCodeListen = listen
		openCodeRandReader = randReader
		openCodeMarshalIndent = marshalIndent
		openCodeWriteLease = writeLease
		openCodeTerminateProcess = terminateProcess
		openCodeKillProcess = killProcess
		openCodeInspectProcess = inspectProcess
		procReadFile = procReader
		openCodeWaitCommand = waitCommand
		openCodeAfter = after
		openCodeReadyPollInterval = readyPoll
		openCodeEventReconnectDelay = reconnectDelay
		openCodeShutdownTimeout = shutdownTimeout
	})
}

func testXDGDirs(t *testing.T) xdgDirs {
	t.Helper()
	root := t.TempDir()
	return xdgDirs{
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
		events:     make(chan openCodeEvent, 8),
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
