package opencodeacp

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

type fakeOpenCodeClient struct {
	mu sync.Mutex

	xdg opencode.XDGDirs

	createSession opencode.NativeSession
	getSession    opencode.NativeSession
	listSessions  []opencode.NativeSession
	forkSession   opencode.NativeSession
	messages      []opencode.NativeMessage
	todos         []opencode.NativeTodo
	providers     opencode.ProvidersResponse
	agents        []opencode.NativeAgent
	commands      []opencode.NativeCommand

	pendingPermissions []opencode.PermissionRequest
	permissionReplies  []fakePermissionReply
	pendingQuestions   []opencode.QuestionRequest
	questionReplies    []fakeQuestionReply
	questionRejects    []fakeQuestionReject

	createSessionFunc func(context.Context, string) (opencode.NativeSession, error)
	sendMessage       func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error)
	runCommand        func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error)

	aborts         []string
	deleted        []string
	closed         bool
	events         chan opencode.Event
	errs           chan error
	createErr      error
	getErr         error
	listErr        error
	deleteErr      error
	messagesErr    error
	commandsErr    error
	commandErr     error
	abortErr       error
	forkErr        error
	todosErr       error
	providersErr   error
	agentsErr      error
	permissionsErr error
	questionsErr   error
	replyErr       error
	closeErr       error
}

type fakePermissionReply struct {
	sessionID string
	requestID string
	route     opencode.PermissionRoute
	reply     string
	message   string
}

type fakeQuestionReply struct {
	sessionID string
	requestID string
	route     opencode.QuestionRoute
	answers   [][]string
}

type fakeQuestionReject struct {
	sessionID string
	requestID string
	route     opencode.QuestionRoute
}

func newFakeOpenCodeClient() *fakeOpenCodeClient {
	return &fakeOpenCodeClient{
		providers: testProviders(),
		events:    make(chan opencode.Event, 16),
		errs:      make(chan error, 16),
	}
}

func (c *fakeOpenCodeClient) Close(context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	return c.closeErr
}

func (c *fakeOpenCodeClient) CreateSession(ctx context.Context, title string) (opencode.NativeSession, error) {
	if c.createSessionFunc != nil {
		return c.createSessionFunc(ctx, title)
	}

	return c.createSession, c.createErr
}

func (c *fakeOpenCodeClient) GetSession(context.Context, string) (opencode.NativeSession, error) {
	return c.getSession, c.getErr
}

func (c *fakeOpenCodeClient) ListSessions(context.Context, string) ([]opencode.NativeSession, error) {
	return append([]opencode.NativeSession(nil), c.listSessions...), c.listErr
}

func (c *fakeOpenCodeClient) DeleteSession(_ context.Context, id string) error {
	c.mu.Lock()
	c.deleted = append(c.deleted, id)
	c.mu.Unlock()

	return c.deleteErr
}

func (c *fakeOpenCodeClient) Commands(context.Context) ([]opencode.NativeCommand, error) {
	return append([]opencode.NativeCommand(nil), c.commands...), c.commandsErr
}

func (c *fakeOpenCodeClient) RunCommand(ctx context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
	if c.runCommand != nil {
		return c.runCommand(ctx, id, req)
	}

	return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop"}}, c.commandErr
}

func (c *fakeOpenCodeClient) SendMessage(ctx context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
	if c.sendMessage != nil {
		return c.sendMessage(ctx, id, req)
	}

	return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
}

func (c *fakeOpenCodeClient) Messages(context.Context, string) ([]opencode.NativeMessage, error) {
	return append([]opencode.NativeMessage(nil), c.messages...), c.messagesErr
}

func (c *fakeOpenCodeClient) Abort(_ context.Context, id string) error {
	c.mu.Lock()
	c.aborts = append(c.aborts, id)
	c.mu.Unlock()

	return c.abortErr
}

func (c *fakeOpenCodeClient) Fork(context.Context, string, string) (opencode.NativeSession, error) {
	return c.forkSession, c.forkErr
}

func (c *fakeOpenCodeClient) Todos(context.Context, string) ([]opencode.NativeTodo, error) {
	return append([]opencode.NativeTodo(nil), c.todos...), c.todosErr
}

func (c *fakeOpenCodeClient) ConfigProviders(context.Context) (opencode.ProvidersResponse, error) {
	return c.providers, c.providersErr
}

func (c *fakeOpenCodeClient) Agents(context.Context) ([]opencode.NativeAgent, error) {
	return append([]opencode.NativeAgent(nil), c.agents...), c.agentsErr
}

