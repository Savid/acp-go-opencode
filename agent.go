package opencodeacp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/observer"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	listSessionsPageSize            = 50
	defaultMaxActiveSessions        = 32
	defaultMaxConcurrentClientCalls = 16
	sessionTurnCapacity             = 1
	closeTimeout                    = 5 * time.Second
	settlementTimeout               = 20 * time.Second
)

var (
	agentJSONMarshal   = json.Marshal
	agentJSONUnmarshal = json.Unmarshal
	newAgentForServe   = NewAgent
	agentRandRead      = rand.Read
)

// Agent exposes OpenCode through ACP.
type Agent struct {
	options         Options
	log             *slog.Logger
	observe         *observer.Observer
	optionsErr      error
	containmentMode RuntimeContainmentMode

	mu                       sync.Mutex
	closed                   bool
	conn                     agentClient
	closeDone                chan struct{}
	closeErr                 error
	sessions                 map[acp.SessionId]*session
	deleted                  map[acp.SessionId]struct{}
	clientCalls              chan struct{}
	nativeTurns              chan struct{}
	clientCapabilities       acp.ClientCapabilities
	positionEncoding         acp.PositionEncodingKind
	runtime                  opencode.Client
	runtimeGeneration        uint64
	runtimeStarting          chan struct{}
	runtimeStartErr          error
	runtimeFatalErr          error
	runtimeNativeRelease     func()
	runtimeXDGScratchRelease func()
	runtimeRetirements       map[uint64]*runtimeRetirement
	directories              map[string]directoryBinding
	directoryIncarnation     directoryBindingIncarnation
	fingerprintKey           [32]byte
	restoreMu                sync.Mutex
}

type directoryBinding struct {
	SessionID      acp.SessionId
	MCPFingerprint string
	Incarnation    directoryBindingIncarnation
}

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

func NewAgent(opts ...Option) *Agent {
	options := applyOptions(opts)
	limits, optionsErr := normalizeConcurrencyLimits(options.ConcurrencyLimits)
	optionsErr = errors.Join(optionsErr, validateContainmentOptions(options))

	options.ConcurrencyLimits = limits

	if options.HealthCheckTimeout <= 0 {
		optionsErr = errors.Join(optionsErr, errors.New("OpenCode health check timeout must be positive"))
	}

	if options.TurnTimeout < 0 {
		optionsErr = errors.Join(optionsErr, errors.New("OpenCode turn timeout cannot be negative"))
	}

	optionsErr = errors.Join(optionsErr, validateImageLimits(options.ImageLimits))
	optionsErr = errors.Join(optionsErr, validateInputHandoffRoot(options.InputHandoffRoot))

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	if options.SessionStore == nil {
		options.SessionStore = NewInMemorySessionStore()
	}

	observe := observer.New(observer.Config{
		MeterProvider:  options.MeterProvider,
		Propagator:     options.TextMapPropagator,
		TracerProvider: options.TracerProvider,
		Version:        options.AgentVersion,
	})
	options.RuntimeResourceHooks = instrumentRuntimeResourceHooks(options.RuntimeResourceHooks, observe)

	mode := containmentMode(options)
	if options.RuntimeResourceHooks.ObserveContainment != nil {
		options.RuntimeResourceHooks.ObserveContainment(context.Background(), mode)
	}

	if mode == RuntimeContainmentBestEffort {
		options.RuntimeResourceHooks.ObserveProcessSnapshot = nil
	}

	if mode == RuntimeContainmentBestEffort {
		log.Warn("Darwin best-effort process containment is enabled; escaped descendants may survive, numeric PGID reuse can cause collateral signalling, marker correlation is not ownership, markers can be scrubbed, and native-root permits do not bound escaped provider work",
			slog.String("containment", string(mode)),
		)
	}

	agent := &Agent{
		options:            options,
		log:                log,
		optionsErr:         optionsErr,
		containmentMode:    mode,
		observe:            observe,
		sessions:           make(map[acp.SessionId]*session),
		deleted:            make(map[acp.SessionId]struct{}),
		directories:        make(map[string]directoryBinding),
		runtimeRetirements: make(map[uint64]*runtimeRetirement),
		clientCalls:        make(chan struct{}, limits.MaxConcurrentClientCalls),
		nativeTurns:        make(chan struct{}, 1),
	}
	if _, err := agentRandRead(agent.fingerprintKey[:]); err != nil {
		agent.optionsErr = errors.Join(agent.optionsErr, fmt.Errorf("create runtime fingerprint key: %w", err))
	}

	return agent
}

