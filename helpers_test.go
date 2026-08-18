package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func boolPtr(value bool) *bool {
	return &value
}

// testNativeOwnedHome builds a durable native home the ownership predicate can
// actually admit. t.TempDir is unusable here: its leaf is created 0777&^umask,
// so it lands on 0755 under the fleet's umask 022 while the predicate requires
// exactly 0700. The home is also a direct child of the temp root so its
// ancestry stays traversable by a foreign target identity, which keeps a
// wrong-owner refusal about the owner rather than about the walk.
func testNativeOwnedHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "acp-go-opencode-native-home-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(home, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(home) })

	return home
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

// narrowedOutsideRoot points os.TempDir at a directory of its own and returns
// a sibling directory outside every allowed image root, so a case can hold a
// path the adapter must refuse even though the real temp directory is a root.
// Every variable os.TempDir consults on any supported platform is set, and the
// session's scratch parent is pinned so it does not follow the narrowing.
func narrowedOutsideRoot(t *testing.T, sess *session) string {
	t.Helper()

	base := t.TempDir()
	sess.agent.options.ScratchDir = filepath.Join(base, "scratch")
	require.NoError(t, os.Mkdir(sess.agent.options.ScratchDir, 0o700))

	tempRoot := filepath.Join(base, "tmp")
	require.NoError(t, os.Mkdir(tempRoot, 0o700))

	outside := filepath.Join(base, "outside")
	require.NoError(t, os.Mkdir(outside, 0o700))

	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tempRoot)
	}

	require.Equal(t, tempRoot, os.TempDir())

	return outside
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
	dispatchMessage   func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error)
	dispatchCommand   func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error)
	abortFunc         func(string) error
	syncHistoryFunc   func(context.Context, map[string]int64) ([]opencode.SyncEvent, error)
	refreshMCPFunc    func(context.Context, []opencode.MCPServerConfig) error

	aborts         []string
	deleted        []string
	closed         bool
	closeCalls     int
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
	scopeOptions   []opencode.ScopeOptions

	providerCatalog        []opencode.ProviderCatalogEntry
	providerCatalogErr     error
	providerCatalogFunc    func() ([]opencode.ProviderCatalogEntry, error)
	providerAuthMethods    map[string][]opencode.ProviderAuthMethod
	providerAuthMethodsErr error
	authorization          opencode.ProviderAuthorization
	authorizeErr           error
	authorizeFunc          func(string, int, map[string]string) (opencode.ProviderAuthorization, error)
	authorizeCalls         []fakeAuthorizeCall
	authCallbackErr        error
	authCallbackFunc       func(string, int, string) error
	authCallbackStarted    chan struct{}
	authCallbackRelease    chan struct{}
	callbackCalls          []fakeAuthorizeCall
	storedAuth             map[string]opencode.ProviderAuthCredential
	storedAuthErr          error
	storedAuthFunc         func(string)
	setAuthErr             error
	setAuthFunc            func(string, opencode.ProviderAuthCredential) error
	setAuthCalls           []fakeSetAuthCall
	removeAuthErr          error
	removedAuth            []string
	disposeErr             error
	disposed               int

	closeSignal chan struct{}
	closeOnce   sync.Once
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
		closeSignal:   make(chan struct{}),
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

// Close records the close and signals it under the same mutex that guards the
// signal, because withBrokerFactory replaces closeSignal and closeOnce when it
// hands the same fake back for a second broker start. Signalling outside the
// mutex read both of them while that replacement was writing them.
func (c *fakeOpenCodeClient) Close(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	c.closeCalls++
	c.closeOnce.Do(func() {
		close(c.closeSignal)
	})

	return c.closeErr
}

func (c *fakeOpenCodeClient) Shutdown(ctx context.Context) error { return c.Close(ctx) }

func (c *fakeOpenCodeClient) Scope(_ context.Context, options opencode.ScopeOptions) (opencode.Client, error) {
	c.mu.Lock()
	c.scopeOptions = append(c.scopeOptions, opencode.ScopeOptions{
		Directory: options.Directory, MCPServers: cloneNativeMCPServerConfigs(options.MCPServers),
		Env:           cloneStringMap(options.Env),
		ExtraPathDirs: append([]string(nil), options.ExtraPathDirs...),
	})
	c.mu.Unlock()

	return c, c.scopeErr
}

