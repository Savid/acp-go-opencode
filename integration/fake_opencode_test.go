//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	opencodeacp "github.com/savid/acp-go-opencode"
)

const (
	envFakeOpenCodeHelper = "ACP_GO_OPENCODE_FAKE_HELPER"
	envFakeOpenCodeMode   = "ACP_GO_OPENCODE_FAKE_MODE"
	fakeModeOK            = "ok"
	fakeModeMissingDoc    = "missing-doc"
)

func TestOpenCodeACPAgentFakeExecutableStdoutNoise(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agent := startAgentWithOpenCodePath(t, ctx, fakeOpenCodeExecutable(t, fakeModeOK), t.TempDir())

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize with fake opencode: %v\nstderr:\n%s", err, agent.stderrString())
	}
	session, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("new session with stdout-noisy fake opencode: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if session.SessionId == "" {
		t.Fatalf("empty fake session response: %#v", session)
	}
}

func TestOpenCodeACPAgentFakeExecutableSharedRuntimeLayout(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	home := t.TempDir()
	agent := startAgentWithOpenCodePath(t, ctx, fakeOpenCodeExecutable(t, fakeModeOK), home)

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	first, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("new first session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	second, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir()))
	if err != nil {
		t.Fatalf("new second session: %v\nstderr:\n%s", err, agent.stderrString())
	}
	if first.SessionId == second.SessionId {
		t.Fatalf("logical session ids were reused: %q", first.SessionId)
	}

	// A scratch-dir launch generates one shared runtime root beneath home, named
	// with the documented acp-go-opencode-runtime- prefix, and lays the XDG
	// directories out inside it. Both sessions share that one root. The runtime
	// also owns a sibling "<root>.control" directory, which is not a second
	// runtime.
	matches, err := filepath.Glob(filepath.Join(home, "acp-go-opencode-runtime-*"))
	if err != nil {
		t.Fatalf("glob shared runtime roots: %v", err)
	}
	runtimeRoots := make([]string, 0, 1)
	for _, match := range matches {
		if strings.HasSuffix(match, ".control") {
			continue
		}
		runtimeRoots = append(runtimeRoots, match)
	}
	if len(runtimeRoots) != 1 {
		t.Fatalf("shared runtime roots = %v, want exactly one", runtimeRoots)
	}
	for _, name := range []string{"data", "config", "cache", "state"} {
		info, statErr := os.Stat(filepath.Join(runtimeRoots[0], name))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("shared runtime %s dir: info=%v err=%v", name, info, statErr)
		}
	}
	for _, sessionID := range []acp.SessionId{first.SessionId, second.SessionId} {
		if _, statErr := os.Stat(filepath.Join(home, string(sessionID))); !os.IsNotExist(statErr) {
			t.Fatalf("per-session native XDG root exists for %q: %v", sessionID, statErr)
		}
	}
}

func TestOpenCodeACPAgentFakeExecutablePermissionDocFailClosed(t *testing.T) {
	requireRunIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agent := startAgentWithOpenCodePath(t, ctx, fakeOpenCodeExecutable(t, fakeModeMissingDoc), t.TempDir())

	conn := acp.NewClientSideConnection(&recordingClient{}, agent.stdin, agent.stdout)
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		t.Fatalf("initialize: %v\nstderr:\n%s", err, agent.stderrString())
	}
	// The fake drops only the native permission-reply path, so version
	// validation must reject the runtime and fail session creation closed. The
	// wire error is a generic internal error by design: the adapter reduces every
	// handler failure to "handler failed" and never leaks the failing detail,
	// which the -debug stream carries instead. The ok-mode tests share this
	// fixture minus that one path and create sessions, so the dropped path is
	// what fails this one.
	_, err := conn.NewSession(ctx, opencodeacp.NewSessionRequest(t.TempDir()))
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) || reqErr.Code != -32603 {
		t.Fatalf("new session with missing permission /doc path err = %v, want internal error\nstderr:\n%s", err, agent.stderrString())
	}
}

