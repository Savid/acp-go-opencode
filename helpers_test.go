package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func boolPtr(value bool) *bool {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

func fixtureImage(tb testing.TB, name string) []byte {
	tb.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "images", name))
	require.NoError(tb, err)

	return data
}

func fixtureImageBase64(tb testing.TB, name string) string {
	tb.Helper()

	return base64.StdEncoding.EncodeToString(fixtureImage(tb, name))
}

// newImageSession returns a session whose workspace is a real temp directory,
// with a recording agent connection wired so update emission is observable.
func newImageSession(t *testing.T) (*session, *recordingAgentClient) {
	t.Helper()

	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, newFakeOpenCodeClient())
	session.cwd = t.TempDir()

	return session, conn
}

func dataURL(mime string, decoded []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(decoded)
}

// hookSessionStore lets a test inject failures on individual store operations
// while delegating everything else to a real in-memory store.
type hookSessionStore struct {
	*InMemorySessionStore

	onList   func() ([]string, error)
	onLoad   func(SessionKey) ([]SessionStoreEntry, error)
	onDelete func(SessionKey) error
}

func (s *hookSessionStore) ListSubkeys(ctx context.Context, key SessionKey) ([]string, error) {
	if s.onList != nil {
		return s.onList()
	}

	return s.InMemorySessionStore.ListSubkeys(ctx, key)
}

func (s *hookSessionStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	if s.onLoad != nil {
		return s.onLoad(key)
	}

	return s.InMemorySessionStore.Load(ctx, key)
}

func (s *hookSessionStore) Delete(ctx context.Context, key SessionKey) error {
	if s.onDelete != nil {
		return s.onDelete(key)
	}

	return s.InMemorySessionStore.Delete(ctx, key)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)

	return data
}

type fakeOpenCodeClient struct {
	mu sync.Mutex

	xdg opencode.XDGDirs

	createSession opencode.NativeSession
	getSession    opencode.NativeSession
	listSessions  []opencode.NativeSession
	forkSession   opencode.NativeSession
	messages      []opencode.NativeMessage
	statuses      map[string]opencode.NativeSessionStatus
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
	syncHistoryFunc   func(context.Context, map[string]int64) ([]opencode.SyncEvent, error)
	refreshMCPFunc    func(context.Context, []opencode.MCPServerConfig) error

	aborts         []string
	deleted        []string
	closed         bool
	events         chan opencode.Event
	errs           chan error
	runtimeExited  chan struct{}
	createErr      error
	getErr         error
	listErr        error
	statusErr      error
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
	scopeErr       error
	syncHistoryErr error
	syncReplayErr  error
	refreshMCPErr  error
	syncEvents     []opencode.SyncEvent
}

type errorSessionStore struct{ err error }

func (s *errorSessionStore) Append(context.Context, SessionKey, []SessionStoreEntry) error {
	return s.err
}
func (s *errorSessionStore) Load(context.Context, SessionKey) ([]SessionStoreEntry, error) {
	return nil, s.err
}
func (s *errorSessionStore) Replace(context.Context, SessionKey, []SessionStoreReplacement) error {
	return s.err
}
func (s *errorSessionStore) Delete(context.Context, SessionKey) error { return s.err }
func (s *errorSessionStore) ListSessions(context.Context) ([]SessionSummary, error) {
	return nil, s.err
}
func (s *errorSessionStore) ListSubkeys(context.Context, SessionKey) ([]string, error) {
	return nil, s.err
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
	runtimeState, err := os.MkdirTemp("", "acp-go-opencode-test-state-")
	if err != nil {
		panic(err)
	}

	return &fakeOpenCodeClient{
		xdg:           opencode.XDGDirs{Root: runtimeState, State: runtimeState},
		providers:     testProviders(),
		events:        make(chan opencode.Event, 16),
		errs:          make(chan error, 16),
		runtimeExited: make(chan struct{}),
	}
}

func testProviders() opencode.ProvidersResponse {
	return opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID: "openai", Name: "OpenAI", Models: map[string]opencode.ProviderModel{
			"gpt-test":  {ID: "gpt-test", Name: "GPT Test", Limit: map[string]any{"context": float64(1000), "output": float64(200)}, Reasoning: true, ToolCall: true},
			"gpt-other": {ID: "gpt-other", Name: "GPT Other"},
		},
	}}}
}

