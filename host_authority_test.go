package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type authorityTrace struct {
	mu            sync.Mutex
	events        []string
	prepared      map[string]bool
	hidden        map[string]string
	process       *authorityTraceProcess
	startErr      error
	prepareErr    error
	prepareAt     int
	reclaimErr    error
	waitErr       error
	unusableStdio bool
	readinessErr  bool
	writeWAL      bool
	hideTree      bool
}

func newAuthorityTrace() *authorityTrace {
	return &authorityTrace{prepared: map[string]bool{}, hidden: map[string]string{}}
}

func (a *authorityTrace) record(event string) {
	a.mu.Lock()
	a.events = append(a.events, event)
	a.mu.Unlock()
}

func (a *authorityTrace) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.events...)
}

func (a *authorityTrace) NativeEnvironment() map[string]string {
	a.record("environment")

	return map[string]string{"PATH": "/authority/bin", "AUTHORITY_CANARY": "present"}
}

func (a *authorityTrace) PrepareNativeTree(_ context.Context, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.prepareAt++
	if a.prepareErr != nil {
		if a.hideTree {
			hidden := path + ".authority"
			if err := os.Rename(path, hidden); err != nil {
				return err
			}

			a.hidden[path] = hidden
		}

		a.events = append(a.events, "prepare-refused:"+filepath.Base(path))

		return a.prepareErr
	}

	if a.hideTree {
		hidden := path + ".authority"
		if err := os.Rename(path, hidden); err != nil {
			return err
		}

		a.hidden[path] = hidden
	}

	a.events = append(a.events, "prepare:"+filepath.Base(path))
	a.prepared[path] = true

	return nil
}

func (a *authorityTrace) ReclaimNativeTree(_ context.Context, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.process != nil && !a.process.settled {
		return errors.New("reclaim before terminal wait")
	}
	if !a.prepared[path] {
		return errors.New("reclaim of unprepared tree")
	}
	if a.reclaimErr != nil {
		return a.reclaimErr
	}

	if hidden := a.hidden[path]; hidden != "" {
		if err := os.Rename(hidden, path); err != nil {
			return err
		}

		delete(a.hidden, path)
	}
	if a.writeWAL {
		database := filepath.Join(path, "data", "opencode.db")
		if _, err := os.Stat(database); err == nil {
			for _, candidate := range []string{database, database + "-wal"} {
				file, openErr := os.OpenFile(candidate, os.O_RDWR|os.O_APPEND, 0)
				if openErr != nil {
					return openErr
				}
				if _, writeErr := file.Write([]byte("reopened")); writeErr != nil {
					_ = file.Close()

					return writeErr
				}
				if closeErr := file.Close(); closeErr != nil {
					return closeErr
				}
			}
			a.events = append(a.events, "wal-reopen")
		}
	}

	a.events = append(a.events, "reclaim:"+filepath.Base(path))
	delete(a.prepared, path)

	return nil
}

func (a *authorityTrace) StartNative(_ context.Context, request NativeRequest) (NativeProcess, error) {
	if a.startErr != nil {
		a.record("start-refused")

		return nil, a.startErr
	}
	a.mu.Lock()
	prepared := a.prepared[request.WorkingDirectory]
	hiddenRoot := a.hidden[request.WorkingDirectory]
	a.events = append(a.events, "start:"+request.Executable)
	a.mu.Unlock()
	if !prepared {
		return nil, errors.New("native start outside prepared tree")
	}
	if hiddenRoot != "" {
		if _, err := os.Stat(request.WorkingDirectory); !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("adapter accessed prepared runtime tree before reclaim")
		}
	}

	port := 0
	for index, argument := range request.Arguments {
		if argument == "--port" && index+1 < len(request.Arguments) {
			port, _ = strconv.Atoi(request.Arguments[index+1])
		}
	}
	if port == 0 {
		return nil, errors.New("native start omitted port")
	}

	process := &authorityTraceProcess{
		authority: a, terminal: make(chan struct{}), stdin: &authorityTraceInput{authority: a},
		waitErr: a.waitErr,
	}
	server := httptest.NewUnstartedServer(a.nativeHandler())
	if err := server.Listener.Close(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	server.Listener = listener
	server.Start()
	process.server = server
	a.process = process
	a.proveCarrierReady()
	if a.writeWAL {
		nativeRoot := request.WorkingDirectory
		if hidden := a.hidden[nativeRoot]; hidden != "" {
			nativeRoot = hidden
		}
		database := filepath.Join(nativeRoot, "data", "opencode.db")
		if err := os.MkdirAll(filepath.Dir(database), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(database, []byte("SQLite format 3\x00"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(database+"-wal", []byte("wal-before-revoke"), 0o600); err != nil {
			return nil, err
		}
	}
	if a.unusableStdio {
		process.stderrUnavailable = true
	}

	return process, nil
}

func (a *authorityTrace) nativeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			if a.readinessErr {
				http.Error(w, "not ready", http.StatusServiceUnavailable)

				return
			}
			writeAuthorityJSON(w, map[string]any{"healthy": true, "version": "9.9.9"})
		case "/doc":
			writeAuthorityJSON(w, authorityOpenCodeDoc())
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case "/instance/dispose", "/config":
			if r.URL.Path == "/instance/dispose" {
				a.record("dispose")
			}
			writeAuthorityJSON(w, map[string]any{})
		default:
			writeAuthorityJSON(w, map[string]any{})
		}
	})
}

func writeAuthorityJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (a *authorityTrace) proveCarrierReady() {
	a.mu.Lock()
	hidden := make([]string, 0, len(a.hidden))
	for _, path := range a.hidden {
		hidden = append(hidden, path)
	}
	a.mu.Unlock()

	for _, root := range hidden {
		paths, err := filepath.Glob(filepath.Join(root, ".session-carrier-*", "session-carrier.mjs"))
		if err != nil || len(paths) != 1 {
			continue
		}

		source, err := os.ReadFile(paths[0])
		if err != nil {
			continue
		}
		endpoint := authorityJSConstant(source, "BROKER_ENDPOINT")
		token := authorityJSConstant(source, "BROKER_TOKEN")
		if endpoint == "" || token == "" {
			continue
		}
		request, err := http.NewRequest(http.MethodPost, endpoint+"/ready", http.NoBody)
		if err != nil {
			return
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}

		return
	}
}

func authorityJSConstant(source []byte, name string) string {
	match := regexp.MustCompile(`const ` + name + ` = ("(?:[^"\\]|\\.)*")`).FindSubmatch(source)
	if len(match) != 2 {
		return ""
	}
	value, _ := strconv.Unquote(string(match[1]))

	return value
}

func authorityOpenCodeDoc() map[string]any {
	paths := map[string]any{}
	for _, path := range []string{
		"/config", "/config/providers", "/command", "/event", "/session/status", "/session", "/session/{sessionID}",
		"/session/{sessionID}/command", "/session/{sessionID}/message", "/session/{sessionID}/prompt_async",
		"/session/{sessionID}/abort", "/session/{sessionID}/fork", "/session/{sessionID}/todo",
		"/session/{sessionID}/revert", "/session/{sessionID}/unrevert", "/permission",
		"/permission/{requestID}/reply", "/question", "/question/{requestID}/reply", "/question/{requestID}/reject",
		"/api/session/{sessionID}/permission/{requestID}/reply", "/api/permission/request",
		"/api/session/{sessionID}/question/{requestID}/reply", "/api/session/{sessionID}/question/{requestID}/reject",
		"/api/question/request",
	} {
		paths[path] = map[string]any{}
	}
	paths["/api/permission/request"] = authorityPendingRequestPath("PermissionV2Request")
	paths["/permission"] = authorityPendingArrayPath("PermissionRequest")
	paths["/api/question/request"] = authorityPendingRequestPath("QuestionV2Request")
	paths["/question"] = authorityPendingArrayPath("QuestionRequest")
	paths["/api/session/{sessionID}/permission/{requestID}/reply"] = authorityReplyPath(
		map[string]any{"type": "object", "properties": map[string]any{
			"reply": map[string]any{"$ref": "#/components/schemas/PermissionV2Reply"}, "message": map[string]any{"type": "string"},
		}, "required": []any{"reply"}}, http.StatusNoContent,
	)
	paths["/permission/{requestID}/reply"] = authorityReplyPath(map[string]any{
		"type": "object", "properties": map[string]any{
			"reply": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"},
		}, "required": []any{"reply"},
	}, http.StatusOK)
	paths["/api/session/{sessionID}/question/{requestID}/reply"] = authorityReplyPath(
		map[string]any{"$ref": "#/components/schemas/QuestionV2Reply"}, http.StatusNoContent,
	)
	paths["/api/session/{sessionID}/question/{requestID}/reject"] = authorityNoContentPath()
	paths["/question/{requestID}/reply"] = authorityReplyPath(nil, http.StatusOK)
	paths["/question/{requestID}/reject"] = authorityReplyPath(nil, http.StatusOK)

	return map[string]any{"paths": paths, "components": map[string]any{"schemas": authoritySchemas()}}
}

