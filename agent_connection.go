package opencodeacp

import (
	"bytes"
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
	"github.com/savid/acp-go-opencode/internal/opencode"
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

// decodeLocalAgentParams reads one in-process request body. Params this adapter
// cannot decode, or that fail validation as a whole, take the uniform rejection
// naming the params body itself rather than a prose token with no field.
func decodeLocalAgentParams[Req any, ReqPtr localAgentParams[Req]](params json.RawMessage) (Req, *acp.RequestError) {
	var value Req

	meta := localRequestMeta(&value)

	var (
		retained any
		present  bool
	)
	if meta != nil {
		params, retained, present = lifecycle.PreserveRequestMeta(params)
	}

	params, owned := preserveRequestNumbers(params)
	if err := json.Unmarshal(params, &value); err != nil {
		return value, unsupportedRequest(jsonFieldParams)
	}

	if present {
		if *meta == nil {
			*meta = make(map[string]any)
		}

		(*meta)[lifecycle.MetaKey] = retained
	}

	owned.restore(&value)

	if err := ReqPtr(&value).Validate(); err != nil {
		return value, unsupportedRequest(jsonFieldParams)
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

	// A shared runtime that is gone and cannot be replaced is the one runtime
	// state a host can act on: no operation that needs a runtime can succeed
	// until the host recovers the identity. Two conditions reach it — a previous
	// incarnation whose process tree is still alive and un-containable, and a
	// directory scope whose MCP disconnect could not be proven, which quarantines
	// the generation for the same reason. A runtime that merely exited never
	// reaches here: the next explicit operation admits a replacement generation.
	if errors.Is(err, ErrContainmentIncomplete) || errors.Is(err, opencode.ErrMCPDisconnectUnproven) {
		return acp.NewInternalError(map[string]any{jsonFieldError: valRuntimeUnavailable})
	}

	// An error carrying no wire classification of its own is the only one whose
	// prose is unknown to this package, so it is the only one reduced to a bare
	// token. The token is closed and vendor-scoped rather than a sentence: a
	// host cannot act on prose, and every classified refusal above already
	// names itself. A native loopback call that failed at runtime or session
	// start adds the one closed class this adapter documents; its route, status,
	// and body stay off the wire.
	var startup *startupError
	if errors.As(err, &startup) {
		return acp.NewInternalError(map[string]any{
			jsonFieldError: valInternalFailure,
			jsonFieldClass: classNativeStartup,
		})
	}

	return acp.NewInternalError(map[string]any{jsonFieldError: valInternalFailure})
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

// localRequestMeta names the SDK request fields this dispatcher carries. The
// owned lifecycle value is restored here after SDK defaults and decoding.
func localRequestMeta(value any) *map[string]any { //nolint:gocritic // The SDK decoder replaces the map; retain the address of its field.
	switch request := value.(type) {
	case *acp.InitializeRequest:
		return &request.Meta
	case *acp.AuthenticateRequest:
		return &request.Meta
	case *acp.LogoutRequest:
		return &request.Meta
	case *acp.CancelNotification:
		return &request.Meta
	case *acp.CloseSessionRequest:
		return &request.Meta
	case *acp.UnstableDeleteSessionRequest:
		return &request.Meta
	case *acp.ListSessionsRequest:
		return &request.Meta
	case *acp.LoadSessionRequest:
		return &request.Meta
	case *acp.NewSessionRequest:
		return &request.Meta
	case *acp.PromptRequest:
		return &request.Meta
	case *acp.ResumeSessionRequest:
		return &request.Meta
	case *acp.SetSessionModeRequest:
		return &request.Meta
	default:
		return nil
	}
}

// retainedRequestNumbers keeps route and image-handoff values out of the SDK's
// float64 maps. Their existing validators still decide when they are relevant.
type retainedRequestNumbers struct {
	route        any
	routePresent bool
	handoffs     map[int]any
}

func preserveRequestNumbers(params json.RawMessage) (json.RawMessage, retainedRequestNumbers) {
	retained := retainedRequestNumbers{handoffs: make(map[int]any)}
	sanitized := rewriteRequestObject(params, func(key string, raw json.RawMessage) json.RawMessage {
		switch {
		case strings.EqualFold(key, "_meta"):
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				retained.route, retained.routePresent = nil, false
			}

			return rewriteRequestObject(raw, func(namespace string, value json.RawMessage) json.RawMessage {
				if namespace != routeEnvelopeKey {
					return value
				}

				retained.route, retained.routePresent = rawOwnedValue(value), true

				return json.RawMessage("null")
			})
		case strings.EqualFold(key, "prompt"):
			var blocks []json.RawMessage
			if json.Unmarshal(raw, &blocks) != nil {
				return raw
			}

			clear(retained.handoffs)

			for index, block := range blocks {
				var header struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(block, &header) != nil || header.Type != mediaTypeImage {
					continue
				}

				blocks[index] = rewriteRequestObject(block, func(member string, content json.RawMessage) json.RawMessage {
					if !strings.EqualFold(member, "_meta") {
						return content
					}

					if bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
						delete(retained.handoffs, index)
					}

					return rewriteRequestObject(content, func(namespace string, value json.RawMessage) json.RawMessage {
						if namespace != handoffEnvelopeKey {
							return value
						}

						retained.handoffs[index] = rawOwnedValue(value)

						return json.RawMessage("null")
					})
				})
			}

			encoded, _ := json.Marshal(blocks)

			return encoded
		default:
			return raw
		}
	})

	return sanitized, retained
}

func (r retainedRequestNumbers) restore(value any) {
	meta := localRequestMeta(value)
	if r.routePresent && meta != nil {
		if *meta == nil {
			*meta = make(map[string]any)
		}

		(*meta)[routeEnvelopeKey] = r.route
	}

	request, ok := value.(*acp.PromptRequest)
	if !ok {
		return
	}

	for index, handoff := range r.handoffs {
		if index < len(request.Prompt) && request.Prompt[index].Image != nil {
			request.Prompt[index].Image.Meta[handoffEnvelopeKey] = handoff
		}
	}
}

func rawOwnedValue(raw json.RawMessage) any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any

	_ = decoder.Decode(&value)

	return value
}

// rewriteRequestObject preserves SDK member order and foreign duplicate keys.
// It rewrites only values selected by the owning metadata decoder.
func rewriteRequestObject(raw json.RawMessage, rewrite func(string, json.RawMessage) json.RawMessage) json.RawMessage {
	if !json.Valid(raw) || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return raw
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	_, _ = decoder.Token()
	out := []byte{'{'}

	for decoder.More() {
		token, _ := decoder.Token()
		key, _ := token.(string)

		var value json.RawMessage

		_ = decoder.Decode(&value)

		if len(out) > 1 {
			out = append(out, ',')
		}

		name, _ := json.Marshal(key)
		out = append(out, name...)
		out = append(out, ':')
		out = append(out, rewrite(key, value)...)
	}

	return append(out, '}')
}