func (c *fakeOpenCodeClient) PendingPermissions(context.Context) ([]opencode.PermissionRequest, error) {
	return append([]opencode.PermissionRequest(nil), c.pendingPermissions...), c.permissionsErr
}

func (c *fakeOpenCodeClient) ReplyPermission(_ context.Context, req opencode.PermissionRequest, reply string, message string) error {
	c.mu.Lock()
	c.permissionReplies = append(c.permissionReplies, fakePermissionReply{
		sessionID: req.SessionID,
		requestID: req.ID,
		route:     req.Route(),
		reply:     reply,
		message:   message,
	})
	c.mu.Unlock()

	return c.replyErr
}

func (c *fakeOpenCodeClient) PendingQuestions(context.Context) ([]opencode.QuestionRequest, error) {
	return append([]opencode.QuestionRequest(nil), c.pendingQuestions...), c.questionsErr
}

func (c *fakeOpenCodeClient) ReplyQuestion(_ context.Context, req opencode.QuestionRequest, answers [][]string) error {
	copied := make([][]string, len(answers))
	for i := range answers {
		copied[i] = append([]string(nil), answers[i]...)
	}
	c.mu.Lock()
	c.questionReplies = append(c.questionReplies, fakeQuestionReply{sessionID: req.SessionID, requestID: req.ID, route: req.Route(), answers: copied})
	c.mu.Unlock()

	return c.replyErr
}

func (c *fakeOpenCodeClient) RejectQuestion(_ context.Context, req opencode.QuestionRequest) error {
	c.mu.Lock()
	c.questionRejects = append(c.questionRejects, fakeQuestionReject{sessionID: req.SessionID, requestID: req.ID, route: req.Route()})
	c.mu.Unlock()

	return c.replyErr
}

func (c *fakeOpenCodeClient) Events() <-chan opencode.Event {
	return c.events
}

func (c *fakeOpenCodeClient) EventErrors() <-chan error {
	return c.errs
}

func (c *fakeOpenCodeClient) XDGDirs() opencode.XDGDirs {
	return c.xdg
}

func (c *fakeOpenCodeClient) abortCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.aborts)
}

func (c *fakeOpenCodeClient) permissionReply(index int) fakePermissionReply {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.permissionReplies[index]
}

func (c *fakeOpenCodeClient) permissionReplyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.permissionReplies)
}

func (c *fakeOpenCodeClient) questionReply(index int) fakeQuestionReply {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.questionReplies[index]
}

func (c *fakeOpenCodeClient) questionReplyCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.questionReplies)
}

func (c *fakeOpenCodeClient) questionRejectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.questionRejects)
}

type recordingAgentClient struct {
	done chan struct{}

	mu sync.Mutex

	updates      []acp.SessionNotification
	extensions   []extensionNotification
	permissions  []acp.RequestPermissionRequest
	elicitations []acp.UnstableCreateElicitationRequest
	scopes       []elicitationScope

	permission               acp.RequestPermissionResponse
	elicitation              acp.UnstableCreateElicitationResponse
	permissionStarted        chan struct{}
	permissionRelease        chan struct{}
	permissionIgnoreContext  bool
	elicitationStarted       chan struct{}
	elicitationRelease       chan struct{}
	elicitationIgnoreContext bool
	permErr                  error
	elicitErr                error
	updateErr                error
	notifyErr                error
}

type extensionNotification struct {
	method string
	params any
}

func newRecordingAgentClient() *recordingAgentClient {
	return &recordingAgentClient{
		done:       make(chan struct{}),
		permission: acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("once")},
		elicitation: acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{}},
		},
	}
}

func (c *recordingAgentClient) Done() <-chan struct{} {
	return c.done
}

func (c *recordingAgentClient) UnstableCreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, request, elicitationScope{})
}

func (c *recordingAgentClient) CreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	c.mu.Lock()
	c.elicitations = append(c.elicitations, request)
	c.scopes = append(c.scopes, scope)
	resp := c.elicitation
	err := c.elicitErr
	started := c.elicitationStarted
	release := c.elicitationRelease
	ignoreContext := c.elicitationIgnoreContext
	c.mu.Unlock()
	signalTestHook(started)
	if release != nil {
		if ignoreContext {
			<-release

			return resp, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return acp.UnstableCreateElicitationResponse{}, ctx.Err()
		}
	}

	return resp, err
}