func authorityPendingRequestPath(item string) map[string]any {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{"data": map[string]any{
			"type": "array", "items": map[string]any{"$ref": "#/components/schemas/" + item},
		}},
	}

	return map[string]any{"get": map[string]any{"responses": map[string]any{
		"200": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": schema}}},
	}}}
}

func authorityPendingArrayPath(item string) map[string]any {
	schema := map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/" + item}}

	return map[string]any{"get": map[string]any{"responses": map[string]any{
		"200": map[string]any{"content": map[string]any{"application/json": map[string]any{"schema": schema}}},
	}}}
}

func authorityReplyPath(schema map[string]any, status int) map[string]any {
	post := map[string]any{"responses": map[string]any{strconv.Itoa(status): map[string]any{"description": "ok"}}}
	if schema != nil {
		post["requestBody"] = map[string]any{"required": true, "content": map[string]any{
			"application/json": map[string]any{"schema": schema},
		}}
	}

	return map[string]any{"post": post}
}

func authorityNoContentPath() map[string]any {
	return map[string]any{"post": map[string]any{"responses": map[string]any{
		"204": map[string]any{"description": "<No Content>"},
	}}}
}

func authoritySchemas() map[string]any {
	events := []struct {
		name     string
		event    string
		required []string
	}{
		{"EventPermissionV2Asked", "permission.v2.asked", []string{"id", "sessionID", "action", "resources"}},
		{"EventPermissionV2Replied", "permission.v2.replied", []string{"sessionID", "requestID", "reply"}},
		{"EventPermissionAsked", "permission.asked", []string{"id", "sessionID", "permission", "patterns"}},
		{"EventPermissionReplied", "permission.replied", []string{"sessionID", "requestID", "reply"}},
		{"EventQuestionV2Asked", "question.v2.asked", []string{"id", "sessionID", "questions"}},
		{"EventQuestionV2Replied", "question.v2.replied", []string{"sessionID", "requestID", "answers"}},
		{"EventQuestionAsked", "question.asked", []string{"id", "sessionID", "questions"}},
		{"EventQuestionReplied", "question.replied", []string{"sessionID", "requestID", "answers"}},
		{"EventMessagePartUpdated", "message.part.updated", []string{"sessionID", "part", "time"}},
		{"EventServerConnected", "server.connected", nil},
		{"EventSessionIdle", "session.idle", []string{"sessionID"}},
		{"EventSessionStatus", "session.status", []string{"sessionID", "status"}},
		{"EventSessionError", "session.error", nil},
	}
	refs := make([]any, 0, len(events))
	schemas := map[string]any{}
	for _, event := range events {
		refs = append(refs, map[string]any{"$ref": "#/components/schemas/" + event.name})
		schemas[event.name] = authorityEventSchema(event.event, event.required)
	}
	schemas["Event"] = map[string]any{"anyOf": refs}
	schemas["QuestionV2Reply"] = map[string]any{
		"type": "object", "properties": map[string]any{"answers": map[string]any{"type": "array"}}, "required": []any{"answers"},
	}
	schemas["OutputFormatJsonSchema"] = map[string]any{
		"type": "object", "properties": map[string]any{
			"type":       map[string]any{"type": "string", "enum": []any{"json_schema"}},
			"schema":     map[string]any{"$ref": "#/components/schemas/JSONSchema"},
			"retryCount": map[string]any{"type": "integer"},
		}, "required": []any{"type", "schema"},
	}

	return schemas
}

func authorityEventSchema(eventType string, required []string) map[string]any {
	properties := map[string]any{}
	for _, property := range required {
		properties[property] = map[string]any{"type": "string"}
	}

	return map[string]any{
		"type": "object", "properties": map[string]any{
			"id": map[string]any{"type": "string"}, "type": map[string]any{"type": "string", "enum": []string{eventType}},
			"properties": map[string]any{
				"type": "object", "properties": properties, "required": required, "additionalProperties": false,
			},
		}, "required": []string{"id", "type", "properties"}, "additionalProperties": false,
	}
}

