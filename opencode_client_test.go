package opencodeacp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeHTTPFakeServerReadinessDocAndQuestionRoutes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if username, password, ok := r.BasicAuth(); !ok || username != "opencode" || password != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seen = append(seen, r.Method+" "+r.URL.Path)
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
		case "/api/session/s/question/q/reply":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode reply body: %v", err)
			}
			writeJSON(t, w, map[string]any{"ok": true})
		case "/api/session/s/question/q/reject":
			writeJSON(t, w, map[string]any{"ok": true})
		case "/api/permission/request":
			writeJSON(t, w, map[string]any{"data": []map[string]any{{"id": "p", "sessionID": "s"}}})
		case "/api/session/s/permission/p/reply":
			writeJSON(t, w, map[string]any{"ok": true})
		default:
			writeJSON(t, w, map[string]any{"id": "s"})
		}
	}))
	defer server.Close()

	client := &openCodeServer{
		httpClient: server.Client(),
		baseURL:    server.URL,
		username:   "opencode",
		password:   "secret",
		events:     make(chan openCodeEvent, 8),
		errs:       make(chan error, 8),
		closed:     make(chan struct{}),
	}
	if err := client.waitReady(ctx, ctx, openCodeStartOptions{SkipVersionGate: true}); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	questions, err := client.PendingQuestions(ctx)
	if err != nil || len(questions) != 1 || questions[0].ID != "q" {
		t.Fatalf("PendingQuestions = %#v err=%v", questions, err)
	}
	if err := client.ReplyQuestion(ctx, "s", "q", [][]string{{"yes"}}); err != nil {
		t.Fatalf("ReplyQuestion: %v", err)
	}
	if err := client.RejectQuestion(ctx, "s", "q"); err != nil {
		t.Fatalf("RejectQuestion: %v", err)
	}
	permissions, err := client.PendingPermissions(ctx)
	if err != nil || len(permissions) != 1 || permissions[0].ID != "p" {
		t.Fatalf("PendingPermissions = %#v err=%v", permissions, err)
	}
	if err := client.ReplyPermission(ctx, "s", "p", "once", "ok"); err != nil {
		t.Fatalf("ReplyPermission: %v", err)
	}
	if !containsString(seen, "GET /doc") || !containsString(seen, "POST /api/session/s/question/q/reply") {
		t.Fatalf("seen paths = %#v", seen)
	}
	close(client.closed)
}

func TestOpenCodeDocFailClosedAndHelpers(t *testing.T) {
	doc := fullOpenCodeDoc()
	paths := doc["paths"].(map[string]any)
	delete(paths, "/api/session/{sessionID}/question/{requestID}/reply")
	if err := validateOpenCodeDoc(doc); err == nil || !strings.Contains(err.Error(), "question") {
		t.Fatalf("validateOpenCodeDoc error = %v", err)
	}
	if compareSemver("1.2.3", "1.2.4") >= 0 || compareSemver("1.3.0", "1.2.9") <= 0 || compareSemver("v1.2.3-beta", "1.2.3") != 0 {
		t.Fatal("compareSemver returned unexpected ordering")
	}
	if safePathName("../a:b") != "__a_b" || safePathName("") != "session" {
		t.Fatalf("safePathName mismatch")
	}
	if got := envMapToSlice(map[string]string{"B": "2", "A": "1"}); !reflect.DeepEqual(got, []string{"A=1", "B=2"}) {
		t.Fatalf("envMapToSlice = %#v", got)
	}
	if passwordHash("secret") == "" {
		t.Fatal("empty password hash")
	}
}

func fullOpenCodeDoc() map[string]any {
	paths := map[string]any{}
	for _, path := range []string{
		"/config/providers",
		"/event",
		"/session",
		"/session/{sessionID}",
		"/session/{sessionID}/message",
		"/session/{sessionID}/abort",
		"/session/{sessionID}/fork",
		"/session/{sessionID}/todo",
		"/session/{sessionID}/revert",
		"/session/{sessionID}/unrevert",
		"/api/session/{sessionID}/permission/{requestID}/reply",
		"/api/permission/request",
		"/api/session/{sessionID}/question/{requestID}/reply",
		"/api/session/{sessionID}/question/{requestID}/reject",
		"/api/question/request",
	} {
		paths[path] = map[string]any{}
	}
	return map[string]any{"paths": paths}
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