func (c *fakeOpenCodeClient) Close(context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	return c.closeErr
}

func (c *fakeOpenCodeClient) Shutdown(ctx context.Context) error { return c.Close(ctx) }

func (c *fakeOpenCodeClient) Scope(context.Context, opencode.ScopeOptions) (opencode.Client, error) {
	return c, c.scopeErr
}

func (c *fakeOpenCodeClient) RefreshMCP(ctx context.Context, servers []opencode.MCPServerConfig) error {
	if c.refreshMCPFunc != nil {
		return c.refreshMCPFunc(ctx, servers)
	}

	return c.refreshMCPErr
}

func (c *fakeOpenCodeClient) CreateSession(ctx context.Context, title string) (opencode.NativeSession, error) {
	if c.createSessionFunc != nil {
		return c.createSessionFunc(ctx, title)
	}

	return c.createSession, c.createErr
}

func (c *fakeOpenCodeClient) CreateSessionWithPolicy(ctx context.Context, title string, _ []opencode.PermissionRule) (opencode.NativeSession, error) {
	created, err := c.CreateSession(ctx, title)
	if err == nil && created.ID != "" {
		c.ensureSyncAggregate(created.ID)
	}

	return created, err
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

func (c *fakeOpenCodeClient) SessionStatus(context.Context) (map[string]opencode.NativeSessionStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return maps.Clone(c.statuses), c.statusErr
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

func (c *fakeOpenCodeClient) RuntimeExited() <-chan struct{} {
	return c.runtimeExited
}

func (c *fakeOpenCodeClient) XDGDirs() opencode.XDGDirs {
	return c.xdg
}

func (c *fakeOpenCodeClient) NativeVersion() string {
	return "1.18.3"
}

func (c *fakeOpenCodeClient) SyncHistory(ctx context.Context, cursors map[string]int64) ([]opencode.SyncEvent, error) {
	if c.syncHistoryFunc != nil {
		return c.syncHistoryFunc(ctx, cursors)
	}
	if c.syncHistoryErr != nil {
		return nil, c.syncHistoryErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []opencode.SyncEvent
	for _, event := range c.syncEvents {
		cursor, present := cursors[event.AggregateID]
		if present && event.Sequence <= cursor {
			continue
		}
		out = append(out, event)
	}

	return out, nil
}

func (c *fakeOpenCodeClient) SyncReplay(_ context.Context, _ string, events []opencode.SyncReplayEvent) error {
	if c.syncReplayErr != nil {
		return c.syncReplayErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range events {
		candidate := opencode.SyncEvent(event)
		found := false
		for _, existing := range c.syncEvents {
			if existing.ID == candidate.ID {
				found = true

				break
			}
		}
		if !found {
			c.syncEvents = append(c.syncEvents, candidate)
		}
	}

	return nil
}

func (c *fakeOpenCodeClient) ensureSyncAggregate(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range c.syncEvents {
		if event.AggregateID == id {
			return
		}
	}
	data := map[string]json.RawMessage{
		"sessionID": json.RawMessage(strconv.Quote(id)),
		"info":      json.RawMessage(`{"id":` + strconv.Quote(id) + `}`),
	}
	c.syncEvents = append(c.syncEvents, opencode.SyncEvent{ID: "evt-" + id, AggregateID: id, Type: "session.created.1", Data: data})
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
			client.xdg, _ = opencode.CreateRuntimeXDGDirs(filepath.Join(root, "session-1"))
		}
	}
	client.ensureSyncAggregate("native-1")
	agent.mu.Lock()
	if agent.runtime == nil {
		agent.runtime = client
	}
	if agent.runtimeGeneration == 0 {
		agent.runtimeGeneration = 1
	}
	generation := agent.runtimeGeneration
	agent.mu.Unlock()

	session := newSession(agent, "session-1", "/tmp/project", nil, testNativeSession("native-1"), client, sessionMeta{}, idmapRecord{
		SessionID:       "session-1",
		NativeSessionID: "native-1",
		Format:          SessionStoreFormat,
	})
	session.runtimeGeneration = generation

	return session
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