type authorityTraceInput struct {
	authority *authorityTrace
	closed    bool
}

func (w *authorityTraceInput) Write(value []byte) (int, error) { return len(value), nil }
func (w *authorityTraceInput) Close() error {
	if !w.closed {
		w.closed = true
		w.authority.record("protocol-close")
	}

	return nil
}

type authorityTraceProcess struct {
	authority         *authorityTrace
	stdin             *authorityTraceInput
	terminal          chan struct{}
	server            *httptest.Server
	revoke            sync.Once
	settled           bool
	waitErr           error
	stderrUnavailable bool
}

func (p *authorityTraceProcess) Stdin() io.WriteCloser { return p.stdin }
func (*authorityTraceProcess) Stdout() io.ReadCloser   { return io.NopCloser(&emptyReader{}) }
func (p *authorityTraceProcess) Stderr() io.ReadCloser {
	if p.stderrUnavailable {
		return nil
	}

	return io.NopCloser(&emptyReader{})
}

func (p *authorityTraceProcess) Wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-p.terminal:
		p.authority.mu.Lock()
		p.settled = true
		p.authority.events = append(p.authority.events, "wait:terminal")
		p.authority.mu.Unlock()

		return NativeResult{Revoked: true}, p.waitErr
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (p *authorityTraceProcess) Revoke(context.Context) error {
	p.revoke.Do(func() {
		p.authority.record("revoke")
		if p.server != nil {
			p.server.Close()
		}
		close(p.terminal)
	})

	return nil
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

type fixedProcessAuthority struct {
	process NativeProcess
}

func (*fixedProcessAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/bin"}
}
func (*fixedProcessAuthority) PrepareNativeTree(context.Context, string) error {
	return nil
}
func (*fixedProcessAuthority) ReclaimNativeTree(context.Context, string) error {
	return nil
}
func (a *fixedProcessAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return a.process, nil
}

type detachingWaitProcess struct {
	terminal chan struct{}
}

func (*detachingWaitProcess) Stdin() io.WriteCloser { return nopWriteCloser{Writer: io.Discard} }
func (*detachingWaitProcess) Stdout() io.ReadCloser { return io.NopCloser(&emptyReader{}) }
func (*detachingWaitProcess) Stderr() io.ReadCloser { return io.NopCloser(&emptyReader{}) }
func (p *detachingWaitProcess) Wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-p.terminal:
		return NativeResult{ExitCode: 23}, nil
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}
func (*detachingWaitProcess) Revoke(context.Context) error { return nil }

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

func TestHostAuthorityWaitCancellationDetachesWithoutContainmentFailure(t *testing.T) {
	process := &detachingWaitProcess{terminal: make(chan struct{})}
	starter := authorityProcessStarter(&fixedProcessAuthority{process: process})
	handle, err := starter(t.Context(), "opencode", nil, []string{"PATH=/bin"}, t.TempDir())
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = handle.Await(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)

	close(process.terminal)
	result, err := handle.Await(t.Context())
	require.NoError(t, err)
	require.Equal(t, 23, result.ExitCode)
}

func TestHostAuthoritySnapshotNeverAccessesPreparedRuntimeTree(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "runtime")
	hidden := root + ".authority"
	require.NoError(t, os.MkdirAll(root, 0o700))
	require.NoError(t, os.Rename(root, hidden))
	require.NoError(t, os.MkdirAll(opencode.ControlRootForXDG(root), 0o700))

	client := newFakeOpenCodeClient()
	originalRoot := client.xdg.Root
	t.Cleanup(func() { _ = os.RemoveAll(originalRoot) })
	client.xdg = opencode.XDGDirs{Root: root}
	store := NewInMemorySessionStore()
	agent := NewAgent(
		WithHostAuthority(&fixedProcessAuthority{}),
		WithSessionStore(store),
	)
	current := testSession(t, agent, client)

	require.NoError(t, current.snapshotToStore(t.Context()))
	require.NoDirExists(t, root)
	require.DirExists(t, hidden)
	entries, err := store.Load(t.Context(), SessionKey{SessionID: string(current.id)})
	require.NoError(t, err)
	require.NotEmpty(t, entries)
}