func TestFakeOpenCodeExecutable(t *testing.T) {
	if os.Getenv(envFakeOpenCodeHelper) != "1" {
		return
	}
	if err := runFakeOpenCodeServer(os.Args, os.Getenv(envFakeOpenCodeMode)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func startAgentWithOpenCodePath(t *testing.T, ctx context.Context, opencodePath string, home string) *liveAgent {
	t.Helper()

	return startAgent(t, ctx,
		"-path", opencodePath,
		"-scratch-dir", home,
		"-opencode-pure",
		"-opencode-health-timeout", "5s",
	)
}

func fakeOpenCodeExecutable(t *testing.T, mode string) string {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake-opencode")
	script := fmt.Sprintf(`#!/bin/sh
%s=1 %s=%s exec %q -test.run '^TestFakeOpenCodeExecutable$' -- "$@"
`, envFakeOpenCodeHelper, envFakeOpenCodeMode, mode, testBinary)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake opencode executable: %v", err)
	}
	return path
}

func runFakeOpenCodeServer(args []string, mode string) error {
	port := ""
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			port = args[i+1]
			break
		}
	}
	if port == "" {
		return fmt.Errorf("fake opencode missing --port in args %q", strings.Join(args, " "))
	}
	if mode == "" {
		mode = fakeModeOK
	}

	carrier := &fakeCarrierLoader{}

	_, _ = fmt.Fprintln(os.Stdout, "native stdout noise before HTTP readiness")
	var stateMu sync.Mutex
	var nextSession int
	sessionDirectories := map[string]string{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/global/health":
			writeFakeJSON(w, map[string]any{"healthy": true, "version": "1.18.3"})
		case r.URL.Path == "/doc":
			writeFakeJSON(w, fakeOpenCodeDoc(mode))
		case r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-r.Context().Done()
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			stateMu.Lock()
			nextSession++
			nativeID := "native-fake-" + strconv.Itoa(nextSession)
			directory := r.URL.Query().Get("directory")
			sessionDirectories[nativeID] = directory
			stateMu.Unlock()
			writeFakeJSON(w, fakeNativeSession(nativeID, directory))
		case r.URL.Path == "/sync/history" && r.Method == http.MethodPost:
			var cursors map[string]int64
			if err := json.NewDecoder(r.Body).Decode(&cursors); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}
			if len(cursors) != 0 {
				writeFakeJSON(w, []any{})

				return
			}
			stateMu.Lock()
			events := make([]any, 0, len(sessionDirectories))
			for nativeID, directory := range sessionDirectories {
				events = append(events, map[string]any{
					"id":           "event-" + nativeID + "-created",
					"aggregate_id": nativeID,
					"seq":          0,
					"type":         "session.created.1",
					"data": map[string]any{
						"sessionID": nativeID,
						"info":      map[string]any{"id": nativeID, "directory": directory},
					},
				})
			}
			stateMu.Unlock()
			writeFakeJSON(w, events)
		case strings.HasPrefix(r.URL.Path, "/session/") && r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/todo"):
			nativeID := strings.TrimPrefix(r.URL.Path, "/session/")
			stateMu.Lock()
			directory, found := sessionDirectories[nativeID]
			stateMu.Unlock()
			if !found {
				http.NotFound(w, r)

				return
			}
			writeFakeJSON(w, fakeNativeSession(nativeID, directory))
		case strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/todo") && r.Method == http.MethodGet:
			writeFakeJSON(w, []any{})
		case strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/abort") && r.Method == http.MethodPost:
			writeFakeJSON(w, map[string]any{"ok": true})
		case r.URL.Path == "/config" && r.Method == http.MethodGet:
			// The first directory-scoped request is where OpenCode instantiates
			// the seeded plugins, so this is where the carrier proof happens.
			if err := carrier.load(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}
			writeFakeJSON(w, map[string]any{})
		case r.URL.Path == "/config/providers":
			writeFakeJSON(w, map[string]any{"providers": []map[string]any{{
				"id":   "openai",
				"name": "OpenAI",
				"models": map[string]any{
					"gpt-test": map[string]any{"id": "gpt-test", "name": "GPT Test"},
				},
			}}})
		case r.URL.Path == "/command" && r.Method == http.MethodGet:
			writeFakeJSON(w, []any{})
		case r.URL.Path == "/agent":
			writeFakeJSON(w, []map[string]any{{"name": "build", "description": "Build"}})
		case r.URL.Path == "/permission" && r.Method == http.MethodGet:
			writeFakeJSON(w, []any{})
		case r.URL.Path == "/question" && r.Method == http.MethodGet:
			writeFakeJSON(w, []any{})
		case r.URL.Path == "/api/permission/request" && r.Method == http.MethodGet:
			writeFakeJSON(w, map[string]any{"location": map[string]any{}, "data": []any{}})
		case r.URL.Path == "/api/question/request" && r.Method == http.MethodGet:
			writeFakeJSON(w, map[string]any{"location": map[string]any{}, "data": []any{}})
		default:
			http.NotFound(w, r)
		}
	})
	server := &http.Server{Addr: "127.0.0.1:" + port, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	return server.ListenAndServe()
}