func (c *fakeOpenCodeClient) scopes() []opencode.ScopeOptions {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]opencode.ScopeOptions(nil), c.scopeOptions...)
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

// DispatchCommand mirrors the native command route: it answers only when the run
// it started has finished, so a test that wants a live command turn publishes its
// own events and blocks here.
func (c *fakeOpenCodeClient) DispatchCommand(ctx context.Context, id string, req opencode.CommandRequest) error {
	if c.dispatchCommand != nil {
		message, err := c.dispatchCommand(ctx, id, req)

		return c.completeFromHook(id, message, err)
	}

	c.publishTurnCompletion(id, "assistant-1")

	return c.commandErr
}

// DispatchMessage mirrors the async prompt route: it acknowledges admission and
// returns. The default publishes the events a completed native turn publishes, so
// an ordinary prompt settles from the native stream exactly as it does live.
func (c *fakeOpenCodeClient) DispatchMessage(ctx context.Context, id string, req opencode.MessageRequest) error {
	if c.dispatchMessage != nil {
		message, err := c.dispatchMessage(ctx, id, req)

		return c.completeFromHook(id, message, err)
	}

	c.publishTurnCompletion(id, "assistant-1")

	return nil
}

// completeFromHook maps a hook result onto native behaviour: an error refuses the
// frame, a message publishes the events a finished turn publishes for it, and no
// message at all acknowledges the frame and leaves the native session running.
func (c *fakeOpenCodeClient) completeFromHook(id string, message opencode.NativeMessage, err error) error {
	if err != nil {
		return err
	}

	if message.Info.ID != "" {
		c.completeTurnWith(id, message)
	}

	return nil
}

// completeTurnWith serves one native message as this session's transcript and
// publishes the native events that finish a turn producing it.
func (c *fakeOpenCodeClient) completeTurnWith(id string, message opencode.NativeMessage) {
	c.mu.Lock()
	c.messages = []opencode.NativeMessage{message}
	c.mu.Unlock()

	c.publishTurnCompletion(id, message.Info.ID)
}

// publishTurnCompletion publishes the native event sequence a finished turn
// publishes: the assistant message's identity, then the session's idle signal.
// The identity names whatever assistant message this fake serves, so a case that
// stages its own transcript needs to stage nothing else.
func (c *fakeOpenCodeClient) publishTurnCompletion(id string, assistantID string) {
	c.mu.Lock()
	if len(c.messages) == 0 {
		c.messages = []opencode.NativeMessage{{Info: opencode.NativeMessageInfo{
			ID: assistantID, SessionID: id, Role: "assistant", Finish: "stop",
		}}}
	}

	for index := len(c.messages) - 1; index >= 0; index-- {
		if c.messages[index].Info.Role == "assistant" {
			assistantID = c.messages[index].Info.ID

			break
		}
	}
	c.mu.Unlock()

	c.publishEvent(opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{"id": assistantID, "sessionID": id, "role": "assistant"}}),
	})
	c.publishSessionIdle(id)
}

// publishSessionIdle publishes the native completion signal for one session.
func (c *fakeOpenCodeClient) publishSessionIdle(id string) {
	c.publishEvent(opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": id}),
	})
}

// publishEvent delivers one native event to whichever consumer is attached.
func (c *fakeOpenCodeClient) publishEvent(event opencode.Event) {
	select {
	case c.events <- event:
	default:
	}
}

func mustJSONValue(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}

	return encoded
}

func (c *fakeOpenCodeClient) Messages(context.Context, string) ([]opencode.NativeMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]opencode.NativeMessage(nil), c.messages...), c.messagesErr
}

func (c *fakeOpenCodeClient) Abort(_ context.Context, id string) error {
	c.mu.Lock()
	c.aborts = append(c.aborts, id)
	hook := c.abortFunc
	c.mu.Unlock()

	if hook != nil {
		return hook(id)
	}

	return c.abortErr
}

// abortedSessions reports the native sessions this client was asked to interrupt.
func (c *fakeOpenCodeClient) abortedSessions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.aborts...)
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

func requireSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test signal")
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