func runManagedTrace(t *testing.T, agent *Agent, authority *authorityTrace, remove bool) ([]string, string, error) {
	t.Helper()

	authority.mu.Lock()
	authority.events = nil
	authority.mu.Unlock()

	runtime, err := agent.startSharedRuntime(context.Background())
	root := ""
	if runtime != nil {
		root = runtime.XDGDirs().Root
		shutdownErr := runtime.Shutdown(context.Background())
		err = errors.Join(err, shutdownErr)
	}
	_ = remove

	return authority.snapshot(), root, err
}

func TestHostAuthorityManagedLaunchTrace(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, _, err := runManagedTrace(t, agent, authority, false)
	require.NoError(t, err)
	normalized := normalizeAuthorityTrace(events)
	require.Equal(t, 1, countAuthorityEvents(normalized, "prepare:acp-go-opencode-runtime-"))
	require.Less(t, indexOfAuthorityEvent(normalized, "prepare:acp-go-opencode-runtime-"), indexOfAuthorityEvent(normalized, "start:opencode"))
	require.Less(t, indexOfAuthorityEvent(normalized, "start:opencode"), indexOfAuthorityEvent(normalized, "protocol-close"))
	require.Less(t, indexOfAuthorityEvent(normalized, "dispose"), indexOfAuthorityEvent(normalized, "protocol-close"))
	require.Less(t, indexOfAuthorityEvent(normalized, "protocol-close"), indexOfAuthorityEvent(normalized, "revoke"))
	require.Less(t, indexOfAuthorityEvent(normalized, "revoke"), indexOfAuthorityEvent(normalized, "wait:terminal"))
	require.Less(t, indexOfAuthorityEvent(normalized, "wait:terminal"), indexOfAuthorityEvent(normalized, "reclaim:acp-go-opencode-runtime-"))
}

func TestHostAuthorityPreparedTreeExclusivity(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, root, err := runManagedTrace(t, agent, authority, false)
	require.NoError(t, err)
	_, statErr := os.Stat(root)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	authority.mu.Lock()
	defer authority.mu.Unlock()
	require.Empty(t, authority.prepared, "the adapter retained a prepared tree after reclaim")
	require.NotContains(t, authority.prepared, opencode.ControlRootForXDG(root),
		"the runtime-owned control lock entered the prepared tree set")
	require.NotEmpty(t, events)
}

func TestHostAuthorityReclaimPrecedesRemoval(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, root, err := runManagedTrace(t, agent, authority, true)
	require.NoError(t, err)
	normalized := normalizeAuthorityTrace(events)
	require.Less(t, indexOfAuthorityEvent(normalized, "wait:terminal"), indexOfAuthorityEvent(normalized, "reclaim:acp-go-opencode-runtime-"))
	_, statErr := os.Stat(root)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestHostAuthorityForcedRevokeLeavesSQLiteWALReopenable(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))

	authority.writeWAL = true
	runtime, err := agent.startSharedRuntime(context.Background())
	require.NoError(t, err)
	root := runtime.XDGDirs().Root
	require.NoError(t, runtime.Shutdown(context.Background()))
	walReopen := indexOfAuthorityEvent(authority.events, "wal-reopen")
	require.GreaterOrEqual(t, walReopen, 0)
	replacement, err := agent.startSharedRuntime(context.Background())
	require.NoError(t, err)
	secondStart := -1
	seenStarts := 0
	for index, event := range authority.events {
		if event == "start:opencode" {
			seenStarts++
			if seenStarts == 2 {
				secondStart = index
			}
		}
	}
	require.Less(t, walReopen, secondStart, "SQLite/WAL must reopen before replacement admission")
	require.NoError(t, replacement.Shutdown(context.Background()))
	for _, path := range []string{filepath.Join(root, "data", "opencode.db"), filepath.Join(root, "data", "opencode.db-wal")} {
		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist, "runtime root is removed only after WAL was reclaimed")
	}
	for index, event := range authority.events {
		if strings.HasPrefix(event, "reclaim:") {
			require.Less(t, indexOfAuthorityEvent(authority.events, "wait:terminal"), index)
		}
	}
}

