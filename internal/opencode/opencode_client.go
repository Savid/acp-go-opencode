//nolint:tagliatelle // OpenCode native JSON fields use modelID/sessionID/providerID spellings.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-opencode/internal/defaults"
)

const (
	opencodeDefaultUsername = "opencode"
	LeaseFileName           = "server.lease"

	// opencodeExecutableName is the OpenCode program name: the default
	// executable, the per-XDG config directory, and the process cmdline
	// marker used by the lease reaper.
	opencodeExecutableName = "opencode"
	// opencodeServeCommand is the OpenCode CLI subcommand that starts the
	// loopback HTTP server.
	opencodeServeCommand = "serve"
	// defaultSessionPathName is the fallback directory name used when no ACP
	// session ID is available for a per-session XDG home.
	defaultSessionPathName = "session"
	// eventTypeServerConnected is the first SSE event type emitted by a
	// healthy opencode serve process.
	eventTypeServerConnected = "server.connected"
)

// Native OpenCode REST routes used by the client and required by the /doc
// contract validation.
const (
	routeConfigProviders      = "/config/providers"
	routeCommand              = "/command"
	routeEvent                = "/event"
	routeSession              = "/session"
	routeSessionStatus        = "/session/status"
	routePermission           = "/permission"
	routeQuestion             = "/question"
	routeAPIPermissionRequest = "/api/permission/request"
	routeAPIQuestionRequest   = "/api/question/request"
)

// Native OpenCode /doc path templates validated during readiness.
const (
	docPathSessionCommand = "/session/{sessionID}/command"
	docPathSessionMessage = "/session/{sessionID}/message"
	docPathQuestionReply  = "/question/{requestID}/reply"
	docPathQuestionReject = "/question/{requestID}/reject"
)

// Native OpenCode wire-field spellings shared between request bodies and the
// /doc schema validation.
const (
	fieldAction     = "action"
	fieldAnswers    = "answers"
	fieldID         = "id"
	fieldPermission = "permission"
	fieldQuestions  = "questions"
	fieldReply      = "reply"
	fieldRequestID  = "requestID"
	fieldSessionID  = "sessionID"
)

// openAPITypeArray is the OpenAPI schema "type" value for arrays.
const openAPITypeArray = "array"

var ErrSSEDisconnect = errors.New("opencode SSE disconnected")

type Client interface {
	Close(context.Context) error
	CreateSession(context.Context, string) (NativeSession, error)
	GetSession(context.Context, string) (NativeSession, error)
	ListSessions(context.Context, string) ([]NativeSession, error)
	DeleteSession(context.Context, string) error
	Commands(context.Context) ([]NativeCommand, error)
	RunCommand(context.Context, string, CommandRequest) (NativeMessage, error)
	SendMessage(context.Context, string, MessageRequest) (NativeMessage, error)
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
	Events() <-chan Event
	EventErrors() <-chan error
	XDGDirs() XDGDirs
}

type StartOptions struct {
	ACPSessionID      ACPSessionID
	Root              string
	Cwd               string
	ExecutablePath    string
	DefaultModel      string
	Env               map[string]string
	Pure              bool
	QuestionTool      bool
	LogLevel          string
	MinimumVersion    string
	HealthTimeout     time.Duration
	Logger            *slog.Logger
	ExistingXDG       XDGDirs
	SkipVersionGate   bool
	AdditionalEnv     map[string]string
	ExpectedNativeID  string
	PermissionSurface bool
	Permission        string
}

type ACPSessionID string

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
	cmd                          *exec.Cmd
	cancel                       context.CancelFunc
	xdg                          XDGDirs
	log                          *slog.Logger
	sessionPermissionListSupport bool
	sessionQuestionListSupport   bool

	events chan Event
	errs   chan error
	closed chan struct{}
	once   sync.Once

	streamMu    sync.Mutex
	streamEpoch uint64
}

type NativeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Agent     string `json:"agent"`
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
	ID         string       `json:"id"`
	SessionID  string       `json:"sessionID"`
	Role       string       `json:"role"`
	ParentID   string       `json:"parentID"`
	ModelID    string       `json:"modelID"`
	ProviderID string       `json:"providerID"`
	Mode       string       `json:"mode"`
	Agent      string       `json:"agent"`
	Finish     string       `json:"finish"`
	Cost       float64      `json:"cost"`
	Tokens     NativeTokens `json:"tokens"`
	Error      *NativeError `json:"error,omitempty"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type NativeError struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Message string `json:"message"`
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
	Cost      float64         `json:"cost"`
	Tokens    NativeTokens    `json:"tokens"`
	Raw       json.RawMessage `json:"-"`
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
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Properties  json.RawMessage `json:"properties"`
	Raw         json.RawMessage `json:"-"`
	StreamEpoch uint64          `json:"-"`
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
	NoReply   bool             `json:"noReply,omitempty"`
	Parts     []map[string]any `json:"parts"`
}

type CommandRequest struct {
	MessageID string           `json:"messageID,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Model     string           `json:"model,omitempty"`
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
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	Limit      map[string]any          `json:"limit"`
	Reasoning  bool                    `json:"reasoning"`
	ToolCall   bool                    `json:"tool_call"`
	Modalities ProviderModelModalities `json:"modalities"`
	Options    map[string]any          `json:"options"`
}