// requireInternalErrorData asserts err is an ACP internal error (code -32603)
// and returns its data map. Construction-time option failures land here rather
// than on invalid params: the caller's request was well formed and the agent
// itself is unserviceable.
func requireInternalErrorData(t testingT, err error) map[string]any {
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

	return data
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

func (c *fakeOpenCodeClient) ProviderCatalog(context.Context) ([]opencode.ProviderCatalogEntry, error) {
	if c.providerCatalogFunc != nil {
		return c.providerCatalogFunc()
	}

	return append([]opencode.ProviderCatalogEntry(nil), c.providerCatalog...), c.providerCatalogErr
}

func (c *fakeOpenCodeClient) ProviderAuthMethods(context.Context) (map[string][]opencode.ProviderAuthMethod, error) {
	return c.providerAuthMethods, c.providerAuthMethodsErr
}

func (c *fakeOpenCodeClient) ProviderAuthorize(_ context.Context, providerID string, method int, inputs map[string]string) (opencode.ProviderAuthorization, error) {
	c.mu.Lock()
	c.authorizeCalls = append(c.authorizeCalls, fakeAuthorizeCall{providerID: providerID, method: method, inputs: inputs})
	c.mu.Unlock()

	if c.authorizeFunc != nil {
		return c.authorizeFunc(providerID, method, inputs)
	}

	return c.authorization, c.authorizeErr
}

func (c *fakeOpenCodeClient) ProviderAuthCallback(ctx context.Context, providerID string, method int, code string) error {
	c.mu.Lock()
	c.callbackCalls = append(c.callbackCalls, fakeAuthorizeCall{providerID: providerID, method: method, code: code})
	started := c.authCallbackStarted
	release := c.authCallbackRelease
	closeSignal := c.closeSignal
	c.mu.Unlock()

	signalTestHook(started)

	if release != nil {
		select {
		case <-release:
		case <-closeSignal:
			return context.Canceled
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if c.authCallbackFunc != nil {
		return c.authCallbackFunc(providerID, method, code)
	}

	return c.authCallbackErr
}

func (c *fakeOpenCodeClient) SetProviderAuth(_ context.Context, providerID string, credential opencode.ProviderAuthCredential) error {
	c.mu.Lock()
	hook := c.setAuthFunc
	c.mu.Unlock()

	// A hook that refuses stands in for a write that never landed, so it records
	// no call and leaves the store as it found it.
	if hook != nil {
		if err := hook(providerID, credential); err != nil {
			return err
		}
	}

	c.mu.Lock()
	c.setAuthCalls = append(c.setAuthCalls, fakeSetAuthCall{providerID: providerID, credential: credential})

	if c.setAuthErr == nil {
		if c.storedAuth == nil {
			c.storedAuth = make(map[string]opencode.ProviderAuthCredential, 1)
		}

		c.storedAuth[providerID] = credential
	}
	c.mu.Unlock()

	return c.setAuthErr
}

func (c *fakeOpenCodeClient) RemoveProviderAuth(_ context.Context, providerID string) error {
	c.mu.Lock()
	c.removedAuth = append(c.removedAuth, providerID)

	if c.removeAuthErr == nil {
		delete(c.storedAuth, providerID)
	}
	c.mu.Unlock()

	return c.removeAuthErr
}

func (c *fakeOpenCodeClient) StoredProviderAuth(_ context.Context, providerID string) (opencode.ProviderAuthCredential, bool, error) {
	c.mu.Lock()
	hook := c.storedAuthFunc
	c.mu.Unlock()

	// The hook runs outside the lock so two legs reading the same provider can be
	// held open at the same time rather than serialized by the fake itself.
	if hook != nil {
		hook(providerID)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.storedAuthErr != nil {
		return opencode.ProviderAuthCredential{}, false, c.storedAuthErr
	}

	credential, ok := c.storedAuth[providerID]

	return credential, ok, nil
}

func (c *fakeOpenCodeClient) DisposeInstance(context.Context) error {
	c.mu.Lock()
	c.disposed++
	c.mu.Unlock()

	return c.disposeErr
}

type fakeAuthorizeCall struct {
	providerID string
	method     int
	inputs     map[string]string
	code       string
}

type fakeSetAuthCall struct {
	providerID string
	credential opencode.ProviderAuthCredential
}

// hangsAfterDispatch makes the next dispatch acknowledge the frame and then leave
// the native session running: the turn stays open until something cancels it or a
// deadline expires. started closes when the dispatch is acknowledged.
func (c *fakeOpenCodeClient) hangsAfterDispatch(started chan struct{}) {
	var once sync.Once

	c.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		once.Do(func() { close(started) })

		return opencode.NativeMessage{}, nil
	}
}

// failsNatively makes the next dispatch acknowledge the frame and then publish one
// native turn failure followed by the session's idle signal, which is the order
// OpenCode publishes them in.
func (c *fakeOpenCodeClient) failsNatively(sessionID string, nativeErr *opencode.NativeError) {
	c.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		c.publishEvent(opencode.Event{
			Type: opencode.EventSessionError,
			Properties: mustJSONValue(map[string]any{
				"sessionID": sessionID,
				"error":     nativeErr,
			}),
		})
		c.publishSessionIdle(sessionID)

		return opencode.NativeMessage{}, nil
	}
}

// refusesDispatch makes the next dispatch refuse the frame, which creates neither
// a submission nor a turn.
func (c *fakeOpenCodeClient) refusesDispatch(err error) {
	c.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{}, err
	}
}

// lifecycleEnvelopes decodes every lifecycle envelope this connection received, in
// the order it received them. A notification carrying no envelope is not a
// lifecycle event and is skipped.
func (c *recordingAgentClient) lifecycleEnvelopes(t *testing.T) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	envelopes := make([]map[string]any, 0, len(c.updates))

	for _, notification := range c.updates {
		envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any)
		if !ok {
			continue
		}

		require.NotNil(t, notification.Update.SessionInfoUpdate,
			"a lifecycle envelope rode an ineligible carrier")
		envelopes = append(envelopes, envelope)
	}

	return envelopes
}