func TestHostAuthorityNoOrdinaryFallback(t *testing.T) {
	authority := newAuthorityTrace()
	authority.startErr = errors.New("authority refused")
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, _, err := runManagedTrace(t, agent, authority, false)
	require.ErrorIs(t, err, authority.startErr)
	require.NotErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Contains(t, events, "start-refused")
	require.Equal(t, 1, countAuthorityEvents(events, "start-refused"))

	called := false
	agent = NewAgent(WithHostAuthority(nil))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		called = true

		return nil, errors.New("unexpected factory call")
	}
	_, err = agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.False(t, called)
}

func TestManagedDurableHomeRestrictsSeedFiles(t *testing.T) {
	authority := newAuthorityTrace()
	agent := NewAgent(
		WithHostAuthority(authority),
		WithHome(t.TempDir()),
		WithSeedFiles(map[string]string{"notes.txt": "unsupported"}),
	)

	_, err := agent.startSharedRuntime(t.Context())
	requireUnsupportedField(t, err, "seedFiles[notes.txt]")
	require.Zero(t, authority.prepareAt)
	require.Zero(t, countAuthorityEvents(authority.snapshot(), "start:opencode"))
}

func TestHostAuthorityManagedFailureMatrix(t *testing.T) {
	tests := []struct {
		name              string
		configure         func(*authorityTrace)
		want              error
		wantText          string
		wantReclaim       bool
		wantOpaqueTree    bool
		wantRetainedTrees bool
	}{
		{
			name: "prepare validation",
			configure: func(authority *authorityTrace) {
				authority.prepareErr = errors.New("carrier prepare rejected")
			},
			want: ErrContainmentIncomplete, wantText: "carrier prepare rejected", wantOpaqueTree: true,
		},
		{
			name: "start validation",
			configure: func(authority *authorityTrace) {
				authority.startErr = errors.New("start request rejected")
			},
			wantText: "start request rejected", wantReclaim: true,
		},
		{
			name: "unusable stdio settled",
			configure: func(authority *authorityTrace) {
				authority.unusableStdio = true
			},
			wantText: "unusable host stdio", wantReclaim: true,
		},
		{
			name: "unusable stdio wait failure",
			configure: func(authority *authorityTrace) {
				authority.unusableStdio = true
				authority.waitErr = errors.New("wait uncertain")
			},
			want: ErrContainmentIncomplete, wantRetainedTrees: true,
		},
		{
			name: "readiness failure",
			configure: func(authority *authorityTrace) {
				authority.readinessErr = true
			},
			wantText: "/global/health", wantReclaim: true,
		},
		{
			name: "wait failure",
			configure: func(authority *authorityTrace) {
				authority.waitErr = errors.New("wait uncertain")
			},
			want: ErrContainmentIncomplete, wantRetainedTrees: true,
		},
		{
			name: "reclaim failure",
			configure: func(authority *authorityTrace) {
				authority.reclaimErr = errors.New("reclaim uncertain")
			},
			want: ErrContainmentIncomplete, wantRetainedTrees: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority := newAuthorityTrace()
			authority.hideTree = true
			test.configure(authority)
			agent := NewAgent(
				WithHostAuthority(authority), WithScratchDir(t.TempDir()),
				WithOpenCodeHealthCheckTimeout(100*time.Millisecond),
			)
			_, _, err := runManagedTrace(t, agent, authority, false)
			require.Error(t, err)
			if test.want != nil {
				require.ErrorIs(t, err, test.want)
			}
			if test.wantText != "" {
				require.ErrorContains(t, err, test.wantText)
			}
			if test.name == "start validation" {
				require.NotErrorIs(t, err, ErrHostAuthorityUnavailable)
				require.NotErrorIs(t, err, ErrContainmentIncomplete)
			}

			events := authority.snapshot()
			reclaimed := false
			for _, event := range events {
				if strings.HasPrefix(event, "reclaim:") {
					reclaimed = true
				}
			}
			require.Equal(t, test.wantReclaim, reclaimed, events)
			if test.wantRetainedTrees {
				authority.mu.Lock()
				require.NotEmpty(t, authority.prepared)
				for _, hidden := range authority.hidden {
					_, statErr := os.Stat(hidden)
					require.NoError(t, statErr)
				}
				authority.mu.Unlock()
			}
			if test.wantOpaqueTree {
				authority.mu.Lock()
				require.Empty(t, authority.prepared)
				require.NotEmpty(t, authority.hidden)
				for _, hidden := range authority.hidden {
					_, statErr := os.Stat(hidden)
					require.NoError(t, statErr)
				}
				authority.mu.Unlock()
			}
		})
	}
}

