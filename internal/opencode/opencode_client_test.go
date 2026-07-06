package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fakeReadinessRoutesHandler(t *testing.T, seen *[]string) http.HandlerFunc {
	t.Helper()

	return func(w http.ResponseWriter, r *http.Request) {
		if username, password, ok := r.BasicAuth(); !ok || username != "opencode" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		*seen = append(*seen, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/global/health":
			writeJSON(t, w, map[string]any{"healthy": true, "version": "9.9.9"})
		case "/doc":
			writeJSON(t, w, fullOpenCodeDoc())
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"type":"server.connected","properties":{}}` + "\n\n"))
		case "/api/question/request":
			writeJSON(t, w, map[string]any{"data": []map[string]any{{"id": "q", "sessionID": "s"}}})
		case "/question":
			writeJSON(t, w, []map[string]any{{"id": "q-session-list", "sessionID": "s"}})
		case "/api/session/s/question/q/reply":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode reply body: %v", err)
			}
			writeJSON(t, w, map[string]any{"ok": true})
		case "/api/session/s/question/q/reject":
			writeJSON(t, w, map[string]any{"ok": true})
		case "/question/q-session/reply":
			writeJSON(t, w, true)
		case "/question/q-session/reject":
			writeJSON(t, w, true)
		case "/question/q-session-list/reject":
			writeJSON(t, w, true)
		case "/api/permission/request":
			writeJSON(t, w, map[string]any{"data": []map[string]any{{"id": "p", "sessionID": "s", "action": "edit"}}})
		case "/permission":
			writeJSON(t, w, []map[string]any{{"id": "p-session-list", "sessionID": "s", "permission": "edit"}})
		case "/api/session/s/permission/p/reply":
			writeJSON(t, w, map[string]any{"ok": true})
		case "/permission/p-session/reply":
			writeJSON(t, w, true)
		case "/permission/p-session-list/reply":
			writeJSON(t, w, true)
		default:
			writeJSON(t, w, map[string]any{"id": "s"})
		}
	}
}

func newFakeReadinessRoutesClient(t *testing.T) (*openCodeServer, *[]string) {
	t.Helper()
	var seen []string
	server := httptest.NewServer(fakeReadinessRoutesHandler(t, &seen))
	t.Cleanup(server.Close)

	client := &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
		events:     make(chan Event, 8),
		errs:       make(chan error, 8),
		closed:     make(chan struct{}),
	}

	return client, &seen
}

func TestOpenCodeHTTPFakeServerReadinessAndQuestionRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, seen := newFakeReadinessRoutesClient(t)
	if err := client.waitReady(ctx, ctx, StartOptions{SkipVersionGate: true}); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	questions, err := client.PendingQuestions(ctx)
	if err != nil || len(questions) != 2 || questions[0].ID != "q-session-list" || questions[0].Route() != QuestionRouteSession || questions[1].ID != "q" {
		t.Fatalf("PendingQuestions = %#v err=%v", questions, err)
	}
	if replyErr := client.ReplyQuestion(ctx, QuestionRequest{ID: "q", SessionID: "s", ReplyRoute: QuestionRouteAPI}, [][]string{{"yes"}}); replyErr != nil {
		t.Fatalf("ReplyQuestion: %v", replyErr)
	}
	if rejectErr := client.RejectQuestion(ctx, QuestionRequest{ID: "q", SessionID: "s", ReplyRoute: QuestionRouteAPI}); rejectErr != nil {
		t.Fatalf("RejectQuestion: %v", rejectErr)
	}
	if sessionReplyErr := client.ReplyQuestion(ctx, QuestionRequest{ID: "q-session", SessionID: "s", ReplyRoute: QuestionRouteSession}, [][]string{{"yes"}}); sessionReplyErr != nil {
		t.Fatalf("ReplyQuestion session route: %v", sessionReplyErr)
	}
	if sessionRejectErr := client.RejectQuestion(ctx, QuestionRequest{ID: "q-session", SessionID: "s", ReplyRoute: QuestionRouteSession}); sessionRejectErr != nil {
		t.Fatalf("RejectQuestion session route: %v", sessionRejectErr)
	}
	if !containsString(*seen, "GET /doc") || !containsString(*seen, "POST /api/session/s/question/q/reply") {
		t.Fatalf("seen paths = %#v", *seen)
	}
	close(client.closed)
	if todos, todosErr := client.Todos(ctx, "s"); !errors.Is(todosErr, context.Canceled) || todos != nil {
		t.Fatalf("closed Todos = %#v err=%v", todos, todosErr)
	}
}

func TestOpenCodeHTTPFakeServerPermissionRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	client, _ := newFakeReadinessRoutesClient(t)
	if err := client.waitReady(ctx, ctx, StartOptions{SkipVersionGate: true}); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	permissions, err := client.PendingPermissions(ctx)
	if err != nil || len(permissions) != 2 || permissions[0].ID != "p-session-list" || permissions[0].Route() != PermissionRouteSession || permissions[1].ID != "p" {
		t.Fatalf("PendingPermissions = %#v err=%v", permissions, err)
	}
	if replyErr := client.ReplyPermission(ctx, permissions[1], "once", "ok"); replyErr != nil {
		t.Fatalf("ReplyPermission: %v", replyErr)
	}
	if sessionReplyErr := client.ReplyPermission(ctx, PermissionRequest{ID: "p-session", SessionID: "s", ReplyRoute: PermissionRouteSession}, "once", "ok"); sessionReplyErr != nil {
		t.Fatalf("ReplyPermission session route: %v", sessionReplyErr)
	}
	close(client.closed)
}

func TestOpenCodePendingRequestErrors(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
	}
	if _, err := client.PendingPermissions(ctx); err == nil {
		t.Fatal("PendingPermissions unexpectedly succeeded")
	}
	if _, err := client.PendingQuestions(ctx); err == nil {
		t.Fatal("PendingQuestions unexpectedly succeeded")
	}
}

func TestOpenCodePendingSessionListErrors(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		call func(*openCodeServer) error
	}{
		{
			name: "permission",
			call: func(client *openCodeServer) error {
				_, err := client.PendingPermissions(ctx)

				return err
			},
		},
		{
			name: "question",
			call: func(client *openCodeServer) error {
				_, err := client.PendingQuestions(ctx)

				return err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			client := &openCodeServer{
				httpClient:                   server.Client(),
				baseURL:                      server.URL,
				sessionPermissionListSupport: true,
				sessionQuestionListSupport:   true,
			}
			if err := tt.call(client); err == nil {
				t.Fatal("session pending list error was ignored")
			}
		})
	}
}

func TestOpenCodeSendMessageUsesNoDeadlineHTTPClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		switch r.URL.Path {
		case "/session/s/message":
			writeJSON(t, w, map[string]any{"info": map[string]any{
				"id":        "assistant",
				"sessionID": "s",
				"role":      "assistant",
				"finish":    "stop",
			}})
		case "/session/s":
			writeJSON(t, w, map[string]any{"id": "s"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &openCodeServer{
		httpClient: &http.Client{Timeout: time.Millisecond},
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
	}
	if _, err := client.SendMessage(context.Background(), "s", MessageRequest{Parts: []map[string]any{{"type": "text", "text": "hello"}}}); err != nil {
		t.Fatalf("SendMessage used deadline client: %v", err)
	}
	if _, err := client.GetSession(context.Background(), "s"); err == nil {
		t.Fatal("regular REST call unexpectedly bypassed timeout")
	}
}

func TestOpenCodeHTTPClientHelperBranches(t *testing.T) {
	if got := (&openCodeServer{}).blockingHTTPClient(); got != http.DefaultClient {
		t.Fatalf("nil blocking client = %#v, want default", got)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"ok": true})
	}))
	defer server.Close()
	client := &openCodeServer{baseURL: server.URL}
	var out map[string]bool
	if err := client.doJSONWithClient(context.Background(), nil, http.MethodGet, "/", nil, nil, &out); err != nil {
		t.Fatalf("doJSONWithClient default client: %v", err)
	}
	if !out["ok"] {
		t.Fatalf("decoded response = %#v", out)
	}
}

func TestOpenCodeDocFailClosedAndHelpers(t *testing.T) {
	doc := fullOpenCodeDoc()
	paths := docMap(t, doc, "paths")
	delete(paths, "/api/session/{sessionID}/question/{requestID}/reply")
	if err := validateOpenCodeDoc(doc); err == nil || !strings.Contains(err.Error(), "question") {
		t.Fatalf("validateOpenCodeDoc error = %v", err)
	}
	for _, path := range []string{"/command", "/session/{sessionID}/command", "/session/{sessionID}/message"} {
		t.Run("route gate missing "+path, func(t *testing.T) {
			doc := cloneOpenCodeDoc(t, fullOpenCodeDoc())
			paths := docMap(t, doc, "paths")
			delete(paths, path)
			err := validateOpenCodeDoc(doc)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("validateOpenCodeDoc error = %v", err)
			}
		})
	}
	t.Run("deleted prompt route is not required", func(t *testing.T) {
		doc := cloneOpenCodeDoc(t, fullOpenCodeDoc())
		paths := docMap(t, doc, "paths")
		delete(paths, "/api/session/{sessionID}/prompt")
		if err := validateOpenCodeDoc(doc); err != nil {
			t.Fatalf("validateOpenCodeDoc without deleted prompt route: %v", err)
		}
	})
	for _, tt := range []struct {
		name   string
		mutate func(*testing.T, map[string]any)
	}{
		{
			name: "permission request wrong method",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				paths := docMap(t, doc, "paths")
				paths["/api/permission/request"] = map[string]any{"post": map[string]any{}}
			},
		},
		{
			name: "permission request wrong schema",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				paths := docMap(t, doc, "paths")
				paths["/api/permission/request"] = pendingRequestPath("WrongRequest")
			},
		},
		{
			name: "permission request missing success response",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				requestPath := docMapPath(t, doc, "paths", "/api/permission/request")
				docMap(t, requestPath, "get")["responses"] = map[string]any{}
			},
		},
		{
			name: "permission request data not array",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				paths := docMap(t, doc, "paths")
				paths["/api/permission/request"] = map[string]any{"get": map[string]any{
					"responses": map[string]any{"200": map[string]any{
						"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"data": map[string]any{"type": "object"},
							},
						}}},
					}},
				}}
			},
		},
		{
			name: "permission reply wrong method",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/permission/{requestID}/reply")
				replyPath["get"] = replyPath["post"]
				delete(replyPath, "post")
			},
		},
		{
			name: "permission reply missing no-content response",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/permission/{requestID}/reply")
				docMap(t, replyPath, "post")["responses"] = map[string]any{}
			},
		},
		{
			name: "permission reply missing request body",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/permission/{requestID}/reply")
				delete(docMap(t, replyPath, "post"), "requestBody")
			},
		},
		{
			name: "permission reply missing reply body",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/permission/{requestID}/reply")
				schema := docMapPath(t, replyPath, "post", "requestBody", "content", "application/json", "schema")
				schema["required"] = []any{}
			},
		},
		{
			name: "permission reply missing reply property",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/permission/{requestID}/reply")
				schema := docMapPath(t, replyPath, "post", "requestBody", "content", "application/json", "schema")
				delete(docMap(t, schema, "properties"), "reply")
			},
		},
		{
			name: "permission reply missing message property",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/permission/{requestID}/reply")
				schema := docMapPath(t, replyPath, "post", "requestBody", "content", "application/json", "schema")
				delete(docMap(t, schema, "properties"), "message")
			},
		},
		{
			name: "session permission reply wrong method",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/permission/{requestID}/reply")
				replyPath["get"] = replyPath["post"]
				delete(replyPath, "post")
			},
		},
		{
			name: "session permission reply missing success response",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/permission/{requestID}/reply")
				docMap(t, replyPath, "post")["responses"] = map[string]any{}
			},
		},
		{
			name: "session permission request wrong schema",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				paths := docMap(t, doc, "paths")
				paths["/permission"] = pendingArrayPath("WrongRequest")
			},
		},
		{
			name: "question request wrong schema",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				paths := docMap(t, doc, "paths")
				paths["/api/question/request"] = pendingRequestPath("WrongRequest")
			},
		},
		{
			name: "question reply wrong method",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/question/{requestID}/reply")
				replyPath["get"] = replyPath["post"]
				delete(replyPath, "post")
			},
		},
		{
			name: "question reply missing no-content response",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/question/{requestID}/reply")
				docMap(t, replyPath, "post")["responses"] = map[string]any{}
			},
		},
		{
			name: "question reply missing request body",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/question/{requestID}/reply")
				delete(docMap(t, replyPath, "post"), "requestBody")
			},
		},
		{
			name: "question reply bad ref",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/question/{requestID}/reply")
				jsonContent := docMapPath(t, replyPath, "post", "requestBody", "content", "application/json")
				jsonContent["schema"] = map[string]any{"$ref": "#/components/schemas/Missing"}
			},
		},
		{
			name: "question reply wrong schema",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				reply := docMapPath(t, doc, "components", "schemas", "QuestionV2Reply")
				reply["required"] = []any{}
			},
		},
		{
			name: "session question reply wrong method",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				replyPath := docMapPath(t, doc, "paths", "/question/{requestID}/reply")
				replyPath["get"] = replyPath["post"]
				delete(replyPath, "post")
			},
		},
		{
			name: "session question reject missing success response",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				rejectPath := docMapPath(t, doc, "paths", "/question/{requestID}/reject")
				docMap(t, rejectPath, "post")["responses"] = map[string]any{}
			},
		},
		{
			name: "session question request wrong schema",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				paths := docMap(t, doc, "paths")
				paths["/question"] = pendingArrayPath("WrongRequest")
			},
		},
		{
			name: "event union missing permission event",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				event := docMapPath(t, doc, "components", "schemas", "Event")
				event["anyOf"] = []any{}
			},
		},
		{
			name: "event schema missing component",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				schemas := docMapPath(t, doc, "components", "schemas")
				delete(schemas, "EventPermissionV2Asked")
			},
		},
		{
			name: "event schema wrong type enum",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				event := docMapPath(t, doc, "components", "schemas", "EventPermissionV2Asked")
				typeSchema := docMapPath(t, event, "properties", "type")
				typeSchema["enum"] = []any{"permission.asked"}
			},
		},
		{
			name: "event schema missing properties",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				event := docMapPath(t, doc, "components", "schemas", "EventPermissionV2Asked")
				properties := docMap(t, event, "properties")
				delete(properties, "properties")
			},
		},
		{
			name: "event schema missing required property",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				event := docMapPath(t, doc, "components", "schemas", "EventQuestionV2Asked")
				properties := docMapPath(t, event, "properties", "properties")
				properties["required"] = []any{"id", "sessionID"}
			},
		},
		{
			name: "question reject missing no-content response",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				rejectPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/question/{requestID}/reject")
				docMap(t, rejectPath, "post")["responses"] = map[string]any{}
			},
		},
		{
			name: "question reject wrong method",
			mutate: func(t *testing.T, doc map[string]any) {
				t.Helper()
				rejectPath := docMapPath(t, doc, "paths", "/api/session/{sessionID}/question/{requestID}/reject")
				rejectPath["get"] = rejectPath["post"]
				delete(rejectPath, "post")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc := cloneOpenCodeDoc(t, fullOpenCodeDoc())
			tt.mutate(t, doc)
			if err := validateOpenCodeDoc(doc); err == nil {
				t.Fatal("validateOpenCodeDoc accepted invalid /doc")
			}
		})
	}
	if compareSemver("1.2.3", "1.2.4") >= 0 || compareSemver("1.3.0", "1.2.9") <= 0 || compareSemver("v1.2.3-beta", "1.2.3") != 0 {
		t.Fatal("compareSemver returned unexpected ordering")
	}
	if SafePathName("../a:b") != "__a_b" || SafePathName("") != "session" {
		t.Fatalf("SafePathName mismatch")
	}
	if got := envMapToSlice(map[string]string{"B": "2", "A": "1"}); !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Fatalf("envMapToSlice = %#v", got)
	}
	if passwordHash("secret") == "" {
		t.Fatal("empty password hash")
	}
	wrapped := errors.New("wrapped")
	if !errors.Is(StreamError{Epoch: 1, Err: wrapped}, wrapped) {
		t.Fatal("StreamError did not unwrap")
	}
	if _, ok := openAPIOperation(map[string]any{}, "/missing", http.MethodGet); ok {
		t.Fatal("missing OpenAPI path returned operation")
	}
	if !openAPIStringEnumContains(map[string]any{"enum": []string{"wanted"}}, "wanted") {
		t.Fatal("string enum helper missed wanted value")
	}
	if openAPIStringEnumContains(map[string]any{"enum": []string{"other"}}, "wanted") {
		t.Fatal("string enum helper matched wrong value")
	}
	if openAPIEventUnionHasSchema(map[string]any{}, "EventPermissionV2Asked") {
		t.Fatal("missing Event union unexpectedly matched")
	}
	if !openAPIObjectHasRequiredProperty(map[string]any{
		"properties": map[string]any{"reply": map[string]any{}},
		"required":   []string{"reply"},
	}, "reply") {
		t.Fatal("required []string property was not recognized")
	}
	if _, ok := openAPIComponentSchema(fullOpenCodeDoc(), "QuestionV2Reply"); ok {
		t.Fatal("non-component ref resolved")
	}
	support, err := validateOptionalOpenCodeGetArrayOperation(map[string]any{"/permission": map[string]any{}}, "/permission", "PermissionRequest")
	if err != nil || support {
		t.Fatalf("optional missing GET support=%v err=%v", support, err)
	}
	_, err = validateOptionalOpenCodeGetArrayOperation(map[string]any{"/permission": map[string]any{
		"get": map[string]any{"responses": map[string]any{}},
	}}, "/permission", "PermissionRequest")
	if err == nil {
		t.Fatal("optional list missing 200 response was accepted")
	}
	if openAPISchemaArrayRef(map[string]any{"type": "object"}, "PermissionRequest") {
		t.Fatal("object schema matched array ref")
	}
}

func docMap(t *testing.T, container map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := container[key].(map[string]any)
	if !ok {
		t.Fatalf("doc value %q is %T, want map[string]any", key, container[key])
	}

	return value
}

func docMapPath(t *testing.T, container map[string]any, keys ...string) map[string]any {
	t.Helper()
	for _, key := range keys {
		container = docMap(t, container, key)
	}

	return container
}

func cloneOpenCodeDoc(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}

	return cloned
}

func fullOpenCodeDoc() map[string]any {
	paths := map[string]any{}
	for _, path := range []string{
		"/config/providers",
		"/command",
		"/event",
		"/session/status",
		"/session",
		"/session/{sessionID}",
		"/session/{sessionID}/command",
		"/session/{sessionID}/message",
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
	} {
		paths[path] = map[string]any{}
	}
	paths["/api/permission/request"] = pendingRequestPath("PermissionV2Request")
	paths["/permission"] = pendingArrayPath("PermissionRequest")
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
	paths["/api/question/request"] = pendingRequestPath("QuestionV2Request")
	paths["/question"] = pendingArrayPath("QuestionRequest")
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
		"post": map[string]any{
			"responses": map[string]any{"204": map[string]any{"description": "<No Content>"}},
		},
	}
	paths["/question/{requestID}/reply"] = map[string]any{
		"post": map[string]any{
			"responses": map[string]any{"200": map[string]any{"description": "Question answered"}},
		},
	}
	paths["/question/{requestID}/reject"] = map[string]any{
		"post": map[string]any{
			"responses": map[string]any{"200": map[string]any{"description": "Question rejected"}},
		},
	}

	return map[string]any{
		"paths": paths,
		"components": map[string]any{"schemas": map[string]any{
			"Event": eventUnion(
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
			"EventPermissionV2Asked": eventSchema("permission.v2.asked", []string{"id", "sessionID", "action", "resources"}),
			"EventPermissionV2Replied": eventSchema("permission.v2.replied", []string{
				"sessionID",
				"requestID",
				"reply",
			}),
			"EventPermissionAsked": eventSchema("permission.asked", []string{"id", "sessionID", "permission", "patterns"}),
			"EventPermissionReplied": eventSchema("permission.replied", []string{
				"sessionID",
				"requestID",
				"reply",
			}),
			"EventQuestionV2Asked":    eventSchema("question.v2.asked", []string{"id", "sessionID", "questions"}),
			"EventQuestionV2Replied":  eventSchema("question.v2.replied", []string{"sessionID", "requestID", "answers"}),
			"EventQuestionAsked":      eventSchema("question.asked", []string{"id", "sessionID", "questions"}),
			"EventQuestionReplied":    eventSchema("question.replied", []string{"sessionID", "requestID", "answers"}),
			"EventMessagePartUpdated": eventSchema("message.part.updated", []string{"sessionID", "part", "time"}),
			"EventServerConnected":    eventSchema("server.connected", nil),
			"QuestionV2Reply": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"answers": map[string]any{"type": "array"},
				},
				"required": []any{"answers"},
			},
		}},
	}
}

func eventUnion(names ...string) map[string]any {
	refs := make([]any, 0, len(names))
	for _, name := range names {
		refs = append(refs, map[string]any{"$ref": "#/components/schemas/" + name})
	}

	return map[string]any{"anyOf": refs}
}

func eventSchema(eventType string, required []string) map[string]any {
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

func pendingRequestPath(itemRef string) map[string]any {
	return map[string]any{
		"get": map[string]any{
			"responses": map[string]any{"200": map[string]any{
				"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"data": map[string]any{
							"type":  "array",
							"items": map[string]any{"$ref": "#/components/schemas/" + itemRef},
						},
					},
				}}},
			}},
		},
	}
}

func pendingArrayPath(itemRef string) map[string]any {
	return map[string]any{
		"get": map[string]any{
			"responses": map[string]any{"200": map[string]any{
				"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"type":  "array",
					"items": map[string]any{"$ref": "#/components/schemas/" + itemRef},
				}}},
			}},
		},
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}
