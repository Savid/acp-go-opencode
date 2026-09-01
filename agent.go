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
	"github.com/savid/acp-go-opencode/internal/lifecycle"
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
	options      Options
	log          *slog.Logger
	observe      *observer.Observer
	optionsErr   error
	providerAuth *providerAuth

	mu                   sync.Mutex
	closed               bool
	conn                 agentClient
	closeDone            chan struct{}
	closeErr             error
	sessions             map[acp.SessionId]*session
	deleted              map[acp.SessionId]struct{}
	clientCalls          chan struct{}
	clientCapabilities   acp.ClientCapabilities
	positionEncoding     acp.PositionEncodingKind
	lifecycle            lifecycle.Negotiated
	runtime              opencode.Client
	runtimeGeneration    uint64
	runtimeStarting      chan struct{}
	runtimeStartErr      error
	runtimeFatalErr      error
	runtimeRetirements   map[uint64]*runtimeRetirement
	runtimeSequencing    *runtimeRetirement
	directories          map[string]directoryBinding
	directoryIncarnation directoryBindingIncarnation
	fingerprintKey       [32]byte
	restoreMu            sync.Mutex
	nativeAdmissionMu    sync.Mutex
	// sessionLifecycleMu serializes lifecycle operations that can reuse, retire,
	// or replace an existing logical session binding. In particular, two active
	// load/resume calls must never both prepare successors for the same map slot.
	sessionLifecycleMu sync.Mutex
	retiredNativeTrees map[string]retiredNativeTree
}

type retiredNativeTree struct {
	reclaimed bool
	cleanup   func() error
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

	optionsErr = errors.Join(optionsErr, validateRuntimeOptions(options))
	if options.hostAuthorityConfigured && options.HostAuthority == nil {
		optionsErr = errors.Join(optionsErr, ErrHostAuthorityUnavailable)
	}

	options.ConcurrencyLimits = limits

	if options.HealthCheckTimeout <= 0 {
		optionsErr = errors.Join(optionsErr, errors.New("OpenCode health check timeout must be positive"))
	}

	if options.TurnTimeout < 0 {
		optionsErr = errors.Join(optionsErr, errors.New("OpenCode turn timeout cannot be negative"))
	}

	optionsErr = errors.Join(optionsErr, validateImageLimits(options.ImageLimits))
	optionsErr = errors.Join(optionsErr, validateInputHandoffRoot(options.InputHandoffRoot))
	optionsErr = errors.Join(optionsErr, validateProviderAuthRoot(options))

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

	agent := &Agent{
		options:            options,
		log:                log,
		optionsErr:         optionsErr,
		observe:            observe,
		sessions:           make(map[acp.SessionId]*session),
		deleted:            make(map[acp.SessionId]struct{}),
		directories:        make(map[string]directoryBinding),
		runtimeRetirements: make(map[uint64]*runtimeRetirement),
		retiredNativeTrees: make(map[string]retiredNativeTree),
		clientCalls:        make(chan struct{}, limits.MaxConcurrentClientCalls),
	}
	if _, err := agentRandRead(agent.fingerprintKey[:]); err != nil {
		agent.optionsErr = errors.Join(agent.optionsErr, fmt.Errorf("create runtime fingerprint key: %w", err))
	}

	agent.providerAuth = newProviderAuth(agent)

	return agent
}