func TestHostAuthorityBusyReclaimBlocksAdmissionUntilRetry(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	authority.reclaimErr = ErrNativeTreeBusy
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))

	_, generation, err := agent.sharedRuntimeBinding(t.Context())
	require.NoError(t, err)
	err = agent.retireSharedRuntime(generation, "busy reclaim proof")
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Nil(t, agent.runtimeFatalErr)
	starts := countAuthorityEvents(authority.snapshot(), "start:opencode")

	_, _, err = agent.sharedRuntimeBinding(t.Context())
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	require.Equal(t, starts, countAuthorityEvents(authority.snapshot(), "start:opencode"), "busy cleanup admitted a new runtime")

	authority.mu.Lock()
	authority.reclaimErr = nil
	authority.mu.Unlock()
	_, _, err = agent.sharedRuntimeBinding(t.Context())
	require.NoError(t, err)
	require.Equal(t, starts+1, countAuthorityEvents(authority.snapshot(), "start:opencode"))
	require.NoError(t, agent.Close())
}

func TestHostAuthorityFailedStartBusyCleanupBlocksReplacementUntilRetry(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	authority.startErr = errors.New("start request rejected")
	authority.reclaimErr = ErrNativeTreeBusy
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))

	_, err := agent.startSharedRuntime(t.Context())
	require.ErrorIs(t, err, authority.startErr)
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Len(t, agent.retiredNativeTrees, 1)
	starts := countAuthorityEvents(authority.snapshot(), "start-refused")

	_, err = agent.startSharedRuntime(t.Context())
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	require.Equal(t, starts, countAuthorityEvents(authority.snapshot(), "start-refused"))
	require.Len(t, agent.retiredNativeTrees, 1)

	authority.mu.Lock()
	authority.startErr = nil
	authority.reclaimErr = nil
	authority.mu.Unlock()

	runtime, err := agent.startSharedRuntime(t.Context())
	require.NoError(t, err)
	require.Empty(t, agent.retiredNativeTrees)
	require.NoError(t, runtime.Shutdown(t.Context()))
}

func TestHostAuthorityPrepareErrorQuarantinesManagedAdmission(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	authority.prepareErr = errors.New("runtime prepare rejected")
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))

	_, _, err := agent.sharedRuntimeBinding(t.Context())
	require.ErrorIs(t, err, authority.prepareErr)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.NotErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, agent.runtimeFatalErr, ErrContainmentIncomplete)
	prepareCalls := authority.prepareAt

	_, _, err = agent.sharedRuntimeBinding(t.Context())
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, prepareCalls, authority.prepareAt)
	require.Zero(t, countAuthorityEvents(authority.snapshot(), "start:opencode"))
}

func countAuthorityEvents(events []string, target string) int {
	count := 0
	for _, event := range events {
		if event == target {
			count++
		}
	}

	return count
}

func normalizeAuthorityTrace(events []string) []string {
	result := make([]string, len(events))
	for index, event := range events {
		if len(event) >= len("prepare:acp-go-opencode-runtime-") && event[:len("prepare:acp-go-opencode-runtime-")] == "prepare:acp-go-opencode-runtime-" {
			result[index] = "prepare:acp-go-opencode-runtime-"

			continue
		}
		if len(event) >= len("reclaim:acp-go-opencode-runtime-") && event[:len("reclaim:acp-go-opencode-runtime-")] == "reclaim:acp-go-opencode-runtime-" {
			result[index] = "reclaim:acp-go-opencode-runtime-"

			continue
		}
		if len(event) >= len("remove:acp-go-opencode-runtime-") && event[:len("remove:acp-go-opencode-runtime-")] == "remove:acp-go-opencode-runtime-" {
			result[index] = "remove:acp-go-opencode-runtime-"

			continue
		}
		result[index] = event
	}

	return result
}

func indexOfAuthorityEvent(events []string, target string) int {
	for index, event := range events {
		if event == target {
			return index
		}
	}

	return -1
}

type edgeAuthority struct {
	environment func() map[string]string
	prepare     func(context.Context, string) error
	reclaim     func(context.Context, string) error
	start       func(context.Context, NativeRequest) (NativeProcess, error)
}

