package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
)

type agentClient interface {
	Done() <-chan struct{}
	BeginCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest, elicitationScope, hostRequestKey) registeredElicitationRequest
	BeginRequestPermission(context.Context, acp.RequestPermissionRequest, hostRequestKey) registeredPermissionRequest
	CreateElicitation(context.Context, acp.UnstableCreateElicitationRequest, elicitationScope) (acp.UnstableCreateElicitationResponse, error)
	UnstableCreateElicitation(context.Context, acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error)
	RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
	SessionUpdate(context.Context, acp.SessionNotification) error
	NotifyExtension(context.Context, string, any) error
}

type elicitationScope struct {
	SessionID  acp.SessionId
	TurnNonce  string
	ToolCallID acp.ToolCallId
	RequestID  *acp.RequestId
}

type localAgentConnection struct {
	agent         *Agent
	conn          *acp.Connection
	initialized   atomic.Bool
	hooks         *establishmentHooks
	registrations *hostRequestRegistrations
	transport     *connectionTransport
}

type connectionTransport struct {
	output io.Writer
	input  io.Reader
	once   sync.Once
}

func newConnectionTransport(output io.Writer, input io.Reader) *connectionTransport {
	return &connectionTransport{output: output, input: input}
}

func (t *connectionTransport) Write(p []byte) (int, error) {
	return t.output.Write(p)
}

func (t *connectionTransport) interrupt() {
	if t == nil {
		return
	}

	t.once.Do(func() {
		if closer, ok := t.output.(io.Closer); ok {
			_ = closer.Close()
		}

		if closer, ok := t.input.(io.Closer); ok {
			_ = closer.Close()
		}
	})
}

type localAgentHandler func(context.Context, *Agent, json.RawMessage) (any, *acp.RequestError)

type localAgentParams[Req any] interface {
	*Req
	Validate() error
}

var (
	_ agentClient = (*localAgentConnection)(nil)

	localAgentHandlers = map[string]localAgentHandler{
		acp.AgentMethodAuthenticate:           localResponse((*Agent).Authenticate),
		acp.AgentMethodInitialize:             localResponse((*Agent).Initialize),
		acp.AgentMethodLogout:                 localResponse((*Agent).Logout),
		acp.AgentMethodSessionCancel:          localNotification((*Agent).Cancel),
		acp.AgentMethodSessionClose:           localResponse((*Agent).CloseSession),
		acp.AgentMethodSessionDelete:          localResponse((*Agent).UnstableDeleteSession),
		acp.AgentMethodSessionList:            localResponse((*Agent).ListSessions),
		acp.AgentMethodSessionLoad:            localResponse((*Agent).LoadSession),
		acp.AgentMethodSessionNew:             localResponse((*Agent).NewSession),
		acp.AgentMethodSessionPrompt:          localResponse((*Agent).Prompt),
		acp.AgentMethodSessionResume:          localResponse((*Agent).ResumeSession),
		acp.AgentMethodSessionSetConfigOption: localResponse((*Agent).SetSessionConfigOption),
		acp.AgentMethodSessionSetMode:         localResponse((*Agent).SetSessionMode),
	}
)

func newLocalAgentConnection(agent *Agent, output io.Writer, input io.Reader) *localAgentConnection {
	hooks := newEstablishmentHooks(agent.log)
	registrations := newHostRequestRegistrations()
	transport := newConnectionTransport(output, input)
	conn := &localAgentConnection{
		agent: agent, hooks: hooks, registrations: registrations, transport: transport,
	}
	inputGate := newConnectionInputGate(newEstablishmentTagReader(input))
	conn.conn = acp.NewConnection(conn.handle, registrations.wrap(hooks.wrap(transport)), inputGate)
	// The SDK sees malformed frames and transport failures before the adapter can
	// classify their fields. Keep that boundary closed: diagnostics above it are
	// structured and redacted by this package.
	conn.conn.SetLogger(slog.New(slog.DiscardHandler))
	inputGate.open()

	return conn
}

func (c *localAgentConnection) InterruptWrites() {
	if c != nil {
		c.transport.interrupt()
	}
}

type connectionInputGate struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

func newConnectionInputGate(reader io.Reader) *connectionInputGate {
	return &connectionInputGate{reader: reader, ready: make(chan struct{})}
}

func (g *connectionInputGate) open() {
	g.once.Do(func() { close(g.ready) })
}

func (g *connectionInputGate) Read(p []byte) (int, error) {
	<-g.ready

	return g.reader.Read(p)
}

func (c *localAgentConnection) Done() <-chan struct{} {
	return c.conn.Done()
}

func (c *localAgentConnection) handle(ctx context.Context, method string, params json.RawMessage) (result any, reqErr *acp.RequestError) {
	ctx, finish := c.agent.observe.StartACPRequest(ctx, method)

	defer func() {
		if reqErr != nil {
			finish(reqErr)
		} else {
			finish(nil)
		}
	}()

	if err := c.agent.ensureOpen(); err != nil {
		reqErr = requestError(ctx, err)

		return nil, reqErr
	}

	if method != acp.AgentMethodInitialize && !c.initialized.Load() {
		reqErr = acp.NewInvalidRequest(map[string]any{
			jsonFieldMethod: method,
			jsonFieldError:  "initialize must be called before other ACP methods",
		})

		return nil, reqErr
	}

	if strings.HasPrefix(method, "_") {
		extensionResult, err := c.agent.HandleExtensionMethod(ctx, method, params)
		if err != nil {
			c.agent.logHandlerFailure(ctx, method, err)
		}

		reqErr = requestError(ctx, err)
		if reqErr == nil {
			c.queueEstablishment(ctx, method, params, extensionResult)
		}

		return extensionResult, reqErr
	}

	handler, ok := localAgentHandlers[method]
	if !ok {
		reqErr = acp.NewMethodNotFound(method)

		return nil, reqErr
	}

	result, reqErr = handler(ctx, c.agent, params)
	if method == acp.AgentMethodInitialize && reqErr == nil {
		c.initialized.Store(true)
	}

	if reqErr == nil {
		c.queueEstablishment(ctx, method, params, result)
	}

	return result, reqErr
}