func fakeOpenCodeDoc(mode string) map[string]any {
	required := []string{
		"/command",
		"/config",
		"/config/providers",
		"/event",
		"/session/status",
		"/session",
		"/session/{sessionID}",
		"/session/{sessionID}/command",
		"/session/{sessionID}/message",
		"/session/{sessionID}/prompt_async",
		"/session/{sessionID}/abort",
		"/session/{sessionID}/fork",
		"/session/{sessionID}/todo",
		"/session/{sessionID}/revert",
		"/session/{sessionID}/unrevert",
		"/permission",
		"/permission/{requestID}/reply",
		"/question",
		"/question/{requestID}/reply",
		"/question/{requestID}/reject",
		"/api/session/{sessionID}/agent",
		"/api/session/{sessionID}/message",
		"/api/session/{sessionID}/model",
		"/api/session/{sessionID}/permission/{requestID}/reply",
		"/api/permission/request",
		"/api/session/{sessionID}/question/{requestID}/reply",
		"/api/session/{sessionID}/question/{requestID}/reject",
		"/api/question/request",
	}
	paths := map[string]any{}
	for _, path := range required {
		if mode == fakeModeMissingDoc && path == "/api/session/{sessionID}/permission/{requestID}/reply" {
			continue
		}
		paths[path] = map[string]any{}
	}
	paths["/api/permission/request"] = fakePendingRequestPath("PermissionV2Request")
	paths["/permission"] = fakePendingArrayPath("PermissionRequest")
	if mode != fakeModeMissingDoc {
		paths["/api/session/{sessionID}/permission/{requestID}/reply"] = map[string]any{
			"post": map[string]any{
				"responses": map[string]any{"204": map[string]any{"description": "<No Content>"}},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"reply":   map[string]any{"$ref": "#/components/schemas/PermissionV2Reply"},
							"message": map[string]any{"type": "string"},
						},
						"required": []any{"reply"},
					}}},
				},
			},
		}
	}
	paths["/permission/{requestID}/reply"] = map[string]any{
		"post": map[string]any{
			"responses": map[string]any{"200": map[string]any{"description": "Permission processed"}},
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"reply":   map[string]any{"type": "string"},
						"message": map[string]any{"type": "string"},
					},
					"required": []any{"reply"},
				}}},
			},
		},
	}
	paths["/api/question/request"] = fakePendingRequestPath("QuestionV2Request")
	paths["/question"] = fakePendingArrayPath("QuestionRequest")
	paths["/api/session/{sessionID}/question/{requestID}/reply"] = map[string]any{
		"post": map[string]any{
			"responses": map[string]any{"204": map[string]any{"description": "<No Content>"}},
			"requestBody": map[string]any{
				"required": true,
				"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"$ref": "#/components/schemas/QuestionV2Reply",
				}}},
			},
		},
	}
	paths["/api/session/{sessionID}/question/{requestID}/reject"] = map[string]any{
		"post": map[string]any{"responses": map[string]any{"204": map[string]any{"description": "<No Content>"}}},
	}
	paths["/question/{requestID}/reply"] = map[string]any{
		"post": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "Question answered"}}},
	}
	paths["/question/{requestID}/reject"] = map[string]any{
		"post": map[string]any{"responses": map[string]any{"200": map[string]any{"description": "Question rejected"}}},
	}
	return map[string]any{
		"paths": paths,
		"components": map[string]any{"schemas": map[string]any{
			"Event": fakeEventUnion(
				"EventSessionIdle",
				"EventSessionStatus",
				"EventSessionError",
				"EventPermissionV2Asked",
				"EventPermissionV2Replied",
				"EventPermissionAsked",
				"EventPermissionReplied",
				"EventQuestionV2Asked",
				"EventQuestionV2Replied",
				"EventQuestionAsked",
				"EventQuestionReplied",
				"EventMessagePartUpdated",
				"EventServerConnected",
			),
			"EventSessionIdle":       fakeEventSchema("session.idle", []string{"sessionID"}),
			"EventSessionStatus":     fakeEventSchema("session.status", []string{"sessionID", "status"}),
			"EventSessionError":      fakeEventSchema("session.error", nil),
			"EventPermissionV2Asked": fakeEventSchema("permission.v2.asked", []string{"id", "sessionID", "action", "resources"}),
			"EventPermissionV2Replied": fakeEventSchema("permission.v2.replied", []string{
				"sessionID",
				"requestID",
				"reply",
			}),
			"EventPermissionAsked": fakeEventSchema("permission.asked", []string{"id", "sessionID", "permission", "patterns"}),
			"EventPermissionReplied": fakeEventSchema("permission.replied", []string{
				"sessionID",
				"requestID",
				"reply",
			}),
			"EventQuestionV2Asked":    fakeEventSchema("question.v2.asked", []string{"id", "sessionID", "questions"}),
			"EventQuestionV2Replied":  fakeEventSchema("question.v2.replied", []string{"sessionID", "requestID", "answers"}),
			"EventQuestionAsked":      fakeEventSchema("question.asked", []string{"id", "sessionID", "questions"}),
			"EventQuestionReplied":    fakeEventSchema("question.replied", []string{"sessionID", "requestID", "answers"}),
			"EventMessagePartUpdated": fakeEventSchema("message.part.updated", []string{"sessionID", "part", "time"}),
			"EventServerConnected":    fakeEventSchema("server.connected", nil),
			"QuestionV2Reply": map[string]any{
				"type":       "object",
				"properties": map[string]any{"answers": map[string]any{"type": "array"}},
				"required":   []any{"answers"},
			},
			"OutputFormatJsonSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":       map[string]any{"type": "string", "enum": []any{"json_schema"}},
					"schema":     map[string]any{"$ref": "#/components/schemas/JSONSchema"},
					"retryCount": map[string]any{"type": "integer"},
				},
				"required": []any{"type", "schema"},
			},
		}},
	}
}