func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) (serveErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	agent := newAgentForServe(opts...)
	defer func() {
		if closeErr := agent.Close(); closeErr != nil {
			agent.log.DebugContext(context.Background(), "close OpenCode ACP agent failed")

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
	a.sessionLifecycleMu.Lock()
	defer a.sessionLifecycleMu.Unlock()

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
	sequencing := a.runtimeSequencing

	stickyRuntimeErr := a.runtimeFatalErr
	if !fatalRuntimeCleanup(stickyRuntimeErr) {
		stickyRuntimeErr = nil
	}

	a.closed = true
	a.mu.Unlock()

	err := stickyRuntimeErr

	// The ladder runs per logical session first and closes the shared native
	// tree exactly once afterwards, and the order is load-bearing rather than
	// tidy. Each session owes the same durable commit a wire `session/close`
	// owes, that commit reads the native scope through the loopback API, and
	// retiring the shared runtime first would destroy the material every
	// still-owed commit needs — an embedded shutdown would then drop state a
	// wire close would have committed, which is the same lost generation
	// however the process ended.
	for _, session := range sessions {
		ctx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
		err = errors.Join(err, session.CloseAndCommit(ctx))

		cancel()
	}

	a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))

	switch {
	case runtime != nil:
		err = errors.Join(err, a.retireSharedRuntime(generation, "shared OpenCode runtime retired while closing agent"))
	case sequencing != nil:
		ctx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
		err = errors.Join(err, a.retryRuntimeCleanup(ctx, sequencing))

		cancel()
	case waiting != nil:
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

	if a.options.hostAuthorityConfigured {
		ctx, cancel := context.WithTimeout(context.Background(), settlementTimeout)

		a.nativeAdmissionMu.Lock()
		err = errors.Join(err, a.retryRetiredNativeTrees(ctx))
		a.nativeAdmissionMu.Unlock()
		cancel()
	}

	a.interruptConnection()

	a.mu.Lock()
	a.conn = nil
	a.sessions = make(map[acp.SessionId]*session)
	a.closeErr = err

	close(closeDone)
	a.mu.Unlock()

	return err
}

func (a *Agent) interruptConnection() {
	if a == nil {
		return
	}

	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()

	if interrupter, ok := conn.(interface{ InterruptWrites() }); ok {
		interrupter.InterruptWrites()
	}
}

func (a *Agent) Initialize(_ context.Context, params acp.InitializeRequest) (acp.InitializeResponse, error) {
	if a.optionsErr != nil {
		return acp.InitializeResponse{}, a.optionsError()
	}

	lifecycleAnswer, err := a.negotiateLifecycle(params.Meta)
	if err != nil {
		return acp.InitializeResponse{}, err
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
			jsonFieldMethod:  ForkSessionMethod,
			jsonFieldRequest: "acp.UnstableForkSessionRequest JSON payload only",
			"response":       "acp.UnstableForkSessionResponse JSON payload only",
		},
		"elicitation": map[string]any{
			"unstable":     true,
			jsonFieldScope: string(lifecycle.CauseSession),
			"tracks":       "ACP v1 elicitation",
		},
		rawEventCapabilityKey: map[string]any{
			jsonFieldMethod:  RawEventMethod,
			"enabledBy":      rawEventEnabledByPath,
			"maxBytes":       rawEventMaxBytes,
			"defaultEnabled": false,
		},
		"sessionStore": map[string]any{
			"format":     SessionStoreFormat,
			jsonFieldKey: []string{jsonFieldSessionID, "subpath"},
		},
		structuredOutputMetaKey: map[string]any{
			"config":        outputSchemaOptionPath,
			"result":        structuredOutputPath,
			jsonFieldSchema: opencode.OutputFormatJSONSchema,
		},
	}

	if a.providerAuth != nil {
		opencodeMeta[providerAuthCapabilityKey] = a.providerAuth.capability()
	}

	capabilityMeta := map[string]any{
		opencodeMetaKey:  opencodeMeta,
		routeEnvelopeKey: map[string]any{metaFieldVersion: routeEnvelopeVersion},
		mediaEnvelopeKey: a.options.mediaEnvelope(),
	}

	// The handoff advertisement answers whether the host's read root reached
	// this adapter, so it is present only when one is configured.
	if a.options.InputHandoffRoot != "" {
		capabilityMeta[handoffEnvelopeKey] = map[string]any{metaFieldVersion: handoffEnvelopeVersion}
	}

	return acp.InitializeResponse{
		Meta:            lifecycleAnswerMeta(lifecycleAnswer),
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
	// This adapter advertises no auth method, but "the key is not read here" and
	// "there is no such method id" are different answers and a host that got the
	// reserved literal wrong is owed the first one.
	if refusal := refuseLifecycleMeta(params.Meta); refusal != nil {
		return acp.AuthenticateResponse{}, refusal
	}

	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
}

func (a *Agent) Logout(_ context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	if refusal := refuseLifecycleMeta(params.Meta); refusal != nil {
		return acp.LogoutResponse{}, refusal
	}

	return acp.LogoutResponse{}, nil
}

// SetSessionMode is not implemented by this adapter, but the family literal is
// refused before the method verdict is given. The reserved key has no meaning on
// any inbound surface the pinned SDK dispatches, and a surface that answers
// method-not-found without reading it would let a host stamp the key anywhere
// unimplemented and be told the key was fine. The refusal is the one every other
// inbound surface gives, so the verdict does not depend on which method carried
// the key.
func (a *Agent) SetSessionMode(_ context.Context, params acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	if refusal := refuseLifecycleMeta(params.Meta); refusal != nil {
		return acp.SetSessionModeResponse{}, refusal
	}

	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	// The reserved family literal is refused before the method is resolved. The
	// key belongs to the family on every inbound surface, so an extension name
	// this adapter does not implement is not a place a host may stamp it and be
	// told the key was fine: "the key is not read here" outranks "there is no
	// such method".
	if refusal := refuseLifecycleRawMeta(params); refusal != nil {
		return nil, refusal
	}

	switch method {
	case ForkSessionMethod:
		var req acp.UnstableForkSessionRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid request parameters"})
		}

		if err := req.Validate(); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: "request validation failed"})
		}

		return a.forkSession(ctx, req)
	default:
		if result, handled, err := a.handleAuthExtensionMethod(ctx, method, params); handled {
			return result, err
		}

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
		return a.optionsError()
	}

	return nil
}

// optionsError reports the construction-time option failure that every entry
// point must answer with. The code is internal error, not invalid params: the
// caller's params are fine, and what is broken is the agent the embedding host
// built, so blaming the request would send the caller chasing its own payload.
// The data carries only the joined prose because no wire field is at fault to
// name, and that text is all an operator has to find the bad option.
func (a *Agent) optionsError() error {
	return errors.Join(
		a.optionsErr,
		acp.NewInternalError(map[string]any{jsonFieldError: a.optionsErr.Error()}),
	)
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

// storeStartedSession installs a fully prepared session into the active set. The
// tombstone check is not once-at-entry: a delete that completed while this
// session was being prepared wins, however far the preparation got, so the
// marker is re-read under the very lock that installs — and never cleared as a
// side effect of installing. Clearing it would unhide an id the host has already
// been told is gone, and the caller tears the prepared replacement down on the
// refusal it gets back.
func (a *Agent) storeStartedSession(session *session) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
	}

	if _, deleted := a.deleted[session.id]; deleted {
		return acp.NewInvalidParams(map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
	}

	if a.runtime == nil {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueSharedRuntimeExited})
	}

	if session.runtimeGeneration != a.runtimeGeneration {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: "shared OpenCode runtime generation changed"})
	}

	if len(a.sessions) >= a.options.ConcurrencyLimits.MaxActiveSessions {
		return acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueBackpressure, jsonFieldLimit: "active_sessions"})
	}

	a.sessions[session.id] = session

	if a.providerAuth != nil {
		a.providerAuth.reopenSession(session.id)
	}

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

	return caps.Form != nil
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