func localResponse[Req any, ReqPtr localAgentParams[Req], Resp any](
	call func(*Agent, context.Context, Req) (Resp, error),
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}

		resp, err := call(agent, ctx, value)
		if err != nil {
			agent.logHandlerFailure(ctx, "", err)

			return nil, requestError(ctx, err)
		}

		return resp, nil
	}
}

func localNotification[Req any, ReqPtr localAgentParams[Req]](
	call func(*Agent, context.Context, Req) error,
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}

		if err := call(agent, ctx, value); err != nil {
			agent.logHandlerFailure(ctx, "", err)

			return nil, requestError(ctx, err)
		}

		return nil, nil
	}
}

func decodeLocalAgentParams[Req any, ReqPtr localAgentParams[Req]](params json.RawMessage) (Req, *acp.RequestError) {
	var value Req
	if err := json.Unmarshal(params, &value); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: "invalid request parameters"})
	}

	if err := ReqPtr(&value).Validate(); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: "request validation failed"})
	}

	return value, nil
}

func (c *localAgentConnection) UnstableCreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, params, elicitationScope{})
}

func (c *localAgentConnection) CreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	raw, err := scopedElicitationParams(params, scope)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}
	defer release()

	return acp.SendRequest[acp.UnstableCreateElicitationResponse](c.conn, ctx, acp.ClientMethodElicitationCreate, raw)
}

func (c *localAgentConnection) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	defer release()

	return acp.SendRequest[acp.RequestPermissionResponse](c.conn, ctx, acp.ClientMethodSessionRequestPermission, params)
}

func (c *localAgentConnection) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	return c.conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, params)
}

func (c *localAgentConnection) NotifyExtension(ctx context.Context, method string, params any) error {
	if method == "" || !strings.HasPrefix(method, "_") {
		return fmt.Errorf("extension method name must start with '_' (got %q)", method)
	}

	release, err := c.agent.acquireClientCall(ctx)
	if err != nil {
		return err
	}
	defer release()

	return c.conn.SendNotification(ctx, method, params)
}

// logHandlerFailure records a handler error the ACP boundary is about to reduce
// to a generic internal error. The wire deliberately carries no detail, so the
// -debug stream is where an operator learns why a request failed. A request
// error already tells the host what went wrong and is not repeated here.
func (a *Agent) logHandlerFailure(ctx context.Context, method string, err error) {
	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return
	}

	a.log.DebugContext(ctx, "ACP handler failed",
		slog.String("method", method), slog.String("error", err.Error()))
}

// requestError decides cancellation from the request context rather than from
// the error, and decides it first. An honored $/cancel_request is the only
// thing that cancels a request context with cause context.Canceled: connection
// teardown cancels the parent with the transport cause, and an adapter deadline
// surfaces context.DeadlineExceeded, so neither is reported as cancelled. Any
// error the handler was carrying when the cancel landed is a casualty of the
// cancel and never the answer, while reading cancellation off the error instead
// would report -32800 for a request nobody cancelled.
func requestError(ctx context.Context, err error) *acp.RequestError {
	if err == nil {
		return nil
	}

	if context.Cause(ctx) == context.Canceled {
		return acp.NewRequestCancelled(map[string]any{jsonFieldError: "request cancelled"})
	}

	var reqErr *acp.RequestError
	if errors.As(err, &reqErr) {
		return reqErr
	}

	return acp.NewInternalError(map[string]any{jsonFieldError: "handler failed"})
}

func scopedElicitationParams(
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (json.RawMessage, error) {
	var payload map[string]any

	switch {
	case params.Form != nil:
		payload = map[string]any{
			jsonFieldMessage:  params.Form.Message,
			jsonFieldMode:     elicitationModeForm,
			"requestedSchema": params.Form.RequestedSchema,
		}
		if len(params.Form.Meta) > 0 {
			payload["_meta"] = params.Form.Meta
		}
	case params.Url != nil:
		payload = map[string]any{
			"elicitationId":  params.Url.ElicitationId,
			jsonFieldMessage: params.Url.Message,
			jsonFieldMode:    elicitationModeURL,
			jsonFieldURL:     params.Url.Url,
		}
		if len(params.Url.Meta) > 0 {
			payload["_meta"] = params.Url.Meta
		}
	default:
		return nil, errors.New("elicitation request must include form or url")
	}

	meta, _ := payload["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}

	if _, exists := meta[routeEnvelopeKey]; exists {
		return nil, fmt.Errorf("native elicitation metadata used reserved key %q", routeEnvelopeKey)
	}

	if scope.TurnNonce != "" {
		route, err := outboundRoute(scope)
		if err != nil {
			return nil, err
		}

		meta[routeEnvelopeKey] = route
	} else if _, correlated := meta[lifecycle.MetaKey]; !correlated {
		return nil, errors.New("out-of-prompt elicitation requires lifecycle correlation")
	}

	payload["_meta"] = meta

	return json.Marshal(payload)
}