type ProviderModelModalities struct {
	Input []string `json:"input"`
}

type processIdentity struct {
	StartTime string
	Cmdline   []string
	Env       map[string]string
}

var (
	openCodeCommandContext                = exec.CommandContext
	openCodeListen                        = net.Listen
	openCodeRandReader          io.Reader = rand.Reader
	openCodeMarshalIndent                 = json.MarshalIndent
	openCodeWriteLease                    = writeLease
	openCodeTerminateProcess              = terminateOpenCodeProcess
	openCodeKillProcess                   = killOpenCodeProcess
	openCodeInspectProcess                = inspectOpenCodeProcess
	procReadFile                          = os.ReadFile
	openCodeWaitCommand                   = func(cmd *exec.Cmd) error { return cmd.Wait() }
	openCodeAfter                         = time.After
	openCodeReadyPollInterval             = 100 * time.Millisecond
	openCodeEventReconnectDelay           = 250 * time.Millisecond
	openCodeShutdownTimeout               = 5 * time.Second
)

func StartServer(ctx context.Context, options StartOptions) (Client, error) {
	if options.Logger == nil {
		options.Logger = slog.Default()
	}

	if options.HealthTimeout <= 0 {
		options.HealthTimeout = defaults.HealthCheckTimeout
	}

	root := options.Root
	if root == "" {
		root = filepath.Join(os.TempDir(), "acp-go-opencode")
	}

	if err := reapStaleLeases(root, options.Logger); err != nil {
		return nil, err
	}

	xdg := options.ExistingXDG
	if xdg.Root == "" {
		var err error

		xdg, err = CreateXDGDirs(root, string(options.ACPSessionID))
		if err != nil {
			return nil, err
		}
	}

	if err := ensureXDGDirs(xdg); err != nil {
		return nil, err
	}

	permissionConfig, err := materializeOpenCodePermissionConfig(xdg, options.Permission)
	if err != nil {
		return nil, err
	}

	port, err := allocatePort()
	if err != nil {
		return nil, err
	}

	password, err := randomPassword()
	if err != nil {
		return nil, err
	}

	username := opencodeDefaultUsername

	executable := options.ExecutablePath
	if executable == "" {
		executable = opencodeExecutableName
	}

	args := []string{opencodeServeCommand, "--hostname", "127.0.0.1", "--port", strconv.Itoa(port)}
	if options.Pure {
		args = append(args, "--pure")
	}

	if options.LogLevel != "" {
		args = append(args, "--log-level", options.LogLevel)
	}

	processCtx, cancel := context.WithCancel(context.Background())

	cmd := openCodeCommandContext(processCtx, executable, args...)
	if options.Cwd != "" {
		cmd.Dir = options.Cwd
	}

	env := mergeProcessEnv(options.Env)
	for key, value := range options.AdditionalEnv {
		env[key] = value
	}

	env["XDG_DATA_HOME"] = xdg.Data
	env["XDG_CONFIG_HOME"] = xdg.Config
	env["XDG_CACHE_HOME"] = xdg.Cache
	env["XDG_STATE_HOME"] = xdg.State
	env["OPENCODE_SERVER_USERNAME"] = username
	env["OPENCODE_SERVER_PASSWORD"] = password

	env["OPENCODE_CONFIG_CONTENT"] = permissionConfig
	if options.QuestionTool {
		env["OPENCODE_ENABLE_QUESTION_TOOL"] = "1"
	}

	cmd.Env = envMapToSlice(env)
	configureOpenCodeProcess(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()

		return nil, err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()

		return nil, err
	}

	if err := openCodeWriteLease(xdg.State, serverLease{
		PID:          0,
		Port:         port,
		StartedAt:    time.Now().UnixMilli(),
		PasswordHash: passwordHash(password),
		XDGRoot:      xdg.Root,
	}); err != nil {
		cancel()

		return nil, err
	}

	if err := cmd.Start(); err != nil {
		cancel()

		return nil, err
	}

	lease := serverLease{
		PID:          cmd.Process.Pid,
		Port:         port,
		StartedAt:    time.Now().UnixMilli(),
		PasswordHash: passwordHash(password),
		XDGRoot:      xdg.Root,
	}
	if identity, err := openCodeInspectProcess(cmd.Process.Pid); err == nil {
		lease.ProcessStartTime = identity.StartTime
	}

	if err := openCodeWriteLease(xdg.State, lease); err != nil {
		cancel()

		_ = openCodeKillProcess(cmd)

		return nil, err
	}

	go drainProcessPipe(options.Logger, "opencode stdout", stdout)
	go drainProcessPipe(options.Logger, "opencode stderr", stderr)

	server := &openCodeServer{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    "http://127.0.0.1:" + strconv.Itoa(port),
		username:   username,
		password:   password,
		cmd:        cmd,
		cancel:     cancel,
		xdg:        xdg,
		log:        options.Logger,
		events:     make(chan Event, 256),
		errs:       make(chan error, 8),
		closed:     make(chan struct{}),
	}

	readyCtx, readyCancel := context.WithTimeout(ctx, options.HealthTimeout)
	defer readyCancel()

	if err := server.waitReady(readyCtx, processCtx, options); err != nil {
		_ = server.Close(context.Background())

		return nil, err
	}

	return server, nil
}