// lifecycleEvents reports the `event.type` of every lifecycle envelope received.
func (c *recordingAgentClient) lifecycleEvents(t *testing.T) []string {
	t.Helper()

	types := make([]string, 0)
	for _, envelope := range c.lifecycleEnvelopes(t) {
		event, ok := envelope["event"].(map[string]any)
		require.True(t, ok, "envelope carries no event")
		eventType, ok := event["type"].(string)
		require.True(t, ok, "event carries no type")
		types = append(types, eventType)
	}

	return types
}

// lifecycleEventsOfType reports every lifecycle event object of one type.
func (c *recordingAgentClient) lifecycleEventsOfType(t *testing.T, want string) []map[string]any {
	t.Helper()

	matches := make([]map[string]any, 0)

	for _, envelope := range c.lifecycleEnvelopes(t) {
		event, ok := envelope["event"].(map[string]any)
		if !ok || event["type"] != want {
			continue
		}

		matches = append(matches, event)
	}

	return matches
}

// requireLifecycleOutcome asserts the stream's last ending idle recorded exactly
// this outcome.
func requireLifecycleOutcome(t *testing.T, connection *recordingAgentClient, want lifecycle.Outcome) {
	t.Helper()

	transitions := connection.lifecycleEventsOfType(t, "state_update")
	require.NotEmpty(t, transitions, "the stream carried no foreground transition")

	for index := len(transitions) - 1; index >= 0; index-- {
		if transitions[index]["state"] != "idle" {
			continue
		}

		require.Equal(t, string(want), transitions[index]["outcome"])

		return
	}

	t.Fatal("the stream carried no ending idle")
}

// requireLifecycleReduces replays every lifecycle envelope this connection
// received through the family reducer. A stream that reduces cleanly is one a
// conformant consumer accepts; a stream that does not fails closed here with the
// violation token that refused it.
func requireLifecycleReduces(t *testing.T, connection *recordingAgentClient) lifecycle.State {
	t.Helper()

	reducer := lifecycle.NewReducer(lifecycle.Options{Negotiated: provenFacts()})

	c := connection
	c.mu.Lock()
	updates := append([]acp.SessionNotification(nil), c.updates...)
	c.mu.Unlock()

	for _, notification := range updates {
		if _, ok := notification.Meta[lifecycle.MetaKey]; !ok {
			continue
		}

		payload, err := json.Marshal(map[string]any{
			"sessionId": notification.SessionId,
			"update":    notification.Update,
			"_meta":     notification.Meta,
		})
		require.NoError(t, err)
		require.NoError(t, reducer.ReduceSessionUpdate(payload), "the emitted lifecycle stream failed closed")
	}

	return reducer.State()
}