// ContainmentMode reports the effective native process boundary.
func (a *Agent) ContainmentMode() RuntimeContainmentMode {
	if a == nil {
		return RuntimeContainmentUnavailable
	}

	return a.containmentMode
}

func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) (serveErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	agent := newAgentForServe(opts...)
	defer func() {
		if closeErr := agent.Close(); closeErr != nil {
			agent.log.DebugContext(context.Background(), "close OpenCode ACP agent failed", slog.String("error", closeErr.Error()))
			serveErr = closeErr
		}
	}()

	conn := newLocalAgentConnection(agent, output, input)
	agent.setAgentClient(conn)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}

func (a *Agent) setAgentClient(conn agentClient) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.conn = conn
}

func (a *Agent) connection() agentClient {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.conn
}

func (a *Agent) Close() error {
	a.mu.Lock()
	if a.closeDone != nil {
		done := a.closeDone
		a.mu.Unlock()
		<-done
		a.mu.Lock()
		err := a.closeErr
		a.mu.Unlock()

		return err
	}

	closeDone := make(chan struct{})
	a.closeDone = closeDone

	sessions := make([]*session, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}

	runtime := a.runtime
	generation := a.runtimeGeneration
	waiting := a.runtimeStarting

	stickyRuntimeErr := a.runtimeFatalErr
	if !fatalRuntimeCleanup(stickyRuntimeErr) {
		stickyRuntimeErr = nil
	}

	a.closed = true
	a.conn = nil
	a.mu.Unlock()

	err := stickyRuntimeErr
	if runtime != nil {
		err = errors.Join(err, a.retireSharedRuntime(generation, "shared OpenCode runtime retired while closing agent"))
	} else if waiting != nil {
		<-waiting
		a.mu.Lock()
		retirement := a.runtimeRetirements[generation]
		startErr := a.runtimeStartErr
		a.mu.Unlock()

		if retirement != nil {
			<-retirement.done
			err = errors.Join(err, retirement.err)
		}

		if fatalRuntimeCleanup(startErr) {
			err = errors.Join(err, startErr)
		}
	}

	for _, session := range sessions {
		ctx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
		err = errors.Join(err, session.Close(ctx))

		cancel()
	}

	a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))

	a.mu.Lock()
	a.sessions = make(map[acp.SessionId]*session)
	a.closeErr = err

	close(closeDone)
	a.mu.Unlock()

	return err
}

func (a *Agent) Initialize(_ context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	if a.optionsErr != nil {
		return acp.InitializeResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: a.optionsErr.Error()})
	}

	title := a.options.AgentTitle
	positionEncoding := selectPositionEncoding(params.ClientCapabilities.PositionEncodings)

	a.mu.Lock()
	a.clientCapabilities = cloneClientCapabilities(params.ClientCapabilities)
	a.positionEncoding = positionEncoding
	a.mu.Unlock()

	opencodeMeta := map[string]any{
		"fork": map[string]any{
			"unstable":       true,
			"method":         ForkSessionMethod,
			jsonFieldRequest: "acp.UnstableForkSessionRequest JSON payload only",
			"response":       "acp.UnstableForkSessionResponse JSON payload only",
		},
		"elicitation": map[string]any{
			"unstable":     true,
			jsonFieldScope: string(RuntimeResourceSession),
			"tracks":       "in-progress ACP elicitation RFD",
		},
		rawEventCapabilityKey: map[string]any{
			"method":         RawEventMethod,
			"enabledBy":      rawEventEnabledByPath,
			"maxBytes":       rawEventMaxBytes,
			"defaultEnabled": false,
		},
		"sessionStore": map[string]any{
			"format":     SessionStoreFormat,
			jsonFieldKey: []string{jsonFieldSessionID, "subpath"},
		},
		structuredOutputMetaKey: map[string]any{
			"config": outputSchemaOptionPath,
			"result": structuredOutputPath,
			"schema": opencode.OutputFormatJSONSchema,
		},
	}

	capabilityMeta := map[string]any{
		opencodeMetaKey:  opencodeMeta,
		routeEnvelopeKey: map[string]any{metaFieldVersions: []int{routeEnvelopeVersion}},
		mediaEnvelopeKey: a.options.mediaEnvelope(),
	}

	// The handoff advertisement answers whether the host's read root reached
	// this adapter, so it is present only when one is configured.
	if a.options.InputHandoffRoot != "" {
		capabilityMeta[handoffEnvelopeKey] = map[string]any{metaFieldVersions: []int{handoffEnvelopeVersion}}
	}

	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    a.options.AgentName,
			Title:   &title,
			Version: a.options.AgentVersion,
		},
		AuthMethods: []acp.AuthMethod{},
		AgentCapabilities: acp.AgentCapabilities{
			Meta:        capabilityMeta,
			LoadSession: true,
			McpCapabilities: acp.McpCapabilities{
				Http: true,
			},
			PositionEncoding: &positionEncoding,
			PromptCapabilities: acp.PromptCapabilities{
				Image:           true,
				EmbeddedContext: true,
			},
			SessionCapabilities: acp.SessionCapabilities{
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
				Close:                 &acp.SessionCloseCapabilities{},
				Delete:                &acp.SessionDeleteCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
			},
		},
	}, nil
}

