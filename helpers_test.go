package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

// absTestPath builds a host-absolute path from POSIX-looking segments, so a
// test states "an absolute working directory" rather than a spelling only one
// platform accepts.
func absTestPath(segments ...string) string {
	root := "/"
	if runtime.GOOS == "windows" {
		root = `C:\`
	}

	return filepath.Join(append([]string{root}, segments...)...)
}

// retainedScratchDir is a scratch parent for a case that deliberately leaves a
// runtime uncontained. t.TempDir fails the test when it cannot remove what it
// created, and a runtime whose containment could not be proven still holds its
// claim lock open — which Windows refuses to unlink. The removal here is best
// effort for exactly that reason.
func retainedScratchDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "acp-go-opencode-retained-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// testURIPath spells a local path the way a file URI carries it: rooted at a
// single slash, so a Windows volume name sits after that slash rather than in
// the URI's authority.
func testURIPath(path string) string {
	return "/" + strings.TrimPrefix(filepath.ToSlash(path), "/")
}

// testFileURI is the file URI that names a local path on this host.
func testFileURI(path string) string {
	return "file://" + testURIPath(path)
}

func boolPtr(value bool) *bool {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

const internalSeamTurnNonce = "test-turn-nonce"

var testPromptSubmissionCounter atomic.Uint64

// Prompt keeps unit tests on the public admission path while allowing fixtures
// to address the session they already hold.
func (s *session) Prompt(ctx context.Context, request acp.PromptRequest) (acp.PromptResponse, error) {
	if request.Meta == nil {
		request.Meta = map[string]any{}
	}
	if _, present := request.Meta[routeEnvelopeKey]; !present {
		for key, value := range requestRouteCarrier(internalSeamTurnNonce) {
			request.Meta[key] = value
		}
	}
	if s.agent.lifecycleNegotiated().Present() {
		if _, present := request.Meta[lifecycle.MetaKey]; !present {
			identity := strconv.FormatUint(testPromptSubmissionCounter.Add(1), 10)
			request.Meta[lifecycle.MetaKey] = map[string]any{
				"version": 1,
				"submission": map[string]any{
					"submissionId": "test-submission-" + identity,
					"clientNonce":  "test-client-" + identity,
				},
			}
		}
	}

	return s.agent.Prompt(ctx, request)
}

func testIncarnation(s *session) *nativeIncarnationBinding {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return s.incarnation
}

func (s *session) beginTurn(ctx context.Context, turnNonces ...string) context.Context {
	turnNonce := ""
	if len(turnNonces) > 0 {
		turnNonce = turnNonces[0]
	}

	turnCtx, cancel := context.WithCancel(ctx)
	turnCtx = withTurnRoute(turnCtx, turnNonce)
	s.lifecycleMu.Lock()
	s.cancel = cancel
	s.turnDone = turnCtx.Done()
	s.cancelled = false
	s.pendingDispatchFailure = nil
	s.turnEpoch++
	s.turnNonce = turnNonce
	s.lifecycleMu.Unlock()

	return turnCtx
}

func (s *session) reopenLifecycleStream() {
	facts := s.agent.lifecycleNegotiated()
	s.lifecycleMu.Lock()
	s.lifecycleFailed = nil
	s.cycleCounter = 0
	s.turnCounter = 0
	s.installLifecycleStream(facts)
	s.lifecycleMu.Unlock()
}

func establishCreatedSession(t *testing.T, agent *Agent, id acp.SessionId) *session {
	t.Helper()

	current, err := agent.session(id)
	require.NoError(t, err)
	current.markEstablishmentResponseWritten()
	require.NoError(t, current.ensureEstablished(context.Background()))

	return current
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
	session := testSession(t, agent, newFakeOpenCodeClient())
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
	scratch := filepath.Join(base, "scratch")
	WithScratchDir(scratch)(&sess.agent.options)
	require.NoError(t, os.Mkdir(scratch, 0o700))

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

	onList    func() ([]string, error)
	onLoad    func(SessionKey) ([]SessionStoreEntry, error)
	onDelete  func(SessionKey) error
	onReplace func(SessionKey) error
}

func (s *hookSessionStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	if s.onReplace != nil {
		if err := s.onReplace(main); err != nil {
			return err
		}
	}

	return s.InMemorySessionStore.Replace(ctx, main, replacements)
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

	createSession    opencode.NativeSession
	getSession       opencode.NativeSession
	listSessions     []opencode.NativeSession
	forkSession      opencode.NativeSession
	messages         []opencode.NativeMessage
	promptMessageIDs map[string]string
	todos            []opencode.NativeTodo
	providers        opencode.ProvidersResponse
	agents           []opencode.NativeAgent
	commands         []opencode.NativeCommand

	pendingPermissions []opencode.PermissionRequest
	permissionReplies  []fakePermissionReply
	permissionReplied  chan struct{}
	pendingQuestions   []opencode.QuestionRequest
	questionReplies    []fakeQuestionReply
	questionReplied    chan struct{}
	questionRejects    []fakeQuestionReject
	questionRejected   chan struct{}

	createSessionFunc  func(context.Context, string) (opencode.NativeSession, error)
	getSessionFunc     func(context.Context, string) (opencode.NativeSession, error)
	scopeFunc          func(opencode.ScopeOptions) error
	dispatchMessage    func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error)
	dispatchCommand    func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error)
	omitPromptEvidence bool
	abortFunc          func(string) error
	syncHistoryFunc    func(context.Context, map[string]int64) ([]opencode.SyncEvent, error)
	refreshMCPFunc     func(context.Context, []opencode.MCPServerConfig) error
	// closeHook runs inside the containment rung, which is the one point of the
	// close ladder that sits between the capture and the durable commit.
	closeHook func()

	aborts              []string
	deleted             []string
	closed              bool
	closeCalls          int
	eventStream         chan opencode.EventStreamItem
	runtimeExited       chan struct{}
	createErr           error
	getErr              error
	listErr             error
	deleteErr           error
	messagesErr         error
	commandsErr         error
	commandErr          error
	abortErr            error
	forkErr             error
	todosErr            error
	providersErr        error
	agentsErr           error
	permissionsErr      error
	questionsErr        error
	replyErr            error
	replyPermissionFunc func(context.Context, opencode.PermissionRequest, string, string) error
	closeErr            error
	scopeErr            error
	syncHistoryErr      error
	syncReplayErr       error
	refreshMCPErr       error
	syncEvents          []opencode.SyncEvent
	scopeOptions        []opencode.ScopeOptions

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
		xdg:              opencode.XDGDirs{Root: runtimeState, State: runtimeState},
		providers:        testProviders(),
		eventStream:      make(chan opencode.EventStreamItem, 256),
		runtimeExited:    make(chan struct{}),
		closeSignal:      make(chan struct{}),
		promptMessageIDs: make(map[string]string),
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
	hook := c.closeHook
	c.mu.Unlock()

	if hook != nil {
		hook()
	}

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

// isClosed reports whether this scope's containment boundary has run.
func (c *fakeOpenCodeClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closed
}

// containmentAttempts reports how many times a boundary tried to contain this
// scope, which is how a caller distinguishes a retried containment from a
// boundary that answered without reaching the scope at all.
func (c *fakeOpenCodeClient) containmentAttempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closeCalls
}

func (c *fakeOpenCodeClient) Scope(_ context.Context, options opencode.ScopeOptions) (opencode.Client, error) {
	c.mu.Lock()
	c.scopeOptions = append(c.scopeOptions, opencode.ScopeOptions{
		Directory: options.Directory, MCPServers: cloneNativeMCPServerConfigs(options.MCPServers),
		Env:           cloneStringMap(options.Env),
		ExtraPathDirs: append([]string(nil), options.ExtraPathDirs...),
	})
	c.mu.Unlock()
	if c.scopeFunc != nil {
		if err := c.scopeFunc(options); err != nil {
			return nil, err
		}
	}

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

func (c *fakeOpenCodeClient) GetSession(ctx context.Context, id string) (opencode.NativeSession, error) {
	if c.getSessionFunc != nil {
		return c.getSessionFunc(ctx, id)
	}

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
		if err == nil {
			c.publishPromptEvidence(id, req.MessageID)
		}

		return c.completeFromHook(id, message, err)
	}
	if c.commandErr != nil {
		return c.commandErr
	}

	c.publishPromptEvidence(id, req.MessageID)
	c.publishTurnCompletion(id, "assistant-1")

	return nil
}

// DispatchMessage mirrors the async prompt route: it acknowledges admission and
// returns. The default publishes the events a completed native turn publishes, so
// an ordinary prompt settles from the native stream exactly as it does live.
func (c *fakeOpenCodeClient) DispatchMessage(ctx context.Context, id string, req opencode.MessageRequest) error {
	if !c.omitPromptEvidence {
		c.publishPromptEvidence(id, req.MessageID)
	}
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

	finish := "stop"
	for index := len(c.messages) - 1; index >= 0; index-- {
		if c.messages[index].Info.Role == "assistant" {
			assistantID = c.messages[index].Info.ID
			if c.messages[index].Info.Finish != "" {
				finish = c.messages[index].Info.Finish
			}

			break
		}
	}
	parentID := c.promptMessageIDs[id]
	c.mu.Unlock()

	c.publishEvent(opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": assistantID, "sessionID": id, "role": "assistant", "parentID": parentID, "finish": finish,
		}}),
	})
	c.publishSessionIdle(id)
}

// stageAssistantMessage serves one assistant message as this session's
// transcript without publishing any event, for hooks that control the event
// order themselves.
func (c *fakeOpenCodeClient) stageAssistantMessage(id, assistantID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.messages = []opencode.NativeMessage{{Info: opencode.NativeMessageInfo{
		ID: assistantID, SessionID: id, Role: "assistant", Finish: "stop",
	}}}
}

// publishSessionIdle publishes the native completion signal for one session.
func (c *fakeOpenCodeClient) publishSessionIdle(id string) {
	c.publishEvent(opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": id}),
	})
}

func (c *fakeOpenCodeClient) publishPromptEvidence(sessionID, messageID string) {
	c.mu.Lock()
	c.promptMessageIDs[sessionID] = messageID
	c.mu.Unlock()

	properties, _ := json.Marshal(map[string]any{"info": map[string]any{
		"id": messageID, "sessionID": sessionID, "role": roleUser,
	}})
	c.publishEvent(opencode.Event{Type: opencode.EventMessageUpdated, Properties: properties})
}

// publishEvent delivers one native event to whichever consumer is attached.
func (c *fakeOpenCodeClient) publishEvent(event opencode.Event) {
	c.eventStream <- opencode.EventStreamItem{Event: &event}
}

func (c *fakeOpenCodeClient) publishStreamTerminal(err error) {
	c.eventStream <- opencode.EventStreamItem{Terminal: err}
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

func (c *fakeOpenCodeClient) ReplyPermission(ctx context.Context, req opencode.PermissionRequest, reply string, message string) error {
	if c.replyPermissionFunc != nil {
		return c.replyPermissionFunc(ctx, req, reply, message)
	}

	c.mu.Lock()
	c.permissionReplies = append(c.permissionReplies, fakePermissionReply{
		sessionID: req.SessionID,
		requestID: req.ID,
		route:     req.Route(),
		reply:     reply,
		message:   message,
	})
	replied := c.permissionReplied
	c.mu.Unlock()
	if replied != nil {
		replied <- struct{}{}
	}

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
	replied := c.questionReplied
	c.mu.Unlock()
	if replied != nil {
		replied <- struct{}{}
	}

	return c.replyErr
}

func (c *fakeOpenCodeClient) RejectQuestion(_ context.Context, req opencode.QuestionRequest) error {
	c.mu.Lock()
	c.questionRejects = append(c.questionRejects, fakeQuestionReject{sessionID: req.SessionID, requestID: req.ID, route: req.Route()})
	rejected := c.questionRejected
	c.mu.Unlock()
	if rejected != nil {
		rejected <- struct{}{}
	}

	return c.replyErr
}

func (c *fakeOpenCodeClient) EventStream() <-chan opencode.EventStreamItem {
	return c.eventStream
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

	permission                     acp.RequestPermissionResponse
	elicitation                    acp.UnstableCreateElicitationResponse
	permissionStarted              chan struct{}
	permissionRelease              chan struct{}
	permissionRegistrationStarted  chan struct{}
	permissionRegistrationRelease  chan struct{}
	permissionIgnoreContext        bool
	permissionRegistrationErr      error
	elicitationStarted             chan struct{}
	elicitationRelease             chan struct{}
	elicitationRegistrationStarted chan struct{}
	elicitationRegistrationRelease chan struct{}
	elicitationIgnoreContext       bool
	elicitationRegistrationErr     error
	permErr                        error
	elicitErr                      error
	updateErr                      error
	notifyErr                      error
	updateHook                     func(acp.SessionNotification)
	updateStarted                  chan struct{}
	updateRelease                  chan struct{}
	notifyStarted                  chan struct{}
	notifyRelease                  chan struct{}
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

func (c *recordingAgentClient) BeginRequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
	key hostRequestKey,
) registeredPermissionRequest {
	registered := make(chan error, 1)
	answered := make(chan permissionRequestResult, 1)

	go func() {
		c.mu.Lock()
		registrationErr := c.permissionRegistrationErr
		registrationStarted := c.permissionRegistrationStarted
		registrationRelease := c.permissionRegistrationRelease
		c.mu.Unlock()

		if registrationErr == nil && (key.streamID == "" || key.actionID == "") {
			registrationErr = errors.New("permission registration correlation is incomplete")
		}

		signalTestHook(registrationStarted)
		if registrationErr == nil && registrationRelease != nil {
			select {
			case <-registrationRelease:
			case <-ctx.Done():
				registrationErr = ctx.Err()
			}
		}

		c.mu.Lock()
		if registrationErr == nil {
			c.permissions = append(c.permissions, request)
		}
		resp := c.permission
		err := c.permErr
		started := c.permissionStarted
		release := c.permissionRelease
		ignoreContext := c.permissionIgnoreContext
		c.mu.Unlock()

		registered <- registrationErr
		if registrationErr != nil {
			answered <- permissionRequestResult{err: registrationErr}

			return
		}

		signalTestHook(started)
		if release != nil {
			if ignoreContext {
				<-release
			} else {
				select {
				case <-release:
				case <-ctx.Done():
					answered <- permissionRequestResult{err: ctx.Err()}

					return
				}
			}
		}

		answered <- permissionRequestResult{response: resp, err: err}
	}()

	return registeredPermissionRequest{registered: registered, answered: answered}
}

func (c *recordingAgentClient) BeginCreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
	key hostRequestKey,
) registeredElicitationRequest {
	registered := make(chan error, 1)
	answered := make(chan elicitationRequestResult, 1)

	go func() {
		c.mu.Lock()
		registrationErr := c.elicitationRegistrationErr
		registrationStarted := c.elicitationRegistrationStarted
		registrationRelease := c.elicitationRegistrationRelease
		c.mu.Unlock()

		if registrationErr == nil && (key.streamID == "" || key.actionID == "") {
			registrationErr = errors.New("elicitation registration correlation is incomplete")
		}

		signalTestHook(registrationStarted)
		if registrationErr == nil && registrationRelease != nil {
			select {
			case <-registrationRelease:
			case <-ctx.Done():
				registrationErr = ctx.Err()
			}
		}

		c.mu.Lock()
		if registrationErr == nil {
			c.elicitations = append(c.elicitations, request)
			c.scopes = append(c.scopes, scope)
		}
		resp := c.elicitation
		err := c.elicitErr
		started := c.elicitationStarted
		release := c.elicitationRelease
		ignoreContext := c.elicitationIgnoreContext
		c.mu.Unlock()

		registered <- registrationErr
		if registrationErr != nil {
			answered <- elicitationRequestResult{err: registrationErr}

			return
		}

		signalTestHook(started)
		if release != nil {
			if ignoreContext {
				<-release
			} else {
				select {
				case <-release:
				case <-ctx.Done():
					answered <- elicitationRequestResult{err: ctx.Err()}

					return
				}
			}
		}

		answered <- elicitationRequestResult{response: resp, err: err}
	}()

	return registeredElicitationRequest{registered: registered, answered: answered}
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

func (c *recordingAgentClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, notification)
	err := c.updateErr
	hook := c.updateHook
	started := c.updateStarted
	release := c.updateRelease
	c.mu.Unlock()
	signalTestHook(started)
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if hook != nil {
		hook(notification)
	}

	return err
}

func (c *recordingAgentClient) NotifyExtension(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	c.extensions = append(c.extensions, extensionNotification{method: method, params: params})
	err := c.notifyErr
	started := c.notifyStarted
	release := c.notifyRelease
	c.mu.Unlock()
	signalTestHook(started)
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return err
}

// availableCommandUpdates lists the command catalogs this connection was sent.
func (c *recordingAgentClient) availableCommandUpdates() []*acp.SessionAvailableCommandsUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()

	var catalogs []*acp.SessionAvailableCommandsUpdate

	for index := range c.updates {
		if catalog := c.updates[index].Update.AvailableCommandsUpdate; catalog != nil {
			catalogs = append(catalogs, catalog)
		}
	}

	return catalogs
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

var testHookClosers sync.Map

func signalTestHook(ch chan struct{}) {
	if ch == nil {
		return
	}

	closer, _ := testHookClosers.LoadOrStore(ch, &sync.Once{})
	once, ok := closer.(*sync.Once)
	if !ok {
		panic("test hook closer has unexpected type")
	}

	once.Do(func() { close(ch) })
}

// promptResult carries a Prompt call's value and its error back to the test
// goroutine. A require.* call inside a goroutine ends that goroutine through
// runtime.Goexit, so the send after it never runs and the receiving test blocks
// until the package timeout; assertions belong on the receiving side.
type promptResult struct {
	response acp.PromptResponse
	err      error
}

// awaitPromptResult receives a prompt outcome under a bounded deadline so a turn
// that never settles fails this test instead of hanging the whole package.
func awaitPromptResult(t *testing.T, done <-chan promptResult) promptResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the prompt to settle")

		return promptResult{}
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
// and data.message is the fixed classification for that cause. It returns
// the decoded data map for any additional field assertions.
func assertTurnFailed(t testingT, err error, wantCause string, _ string) map[string]any {
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
	wantMessage := map[string]string{
		causeProvider:  "OpenCode provider request failed",
		causeTransport: "OpenCode transport failed",
		causeTimeout:   "OpenCode turn timed out",
	}[wantCause]
	if data[jsonFieldMessage] != wantMessage {
		t.Fatalf("data.message = %#v, want %q", data[jsonFieldMessage], wantMessage)
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

func testSession(t *testing.T, agent *Agent, client *fakeOpenCodeClient) *session {
	t.Helper()
	if agent.connection() == nil {
		agent.setAgentClient(newRecordingAgentClient())
	}

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

	session := newSession(agent, "session-1", absTestPath("tmp", "project"), nil, testNativeSession("native-1"), client, sessionMeta{}, idmapRecord{
		SessionID:       "session-1",
		NativeSessionID: "native-1",
		Format:          SessionStoreFormat,
	})
	session.runtimeGeneration = generation
	session.stampIncarnationGeneration(client, generation)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	// The prompt path settles on the native event stream, so a test session is
	// established exactly as a production session is: the opening snapshot (when
	// the agent negotiated the lifecycle extension) and then the pump.
	session.markEstablishmentResponseWritten()
	if err := session.establish(context.Background()); err != nil {
		t.Fatalf("establish test session: %v", err)
	}
	t.Cleanup(func() {
		session.stopPump()
		session.delivery.close()
	})

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

// refusesDispatch makes the next dispatch refuse the frame, which creates neither
// a submission nor a turn.
func (c *fakeOpenCodeClient) refusesDispatch(err error) {
	c.omitPromptEvidence = true
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

	envelopes := c.lifecycleEnvelopes(t)
	types := make([]string, 0, len(envelopes))
	for _, envelope := range envelopes {
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

// negotiatedTestFacts is the connection's answered lifecycle configuration:
// the proven facts plus the version marker the answer carries on the wire.
func negotiatedTestFacts() lifecycle.Negotiated {
	facts := provenFacts()
	facts.Version = lifecycle.Version

	return facts
}

// requireLifecycleReduces replays every lifecycle envelope this connection
// received through the family reducer. A stream that reduces cleanly is one a
// conformant consumer accepts; a stream that does not fails closed here with the
// violation token that refused it.
func requireLifecycleReduces(t *testing.T, connection *recordingAgentClient) lifecycle.State {
	t.Helper()

	reducer := lifecycle.NewReducer(lifecycle.Options{Negotiated: negotiatedTestFacts()})

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