func (a edgeAuthority) NativeEnvironment() map[string]string { return a.environment() }
func (a edgeAuthority) PrepareNativeTree(ctx context.Context, path string) error {
	if a.prepare == nil {
		return nil
	}

	return a.prepare(ctx, path)
}
func (a edgeAuthority) ReclaimNativeTree(ctx context.Context, path string) error {
	if a.reclaim == nil {
		return nil
	}

	return a.reclaim(ctx, path)
}
func (a edgeAuthority) StartNative(ctx context.Context, request NativeRequest) (NativeProcess, error) {
	return a.start(ctx, request)
}

type edgeNativeProcess struct {
	stdin  func() io.WriteCloser
	stdout func() io.ReadCloser
	stderr func() io.ReadCloser
	wait   func(context.Context) (NativeResult, error)
	revoke func(context.Context) error
}

func (p edgeNativeProcess) Stdin() io.WriteCloser { return p.stdin() }
func (p edgeNativeProcess) Stdout() io.ReadCloser { return p.stdout() }
func (p edgeNativeProcess) Stderr() io.ReadCloser { return p.stderr() }
func (p edgeNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	return p.wait(ctx)
}
func (p edgeNativeProcess) Revoke(ctx context.Context) error { return p.revoke(ctx) }

func usableEdgeNativeProcess() edgeNativeProcess {
	return edgeNativeProcess{
		stdin:  func() io.WriteCloser { return nopWriteCloser{Writer: io.Discard} },
		stdout: func() io.ReadCloser { return io.NopCloser(&emptyReader{}) },
		stderr: func() io.ReadCloser { return io.NopCloser(&emptyReader{}) },
		wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
		revoke: func(context.Context) error { return nil },
	}
}

func TestNativeAuthorityDefensiveEdges(t *testing.T) {
	_, err := hostAuthorityEnvironment(edgeAuthority{environment: func() map[string]string { panic("environment") }})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	_, err = hostAuthorityEnvironment(edgeAuthority{environment: func() map[string]string { return nil }})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	_, err = hostAuthorityEnvironment(edgeAuthority{environment: func() map[string]string {
		return map[string]string{"BAD=KEY": "value"}
	}})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	startPanic := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start:       func(context.Context, NativeRequest) (NativeProcess, error) { panic("start") },
	})
	_, err = startPanic(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	startErr := errors.New("start refused")
	refusing := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return nil, startErr
		},
	})
	_, err = refusing(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, startErr)

	var typedNil *edgeNativeProcess
	typedNilStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return typedNil, nil
		},
	})
	_, err = typedNilStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	stdioPanicProcess := usableEdgeNativeProcess()
	stdioPanicProcess.stdin = func() io.WriteCloser { panic("stdin") }
	stdioPanicStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return stdioPanicProcess, nil
		},
	})
	_, err = stdioPanicStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	missingStdioProcess := usableEdgeNativeProcess()
	missingStdioProcess.stderr = func() io.ReadCloser { return nil }
	missingStdioStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return missingStdioProcess, nil
		},
	})
	_, err = missingStdioStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorContains(t, err, "unusable host stdio")

	waitPanicProcess := usableEdgeNativeProcess()
	waitPanicProcess.wait = func(context.Context) (NativeResult, error) { panic("wait") }
	waitPanicStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return waitPanicProcess, nil
		},
	})
	handle, err := waitPanicStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.NoError(t, err)
	_, err = handle.Await(t.Context())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	stopPanicProcess := usableEdgeNativeProcess()
	stopPanicProcess.revoke = func(context.Context) error { panic("revoke") }
	stopPanicStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return stopPanicProcess, nil
		},
	})
	handle, err = stopPanicStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.NoError(t, err)
	require.ErrorIs(t, handle.Stop(t.Context()), ErrHostAuthorityUnavailable)

	settleRevokePanic := usableEdgeNativeProcess()
	settleRevokePanic.revoke = func(context.Context) error { panic("revoke") }
	require.ErrorIs(t, settleUnusableNativeProcess(settleRevokePanic), ErrHostAuthorityUnavailable)
	settleWaitPanic := usableEdgeNativeProcess()
	settleWaitPanic.wait = func(context.Context) (NativeResult, error) { panic("wait") }
	require.ErrorIs(t, settleUnusableNativeProcess(settleWaitPanic), ErrHostAuthorityUnavailable)
	require.True(t, nativeProcessNil(nil))
	require.False(t, nativeProcessNil(usableEdgeNativeProcess()))
}
