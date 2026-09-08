//nolint:tagliatelle // OpenCode native JSON fields use modelID/sessionID/providerID spellings.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
)

const (
	opencodeDefaultUsername = "opencode"

	// opencodeExecutableName is the OpenCode program name: the default
	// executable and the per-XDG config directory.
	opencodeExecutableName = "opencode"
	// opencodeServeCommand is the OpenCode CLI subcommand that starts the
	// loopback HTTP server.
	opencodeServeCommand = "serve"
	// eventTypeServerConnected is the first SSE event type emitted by a
	// healthy opencode serve process.
	eventTypeServerConnected = "server.connected"
)

// Native OpenCode REST routes used by the client and required by the /doc
// contract validation.
const (
	routeConfig               = "/config"
	routeConfigProviders      = "/config/providers"
	routeDoc                  = "/doc"
	routeGlobalHealth         = "/global/health"
	fieldName                 = "name"
	routeCommand              = "/command"
	routeEvent                = "/event"
	routeSession              = "/session"
	routePromptAsync          = "/prompt_async"
	routePermission           = "/permission"
	routeQuestion             = "/question"
	routeAPIPermissionRequest = "/api/permission/request"
	routeAPIQuestionRequest   = "/api/question/request"

	roleAssistant = "assistant"
)

// Native OpenCode /doc path templates validated during readiness.
const (
	docPathSessionCommand     = "/session/{sessionID}/command"
	docPathSessionMessage     = "/session/{sessionID}/message"
	docPathSessionPromptAsync = "/session/{sessionID}/prompt_async"
	docPathQuestionReply      = "/question/{requestID}/reply"
	docPathQuestionReject     = "/question/{requestID}/reject"
)

// Native OpenCode wire-field spellings shared between request bodies and the
// /doc schema validation.
const (
	fieldAction               = "action"
	fieldAnswers              = "answers"
	fieldDirectory            = "directory"
	fieldID                   = "id"
	fieldMCP                  = "mcp"
	fieldMetadata             = "metadata"
	sessionCarrierMetadataKey = "acp-go-opencode"
	sessionCarrierRefKey      = "ref"
	fieldPermission           = "permission"
	fieldQuestions            = "questions"
	fieldReply                = "reply"
	fieldRequestID            = "requestID"
	fieldSessionID            = "sessionID"
	fieldMessageID            = "messageID"
	fieldCallID               = "callID"
	fieldStatus               = "status"
)

// openAPITypeArray is the OpenAPI schema "type" value for arrays.
const openAPITypeArray = "array"

// sseEventLineLimitBytes caps one native SSE event line. Tool-state
// attachments ride the event stream as base64 data URLs, so a single line can
// carry several multi-megabyte images; the cap only bounds a runaway line from
// the wrapper-owned loopback server, far above any configured image limit.
const sseEventLineLimitBytes = 64 * 1024 * 1024

// One SSE event may span multiple data lines. The aggregate is bounded by the
// same ceiling as an individual line so a peer cannot bypass the parser's memory
// bound by splitting one malformed or unterminated event into legal-size lines.
const sseEventLimitBytes = sseEventLineLimitBytes

var (
	ErrSSEDisconnect                = errors.New("opencode SSE disconnected")
	ErrSSEEventTooLarge             = errors.New("opencode SSE event exceeds size limit")
	ErrMCPDisconnectUnproven        = errors.New("opencode MCP disconnect unproven")
	ErrScopeRuntimeShutdown         = errors.New("directory-scoped OpenCode client cannot shut down the shared runtime")
	ErrProcessContainmentIncomplete = errors.New("OpenCode process containment incomplete")
	ErrRuntimeScratchCleanup        = errors.New("OpenCode runtime scratch cleanup incomplete")
	errPrepareOpaque                = errors.New("native tree prepare result is opaque")
	errTreeReclaimPending           = errors.New("native tree reclaim pending")
	errProcessStartSettled          = errors.New("native process start failure settled")
	errNativeCleanupRetained        = errors.New("native cleanup retained")
)

type nativeBoundaryError struct {
	err    error
	marker error
}

func (e *nativeBoundaryError) Error() string { return e.err.Error() }
func (e *nativeBoundaryError) Unwrap() error { return e.err }
func (e *nativeBoundaryError) Is(target error) bool {
	return target == e.marker || errors.Is(e.err, target)
}

func MarkPrepareOpaque(err error) error {
	return &nativeBoundaryError{err: err, marker: errPrepareOpaque}
}

func MarkTreeReclaimPending(err error) error {
	return &nativeBoundaryError{err: err, marker: errTreeReclaimPending}
}

func MarkProcessStartSettled(err error) error {
	return &nativeBoundaryError{err: err, marker: errProcessStartSettled}
}

func NativeCleanupRetained(err error) bool {
	return errors.Is(err, errNativeCleanupRetained)
}

func retainNativeCleanup(err error) error {
	return &nativeBoundaryError{err: err, marker: errNativeCleanupRetained}
}

type Client interface {
	Close(context.Context) error
	Shutdown(context.Context) error
	Scope(context.Context, ScopeOptions) (Client, error)
	RefreshMCP(context.Context, []MCPServerConfig) error
	CreateSession(context.Context, string) (NativeSession, error)
	CreateSessionWithPolicy(context.Context, string, []PermissionRule) (NativeSession, error)
	GetSession(context.Context, string) (NativeSession, error)
	ListSessions(context.Context, string) ([]NativeSession, error)
	DeleteSession(context.Context, string) error
	Commands(context.Context) ([]NativeCommand, error)
	// DispatchCommand posts one resolved native command. The native command
	// route answers only when the agent loop it started has finished, so its
	// return is a completion report and never the dispatch acknowledgement.
	DispatchCommand(context.Context, string, CommandRequest) error
	// DispatchMessage posts one prompt frame and returns when the native
	// dispatcher has accepted durable ownership of it. It waits for nothing
	// else: the turn's transcript and its completion arrive as native events.
	DispatchMessage(context.Context, string, MessageRequest) error
	Messages(context.Context, string) ([]NativeMessage, error)
	Abort(context.Context, string) error
	Fork(context.Context, string, string) (NativeSession, error)
	Todos(context.Context, string) ([]NativeTodo, error)
	ConfigProviders(context.Context) (ProvidersResponse, error)
	Agents(context.Context) ([]NativeAgent, error)
	PendingPermissions(context.Context) ([]PermissionRequest, error)
	ReplyPermission(context.Context, PermissionRequest, string, string) error
	PendingQuestions(context.Context) ([]QuestionRequest, error)
	ReplyQuestion(context.Context, QuestionRequest, [][]string) error
	RejectQuestion(context.Context, QuestionRequest) error
	EventStream() <-chan EventStreamItem
	RuntimeExited() <-chan struct{}
	XDGDirs() XDGDirs
	NativeVersion() string
	SyncHistory(context.Context, map[string]int64) ([]SyncEvent, error)
	SyncReplay(context.Context, string, []SyncReplayEvent) error
	ProviderCatalog(context.Context) ([]ProviderCatalogEntry, error)
	ProviderAuthMethods(context.Context) (map[string][]ProviderAuthMethod, error)
	ProviderAuthorize(context.Context, string, int, map[string]string) (ProviderAuthorization, error)
	ProviderAuthCallback(context.Context, string, int, string) error
	SetProviderAuth(context.Context, string, ProviderAuthCredential) error
	RemoveProviderAuth(context.Context, string) error
	StoredProviderAuth(context.Context, string) (ProviderAuthCredential, bool, error)
	DisposeInstance(context.Context) error
}

type ScopeOptions struct {
	Directory  string
	MCPServers []MCPServerConfig
	// Env and ExtraPathDirs are the addressed-session carrier. They stay in the
	// adapter's in-memory broker while an opaque reference is written onto the
	// native session this scope creates or adopts. The shared runtime process
	// environment is fixed at exec and is never republished from here.
	Env           map[string]string
	ExtraPathDirs []string
}

type PermissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

type SyncEvent struct {
	ID          string                     `json:"id"`
	AggregateID string                     `json:"aggregate_id"`
	Sequence    int64                      `json:"seq"`
	Type        string                     `json:"type"`
	Data        map[string]json.RawMessage `json:"data"`
}

type SyncReplayEvent struct {
	ID          string                     `json:"id"`
	AggregateID string                     `json:"aggregateID"`
	Sequence    int64                      `json:"seq"`
	Type        string                     `json:"type"`
	Data        map[string]json.RawMessage `json:"data"`
}

type StartOptions struct {
	Root        string
	ControlRoot string
	// ScratchParent is the already-resolved parent directory for ephemeral
	// on-disk materialization. The root package resolves it (system temp
	// directory when unset); this package never consults the system temp
	// directory itself. It is used only as the fallback root when Root is empty.
	ScratchParent  string
	ExecutablePath string
	// BrowserShim shadows every browser launcher on the child's PATH for the
	// lifetime of a login leg. The caller owns the directory; leaving it nil
	// leaves the child free to open the operator's desktop browser.
	BrowserShim       *BrowserShim
	Env               map[string]string
	NativeEnvironment func() map[string]string
	PrepareTree       func(context.Context, string) error
	ReclaimTree       func(context.Context, string) error
	RetainTree        func(string, bool, func() error)
	StartProcess      ProcessStarter
	Pure              bool
	QuestionTool      bool
	LogLevel          string
	MinVersion        string
	HealthTimeout     time.Duration
	Logger            *slog.Logger
	ExistingXDG       XDGDirs
	RemoveRoot        bool
	SkipVersionGate   bool
	SeedFiles         map[string]string
	// PluginSeedDir is the adapter-owned cache of the npm tree OpenCode installs
	// for its plugin loader. A hit is copied into the runtime config root before
	// launch so OpenCode skips the install. A miss is filled first by a priming
	// launch — a throwaway runtime under ScratchParent that boots to the carrier
	// handshake, serves no session, and is harvested once its tree is the
	// adapter's to read — so the runtime itself always starts from a restored
	// tree. Empty disables seeding, as does Pure.
	PluginSeedDir string
}

// MCPServerConfig describes one MCP server exposed to the native OpenCode
// process through the generated opencode.json. URL selects the remote
// transport; otherwise Command selects the local (stdio) transport.
type MCPServerConfig struct {
	Name    string
	URL     string
	Headers map[string]string
	Command []string
	Env     map[string]string
}

type XDGDirs struct {
	Root   string
	Data   string
	Config string
	Cache  string
	State  string
}

type openCodeServer struct {
	httpClient                   *http.Client
	baseURL                      string
	username                     string
	password                     string
	cancel                       context.CancelFunc
	xdg                          XDGDirs
	log                          *slog.Logger
	sessionPermissionListSupport bool
	sessionQuestionListSupport   bool
	nativeVersion                string

	eventStream             chan EventStreamItem
	eventReader             func(context.Context) error
	closed                  chan struct{}
	directory               string
	scopeCancel             context.CancelFunc
	runtimeShutdown         *runtimeShutdownState
	runtimeCloseOnce        sync.Once
	runtimeClosed           chan struct{}
	runtimeExited           chan struct{}
	runtimeWatchDone        chan struct{}
	mcpNames                []string
	scopeCloseMu            sync.Mutex
	scopeClosed             bool
	ordinaryHomeLock        *homelock.Lock
	process                 ProcessHandle
	settlement              *processSettlement
	inputCloseOnce          sync.Once
	inputCloseErr           error
	streamCloseOnce         sync.Once
	streamCloseErr          error
	stdoutDone              chan struct{}
	stderrDone              chan struct{}
	preparedTrees           []preparedNativeTree
	reclaimTree             func(context.Context, string) error
	sessionCarrierBroker    *sessionCarrierBroker
	sessionCarrierReference string
	pure                    bool
}

type preparedNativeTree struct {
	path      string
	cleanup   func() error
	reclaimed bool
}

type runtimeShutdownAttempt struct {
	done chan struct{}
	err  error
}

type runtimeShutdownState struct {
	mu       sync.Mutex
	current  *runtimeShutdownAttempt
	terminal *runtimeShutdownAttempt
}