func (s *openCodeServer) waitReady(ctx context.Context, eventCtx context.Context, options StartOptions) error {
	var health struct {
		Healthy bool   `json:"healthy"`
		Version string `json:"version"`
	}
	for {
		err := s.getJSON(ctx, "/global/health", nil, &health)
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

	if !options.SkipVersionGate && options.MinimumVersion != "" && compareSemver(health.Version, options.MinimumVersion) < 0 {
		return fmt.Errorf("opencode %s is below minimum %s", health.Version, options.MinimumVersion)
	}

	var doc map[string]any
	if err := s.getJSON(ctx, "/doc", nil, &doc); err != nil {
		return fmt.Errorf("load opencode /doc: %w", err)
	}

	docCapabilities, err := inspectOpenCodeDoc(doc)
	if err != nil {
		return err
	}

	s.sessionPermissionListSupport = docCapabilities.sessionPermissionList

	s.sessionQuestionListSupport = docCapabilities.sessionQuestionList
	go s.readEvents(eventCtx)

	select {
	case event := <-s.events:
		if event.Type != eventTypeServerConnected {
			return fmt.Errorf("first opencode event was %q, want server.connected", event.Type)
		}
	case err := <-s.errs:
		return fmt.Errorf("opencode event stream failed during readiness: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

func (s *openCodeServer) Close(ctx context.Context) error {
	var err error

	s.once.Do(func() {
		terminateProcess := openCodeTerminateProcess
		killProcess := openCodeKillProcess
		waitCommand := openCodeWaitCommand
		after := openCodeAfter
		shutdownTimeout := openCodeShutdownTimeout

		close(s.closed)

		if s.cancel != nil {
			s.cancel()
		}

		if s.cmd != nil && s.cmd.Process != nil {
			_ = terminateProcess(s.cmd)

			done := make(chan error, 1)
			go func() { done <- waitCommand(s.cmd) }()

			select {
			case waitErr := <-done:
				if waitErr != nil && s.log != nil {
					s.log.DebugContext(ctx, "opencode exited during shutdown", slog.Any("error", waitErr))
				}
			case <-ctx.Done():
				_ = killProcess(s.cmd)
				err = ctx.Err()
			case <-after(shutdownTimeout):
				_ = killProcess(s.cmd)
				err = errors.New("opencode process did not exit after shutdown")
			}
		}

		_ = os.Remove(filepath.Join(s.xdg.State, LeaseFileName))
	})

	return err
}

func (s *openCodeServer) Events() <-chan Event {
	return s.events
}

func (s *openCodeServer) EventErrors() <-chan error {
	return s.errs
}

func (s *openCodeServer) XDGDirs() XDGDirs {
	return s.xdg
}

func (s *openCodeServer) CreateSession(ctx context.Context, title string) (NativeSession, error) {
	body := map[string]any{}
	if title != "" {
		body["title"] = title
	}

	var out NativeSession

	err := s.doJSON(ctx, http.MethodPost, routeSession, nil, body, &out)

	return out, err
}

func (s *openCodeServer) GetSession(ctx context.Context, id string) (NativeSession, error) {
	var out NativeSession

	err := s.getJSON(ctx, "/session/"+url.PathEscape(id), nil, &out)

	return out, err
}

func (s *openCodeServer) ListSessions(ctx context.Context, cwd string) ([]NativeSession, error) {
	query := url.Values{}
	if cwd != "" {
		query.Set("directory", cwd)
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

func (s *openCodeServer) RunCommand(ctx context.Context, id string, req CommandRequest) (NativeMessage, error) {
	var out NativeMessage
	if err := s.doJSONWithClient(ctx, s.blockingHTTPClient(), http.MethodPost, "/session/"+url.PathEscape(id)+routeCommand, nil, req, &out); err != nil {
		return NativeMessage{}, err
	}

	if err := assistantMessageError(out); err != nil {
		return NativeMessage{}, err
	}

	return out, nil
}

func (s *openCodeServer) SendMessage(ctx context.Context, id string, req MessageRequest) (NativeMessage, error) {
	var out NativeMessage
	if err := s.doJSONWithClient(ctx, s.blockingHTTPClient(), http.MethodPost, "/session/"+url.PathEscape(id)+"/message", nil, req, &out); err != nil {
		return NativeMessage{}, err
	}

	if err := assistantMessageError(out); err != nil {
		return NativeMessage{}, err
	}

	return out, nil
}

func assistantMessageError(message NativeMessage) error {
	if !strings.EqualFold(message.Info.Finish, "error") && message.Info.Error == nil {
		return nil
	}

	if message.Info.Error == nil {
		return fmt.Errorf("opencode assistant error")
	}

	detail := firstNonEmpty(message.Info.Error.Message, message.Info.Error.Type, message.Info.Error.Name)
	if detail == "" {
		return fmt.Errorf("opencode assistant error")
	}

	return fmt.Errorf("opencode assistant error: %s", detail)
}

func (s *openCodeServer) Messages(ctx context.Context, id string) ([]NativeMessage, error) {
	var out []NativeMessage

	err := s.getJSON(ctx, "/session/"+url.PathEscape(id)+"/message", nil, &out)

	return out, err
}

func (s *openCodeServer) SessionStatus(ctx context.Context) (map[string]NativeSessionStatus, error) {
	out := map[string]NativeSessionStatus{}
	err := s.getJSON(ctx, routeSessionStatus, nil, &out)

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
	return fmt.Sprintf("opencode %s %s returned %s: %s", e.Method, e.Path, e.Status, e.Body)
}

func IsBadRequest(err error) bool {
	var httpErr *HTTPError

	return errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusBadRequest
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
	for {
		epoch := s.nextStreamEpoch()
		if err := s.readEventStream(ctx, epoch); err != nil {
			select {
			case s.errs <- StreamError{Epoch: epoch, Err: err}:
			default:
			}
		}

		select {
		case <-s.closed:
			return
		case <-openCodeAfter(openCodeEventReconnectDelay):
		}
	}
}

func (s *openCodeServer) nextStreamEpoch() uint64 {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()

	s.streamEpoch++

	return s.streamEpoch
}

type StreamError struct {
	Epoch uint64
	Err   error
}

func (e StreamError) Error() string {
	return e.Err.Error()
}

func (e StreamError) Unwrap() error {
	return e.Err
}

func StreamErrorEpoch(err error) uint64 {
	var streamErr StreamError
	if errors.As(err, &streamErr) {
		return streamErr.Epoch
	}

	return 0
}

func (s *openCodeServer) readEventStream(ctx context.Context, epochs ...uint64) error {
	var epoch uint64
	if len(epochs) > 0 {
		epoch = epochs[0]
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+routeEvent, http.NoBody)
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
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("opencode event stream returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

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

		event.StreamEpoch = epoch
		select {
		case s.events <- event:
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

		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}

			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
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

func validateOpenCodeDoc(doc map[string]any) error {
	_, err := inspectOpenCodeDoc(doc)

	return err
}

func inspectOpenCodeDoc(doc map[string]any) (openCodeDocCapabilities, error) {
	rawPaths, _ := doc["paths"].(map[string]any)

	required := []string{
		routeConfigProviders,
		routeCommand,
		routeEvent,
		routeSessionStatus,
		routeSession,
		"/session/{sessionID}",
		docPathSessionCommand,
		docPathSessionMessage,
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
		for _, value := range required {
			if value == property {
				return true
			}
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
		for _, value := range values {
			if value == want {
				return true
			}
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

func CreateXDGDirs(root string, sessionID string) (XDGDirs, error) {
	if sessionID == "" {
		sessionID = defaultSessionPathName
	}

	base := filepath.Join(root, SafePathName(sessionID))
	dirs := XDGDirs{
		Root:   base,
		Data:   filepath.Join(base, "data"),
		Config: filepath.Join(base, "config"),
		Cache:  filepath.Join(base, "cache"),
		State:  filepath.Join(base, "state"),
	}

	return dirs, ensureXDGDirs(dirs)
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

func materializeOpenCodePermissionConfig(dirs XDGDirs, permission string) (string, error) {
	if err := validateOpenCodePermission(permission); err != nil {
		return "", err
	}

	config := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		fieldPermission: map[string]string{
			"*": normalizeOpenCodePermission(permission),
		},
	}

	data, err := openCodeMarshalIndent(config, "", "  ")
	if err != nil {
		return "", err
	}

	data = append(data, '\n')

	configDir := filepath.Join(dirs.Config, opencodeExecutableName)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(configDir, "opencode.json"), data, 0o600); err != nil {
		return "", err
	}

	return string(data), nil
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

func passwordHash(password string) string {
	sum := sha256.Sum256([]byte(password))

	return hex.EncodeToString(sum[:])
}

type serverLease struct {
	PID              int    `json:"pid"`
	Port             int    `json:"port"`
	StartedAt        int64  `json:"startedAtUnixMilli"`
	PasswordHash     string `json:"passwordHash"`
	XDGRoot          string `json:"xdgRoot,omitempty"`
	ProcessStartTime string `json:"processStartTime,omitempty"`
}

func writeLease(stateDir string, lease serverLease) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}

	data, err := openCodeMarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(stateDir, LeaseFileName), data, 0o600)
}

func reapStaleLeases(root string, log *slog.Logger) error {
	if root == "" {
		return nil
	}

	matches, err := filepath.Glob(filepath.Join(root, "*", "state", LeaseFileName))
	if err != nil {
		return err
	}

	for _, match := range matches {
		ReapLeaseFile(match, log)
	}

	return nil
}

func ReapLeaseFile(path string, log *slog.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}

		return
	}

	var lease serverLease
	if err := json.Unmarshal(data, &lease); err != nil {
		_ = os.Remove(path)

		return
	}

	if lease.PID > 0 && leaseMatchesProcess(path, lease) {
		if err := killProcessID(lease.PID); err != nil && log != nil {
			log.Debug("reap stale opencode lease failed", slog.Int("pid", lease.PID), slog.String("error", err.Error()))
		}
	}

	_ = os.Remove(path)
}

func leaseMatchesProcess(path string, lease serverLease) bool {
	if lease.PID <= 0 || lease.ProcessStartTime == "" {
		return false
	}

	identity, err := openCodeInspectProcess(lease.PID)
	if err != nil {
		return false
	}

	if identity.StartTime != lease.ProcessStartTime {
		return false
	}

	stateDir := filepath.Dir(path)
	if identity.Env["XDG_STATE_HOME"] != stateDir {
		return false
	}

	if passwordHash(identity.Env["OPENCODE_SERVER_PASSWORD"]) != lease.PasswordHash {
		return false
	}

	if lease.XDGRoot != "" && filepath.Clean(lease.XDGRoot) != filepath.Clean(filepath.Dir(stateDir)) {
		return false
	}

	return cmdlineLooksLikeOpenCodeServe(identity.Cmdline)
}

func cmdlineLooksLikeOpenCodeServe(args []string) bool {
	for _, arg := range args {
		if arg == opencodeServeCommand {
			return true
		}
	}

	for _, arg := range args {
		if strings.Contains(filepath.Base(arg), opencodeExecutableName) {
			return true
		}
	}

	return false
}

func mergeProcessEnv(overlays ...map[string]string) map[string]string {
	env := map[string]string{}

	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			env[key] = value
		}
	}

	for _, overlay := range overlays {
		for key, value := range overlay {
			if key != "" {
				env[key] = value
			}
		}
	}

	return env
}

func envMapToSlice(env map[string]string) []string {
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
		if log != nil {
			log.Debug("opencode process output", slog.String("pipe", name), slog.String("line", scanner.Text()))
		}
	}
}

func compareSemver(got string, want string) int {
	g := parseSemver(got)

	w := parseSemver(want)
	for i := range g {
		if g[i] < w[i] {
			return -1
		}

		if g[i] > w[i] {
			return 1
		}
	}

	return 0
}

func parseSemver(value string) [3]int {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	parts := strings.Split(value, ".")

	var out [3]int
	for i := 0; i < len(parts) && i < len(out); i++ {
		part := parts[i]
		for j, r := range part {
			if r < '0' || r > '9' {
				part = part[:j]

				break
			}
		}

		n, _ := strconv.Atoi(part)
		out[i] = n
	}

	return out
}

func SafePathName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultSessionPathName
	}

	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "..", "_")

	return replacer.Replace(value)
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