func (a *Agent) Authenticate(_ context.Context, params acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
}

func (a *Agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

func (a *Agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	switch method {
	case ForkSessionMethod:
		var req acp.UnstableForkSessionRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
		}

		if err := req.Validate(); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
		}

		return a.forkSession(ctx, req)
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

// ensureOpen refuses an agent that is closed or was built with options this
// package rejected. The options answer is not left to initialize alone: an
// in-process host may never call it, and an agent whose limits or read root were
// refused must not serve a turn under them.
func (a *Agent) ensureOpen() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
	}

	if a.optionsErr != nil {
		return acp.NewInvalidParams(map[string]any{jsonFieldError: a.optionsErr.Error()})
	}

	return nil
}

func (a *Agent) sessionStore() SessionStore {
	if a.options.SessionStore == nil {
		return NewInMemorySessionStore()
	}

	return a.options.SessionStore
}

func (a *Agent) sessionStoreContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := a.options.SessionStoreLoadTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	return context.WithTimeout(ctx, timeout)
}

func (a *Agent) acquireClientCall(ctx context.Context) (func(), error) {
	select {
	case a.clientCalls <- struct{}{}:
		return func() { <-a.clientCalls }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueBackpressure, jsonFieldLimit: "client_calls"})
	}
}

// acquireNativeTurn serializes prompts across every logical session sharing
// this Agent's native runtime. Cancellation retires that whole runtime, so a
// second active native turn could otherwise be killed as collateral work.
func (a *Agent) acquireNativeTurn(ctx context.Context) (func(), error) {
	select {
	case a.nativeTurns <- struct{}{}:
		return func() { <-a.nativeTurns }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Agent) session(id acp.SessionId) (*session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.deleted[id]; ok {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
	}

	session := a.sessions[id]
	if session == nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
	}

	return session, nil
}

func (a *Agent) storeStartedSession(session *session) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
	}

	if a.runtime == nil {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "shared OpenCode runtime exited"})
	}

	if session.runtimeGeneration != a.runtimeGeneration {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "shared OpenCode runtime generation changed"})
	}

	if len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueBackpressure, jsonFieldLimit: "active_sessions"})
	}

	a.sessions[session.id] = session
	delete(a.deleted, session.id)

	a.observe.AddActiveSession(context.Background(), 1)

	return nil
}

func (a *Agent) removeSessionIf(id acp.SessionId, target *session) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.sessions[id] != target {
		return false
	}

	delete(a.sessions, id)

	return true
}

func (a *Agent) isDeleted(id acp.SessionId) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, ok := a.deleted[id]

	return ok
}

func (a *Agent) clientElicitationCapabilities() *acp.ElicitationCapabilities {
	a.mu.Lock()
	defer a.mu.Unlock()

	caps := a.clientCapabilities.Elicitation
	if caps == nil {
		return nil
	}

	encoded, err := agentJSONMarshal(caps)
	if err != nil {
		return caps
	}

	var cloned acp.ElicitationCapabilities
	if err := agentJSONUnmarshal(encoded, &cloned); err != nil {
		return caps
	}

	return &cloned
}

func (a *Agent) clientSupportsFormElicitation() bool {
	caps := a.clientElicitationCapabilities()
	if caps == nil {
		return false
	}

	return caps.Form != nil || caps.Url == nil
}

func selectPositionEncoding(values []acp.PositionEncodingKind) acp.PositionEncodingKind {
	for _, value := range values {
		if value == acp.PositionEncodingKindUtf8 {
			return value
		}
	}

	for _, value := range values {
		if value == acp.PositionEncodingKindUtf16 {
			return value
		}
	}

	return acp.PositionEncodingKindUtf16
}

func cloneClientCapabilities(caps acp.ClientCapabilities) acp.ClientCapabilities {
	encoded, err := agentJSONMarshal(caps)
	if err != nil {
		return caps
	}

	var cloned acp.ClientCapabilities
	if err := agentJSONUnmarshal(encoded, &cloned); err != nil {
		return caps
	}

	return cloned
}