func newRuntimeShutdownState() *runtimeShutdownState {
	return &runtimeShutdownState{}
}

type NativeSession struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Directory string         `json:"directory"`
	Agent     string         `json:"agent"`
	Metadata  map[string]any `json:"metadata"`
	Model     struct {
		ID         string `json:"id"`
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type NativeMessage struct {
	Info  NativeMessageInfo `json:"info"`
	Parts []NativePart      `json:"parts"`
}

type NativeMessageInfo struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Role       string          `json:"role"`
	ParentID   string          `json:"parentID"`
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Mode       string          `json:"mode"`
	Agent      string          `json:"agent"`
	Finish     string          `json:"finish"`
	Cost       float64         `json:"cost"`
	Tokens     NativeTokens    `json:"tokens"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Error      *NativeError    `json:"error,omitempty"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type NativeError struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Message string `json:"message"`
	Data    struct {
		Message      string `json:"message"`
		StatusCode   int    `json:"statusCode"`
		ResponseBody string `json:"responseBody"`
	} `json:"data"`
}

type SessionError struct {
	SessionID string       `json:"sessionID"`
	Error     *NativeError `json:"error"`
}

// providerCode parses the native error's provider response body and returns the
// provider error code, falling back to the provider error type. It is lenient:
// a missing or malformed responseBody yields an empty string rather than an
// error, so other error-union shapes never break decoding.
func (e *NativeError) providerCode() string {
	if e == nil || e.Data.ResponseBody == "" {
		return ""
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(e.Data.ResponseBody), &body); err != nil {
		return ""
	}

	return firstNonEmpty(body.Error.Code, body.Error.Type)
}

// AssistantError is the typed error returned when an OpenCode
// assistant/provider turn fails. It carries the machine-readable native fields
// so the prompt site can surface a structured ACP error.
type AssistantError struct {
	detail       string
	statusCode   int
	providerCode string
	name         string
}

func (e *AssistantError) Error() string {
	if e.detail == "" {
		return "opencode assistant error"
	}

	return fmt.Sprintf("opencode assistant error: %s", e.detail)
}

func (e *AssistantError) Detail() string {
	if e == nil {
		return ""
	}

	return e.detail
}

func (e *AssistantError) StatusCode() int {
	if e == nil {
		return 0
	}

	return e.statusCode
}

func (e *AssistantError) ProviderCode() string {
	if e == nil {
		return ""
	}

	return e.providerCode
}

type NativePart struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionID"`
	MessageID string          `json:"messageID"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	CallID    string          `json:"callID"`
	Tool      string          `json:"tool"`
	State     json.RawMessage `json:"state"`
	Reason    string          `json:"reason"`
	Mime      string          `json:"mime"`
	Filename  string          `json:"filename"`
	URL       string          `json:"url"`
	Cost      float64         `json:"cost"`
	Tokens    NativeTokens    `json:"tokens"`
	Raw       json.RawMessage `json:"-"`
}

// NativeAttachment is a native file part carried inside a completed tool
// state's attachments array. The URL is a data URL, a file URL/path, or a
// remote location.
type NativeAttachment struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Mime     string `json:"mime"`
	Filename string `json:"filename"`
	URL      string `json:"url"`
}

func (p *NativePart) UnmarshalJSON(data []byte) error {
	type alias NativePart

	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*p = NativePart(value)
	p.Raw = append(p.Raw[:0], data...)

	return nil
}

type NativeTokens struct {
	Total     float64 `json:"total"`
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	Reasoning float64 `json:"reasoning"`
	Cache     struct {
		Read  float64 `json:"read"`
		Write float64 `json:"write"`
	} `json:"cache"`
}

type NativeTodo struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

type NativeSessionStatus struct {
	Type string `json:"type"`
}

type NativeAgent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
}

type NativeCommand struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Agent       string   `json:"agent,omitempty"`
	Model       string   `json:"model,omitempty"`
	Source      string   `json:"source,omitempty"`
	Template    any      `json:"template,omitempty"`
	Subtask     bool     `json:"subtask,omitempty"`
	Hints       []string `json:"hints"`
}

type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
	Raw        json.RawMessage `json:"-"`
}

// EventStreamItem is one ordered delivery from a directory-scoped native
// stream. Exactly one of Event and Terminal is set. The terminal marker shares
// the bounded channel with events, so it cannot be dropped when the channel is
// full and cannot overtake the final event accepted from the SSE body.
type EventStreamItem struct {
	Event    *Event
	Terminal error
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type alias Event

	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*e = Event(value)
	e.Raw = append(e.Raw[:0], data...)

	return nil
}

type PermissionRequest struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionID"`
	Action     string          `json:"action"`
	Permission string          `json:"permission"`
	Resources  []string        `json:"resources"`
	Patterns   []string        `json:"patterns"`
	Save       []string        `json:"save"`
	Always     []string        `json:"always"`
	Metadata   map[string]any  `json:"metadata"`
	Source     map[string]any  `json:"source"`
	Tool       PermissionTool  `json:"tool"`
	ReplyRoute PermissionRoute `json:"-"`
}

type PermissionTool struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type PermissionRoute string

const (
	PermissionRouteSession PermissionRoute = "session"
	PermissionRouteAPI     PermissionRoute = "api"
)

func (r PermissionRequest) Route() PermissionRoute {
	if r.ReplyRoute != "" {
		return r.ReplyRoute
	}

	if r.Action != "" {
		return PermissionRouteAPI
	}

	return PermissionRouteSession
}

// ToolCall reports the tool call this permission request holds up. The two native
// shapes carry that correlation in different members — the session route names a
// `tool` object and the API route names a `source` of type tool — and a reader
// outside this package must not have to know which arrived.
func (r PermissionRequest) ToolCall() PermissionTool {
	if r.Tool.CallID != "" {
		return r.Tool
	}

	messageID, _ := r.Source[fieldMessageID].(string)
	callID, _ := r.Source[fieldCallID].(string)

	return PermissionTool{MessageID: messageID, CallID: callID}
}

func (r PermissionRequest) ActionName() string {
	return firstNonEmpty(r.Action, r.Permission)
}

func (r PermissionRequest) ResourceList() []string {
	if len(r.Resources) > 0 {
		return append([]string(nil), r.Resources...)
	}

	return append([]string(nil), r.Patterns...)
}

type QuestionRequest struct {
	ID         string         `json:"id"`
	SessionID  string         `json:"sessionID"`
	Questions  []QuestionInfo `json:"questions"`
	Tool       QuestionTool   `json:"tool"`
	ReplyRoute QuestionRoute  `json:"-"`
}

type QuestionRoute string

const (
	QuestionRouteSession QuestionRoute = "session"
	QuestionRouteAPI     QuestionRoute = "api"
)

func (r QuestionRequest) Route() QuestionRoute {
	if r.ReplyRoute != "" {
		return r.ReplyRoute
	}

	return QuestionRouteSession
}

type QuestionInfo struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
	Multiple bool             `json:"multiple"`
	Custom   bool             `json:"custom"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type QuestionTool struct {
	MessageID string `json:"messageID"`
	CallID    string `json:"callID"`
}

type MessageRequest struct {
	MessageID string           `json:"messageID,omitempty"`
	Model     *ModelSelector   `json:"model,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Variant   string           `json:"variant,omitempty"`
	Parts     []map[string]any `json:"parts"`
	Format    *OutputFormat    `json:"format,omitempty"`
}

const OutputFormatJSONSchema = "json_schema"

type OutputFormat struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