func (c *recordingAgentClient) RequestPermission(ctx context.Context, request acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.permissions = append(c.permissions, request)
	resp := c.permission
	err := c.permErr
	started := c.permissionStarted
	release := c.permissionRelease
	ignoreContext := c.permissionIgnoreContext
	c.mu.Unlock()
	signalTestHook(started)
	if release != nil {
		if ignoreContext {
			<-release

			return resp, err
		}
		select {
		case <-release:
		case <-ctx.Done():
			return acp.RequestPermissionResponse{}, ctx.Err()
		}
	}

	return resp, err
}

func (c *recordingAgentClient) SessionUpdate(_ context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, notification)
	err := c.updateErr
	c.mu.Unlock()

	return err
}

func (c *recordingAgentClient) NotifyExtension(_ context.Context, method string, params any) error {
	c.mu.Lock()
	c.extensions = append(c.extensions, extensionNotification{method: method, params: params})
	err := c.notifyErr
	c.mu.Unlock()

	return err
}

func (c *recordingAgentClient) updateCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.updates)
}

func (c *recordingAgentClient) permissionRequestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.permissions)
}

func signalTestHook(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func assertInvalidModelField(t testingT, err error, field string) {
	t.Helper()
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %v, want RequestError", err)
	}
	if reqErr.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", reqErr.Code)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data = %#v, want map", reqErr.Data)
	}
	if data["error"] != "invalid_model" || data["field"] != field {
		t.Fatalf("invalid model data = %#v, want field %q", data, field)
	}
}

// requireInvalidParamsData asserts err is an ACP invalid-params error (code
// -32602) whose data map equals want exactly.
func requireInvalidParamsData(t testingT, err error, want map[string]any) {
	t.Helper()

	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %v, want RequestError", err)
	}

	if reqErr.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", reqErr.Code)
	}

	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data = %#v, want map", reqErr.Data)
	}

	if !reflect.DeepEqual(data, want) {
		t.Fatalf("invalid params data = %#v, want %#v", data, want)
	}
}

// assertTurnFailed asserts err is the uniform OpenCode turn-failure error:
// code -32603, data.error == "opencode_turn_failed", data.cause == wantCause,
// and data.message contains wantMessageSubstr (skipped when empty). It returns
// the decoded data map for any additional field assertions.
func assertTurnFailed(t testingT, err error, wantCause string, wantMessageSubstr string) map[string]any {
	t.Helper()
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %v, want RequestError", err)
	}
	if reqErr.Code != -32603 {
		t.Fatalf("error code = %d, want -32603", reqErr.Code)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data = %#v, want map", reqErr.Data)
	}
	if data[jsonFieldError] != turnFailedErrorTag {
		t.Fatalf("data.error = %#v, want %q", data[jsonFieldError], turnFailedErrorTag)
	}
	if data[jsonFieldCause] != wantCause {
		t.Fatalf("data.cause = %#v, want %q", data[jsonFieldCause], wantCause)
	}
	message, _ := data[jsonFieldMessage].(string)
	if wantMessageSubstr != "" && !strings.Contains(message, wantMessageSubstr) {
		t.Fatalf("data.message = %q, want substring %q", message, wantMessageSubstr)
	}

	return data
}

func assertUnknownSessionError(t testingT, err error) {
	t.Helper()
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("error = %v, want RequestError", err)
	}
	if reqErr.Code != -32602 {
		t.Fatalf("error code = %d, want -32602", reqErr.Code)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("error data = %#v, want map", reqErr.Data)
	}
	if data[jsonFieldError] != errValueSessionUnknown || data[jsonFieldField] != jsonFieldSessionID {
		t.Fatalf("unknown session data = %#v, want {error:unknown session, field:sessionId}", data)
	}
}

type testingT interface {
	Helper()
	Fatalf(string, ...any)
}

func testNativeSession(id string) opencode.NativeSession {
	native := opencode.NativeSession{ID: id, Title: "Test", Agent: "build"}
	native.Model.ProviderID = "openai"
	native.Model.ModelID = "gpt-test"
	native.Time.Updated = 1_700_000_000_000

	return native
}

func testSession(agent *Agent, client *fakeOpenCodeClient) *session {
	if client.xdg.Root == "" {
		root, err := os.MkdirTemp("", "acp-go-opencode-test-*")
		if err == nil {
			client.xdg, _ = opencode.CreateXDGDirs(root, "session-1")
		}
	}

	return newSession(agent, "session-1", "/tmp/project", nil, testNativeSession("native-1"), client, sessionMeta{}, idmapRecord{
		SessionID:       "session-1",
		NativeSessionID: "native-1",
		Format:          SessionStoreFormat,
	})
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