func fakeEventUnion(names ...string) map[string]any {
	refs := make([]any, 0, len(names))
	for _, name := range names {
		refs = append(refs, map[string]any{"$ref": "#/components/schemas/" + name})
	}
	return map[string]any{"anyOf": refs}
}

func fakeEventSchema(eventType string, required []string) map[string]any {
	properties := map[string]any{}
	for _, property := range required {
		properties[property] = map[string]any{"type": "string"}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":   map[string]any{"type": "string"},
			"type": map[string]any{"type": "string", "enum": []string{eventType}},
			"properties": map[string]any{
				"type":                 "object",
				"properties":           properties,
				"required":             required,
				"additionalProperties": false,
			},
		},
		"required":             []string{"id", "type", "properties"},
		"additionalProperties": false,
	}
}

func fakePendingRequestPath(itemRef string) map[string]any {
	return map[string]any{
		"get": map[string]any{"responses": map[string]any{"200": map[string]any{
			"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{"data": map[string]any{
					"type":  "array",
					"items": map[string]any{"$ref": "#/components/schemas/" + itemRef},
				}},
			}}},
		}}},
	}
}

func fakePendingArrayPath(itemRef string) map[string]any {
	return map[string]any{
		"get": map[string]any{"responses": map[string]any{"200": map[string]any{
			"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
				"type":  "array",
				"items": map[string]any{"$ref": "#/components/schemas/" + itemRef},
			}}},
		}}},
	}
}