type CommandRequest struct {
	MessageID string           `json:"messageID,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Model     string           `json:"model,omitempty"`
	Variant   string           `json:"variant,omitempty"`
	Command   string           `json:"command"`
	Arguments string           `json:"arguments"`
	Parts     []map[string]any `json:"parts,omitempty"`
}

type ModelSelector struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

type ProvidersResponse struct {
	Providers []ProviderInfo    `json:"providers"`
	Default   map[string]string `json:"default"`
	Raw       json.RawMessage   `json:"-"`
}

func (p *ProvidersResponse) UnmarshalJSON(data []byte) error {
	type alias ProvidersResponse

	var value alias
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	*p = ProvidersResponse(value)
	p.Raw = append(p.Raw[:0], data...)

	return nil
}

type ProviderInfo struct {
	ID     string                   `json:"id"`
	Name   string                   `json:"name"`
	Models map[string]ProviderModel `json:"models"`
}

type ProviderModel struct {
	ID           string                     `json:"id"`
	Name         string                     `json:"name"`
	Limit        map[string]any             `json:"limit"`
	Capabilities *ProviderModelCapabilities `json:"capabilities"`
	Options      map[string]any             `json:"options"`
	// Variants are the model's named request presets, keyed by the name a
	// prompt selects one with. On a reasoning model the catalog names them by
	// effort level, so the keys are the effort levels the model supports.
	Variants map[string]map[string]any `json:"variants"`
}

// ProviderModelCapabilities is the authenticated provider catalog's exhaustive
// per-model capability record.
type ProviderModelCapabilities struct {
	Input ProviderModelInputCapabilities `json:"input"`
}

// ProviderModelInputCapabilities reports which input kinds the model accepts.
// A nil field means the catalog did not state the fact either way.
type ProviderModelInputCapabilities struct {
	Image *bool `json:"image"`
}

var (
	openCodeAcquireHomeLock             = homelock.Acquire
	openCodeHTTPClient                  = func() *http.Client { return &http.Client{Timeout: 30 * time.Second} }
	openCodeListen                      = net.Listen
	openCodeRandReader        io.Reader = rand.Reader
	openCodeMarshalIndent               = json.MarshalIndent
	openCodeSeedMkdirAll                = os.MkdirAll
	openCodeSeedWriteFile               = os.WriteFile
	openCodeRemoveAll                   = os.RemoveAll
	openCodeAfter                       = time.After
	openCodeReadyPollInterval           = 100 * time.Millisecond
	openCodeShutdownTimeout             = 5 * time.Second
)

// HealthCheckTimeout is the default bound on OpenCode server readiness checks.
const HealthCheckTimeout = 60 * time.Second

func normalizedStartOptions(options StartOptions) StartOptions {
	if options.NativeEnvironment == nil {
		options.NativeEnvironment = captureProcessEnvironment
	}

	if options.StartProcess == nil {
		options.StartProcess = startOrdinaryProcess
	}

	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	if options.HealthTimeout <= 0 {
		options.HealthTimeout = HealthCheckTimeout
	}

	return options
}

// resolveRuntimeXDG resolves the XDG root this server owns, creating it when the
// caller named a path rather than supplying an existing set.
func resolveRuntimeXDG(options StartOptions) (XDGDirs, error) {
	xdg := options.ExistingXDG
	if xdg.Root == "" {
		root := options.Root
		if root == "" {
			if options.ScratchParent == "" {
				return XDGDirs{}, fmt.Errorf("OpenCode runtime root is required")
			}

			root = filepath.Join(options.ScratchParent, "acp-go-opencode")
		}

		created, err := CreateRuntimeXDGDirs(root)
		if err != nil {
			return XDGDirs{}, err
		}

		xdg = created
	}

	if err := ensureXDGDirs(xdg); err != nil {
		return XDGDirs{}, err
	}

	return xdg, nil
}

func runtimeConfigContent(seedFiles map[string]string, sessionCarrierPlugin string) (string, map[string][]byte, error) {
	writes, seededConfig, err := planOpenCodeSeedWrites(seedFiles)
	if err != nil {
		return "", nil, err
	}

	if seededConfig == nil {
		seededConfig = map[string]any{}
	}

	for _, forbidden := range []string{fieldPermission, fieldMCP} {
		if _, exists := seededConfig[forbidden]; exists {
			return "", nil, fmt.Errorf("shared runtime seed must not contain session-scoped %q", forbidden)
		}
	}

	if sessionCarrierPlugin != "" {
		plugins := make([]any, 0, 1)

		if value, exists := seededConfig["plugin"]; exists {
			var ok bool

			plugins, ok = value.([]any)
			if !ok {
				return "", nil, errors.New("seeded opencode.json plugin must be an array")
			}
		}

		seededConfig["plugin"] = append(plugins, sessionCarrierPlugin)
	}

	config := deepMergeJSON(seededConfig, map[string]any{"$schema": "https://opencode.ai/config.json"})

	data, err := openCodeMarshalIndent(config, "", "  ")
	if err != nil {
		return "", nil, err
	}

	data = append(data, '\n')
	writes[openCodeConfigFileName] = data

	return string(data), writes, nil
}

// StartServer launches one OpenCode runtime and returns it once it is ready to
// serve sessions.
func StartServer(ctx context.Context, options StartOptions) (Client, error) {
	server, err := startServer(ctx, options)
	if err != nil {
		return nil, err
	}

	return server, nil
}

func startServer(ctx context.Context, options StartOptions) (_ *openCodeServer, resultErr error) { //nolint:gocyclo // Startup is one ordered transaction across materialization, authority transfer, launch, and readiness.
	options = normalizedStartOptions(options)
	managedCallbacks := 0

	for _, configured := range []bool{options.PrepareTree != nil, options.ReclaimTree != nil} {
		if configured {
			managedCallbacks++
		}
	}

	if managedCallbacks == 1 {
		return nil, errors.New("native tree authority is incomplete")
	}

	xdg, resolveErr := resolveRuntimeXDG(options)
	if resolveErr != nil {
		return nil, resolveErr
	}

	var sessionCarrier sessionCarrierPlugin

	if !options.Pure {
		var carrierErr error

		sessionCarrier, carrierErr = materializeSessionCarrierPlugin(xdg.Root)
		if carrierErr != nil {
			return nil, carrierErr
		}
	}

	preparedTrees := make([]preparedNativeTree, 0, 1)
	runtimeTree := preparedNativeTree{path: xdg.Root}

	if sessionCarrier.Cleanup != nil {
		runtimeTree.cleanup = sessionCarrier.Cleanup
	}

	if options.RemoveRoot {
		carrierCleanup := runtimeTree.cleanup
		runtimeTree.cleanup = func() error {
			var carrierErr error
			if carrierCleanup != nil {
				carrierErr = carrierCleanup()
			}

			cleanupErr := errors.Join(
				carrierErr,
				openCodeRemoveAll(xdg.Root),
				openCodeRemoveAll(ControlRootForXDG(xdg.Root)),
			)
			if cleanupErr != nil {
				return errors.Join(ErrRuntimeScratchCleanup, cleanupErr)
			}

			return nil
		}
	}

	transferred := false

	retainPrepared := false

	defer func() {
		if transferred {
			return
		}

		if retainPrepared {
			retainPreparedTrees(preparedTrees, options.RetainTree)

			resultErr = retainNativeCleanup(resultErr)

			return
		}

		if options.ReclaimTree != nil {
			var reclaimErr error

			preparedTrees, reclaimErr = reclaimPreparedTreesDetached(preparedTrees, options.ReclaimTree)
			resultErr = errors.Join(resultErr, reclaimErr)

			if len(preparedTrees) > 0 {
				retainPreparedTrees(preparedTrees, options.RetainTree)

				resultErr = retainNativeCleanup(resultErr)
			}
		} else {
			for _, tree := range preparedTrees {
				if tree.cleanup != nil {
					resultErr = errors.Join(resultErr, tree.cleanup())
				}
			}
		}
	}()

	controlRoot := options.ControlRoot
	if controlRoot == "" {
		controlRoot = ControlRootForXDG(xdg.Root)
	}

	if controlErr := ensureRuntimeControlRoot(controlRoot); controlErr != nil {
		return nil, controlErr
	}

	runtimeConfig, configErr := materializeOpenCodeRuntimeConfig(xdg, options.SeedFiles, sessionCarrier.URL)
	if configErr != nil {
		return nil, configErr
	}

	executable := options.ExecutablePath
	if executable == "" {
		executable = opencodeExecutableName
	}

	// The environment is built here, ahead of tree preparation, because the seed
	// key must resolve the executable against the PATH the child will get. Its
	// failures are reported below, after the tree joins the prepared set, so a
	// refused environment still cleans the carrier up.
	baseEnvironment := options.NativeEnvironment()

	var (
		environment    map[string]string
		environmentErr error
	)

	if baseEnvironment != nil {
		environment, environmentErr = buildProcessEnvironmentFrom(baseEnvironment, withoutManagedRootOverrides(options.Env))
	}

	// The seed lands before the tree is prepared, because a prepared tree is the
	// host's until it is reclaimed. A miss primes the cache through a launch of
	// its own before this runtime goes on, so the runtime never pays the install.
	if options.PluginSeedDir != "" && !options.Pure && environmentErr == nil && environment != nil {
		seedRuntimePlugins(ctx, options, executable, environment, xdg)
	}

	if options.PrepareTree != nil {
		for _, tree := range []preparedNativeTree{runtimeTree} {
			if prepareErr := options.PrepareTree(ctx, tree.path); prepareErr != nil {
				if errors.Is(prepareErr, errPrepareOpaque) {
					retainPrepared = true
					prepareErr = errors.Join(prepareErr, sessionCarrier.Broker.Close())
				} else if tree.cleanup != nil {
					prepareErr = errors.Join(prepareErr, tree.cleanup())
				}

				if retainPrepared {
					prepareErr = retainNativeCleanup(prepareErr)
				}

				return nil, prepareErr
			}

			preparedTrees = append(preparedTrees, tree)
		}
	} else {
		preparedTrees = append(preparedTrees, runtimeTree)
	}

	port, portErr := allocatePort()
	if portErr != nil {
		return nil, portErr
	}

	password, passwordErr := randomPassword()
	if passwordErr != nil {
		return nil, passwordErr
	}

	username := opencodeDefaultUsername

	arguments := []string{opencodeServeCommand, "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)}
	if options.Pure {
		arguments = append(arguments, "--pure")
	}

	if options.LogLevel != "" {
		arguments = append(arguments, "--log-level", options.LogLevel)
	}

	if baseEnvironment == nil {
		return nil, errors.New("native environment is unavailable")
	}

	if environmentErr != nil {
		return nil, environmentErr
	}

	environment["XDG_DATA_HOME"] = xdg.Data
	environment["XDG_CONFIG_HOME"] = xdg.Config
	environment["XDG_CACHE_HOME"] = xdg.Cache
	environment["XDG_STATE_HOME"] = xdg.State
	environment["OPENCODE_SERVER_USERNAME"] = username
	environment["OPENCODE_SERVER_PASSWORD"] = password
	environment["OPENCODE_CONFIG_CONTENT"] = runtimeConfig

	if options.QuestionTool {
		environment["OPENCODE_ENABLE_QUESTION_TOOL"] = "1"
	}

	nativeEnvironment := envMapToSlice(environment)
	if options.BrowserShim != nil {
		nativeEnvironment = options.BrowserShim.environ(nativeEnvironment)
	}

	homeLock, lockErr := openCodeAcquireHomeLock(controlRoot)
	if lockErr != nil {
		return nil, lockErr
	}
	defer func() {
		if !transferred {
			resultErr = errors.Join(resultErr, homeLock.Release())
		}
	}()

	process, startErr := options.StartProcess(ctx, executable, arguments, nativeEnvironment, xdg.Root)
	if startErr != nil {
		if options.ReclaimTree != nil && !errors.Is(startErr, errProcessStartSettled) {
			retainPrepared = true
		}

		if retainPrepared {
			startErr = retainNativeCleanup(startErr)
		}

		return nil, startErr
	}

	if !process.valid() {
		if options.ReclaimTree != nil {
			retainPrepared = true
		}

		err := errors.New("native process handle is incomplete")
		if retainPrepared {
			err = retainNativeCleanup(err)
		}

		return nil, err
	}

	settlement := newProcessSettlement(process)
	settlement.start()

	runtimeExited := make(chan struct{})
	runtimeWatchDone := make(chan struct{})

	go observeProcessSettlement(settlement, runtimeExited, runtimeWatchDone)

	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})

	go func() {
		defer close(stdoutDone)

		drainProcessPipe(options.Logger, "opencode stdout", process.Output)
	}()
	go func() {
		defer close(stderrDone)

		drainProcessPipe(options.Logger, "opencode stderr", process.Errors)
	}()

	processCtx, cancel := context.WithCancel(context.Background())
	server := &openCodeServer{
		httpClient: openCodeHTTPClient(), baseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		username: username, password: password, cancel: cancel, xdg: xdg, log: options.Logger,
		eventStream: make(chan EventStreamItem, 256), closed: make(chan struct{}),
		runtimeShutdown: newRuntimeShutdownState(), runtimeClosed: make(chan struct{}), runtimeExited: runtimeExited,
		runtimeWatchDone: runtimeWatchDone, stdoutDone: stdoutDone, stderrDone: stderrDone,
		ordinaryHomeLock: homeLock, process: process, settlement: settlement,
		preparedTrees: preparedTrees, reclaimTree: options.ReclaimTree,
		sessionCarrierBroker: sessionCarrier.Broker, pure: options.Pure,
	}
	transferred = true

	readyCtx, readyCancel := context.WithTimeout(ctx, options.HealthTimeout)
	defer readyCancel()

	if readyErr := server.waitReady(readyCtx, processCtx, options); readyErr != nil {
		return nil, settleFailedServerStart(server, options.RetainTree, readyErr)
	}

	if sessionCarrier.URL != "" {
		carrierCtx, carrierCancel := context.WithTimeout(ctx, options.HealthTimeout)
		carrierErr := server.proveSessionCarrierLoaded(carrierCtx, sessionCarrier.Proof)

		carrierCancel()

		if carrierErr != nil {
			return nil, settleFailedServerStart(server, options.RetainTree, carrierErr)
		}

		// A prepared tree is the host's until it is reclaimed, so the module can
		// only be erased where this adapter still owns the runtime root. Under
		// host authority the bootstrap stays for the runtime's life.
		if options.PrepareTree == nil {
			if eraseErr := eraseSessionCarrierBootstrap(sessionCarrier); eraseErr != nil {
				return nil, settleFailedServerStart(server, options.RetainTree, eraseErr)
			}
		}
	}

	return server, nil
}

func observeProcessSettlement(settlement *processSettlement, runtimeExited, done chan struct{}) {
	defer close(done)

	<-settlement.done

	if _, observed, _, _ := settlement.observation(); observed {
		close(runtimeExited)
	}
}

func settleFailedServerStart(server *openCodeServer, retain func(string, bool, func() error), startErr error) error {
	err := errors.Join(startErr, server.Shutdown(context.Background()))
	if len(server.preparedTrees) == 0 {
		return err
	}

	retainPreparedTrees(server.preparedTrees, retain)
	server.preparedTrees = nil

	return retainNativeCleanup(err)
}

func retainPreparedTrees(trees []preparedNativeTree, retain func(string, bool, func() error)) {
	if retain == nil {
		return
	}

	for _, tree := range trees {
		retain(tree.path, tree.reclaimed, tree.cleanup)
	}
}

func ControlRootForXDG(root string) string {
	return root + ".control"
}

// proveSessionCarrierLoaded requires the generated plugin to be live before the
// runtime is handed to any session.
//
// OpenCode instantiates a plugin lazily, with the first directory-scoped
// request rather than at listen time, and it treats a plugin it cannot load as
// non-fatal: the server reaches readiness, shell operations succeed, and the
// carrier is simply absent. The scoped request below forces the load, and the
// plugin acknowledges readiness through the in-memory carrier broker.
func (s *openCodeServer) proveSessionCarrierLoaded(ctx context.Context, proof sessionCarrierProof) error {
	var ignored map[string]any

	if err := s.getJSON(ctx, routeConfig, url.Values{fieldDirectory: {proof.Directory}}, &ignored); err != nil {
		return fmt.Errorf("drive the OpenCode session carrier plugin: %w", err)
	}

	select {
	case <-proof.Ready:
		return nil
	case <-ctx.Done():
		return errors.New("opencode session carrier plugin did not load; refusing a runtime whose shell operations would carry no session")
	}
}

func (s *openCodeServer) waitReady(ctx context.Context, eventCtx context.Context, options StartOptions) error {
	started := time.Now()

	var health struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}

	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := s.getJSON(attemptCtx, routeGlobalHealth, nil, &health)

		cancel()

		if err == nil && health.Healthy {
			break
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("opencode health check failed: %w", err)
			}

			return ctx.Err()
		case <-openCodeAfter(openCodeReadyPollInterval):
		}
	}

	if !options.SkipVersionGate && options.MinVersion != "" {
		if err := checkMinVersion(health.Version, options.MinVersion); err != nil {
			return err
		}
	}

	s.nativeVersion = health.Version

	if s.log != nil {
		s.log.DebugContext(ctx, "opencode startup stage complete", slog.String("stage", "health"), slog.Duration("elapsed", time.Since(started)))
	}

	var doc map[string]any
	if err := s.getJSON(ctx, routeDoc, nil, &doc); err != nil {
		return fmt.Errorf("load opencode /doc: %w", err)
	}

	if s.log != nil {
		s.log.DebugContext(ctx, "opencode startup stage complete", slog.String("stage", "openapi"), slog.Duration("elapsed", time.Since(started)))
	}

	docCapabilities, err := inspectOpenCodeDoc(doc)
	if err != nil {
		return err
	}

	s.sessionPermissionListSupport = docCapabilities.sessionPermissionList

	s.sessionQuestionListSupport = docCapabilities.sessionQuestionList

	// The readiness handshake is the whole purpose of the runtime-wide event
	// subscription: every session consumes its own directory-scoped stream, so
	// nothing reads this one again. The subscription is cancelled the moment the
	// handshake frame arrives rather than left running with no reader, where a
	// full buffer would hold an SSE connection open for the process's whole life.
	handshakeCtx, endHandshake := context.WithCancel(eventCtx)
	defer endHandshake()

	go s.readEvents(handshakeCtx)

	select {
	case item := <-s.eventStream:
		if item.Terminal != nil {
			return fmt.Errorf("opencode event stream failed during readiness: %w", item.Terminal)
		}

		if item.Event == nil || item.Event.Type != eventTypeServerConnected {
			eventType := ""
			if item.Event != nil {
				eventType = item.Event.Type
			}

			return fmt.Errorf("first opencode event was %q, want server.connected", eventType)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

func (s *openCodeServer) Close(ctx context.Context) error {
	if s.scopeCancel == nil {
		return nil
	}

	s.scopeCloseMu.Lock()
	defer s.scopeCloseMu.Unlock()

	if s.scopeClosed {
		return nil
	}

	s.releaseSessionCarrier()

	if err := s.disconnectMCP(ctx); err != nil {
		return err
	}

	close(s.closed)
	s.scopeCancel()
	s.scopeClosed = true

	return nil
}

func (s *openCodeServer) releaseSessionCarrier() {
	s.sessionCarrierBroker.remove(s.sessionCarrierReference)
}

func (s *openCodeServer) Shutdown(ctx context.Context) error {
	if s.scopeCancel != nil {
		return ErrScopeRuntimeShutdown
	}

	state := s.runtimeShutdown
	if state == nil {
		state = newRuntimeShutdownState()
		s.runtimeShutdown = state
	}

	state.mu.Lock()
	if state.terminal != nil {
		attempt := state.terminal
		state.mu.Unlock()

		return attempt.err
	}

	attempt := state.current
	if attempt == nil {
		attempt = &runtimeShutdownAttempt{done: make(chan struct{})}
		state.current = attempt

		go func() {
			attempt.err = s.shutdownRuntime()

			state.mu.Lock()
			state.current = nil

			if !runtimeShutdownRetryable(attempt.err) {
				state.terminal = attempt
			}
			state.mu.Unlock()
			close(attempt.done)
		}()
	}
	state.mu.Unlock()

	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runtimeShutdownRetryable(err error) bool {
	return errors.Is(err, ErrProcessContainmentIncomplete) ||
		errors.Is(err, errTreeReclaimPending) ||
		errors.Is(err, ErrRuntimeScratchCleanup)
}

func (s *openCodeServer) shutdownRuntime() error {
	waitSucceeded := false

	if s.runtimeClosed != nil {
		s.runtimeCloseOnce.Do(func() { close(s.runtimeClosed) })
	}

	var (
		err       error
		revokeErr error
	)

	if s.sessionCarrierBroker != nil {
		err = errors.Join(err, s.sessionCarrierBroker.Close())
	}

	if s.settlement != nil {
		_, _, alreadyTerminal, _ := s.settlement.observation()
		if !alreadyTerminal {
			disposeCtx, disposeCancel := context.WithTimeout(context.Background(), processContainmentTimeout)
			disposeErr := s.DisposeInstance(disposeCtx)

			var httpErr *HTTPError

			disposeCancel()

			if errors.As(disposeErr, &httpErr) && (httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusMethodNotAllowed) {
				disposeErr = nil
			}

			err = errors.Join(err, disposeErr)
			err = errors.Join(err, s.closeProcessInput())

			revokeCtx, revokeCancel := context.WithTimeout(context.Background(), processContainmentTimeout)
			revokeErr = s.process.Stop(revokeCtx)

			if revokeCtx.Err() != nil && revokeErr != nil {
				revokeErr = errors.Join(ErrProcessContainmentIncomplete, revokeErr)
			}

			revokeCancel()
		}

		outcome, waitErr := s.settlement.waitTerminal()

		if waitErr == nil && outcome.Revoked {
			revokeErr = nil
		}

		err = errors.Join(err, revokeErr, waitErr)
		waitSucceeded = waitErr == nil
	}

	err = errors.Join(err, s.closeOwnedProcessWorkers())

	if s.cancel != nil {
		s.cancel()
	}

	if s.ordinaryHomeLock != nil && waitSucceeded {
		err = errors.Join(err, s.ordinaryHomeLock.Release())
		s.ordinaryHomeLock = nil
	}

	if s.settlement != nil && waitSucceeded {
		select {
		case <-s.settlement.done:
			if s.reclaimTree != nil {
				var reclaimErr error

				s.preparedTrees, reclaimErr = reclaimPreparedTreesDetached(s.preparedTrees, s.reclaimTree)
				err = errors.Join(err, reclaimErr)
			} else {
				var cleanupErr error

				s.preparedTrees, cleanupErr = cleanupPreparedTreesRetaining(s.preparedTrees)
				err = errors.Join(err, cleanupErr)
			}
		default:
		}
	}

	return err
}

func (s *openCodeServer) closeProcessInput() error {
	s.inputCloseOnce.Do(func() {
		if s.process.Input != nil {
			s.inputCloseErr = processPipeCloseError(s.process.Input.Close())
		}
	})

	return s.inputCloseErr
}

func (s *openCodeServer) closeOwnedProcessWorkers() error {
	if s.settlement != nil {
		s.settlement.cancel()
	}

	s.streamCloseOnce.Do(func() {
		s.streamCloseErr = s.closeProcessInput()
		if s.process.Output != nil {
			s.streamCloseErr = errors.Join(s.streamCloseErr, processPipeCloseError(s.process.Output.Close()))
		}

		if s.process.Errors != nil {
			s.streamCloseErr = errors.Join(s.streamCloseErr, processPipeCloseError(s.process.Errors.Close()))
		}
	})

	doneChannels := []<-chan struct{}{s.stdoutDone, s.stderrDone, s.runtimeWatchDone}
	if s.settlement != nil {
		doneChannels = append(doneChannels, s.settlement.done)
	}

	for _, done := range doneChannels {
		if done == nil {
			continue
		}

		<-done
	}

	return s.streamCloseErr
}

func processPipeCloseError(err error) error {
	if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}

	return err
}

func cleanupPreparedTreesRetaining(trees []preparedNativeTree) ([]preparedNativeTree, error) {
	var result error

	remaining := make([]preparedNativeTree, 0, len(trees))

	for index := len(trees) - 1; index >= 0; index-- {
		tree := trees[index]
		if tree.cleanup == nil {
			continue
		}

		if err := tree.cleanup(); err != nil {
			result = errors.Join(result, err)

			remaining = append(remaining, tree)
		}
	}

	slices.Reverse(remaining)

	return remaining, result
}

func reclaimPreparedTrees(
	ctx context.Context,
	trees []preparedNativeTree,
	reclaim func(context.Context, string) error,
) error {
	_, err := reclaimPreparedTreesRetaining(ctx, trees, reclaim)

	return err
}

func reclaimPreparedTreesRetaining(
	ctx context.Context,
	trees []preparedNativeTree,
	reclaim func(context.Context, string) error,
) ([]preparedNativeTree, error) {
	var result error

	remaining := make([]preparedNativeTree, 0, len(trees))

	for index := len(trees) - 1; index >= 0; index-- {
		tree := trees[index]
		if !tree.reclaimed {
			if err := reclaim(ctx, tree.path); err != nil {
				result = errors.Join(result, err)

				remaining = append(remaining, tree)

				continue
			}

			tree.reclaimed = true
		}

		if tree.cleanup != nil {
			if err := tree.cleanup(); err != nil {
				result = errors.Join(result, err)

				remaining = append(remaining, tree)
			}
		}
	}

	slices.Reverse(remaining)

	return remaining, result
}

func reclaimPreparedTreesDetached(
	trees []preparedNativeTree,
	reclaim func(context.Context, string) error,
) ([]preparedNativeTree, error) {
	return reclaimPreparedTreesRetaining(context.Background(), trees, func(_ context.Context, path string) error {
		reclaimCtx, cancel := context.WithTimeout(context.Background(), processContainmentTimeout)
		err := reclaim(reclaimCtx, path)
		ctxErr := reclaimCtx.Err()

		cancel()

		if err == nil || errors.Is(err, errTreeReclaimPending) {
			return err
		}

		if ctxErr != nil {
			return errors.Join(ErrProcessContainmentIncomplete, err, ctxErr)
		}

		return errors.Join(ErrProcessContainmentIncomplete, err)
	})
}

func (s *openCodeServer) Scope(ctx context.Context, options ScopeOptions) (Client, error) {
	if options.Directory == "" {
		return nil, fmt.Errorf("opencode scope directory is required")
	}

	// Pure mode omits the carrier plugin, so nothing would apply either half of
	// the carrier. Refusing here keeps the failure at admission rather than
	// letting a session believe it holds an environment it never receives.
	if s.pure && (len(options.ExtraPathDirs) > 0 || len(options.Env) > 0) {
		return nil, errors.New("opencode pure mode does not support the addressed session carrier")
	}

	carrierReference := ""

	if !s.pure {
		if s.sessionCarrierBroker == nil {
			return nil, errors.New("opencode session carrier broker is unavailable")
		}

		var err error

		carrierReference, err = s.sessionCarrierBroker.put(sessionCarrierPayload{
			Env:           options.Env,
			ExtraPathDirs: options.ExtraPathDirs,
		})
		if err != nil {
			return nil, fmt.Errorf("register OpenCode session carrier: %w", err)
		}
	}

	scopeCtx, cancel := context.WithCancel(context.Background())
	scope := &openCodeServer{
		httpClient: s.httpClient, baseURL: s.baseURL, username: s.username,
		password: s.password, cancel: s.cancel, xdg: s.xdg,
		log: s.log, sessionPermissionListSupport: s.sessionPermissionListSupport,
		sessionQuestionListSupport: s.sessionQuestionListSupport,
		nativeVersion:              s.nativeVersion,
		eventStream:                make(chan EventStreamItem, 256),
		closed:                     make(chan struct{}), directory: options.Directory,
		scopeCancel: cancel, runtimeShutdown: s.runtimeShutdown, runtimeClosed: s.runtimeClosed,
		runtimeExited:           s.runtimeExited,
		sessionCarrierBroker:    s.sessionCarrierBroker,
		sessionCarrierReference: carrierReference,
		pure:                    s.pure,
	}

	if err := scope.registerMCP(ctx, options.MCPServers); err != nil {
		cancel()
		scope.releaseSessionCarrier()

		return nil, err
	}

	go scope.readEvents(scopeCtx)

	select {
	case item := <-scope.eventStream:
		if item.Terminal != nil {
			closeErr := scope.Close(context.Background())

			return nil, errors.Join(fmt.Errorf("opencode directory event stream failed: %w", item.Terminal), closeErr)
		}

		if item.Event == nil || item.Event.Type != eventTypeServerConnected {
			closeErr := scope.Close(context.Background())
			eventType := ""

			if item.Event != nil {
				eventType = item.Event.Type
			}

			return nil, errors.Join(fmt.Errorf("first directory-scoped event was %q", eventType), closeErr)
		}
	case <-ctx.Done():
		closeErr := scope.Close(context.Background())

		return nil, errors.Join(ctx.Err(), closeErr)
	}

	return scope, nil
}

func (s *openCodeServer) registerMCP(ctx context.Context, servers []MCPServerConfig) error {
	for _, server := range servers {
		config := openCodeMCPConfigBlock([]MCPServerConfig{server})[server.Name]
		s.mcpNames = append(s.mcpNames, server.Name)

		var response map[string]struct {
			Status string `json:"status"`
		}
		if err := s.doJSON(ctx, http.MethodPost, "/mcp", nil, map[string]any{
			fieldName: server.Name,
			"config":  config,
		}, &response); err != nil {
			cleanupErr := s.disconnectMCP(ctx)

			return errors.Join(fmt.Errorf("register directory MCP %q: %w", server.Name, err), cleanupErr)
		}

		status, ok := response[server.Name]
		if !ok || status.Status != "connected" {
			cleanupErr := s.disconnectMCP(ctx)

			return errors.Join(fmt.Errorf("directory MCP %q did not connect", server.Name), cleanupErr)
		}
	}

	return nil
}

// RefreshMCP forces OpenCode to discard its cached tool catalog and reconnect
// the directory-scoped MCP servers from their original session configuration.
// A lifecycle request may register an endpoint before its host-side grant is
// armed; reconnecting immediately before the first prompt makes the armed
// catalog authoritative without weakening directory ownership or teardown.
func (s *openCodeServer) RefreshMCP(ctx context.Context, servers []MCPServerConfig) error {
	if err := s.unregisterMCP(ctx); err != nil {
		return fmt.Errorf("disconnect directory MCP before refresh: %w", err)
	}

	if err := s.registerMCP(ctx, servers); err != nil {
		return fmt.Errorf("reconnect directory MCP after refresh: %w", err)
	}

	return nil
}

func (s *openCodeServer) unregisterMCP(ctx context.Context) error {
	var (
		result    error
		remaining []string
	)

	for i := len(s.mcpNames) - 1; i >= 0; i-- {
		name := s.mcpNames[i]

		err := s.doJSON(ctx, http.MethodDelete, "/mcp/"+url.PathEscape(name), nil, nil, nil)
		if err != nil && !isHTTPStatus(err, http.StatusNotFound) {
			result = errors.Join(result, err)

			remaining = append(remaining, name)
		}
	}

	slices.Reverse(remaining)
	s.mcpNames = remaining

	if result != nil {
		return errors.Join(ErrMCPDisconnectUnproven, result)
	}

	return nil
}

func (s *openCodeServer) disconnectMCP(ctx context.Context) error {
	disconnectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openCodeShutdownTimeout)
	defer cancel()

	return s.unregisterMCP(disconnectCtx)
}

func (s *openCodeServer) EventStream() <-chan EventStreamItem {
	return s.eventStream
}

func (s *openCodeServer) RuntimeExited() <-chan struct{} {
	return s.runtimeExited
}

// RuntimeRevoked distinguishes an authority- or adapter-initiated teardown
// from a native runtime that exited on its own.
func (s *openCodeServer) RuntimeRevoked() bool {
	if s.settlement == nil {
		return false
	}

	result, _, terminal, _ := s.settlement.observation()

	return terminal && result.Revoked
}

func (s *openCodeServer) XDGDirs() XDGDirs {
	return s.xdg
}

// NativeVersion reports the OpenCode server version probed during readiness.
func (s *openCodeServer) NativeVersion() string {
	return s.nativeVersion
}

// checkMinVersion fails closed when the installed version is below the
// minimum or when either version does not parse as dotted integers.
func checkMinVersion(installed, minimum string) error {
	cmp, err := compareVersions(installed, minimum)
	if err != nil {
		return fmt.Errorf("opencode version gate: %w", err)
	}

	if cmp < 0 {
		return fmt.Errorf("opencode version %s is below minimum supported version %s", installed, minimum)
	}

	return nil
}

func compareVersions(a, b string) (int, error) {
	left, err := parseVersion(a)
	if err != nil {
		return 0, err
	}

	right, err := parseVersion(b)
	if err != nil {
		return 0, err
	}

	for i := range max(len(left), len(right)) {
		var l, r int
		if i < len(left) {
			l = left[i]
		}

		if i < len(right) {
			r = right[i]
		}

		if l != r {
			if l < r {
				return -1, nil
			}

			return 1, nil
		}
	}

	return 0, nil
}

func parseVersion(version string) ([]int, error) {
	segments := strings.Split(strings.TrimPrefix(version, "v"), ".")
	parsed := make([]int, 0, len(segments))

	for _, segment := range segments {
		value, err := strconv.Atoi(segment)
		if err != nil || value < 0 {
			return nil, fmt.Errorf("unparsable opencode version %q", version)
		}

		parsed = append(parsed, value)
	}

	return parsed, nil
}

func (s *openCodeServer) CreateSession(ctx context.Context, title string) (NativeSession, error) {
	return s.CreateSessionWithPolicy(ctx, title, nil)
}

func (s *openCodeServer) CreateSessionWithPolicy(ctx context.Context, title string, permission []PermissionRule) (NativeSession, error) {
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}

	if len(permission) > 0 {
		body[fieldPermission] = permission
	}

	if s.carriesSession() {
		body[fieldMetadata] = s.sessionCarrierMetadata(nil)
	}

	var out NativeSession

	err := s.doJSON(ctx, http.MethodPost, routeSession, nil, body, &out)

	return out, err
}

// carriesSession reports whether this scope owns an addressed native session.
// Only a directory-scoped client does; the shared runtime handle itself never
// writes a carrier.
func (s *openCodeServer) carriesSession() bool {
	return s.directory != "" && !s.pure
}

// sessionCarrierMetadata replaces the adapter's namespace on a native session's
// metadata and leaves every other namespace alone. Replacement rather than
// merge is what makes a rebind authoritative: a value the session no longer
// holds must not survive in the environment its next command runs under.
func (s *openCodeServer) sessionCarrierMetadata(metadata map[string]any) map[string]any {
	result := maps.Clone(metadata)
	if result == nil {
		result = map[string]any{}
	}

	result[sessionCarrierMetadataKey] = map[string]any{
		sessionCarrierRefKey: s.sessionCarrierReference,
	}

	return result
}

func (s *openCodeServer) setSessionCarrier(ctx context.Context, native *NativeSession) error {
	var ignored NativeSession

	metadata := s.sessionCarrierMetadata(native.Metadata)
	native.Metadata = metadata

	return s.doJSON(ctx, http.MethodPatch, routeSession+"/"+url.PathEscape(native.ID), nil, map[string]any{
		fieldMetadata: metadata,
	}, &ignored)
}

func (s *openCodeServer) SyncHistory(ctx context.Context, cursors map[string]int64) ([]SyncEvent, error) {
	if cursors == nil {
		cursors = map[string]int64{}
	}

	var out []SyncEvent

	err := s.doJSON(ctx, http.MethodPost, "/sync/history", nil, cursors, &out)

	return out, err
}

func (s *openCodeServer) SyncReplay(ctx context.Context, directory string, events []SyncReplayEvent) error {
	if directory == "" || len(events) == 0 {
		return fmt.Errorf("sync replay requires a directory and events")
	}

	return s.doJSON(ctx, http.MethodPost, "/sync/replay", nil, map[string]any{
		fieldDirectory: directory,
		"events":       events,
	}, nil)
}

func (s *openCodeServer) GetSession(ctx context.Context, id string) (NativeSession, error) {
	var out NativeSession

	err := s.getJSON(ctx, "/session/"+url.PathEscape(id), nil, &out)
	if err == nil && s.carriesSession() {
		err = s.setSessionCarrier(ctx, &out)
	}

	return out, err
}

func (s *openCodeServer) ListSessions(ctx context.Context, cwd string) ([]NativeSession, error) {
	query := url.Values{}
	if cwd != "" {
		query.Set(fieldDirectory, cwd)
	}

	var out []NativeSession

	err := s.getJSON(ctx, routeSession, query, &out)

	return out, err
}

func (s *openCodeServer) DeleteSession(ctx context.Context, id string) error {
	var ignored any

	return s.doJSON(ctx, http.MethodDelete, "/session/"+url.PathEscape(id), nil, nil, &ignored)
}

func (s *openCodeServer) Commands(ctx context.Context) ([]NativeCommand, error) {
	var out []NativeCommand

	err := s.getJSON(ctx, routeCommand, nil, &out)

	return out, err
}

// DispatchCommand posts one resolved native command. The native route expands
// the command template and then runs the agent loop to completion before it
// answers, so this call reports the run's outcome rather than its admission; the
// caller takes admission and completion from the session's own native events.
func (s *openCodeServer) DispatchCommand(ctx context.Context, id string, req CommandRequest) error {
	var out NativeMessage
	if err := s.doJSONWithClient(ctx, s.blockingHTTPClient(), http.MethodPost, "/session/"+url.PathEscape(id)+routeCommand, nil, req, &out); err != nil {
		_, recovered := s.recoverBlockingTurnFailure(ctx, id, err)

		return recovered
	}

	return AssistantMessageError(out)
}

// DispatchMessage posts one prompt frame to the native async prompt route. That
// route resolves the addressed session, schedules the agent loop, and answers
// `204 No Content` — the native dispatcher's own acknowledgement that it holds
// the frame. A refusal answers with a status instead, and a transport failure
// leaves admission unknown, so both are reported as a refusal: an admitted frame
// this call could not confirm still announces itself on the session's event
// stream, where it opens an agent-origin turn rather than a silently accepted one.
func (s *openCodeServer) DispatchMessage(ctx context.Context, id string, req MessageRequest) error {
	return s.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(id)+routePromptAsync, nil, req, nil)
}

// recoverBlockingTurnFailure resolves the real cause of a transport failure on
// the blocking message/command POST. OpenCode completes the turn server-side
// even when the client connection drops mid-body, persisting the true cause on
// the assistant message, so on any non-HTTP transport error we re-fetch the
// session messages and surface the persisted assistant error when present. An
// HTTP status error is a completed response rather than a transport failure and
// passes through unchanged; when no persisted assistant error is recoverable the
// transport failure is surfaced with route context so the caller classifies it
// as cause "transport" and never sees a bare stream error such as "unexpected
// EOF".
func (s *openCodeServer) recoverBlockingTurnFailure(ctx context.Context, id string, transportErr error) (NativeMessage, error) {
	var httpErr *HTTPError
	if errors.As(transportErr, &httpErr) {
		return NativeMessage{}, transportErr
	}

	messages, err := s.Messages(ctx, id)
	if err != nil {
		// Both the blocking POST and the recovery re-fetch failed: name both
		// failures with context so the surfaced transport cause is never a bare
		// stream error such as "unexpected EOF".
		return NativeMessage{}, fmt.Errorf("opencode message POST: %w; message re-fetch failed: %v", transportErr, err)
	}

	if assistantErr := lastAssistantError(messages); assistantErr != nil {
		return NativeMessage{}, assistantErr
	}

	// The re-fetch succeeded but persisted no assistant error: surface the POST
	// transport failure with route context rather than a bare stream error.
	return NativeMessage{}, fmt.Errorf("opencode message POST: %w", transportErr)
}

// lastAssistantError runs AssistantMessageError against the most recent
// assistant message and returns its typed error, or nil when the latest
// assistant message carries no error.
func lastAssistantError(messages []NativeMessage) error {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Info.Role != roleAssistant {
			continue
		}

		return AssistantMessageError(messages[i])
	}

	return nil
}

func AssistantMessageError(message NativeMessage) error {
	if !strings.EqualFold(message.Info.Finish, "error") && message.Info.Error == nil {
		return nil
	}

	if message.Info.Error == nil {
		return &AssistantError{}
	}

	return AssistantErrorFromNativeError(message.Info.Error)
}

func AssistantErrorFromNativeError(nerr *NativeError) *AssistantError {
	if nerr == nil {
		return &AssistantError{}
	}

	detail := firstNonEmpty(
		nerr.Message,
		nerr.Data.Message,
		nerr.Type,
		nerr.Name,
	)

	return &AssistantError{
		detail:       detail,
		statusCode:   nerr.Data.StatusCode,
		providerCode: nerr.providerCode(),
		name:         nerr.Name,
	}
}

func (s *openCodeServer) Messages(ctx context.Context, id string) ([]NativeMessage, error) {
	return s.messagesWithClient(ctx, s.httpClient, id)
}

func (s *openCodeServer) messagesWithClient(ctx context.Context, client *http.Client, id string) ([]NativeMessage, error) {
	var out []NativeMessage

	err := s.doJSONWithClient(ctx, client, http.MethodGet, "/session/"+url.PathEscape(id)+"/message", nil, nil, &out)

	return out, err
}

func (s *openCodeServer) Abort(ctx context.Context, id string) error {
	var ignored any

	return s.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(id)+"/abort", nil, map[string]any{}, &ignored)
}

func (s *openCodeServer) Fork(ctx context.Context, id string, messageID string) (NativeSession, error) {
	body := map[string]any{}
	if messageID != "" {
		body["messageID"] = messageID
	}

	var out NativeSession

	err := s.doJSON(ctx, http.MethodPost, "/session/"+url.PathEscape(id)+"/fork", nil, body, &out)
	if err == nil && s.carriesSession() {
		err = s.setSessionCarrier(ctx, &out)
	}

	return out, err
}

func (s *openCodeServer) Todos(ctx context.Context, id string) ([]NativeTodo, error) {
	select {
	case <-s.closed:
		return nil, context.Canceled
	default:
	}

	var out []NativeTodo

	err := s.getJSON(ctx, "/session/"+url.PathEscape(id)+"/todo", nil, &out)

	return out, err
}

func (s *openCodeServer) ConfigProviders(ctx context.Context) (ProvidersResponse, error) {
	var out ProvidersResponse

	err := s.getJSON(ctx, routeConfigProviders, nil, &out)

	return out, err
}

func (s *openCodeServer) Agents(ctx context.Context) ([]NativeAgent, error) {
	var out []NativeAgent

	err := s.getJSON(ctx, "/agent", nil, &out)

	return out, err
}

func (s *openCodeServer) PendingPermissions(ctx context.Context) ([]PermissionRequest, error) {
	var out []PermissionRequest

	if s.sessionPermissionListSupport {
		var sessionRequests []PermissionRequest
		if err := s.getJSON(ctx, routePermission, nil, &sessionRequests); err != nil {
			return nil, err
		}

		for i := range sessionRequests {
			sessionRequests[i].ReplyRoute = PermissionRouteSession
		}

		out = append(out, sessionRequests...)
	}

	var response struct {
		Data []PermissionRequest `json:"data"`
	}
	if err := s.getJSON(ctx, routeAPIPermissionRequest, nil, &response); err != nil {
		return nil, err
	}

	for i := range response.Data {
		response.Data[i].ReplyRoute = PermissionRouteAPI
	}

	out = append(out, response.Data...)

	return out, nil
}

func (s *openCodeServer) ReplyPermission(ctx context.Context, req PermissionRequest, reply string, message string) error {
	body := map[string]any{fieldReply: reply}
	if message != "" {
		body["message"] = message
	}

	if req.Route() == PermissionRouteAPI {
		path := "/api/session/" + url.PathEscape(req.SessionID) + "/permission/" + url.PathEscape(req.ID) + "/reply"

		return s.doJSON(ctx, http.MethodPost, path, nil, body, nil)
	}

	path := "/permission/" + url.PathEscape(req.ID) + "/reply"

	return s.doJSON(ctx, http.MethodPost, path, nil, body, nil)
}

func (s *openCodeServer) PendingQuestions(ctx context.Context) ([]QuestionRequest, error) {
	var out []QuestionRequest

	if s.sessionQuestionListSupport {
		var sessionRequests []QuestionRequest
		if err := s.getJSON(ctx, routeQuestion, nil, &sessionRequests); err != nil {
			return nil, err
		}

		for i := range sessionRequests {
			sessionRequests[i].ReplyRoute = QuestionRouteSession
		}

		out = append(out, sessionRequests...)
	}

	var response struct {
		Data []QuestionRequest `json:"data"`
	}
	if err := s.getJSON(ctx, routeAPIQuestionRequest, nil, &response); err != nil {
		return nil, err
	}

	for i := range response.Data {
		response.Data[i].ReplyRoute = QuestionRouteAPI
	}

	out = append(out, response.Data...)

	return out, nil
}

func (s *openCodeServer) ReplyQuestion(ctx context.Context, req QuestionRequest, answers [][]string) error {
	body := map[string]any{fieldAnswers: answers}

	if req.Route() == QuestionRouteAPI {
		path := "/api/session/" + url.PathEscape(req.SessionID) + "/question/" + url.PathEscape(req.ID) + "/reply"

		return s.doJSON(ctx, http.MethodPost, path, nil, body, nil)
	}

	path := "/question/" + url.PathEscape(req.ID) + "/reply"

	return s.doJSON(ctx, http.MethodPost, path, nil, body, nil)
}

func (s *openCodeServer) RejectQuestion(ctx context.Context, req QuestionRequest) error {
	if req.Route() == QuestionRouteAPI {
		path := "/api/session/" + url.PathEscape(req.SessionID) + "/question/" + url.PathEscape(req.ID) + "/reject"

		return s.doJSON(ctx, http.MethodPost, path, nil, map[string]any{}, nil)
	}

	path := "/question/" + url.PathEscape(req.ID) + "/reject"

	return s.doJSON(ctx, http.MethodPost, path, nil, map[string]any{}, nil)
}

func (s *openCodeServer) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	return s.doJSON(ctx, http.MethodGet, path, query, nil, out)
}

func (s *openCodeServer) doJSON(ctx context.Context, method string, path string, query url.Values, body any, out any) error {
	return s.doJSONWithClient(ctx, s.httpClient, method, path, query, body, out)
}

func (s *openCodeServer) doJSONWithClient(
	ctx context.Context,
	client *http.Client,
	method string,
	path string,
	query url.Values,
	body any,
	out any,
) error {
	var reader io.Reader

	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(data)
	}

	if s.directory != "" && path != routeGlobalHealth && path != routeDoc && path != "/sync/history" && path != "/sync/replay" {
		if query == nil {
			query = url.Values{}
		} else {
			query = maps.Clone(query)
		}

		if query.Get(fieldDirectory) == "" {
			query.Set(fieldDirectory, s.directory)
		}
	}

	reqURL := s.baseURL + path
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, reader)
	if err != nil {
		return err
	}

	req.SetBasicAuth(s.username, s.password)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("Accept", "application/json")

	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return &HTTPError{
			Method:     method,
			Path:       path,
			Status:     resp.Status,
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(data)),
		}
	}

	if out == nil {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return err
		}

		return nil
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}

type HTTPError struct {
	Method     string
	Path       string
	Status     string
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("opencode %s %s returned %s", e.Method, e.Path, e.Status)
}

func IsBadRequest(err error) bool {
	return isHTTPStatus(err, http.StatusBadRequest)
}

func isHTTPStatus(err error, status int) bool {
	var httpErr *HTTPError

	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func (s *openCodeServer) blockingHTTPClient() *http.Client {
	if s.httpClient == nil {
		return http.DefaultClient
	}

	client := *s.httpClient
	client.Timeout = 0

	return &client
}

func (s *openCodeServer) readEvents(ctx context.Context) {
	var err error

	defer func() {
		if recover() != nil {
			err = errors.New("opencode SSE producer panicked")
		}

		s.publishEventStreamTerminal(ctx, err)
	}()

	read := s.readEventStream
	if s.eventReader != nil {
		read = s.eventReader
	}

	err = read(ctx)
}

func (s *openCodeServer) publishEventStreamTerminal(ctx context.Context, err error) {
	defer func() { _ = recover() }()

	if err == nil {
		return
	}

	select {
	case <-s.closed:
		return
	case <-ctx.Done():
		return
	default:
	}

	select {
	case s.eventStream <- EventStreamItem{Terminal: err}:
	case <-s.closed:
	case <-ctx.Done():
	}
}

func (s *openCodeServer) readEventStream(ctx context.Context) error {
	return s.readEventStreamWithLimit(ctx, sseEventLimitBytes)
}

func (s *openCodeServer) readEventStreamWithLimit(ctx context.Context, eventLimit int) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("opencode SSE reader panicked")
		}
	}()

	eventURL := s.baseURL + routeEvent
	if s.directory != "" {
		eventURL += "?" + url.Values{fieldDirectory: []string{s.directory}}.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventURL, http.NoBody)
	if err != nil {
		return err
	}

	req.SetBasicAuth(s.username, s.password)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.eventHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("opencode event stream returned %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), sseEventLineLimitBytes)

	var data strings.Builder

	flush := func() error {
		if data.Len() == 0 {
			return nil
		}

		payload := data.String()
		data.Reset()

		var event Event
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return err
		}

		select {
		case s.eventStream <- EventStreamItem{Event: &event}:
		case <-s.closed:
			return io.EOF
		case <-ctx.Done():
			return ctx.Err()
		}

		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}

			continue
		}

		if after, ok := strings.CutPrefix(line, "data:"); ok {
			fragment := strings.TrimSpace(after)

			separator := 0
			if data.Len() > 0 {
				separator = 1
			}

			if eventLimit <= 0 || data.Len()+separator > eventLimit || len(fragment) > eventLimit-data.Len()-separator {
				return ErrSSEEventTooLarge
			}

			if separator != 0 {
				data.WriteByte('\n')
			}

			data.WriteString(fragment)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if err := flush(); err != nil {
		return err
	}

	select {
	case <-s.closed:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("%w: event stream closed", ErrSSEDisconnect)
	}
}

type openCodeDocCapabilities struct {
	sessionPermissionList bool
	sessionQuestionList   bool
}

func inspectOpenCodeDoc(doc map[string]any) (openCodeDocCapabilities, error) {
	rawPaths, _ := doc["paths"].(map[string]any)

	required := []string{
		routeConfig,
		routeConfigProviders,
		routeCommand,
		routeEvent,
		routeSession,
		"/session/{sessionID}",
		docPathSessionCommand,
		docPathSessionMessage,
		docPathSessionPromptAsync,
		"/session/{sessionID}/abort",
		"/session/{sessionID}/fork",
		"/session/{sessionID}/todo",
		"/session/{sessionID}/revert",
		"/session/{sessionID}/unrevert",
		routePermission,
		"/permission/{requestID}/reply",
		routeQuestion,
		docPathQuestionReply,
		docPathQuestionReject,
		"/api/session/{sessionID}/permission/{requestID}/reply",
		routeAPIPermissionRequest,
		"/api/session/{sessionID}/question/{requestID}/reply",
		"/api/session/{sessionID}/question/{requestID}/reject",
		routeAPIQuestionRequest,
	}
	for _, path := range required {
		if _, ok := rawPaths[path]; !ok {
			return openCodeDocCapabilities{}, fmt.Errorf("opencode version mismatch: /doc missing required path %s", path)
		}
	}

	if err := validateOpenCodeGetListOperation(rawPaths, routeAPIPermissionRequest, "PermissionV2Request"); err != nil {
		return openCodeDocCapabilities{}, err
	}

	if err := validateOpenCodePermissionReply(rawPaths); err != nil {
		return openCodeDocCapabilities{}, err
	}

	if err := validateOpenCodeSessionPermissionReply(rawPaths); err != nil {
		return openCodeDocCapabilities{}, err
	}

	sessionPermissionList, err := validateOptionalOpenCodeGetArrayOperation(rawPaths, routePermission, "PermissionRequest")
	if err != nil {
		return openCodeDocCapabilities{}, err
	}

	if questionListErr := validateOpenCodeGetListOperation(rawPaths, routeAPIQuestionRequest, "QuestionV2Request"); questionListErr != nil {
		return openCodeDocCapabilities{}, questionListErr
	}

	if questionReplyErr := validateOpenCodeQuestionReply(doc, rawPaths); questionReplyErr != nil {
		return openCodeDocCapabilities{}, questionReplyErr
	}

	if questionRoutesErr := validateOpenCodeSessionQuestionRoutes(rawPaths); questionRoutesErr != nil {
		return openCodeDocCapabilities{}, questionRoutesErr
	}

	sessionQuestionList, err := validateOptionalOpenCodeGetArrayOperation(rawPaths, routeQuestion, "QuestionRequest")
	if err != nil {
		return openCodeDocCapabilities{}, err
	}

	if err := validateOpenCodePostNoContent(rawPaths, "/api/session/{sessionID}/question/{requestID}/reject"); err != nil {
		return openCodeDocCapabilities{}, err
	}

	if err := validateOpenCodeEventSchemas(doc); err != nil {
		return openCodeDocCapabilities{}, err
	}

	if err := validateOpenCodeStructuredOutputSchema(doc); err != nil {
		return openCodeDocCapabilities{}, err
	}

	return openCodeDocCapabilities{
		sessionPermissionList: sessionPermissionList,
		sessionQuestionList:   sessionQuestionList,
	}, nil
}

func (s *openCodeServer) eventHTTPClient() *http.Client {
	if s.httpClient == nil {
		return http.DefaultClient
	}

	client := *s.httpClient
	client.Timeout = 0

	return &client
}

type openCodeEventContract struct {
	schema             string
	event              string
	requiredProperties []string
}

func validateOpenCodeEventSchemas(doc map[string]any) error {
	for _, contract := range []openCodeEventContract{
		// The idle event is this adapter's completion authority for a native
		// turn, and the status event is how a session reports that it took work
		// on. A build that publishes neither cannot report a foreground
		// boundary at all, so readiness refuses it rather than falling back to
		// a poll.
		{schema: "EventSessionIdle", event: EventSessionIdle, requiredProperties: []string{fieldSessionID}},
		{schema: "EventSessionStatus", event: EventSessionStatus, requiredProperties: []string{fieldSessionID, fieldStatus}},
		{schema: "EventSessionError", event: EventSessionError},
		{schema: "EventPermissionV2Asked", event: "permission.v2.asked", requiredProperties: []string{fieldID, fieldSessionID, fieldAction, "resources"}},
		{schema: "EventPermissionV2Replied", event: "permission.v2.replied", requiredProperties: []string{fieldSessionID, fieldRequestID, fieldReply}},
		{schema: "EventPermissionAsked", event: "permission.asked", requiredProperties: []string{fieldID, fieldSessionID, fieldPermission, "patterns"}},
		{schema: "EventPermissionReplied", event: "permission.replied", requiredProperties: []string{fieldSessionID, fieldRequestID, fieldReply}},
		{schema: "EventQuestionV2Asked", event: "question.v2.asked", requiredProperties: []string{fieldID, fieldSessionID, fieldQuestions}},
		{schema: "EventQuestionV2Replied", event: "question.v2.replied", requiredProperties: []string{fieldSessionID, fieldRequestID, fieldAnswers}},
		{schema: "EventQuestionAsked", event: "question.asked", requiredProperties: []string{fieldID, fieldSessionID, fieldQuestions}},
		{schema: "EventQuestionReplied", event: "question.replied", requiredProperties: []string{fieldSessionID, fieldRequestID, fieldAnswers}},
		{schema: "EventMessagePartUpdated", event: "message.part.updated", requiredProperties: []string{fieldSessionID, "part", "time"}},
		{schema: "EventServerConnected", event: eventTypeServerConnected},
	} {
		if err := validateOpenCodeEventSchema(doc, contract); err != nil {
			return err
		}
	}

	return nil
}

func validateOpenCodeStructuredOutputSchema(doc map[string]any) error {
	schema, ok := openAPIComponentSchema(doc, "#/components/schemas/OutputFormatJsonSchema")
	if !ok {
		return fmt.Errorf("opencode /doc missing structured output schema OutputFormatJsonSchema")
	}

	typeSchema, ok := openAPIObjectProperty(schema, "type")
	if !ok || !openAPIStringEnumContains(typeSchema, OutputFormatJSONSchema) {
		return fmt.Errorf(
			"opencode /doc structured output schema missing type %q",
			OutputFormatJSONSchema,
		)
	}

	if _, ok := openAPIObjectProperty(schema, "schema"); !ok {
		return fmt.Errorf("opencode /doc structured output schema missing schema property")
	}

	return nil
}

func validateOpenCodeEventSchema(doc map[string]any, contract openCodeEventContract) error {
	if !openAPIEventUnionHasSchema(doc, contract.schema) {
		return fmt.Errorf("opencode /doc Event union missing %s", contract.schema)
	}

	schema, ok := openAPIComponentSchema(doc, "#/components/schemas/"+contract.schema)
	if !ok {
		return fmt.Errorf("opencode /doc missing event schema %s", contract.schema)
	}

	typeSchema, ok := openAPIObjectProperty(schema, "type")
	if !ok || !openAPIStringEnumContains(typeSchema, contract.event) {
		return fmt.Errorf("opencode /doc event schema %s missing type %q", contract.schema, contract.event)
	}

	propertiesSchema, ok := openAPIObjectProperty(schema, "properties")
	if !ok {
		return fmt.Errorf("opencode /doc event schema %s missing properties schema", contract.schema)
	}

	for _, property := range contract.requiredProperties {
		if !openAPIObjectHasRequiredProperty(propertiesSchema, property) {
			return fmt.Errorf("opencode /doc event schema %s properties missing required %s", contract.schema, property)
		}
	}

	return nil
}

func validateOpenCodeGetListOperation(paths map[string]any, path string, itemRef string) error {
	operation, ok := openAPIOperation(paths, path, http.MethodGet)
	if !ok {
		return fmt.Errorf("opencode /doc path %s missing GET operation", path)
	}

	if !openAPIHasResponse(operation, "200") {
		return fmt.Errorf("opencode /doc GET %s missing 200 response", path)
	}

	schema, ok := openAPIJSONResponseSchema(operation, "200")
	if !ok || !openAPISchemaDataArrayRef(schema, itemRef) {
		return fmt.Errorf("opencode /doc GET %s response schema is not pending %s list", path, itemRef)
	}

	return nil
}

func validateOptionalOpenCodeGetArrayOperation(paths map[string]any, path string, itemRef string) (bool, error) {
	operation, ok := openAPIOperation(paths, path, http.MethodGet)
	if !ok {
		return false, nil
	}

	if !openAPIHasResponse(operation, "200") {
		return false, fmt.Errorf("opencode /doc GET %s missing 200 response", path)
	}

	schema, ok := openAPIJSONResponseSchema(operation, "200")
	if !ok || !openAPISchemaArrayRef(schema, itemRef) {
		return false, fmt.Errorf("opencode /doc GET %s response schema is not pending %s array", path, itemRef)
	}

	return true, nil
}

func validateOpenCodePermissionReply(paths map[string]any) error {
	const path = "/api/session/{sessionID}/permission/{requestID}/reply"

	operation, ok := openAPIOperation(paths, path, http.MethodPost)
	if !ok {
		return fmt.Errorf("opencode /doc path %s missing POST operation", path)
	}

	if !openAPIHasResponse(operation, "204") {
		return fmt.Errorf("opencode /doc POST %s missing 204 response", path)
	}

	schema, ok := openAPIJSONRequestSchema(operation)
	if !ok {
		return fmt.Errorf("opencode /doc POST %s missing JSON request schema", path)
	}

	if !openAPIObjectHasRequiredProperty(schema, fieldReply) {
		return fmt.Errorf("opencode /doc POST %s request schema missing required reply", path)
	}

	if !openAPIObjectHasProperty(schema, "message") {
		return fmt.Errorf("opencode /doc POST %s request schema missing message property", path)
	}

	return nil
}

func validateOpenCodeSessionPermissionReply(paths map[string]any) error {
	const path = "/permission/{requestID}/reply"

	operation, ok := openAPIOperation(paths, path, http.MethodPost)
	if !ok {
		return fmt.Errorf("opencode /doc path %s missing POST operation", path)
	}

	if !openAPIHasResponse(operation, "200") {
		return fmt.Errorf("opencode /doc POST %s missing 200 response", path)
	}

	return nil
}

func validateOpenCodeQuestionReply(doc map[string]any, paths map[string]any) error {
	const path = "/api/session/{sessionID}/question/{requestID}/reply"

	operation, ok := openAPIOperation(paths, path, http.MethodPost)
	if !ok {
		return fmt.Errorf("opencode /doc path %s missing POST operation", path)
	}

	if !openAPIHasResponse(operation, "204") {
		return fmt.Errorf("opencode /doc POST %s missing 204 response", path)
	}

	schema, ok := openAPIJSONRequestSchema(operation)
	if !ok {
		return fmt.Errorf("opencode /doc POST %s missing JSON request schema", path)
	}

	if ref, _ := schema["$ref"].(string); ref != "" {
		resolved, ok := openAPIComponentSchema(doc, ref)
		if !ok {
			return fmt.Errorf("opencode /doc POST %s request schema ref %s missing", path, ref)
		}

		schema = resolved
	}

	if !openAPIObjectHasRequiredProperty(schema, fieldAnswers) {
		return fmt.Errorf("opencode /doc POST %s request schema missing required answers", path)
	}

	return nil
}

func validateOpenCodeSessionQuestionRoutes(paths map[string]any) error {
	for _, path := range []string{docPathQuestionReply, docPathQuestionReject} {
		operation, ok := openAPIOperation(paths, path, http.MethodPost)
		if !ok {
			return fmt.Errorf("opencode /doc path %s missing POST operation", path)
		}

		if !openAPIHasResponse(operation, "200") {
			return fmt.Errorf("opencode /doc POST %s missing 200 response", path)
		}
	}

	return nil
}

func validateOpenCodePostNoContent(paths map[string]any, path string) error {
	operation, ok := openAPIOperation(paths, path, http.MethodPost)
	if !ok {
		return fmt.Errorf("opencode /doc path %s missing POST operation", path)
	}

	if !openAPIHasResponse(operation, "204") {
		return fmt.Errorf("opencode /doc POST %s missing 204 response", path)
	}

	return nil
}

func openAPIOperation(paths map[string]any, path string, method string) (map[string]any, bool) {
	pathItem, _ := paths[path].(map[string]any)
	if pathItem == nil {
		return nil, false
	}

	operation, _ := pathItem[strings.ToLower(method)].(map[string]any)

	return operation, operation != nil
}

func openAPIHasResponse(operation map[string]any, status string) bool {
	responses, _ := operation["responses"].(map[string]any)
	_, ok := responses[status]

	return ok
}

func openAPIJSONResponseSchema(operation map[string]any, status string) (map[string]any, bool) {
	responses, _ := operation["responses"].(map[string]any)
	response, _ := responses[status].(map[string]any)

	return openAPIJSONContentSchema(response)
}

func openAPIJSONRequestSchema(operation map[string]any) (map[string]any, bool) {
	body, _ := operation["requestBody"].(map[string]any)
	if required, _ := body["required"].(bool); !required {
		return nil, false
	}

	return openAPIJSONContentSchema(body)
}

func openAPIJSONContentSchema(container map[string]any) (map[string]any, bool) {
	content, _ := container["content"].(map[string]any)
	jsonContent, _ := content["application/json"].(map[string]any)
	schema, _ := jsonContent["schema"].(map[string]any)

	return schema, schema != nil
}

func openAPISchemaDataArrayRef(schema map[string]any, want string) bool {
	properties, _ := schema["properties"].(map[string]any)

	data, _ := properties["data"].(map[string]any)
	if dataType, _ := data["type"].(string); dataType != openAPITypeArray {
		return false
	}

	items, _ := data["items"].(map[string]any)
	ref, _ := items["$ref"].(string)

	return strings.HasSuffix(ref, "/"+want)
}

func openAPISchemaArrayRef(schema map[string]any, want string) bool {
	if schemaType, _ := schema["type"].(string); schemaType != openAPITypeArray {
		return false
	}

	items, _ := schema["items"].(map[string]any)
	ref, _ := items["$ref"].(string)

	return strings.HasSuffix(ref, "/"+want)
}

func openAPIObjectHasRequiredProperty(schema map[string]any, property string) bool {
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties[property]; !ok {
		return false
	}

	switch required := schema["required"].(type) {
	case []any:
		for _, raw := range required {
			if value, _ := raw.(string); value == property {
				return true
			}
		}
	case []string:
		if slices.Contains(required, property) {
			return true
		}
	}

	return false
}

func openAPIObjectHasProperty(schema map[string]any, property string) bool {
	properties, _ := schema["properties"].(map[string]any)
	_, ok := properties[property]

	return ok
}

func openAPIObjectProperty(schema map[string]any, property string) (map[string]any, bool) {
	properties, _ := schema["properties"].(map[string]any)
	value, _ := properties[property].(map[string]any)

	return value, value != nil
}

func openAPIStringEnumContains(schema map[string]any, want string) bool {
	switch values := schema["enum"].(type) {
	case []any:
		for _, raw := range values {
			if value, _ := raw.(string); value == want {
				return true
			}
		}
	case []string:
		if slices.Contains(values, want) {
			return true
		}
	}

	return false
}

func openAPIEventUnionHasSchema(doc map[string]any, schemaName string) bool {
	event, ok := openAPIComponentSchema(doc, "#/components/schemas/Event")
	if !ok {
		return false
	}

	for _, key := range []string{"anyOf", "oneOf"} {
		values, _ := event[key].([]any)
		for _, raw := range values {
			option, _ := raw.(map[string]any)

			ref, _ := option["$ref"].(string)
			if strings.HasSuffix(ref, "/"+schemaName) {
				return true
			}
		}
	}

	return false
}

func openAPIComponentSchema(doc map[string]any, ref string) (map[string]any, bool) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, false
	}

	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	schema, _ := schemas[strings.TrimPrefix(ref, prefix)].(map[string]any)

	return schema, schema != nil
}

// CreateRuntimeXDGDirs materializes one shared runtime XDG root without a
// session-derived path component.
func CreateRuntimeXDGDirs(root string) (XDGDirs, error) {
	dirs := RuntimeXDGDirs(root)

	return dirs, ensureXDGDirs(dirs)
}

// RuntimeXDGDirs returns the XDG layout without touching the filesystem.
func RuntimeXDGDirs(root string) XDGDirs {
	return XDGDirs{
		Root:   root,
		Data:   filepath.Join(root, "data"),
		Config: filepath.Join(root, "config"),
		Cache:  filepath.Join(root, "cache"),
		State:  filepath.Join(root, "state"),
	}
}

func ensureXDGDirs(dirs XDGDirs) error {
	for _, dir := range []string{dirs.Root, dirs.Data, dirs.Config, dirs.Cache, dirs.State} {
		if dir == "" {
			return fmt.Errorf("xdg directory is empty")
		}

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	return nil
}

// openCodeConfigDir is the directory OpenCode reads its configuration from and
// installs its plugin loader into, beneath the runtime's XDG config root.
func openCodeConfigDir(dirs XDGDirs) string {
	return filepath.Join(dirs.Config, opencodeExecutableName)
}

const (
	openCodeConfigFileName    = "opencode.json"
	openCodeSeedManifestName  = ".seed-manifest.json"
	openCodeSeedBackupSuffix  = ".seed.bak"
	openCodeSeedManifestField = "seedFiles"
)

// materializeOpenCodeRuntimeConfig writes only immutable process-level seed
// configuration. Permission and MCP state are session/directory scoped and
// must never enter OPENCODE_CONFIG_CONTENT on a multiplexed runtime.
func materializeOpenCodeRuntimeConfig(dirs XDGDirs, seedFiles map[string]string, sessionCarrierPlugin string) (string, error) {
	configDir := openCodeConfigDir(dirs)
	if err := openCodeSeedMkdirAll(configDir, 0o700); err != nil {
		return "", err
	}

	content, writes, err := runtimeConfigContent(seedFiles, sessionCarrierPlugin)
	if err != nil {
		return "", err
	}

	if err := applyOpenCodeSeedGuard(configDir, writes); err != nil {
		return "", err
	}

	return content, nil
}

// openCodeMCPConfigBlock renders session MCP servers as the native opencode.json
// "mcp" object: remote entries for HTTP servers, local entries for stdio ones.
func openCodeMCPConfigBlock(servers []MCPServerConfig) map[string]any {
	if len(servers) == 0 {
		return nil
	}

	block := make(map[string]any, len(servers))

	for _, server := range servers {
		entry := map[string]any{"enabled": true}
		if server.URL != "" {
			entry["type"] = "remote"
			entry["url"] = server.URL

			if len(server.Headers) > 0 {
				entry["headers"] = server.Headers
			}
		} else {
			entry["type"] = "local"
			entry["command"] = server.Command

			if len(server.Env) > 0 {
				entry["environment"] = server.Env
			}
		}

		block[server.Name] = entry
	}

	return block
}

// planOpenCodeSeedWrites validates and collects every seeded file into a
// relpath→bytes plan without touching disk. The seeded opencode.json is not
// added to the plan: its parsed contents are returned so the caller can
// deep-merge the wrapper's managed keys on top before authoring the final file.
// Relpath keys are slash-normalized so the manifest and guard are deterministic
// across platforms.
func planOpenCodeSeedWrites(seedFiles map[string]string) (map[string][]byte, map[string]any, error) {
	writes := make(map[string][]byte, len(seedFiles))

	var seededConfig map[string]any

	for rel, contents := range seedFiles {
		clean, err := validateOpenCodeSeedPath(rel)
		if err != nil {
			return nil, nil, err
		}

		if clean == openCodeConfigFileName {
			parsed := map[string]any{}
			if err := json.Unmarshal([]byte(contents), &parsed); err != nil {
				return nil, nil, unsupportedField(seedFileField(rel))
			}

			seededConfig = parsed

			continue
		}

		writes[filepath.ToSlash(clean)] = []byte(contents)
	}

	return writes, seededConfig, nil
}

// applyOpenCodeSeedGuard writes the planned files under configDir behind an
// ownership manifest so a seed pass never overwrites a file the wrapper did not
// create. Per relpath: a missing target is written and recorded; a target the
// manifest already owns is overwritten (keeping a .seed.bak of the prior bytes
// when they change, or skipped entirely when identical); a target that exists
// but is absent from the manifest — an operator-authored file — fails closed
// with the uniform unsupported error, leaving every file untouched. The manifest
// and .seed.bak sidecars are seed-owned and never treated as seed targets.
func applyOpenCodeSeedGuard(configDir string, writes map[string][]byte) error {
	manifest, err := loadOpenCodeSeedManifest(configDir)
	if err != nil {
		return err
	}

	managed := make(map[string]struct{}, len(manifest))
	for _, rel := range manifest {
		managed[rel] = struct{}{}
	}

	rels := make([]string, 0, len(writes))
	for rel := range writes {
		rels = append(rels, rel)
	}

	slices.Sort(rels)

	// Phase 1: fail closed before any write if a target is an unmanaged file.
	for _, rel := range rels {
		target := filepath.Join(configDir, filepath.FromSlash(rel))
		if _, statErr := os.Stat(target); statErr == nil {
			if _, ok := managed[rel]; !ok {
				return unsupportedField(seedFileField(rel))
			}
		}
	}

	// Phase 2: back up changed managed files, then write.
	added := false

	for _, rel := range rels {
		target := filepath.Join(configDir, filepath.FromSlash(rel))
		contents := writes[rel]

		existing, readErr := os.ReadFile(target)
		switch {
		case readErr == nil:
			if bytes.Equal(existing, contents) {
				continue
			}
			// #nosec G703 -- target is confined to configDir by validateOpenCodeSeedPath; the suffix is a constant.
			if err := openCodeSeedWriteFile(target+openCodeSeedBackupSuffix, existing, 0o600); err != nil {
				return err
			}
		case errors.Is(readErr, os.ErrNotExist):
			if err := openCodeSeedMkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
		default:
			return readErr
		}

		if err := openCodeSeedWriteFile(target, contents, 0o600); err != nil {
			return err
		}

		if _, ok := managed[rel]; !ok {
			managed[rel] = struct{}{}
			manifest = append(manifest, rel)
			added = true
		}
	}

	if added {
		if err := writeOpenCodeSeedManifest(configDir, manifest); err != nil {
			return err
		}
	}

	return nil
}

// loadOpenCodeSeedManifest reads the seed-root ownership manifest, returning an
// empty list when it is absent so a fresh per-session root treats every write as
// a first write.
func loadOpenCodeSeedManifest(configDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(configDir, openCodeSeedManifestName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	var manifest []string
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, unsupportedField(openCodeSeedManifestField)
	}

	return manifest, nil
}

// writeOpenCodeSeedManifest persists the ownership manifest as a sorted,
// deterministic JSON array of managed relative paths.
func writeOpenCodeSeedManifest(configDir string, manifest []string) error {
	sorted := slices.Clone(manifest)
	slices.Sort(sorted)

	data, err := openCodeMarshalIndent(sorted, "", "  ")
	if err != nil {
		return err
	}

	data = append(data, '\n')

	return openCodeSeedWriteFile(filepath.Join(configDir, openCodeSeedManifestName), data, 0o600)
}

// validateOpenCodeSeedPath confines a seeded relative path to the config root,
// rejecting empty keys, rooted paths, and parent-directory escapes with the
// uniform unsupported error. It returns the cleaned, slash-normalized path.
func validateOpenCodeSeedPath(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" || rootedPath(rel) {
		return "", unsupportedField(seedFileField(rel))
	}

	if slices.Contains(strings.Split(filepath.ToSlash(rel), "/"), "..") {
		return "", unsupportedField(seedFileField(rel))
	}

	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || rootedPath(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", unsupportedField(seedFileField(rel))
	}

	return clean, nil
}

func seedFileField(rel string) string {
	return fmt.Sprintf("seedFiles[%s]", rel)
}

// UnsupportedFieldError names a caller-supplied option field the native
// runtime refuses. It carries the field path alone so the ACP surface can
// answer with the uniform unsupported-field rejection instead of an internal
// error built from this package's prose.
type UnsupportedFieldError struct {
	Field string
}

func (e *UnsupportedFieldError) Error() string {
	return "unsupported field " + e.Field
}

func unsupportedField(path string) error {
	return &UnsupportedFieldError{Field: path}
}

// deepMergeJSON returns base with override applied on top: nested maps are
// merged recursively, and override wins for every conflicting key.
func deepMergeJSON(base, override map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(override))
	maps.Copy(merged, base)

	for key, value := range override {
		if existing, ok := merged[key].(map[string]any); ok {
			if next, ok := value.(map[string]any); ok {
				merged[key] = deepMergeJSON(existing, next)

				continue
			}
		}

		merged[key] = value
	}

	return merged
}

func allocatePort() (int, error) {
	ln, err := openCodeListen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("allocated address is not tcp")
	}

	return addr.Port, nil
}

func randomPassword() (string, error) {
	var b [32]byte
	if _, err := io.ReadFull(openCodeRandReader, b[:]); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func envMapToSlice(env map[string]string) []string {
	env = composeEnvironment(env)

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}

	return out
}

func drainProcessPipe(log *slog.Logger, name string, reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)

	for scanner.Scan() {
	}

	_ = log
	_ = name
}

func IntFromNumber(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		if typed > 0 && typed <= math.MaxInt {
			return int(typed), true
		}
	case int:
		if typed > 0 {
			return typed, true
		}
	case json.Number:
		n, err := typed.Int64()
		if err == nil && n > 0 && n <= int64(math.MaxInt) {
			return int(n), true
		}
	}

	return 0, false
}