func fakeNativeSession(id string, directory string) map[string]any {
	return map[string]any{
		"id":        id,
		"title":     "Fake",
		"directory": directory,
		"agent":     "build",
		"model": map[string]any{
			"providerID": "openai",
			"modelID":    "gpt-test",
		},
		"time": map[string]any{"updated": time.Now().UnixMilli()},
	}
}

func writeFakeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// fakeCarrierLoader stands in for OpenCode's plugin loader. OpenCode
// instantiates the generated session-carrier plugin on the first
// directory-scoped request, and the plugin then proves itself to the wrapper's
// broker before the wrapper hands out a session. The fake performs that proof
// from the plugin source the wrapper seeded through OPENCODE_CONFIG_CONTENT.
type fakeCarrierLoader struct {
	once sync.Once
	err  error
}

func (l *fakeCarrierLoader) load() error {
	l.once.Do(func() {
		l.err = announceFakeCarrierReady(os.Getenv("OPENCODE_CONFIG_CONTENT"))
	})

	return l.err
}

// announceFakeCarrierReady posts the startup proof the seeded carrier plugin
// would post. A runtime that seeded no carrier plugin has nothing to prove.
func announceFakeCarrierReady(configContent string) error {
	var config struct {
		Plugin []any `json:"plugin"`
	}
	if err := json.Unmarshal([]byte(configContent), &config); err != nil {
		return fmt.Errorf("fake opencode: decode OPENCODE_CONFIG_CONTENT: %w", err)
	}

	for _, entry := range config.Plugin {
		plugin, ok := entry.(string)
		if !ok || !strings.HasPrefix(plugin, "file://") {
			continue
		}

		pluginURL, err := url.Parse(plugin)
		if err != nil {
			return fmt.Errorf("fake opencode: parse plugin URL %q: %w", plugin, err)
		}

		source, err := os.ReadFile(pluginURL.Path)
		if err != nil {
			return fmt.Errorf("fake opencode: read carrier plugin: %w", err)
		}

		endpoint, err := fakePluginConstant(source, "BROKER_ENDPOINT")
		if err != nil {
			return err
		}

		token, err := fakePluginConstant(source, "BROKER_TOKEN")
		if err != nil {
			return err
		}

		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint+"/ready", http.NoBody)
		if err != nil {
			return fmt.Errorf("fake opencode: build carrier proof: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token)

		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return fmt.Errorf("fake opencode: post carrier proof: %w", err)
		}
		_ = response.Body.Close()

		if response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("fake opencode: carrier proof status %d", response.StatusCode)
		}

		return nil
	}

	return nil
}

// fakePluginConstant reads one of the JS string constants the wrapper renders
// into the carrier plugin. The literal is JSON-encoded, so JSON decodes it.
func fakePluginConstant(source []byte, name string) (string, error) {
	pattern := regexp.MustCompile(`(?m)^const ` + regexp.QuoteMeta(name) + ` = ("(?:[^"\\]|\\.)*")$`)
	match := pattern.FindSubmatch(source)
	if match == nil {
		return "", fmt.Errorf("fake opencode: carrier plugin has no %s constant", name)
	}

	var value string
	if err := json.Unmarshal(match[1], &value); err != nil {
		return "", fmt.Errorf("fake opencode: decode %s: %w", name, err)
	}

	return value, nil
}
