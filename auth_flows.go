package opencodeacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// Closed flow states.
const (
	authStatePending       = "pending"
	authStateAuthenticated = "authenticated"
	authStateSaved         = "saved"
	authStateFailed        = "failed"
	authStateCancelled     = "cancelled"
	authStateExpired       = "expired"
)

// Closed flow reasons, legal only against the state each pairs with.
const (
	authReasonProviderRefused   = "provider_refused"
	authReasonNativeVeto        = "native_veto"
	authReasonTransport         = "transport"
	authReasonProcess           = "process"
	authReasonAcceptanceUnknown = "acceptance_unknown"
	authReasonHarvestFailed     = "harvest_failed"
	authReasonOwnerCancel       = "owner_cancel"
	authReasonSuperseded        = "superseded"
	authReasonSessionClosed     = "session_closed"
	authReasonDeadline          = "deadline"
)

// Closed interaction discriminator.
const (
	authInteractionWait     = "wait"
	authInteractionCallback = "callback"
	authInteractionSecret   = "secret"
)

const authCallbackInputCode = "code"

const (
	// authSafetyDeadline bounds a flow independently of the harness, which
	// supplies no expiry of its own on this surface.
	authSafetyDeadline = 15 * time.Minute
	// authPollFloor is the fastest cadence a status call may drive a native
	// read at, so consumer poll cadence never propagates into a provider.
	authPollFloor = 5 * time.Second
	// authSlowDownStep is added to the adapter's own interval when a native
	// read answers with a rate-limit refusal.
	authSlowDownStep = 5 * time.Second
	// authNativeCallTimeout bounds one non-blocking native auth call.
	authNativeCallTimeout = 30 * time.Second
)

var (
	authRandRead = rand.Read
	authNow      = time.Now
)

// authFlow is the session-scoped record of one login. The presentation it can
// replay lives here and nowhere else: it carries url, message, and userCode,
// which are code-bearing for the flow's life.
type authFlow struct {
	id                 string
	sessionID          acp.SessionId
	providerID         string
	connectionID       string
	revision           int64
	bindingGeneration  int64
	method             authCatalogMethod
	authorizeRequestID string
	inputs             map[string]string
	presentation       authAuthorizeResult

	createdAt           int64
	state               string
	reason              string
	expiresAt           time.Time
	credentialExpiresAt int64
	// presented reports whether the mint published this record's presentation.
	// A record that never reached one has no verbatim answer to replay.
	presented bool

	broker *authBroker

	nextProbeAt   time.Time
	probeInterval time.Duration

	disarm chan struct{}
}

type authAuthorizeResult struct {
	Interaction    string `json:"interaction"`
	URL            string `json:"url,omitempty"`
	Message        string `json:"message"`
	UserCode       string `json:"userCode,omitempty"`
	CallbackInput  string `json:"callbackInput,omitempty"`
	FlowID         string `json:"flowId"`
	FlowExpiresAt  int64  `json:"flowExpiresAt"`
	PollIntervalMs int64  `json:"pollIntervalMs,omitempty"`
}

type authFlowIDResult struct {
	FlowID string `json:"flowId"`
}

type authStatusResult struct {
	FlowID    string `json:"flowId"`
	State     string `json:"state"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func authTerminal(state string) bool {
	return state != authStatePending
}

// newAuthToken mints an opaque adapter-owned identifier from 16 CSPRNG bytes,
// encoded unpadded base64url. Native flow handles never cross the boundary.
func newAuthToken() (string, error) {
	var value [16]byte
	if _, err := authRandRead(value[:]); err != nil {
		return "", fmt.Errorf("create provider auth token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

// authorize starts exactly one flow per (sessionId, providerId). It records the
// idempotency key before any native mint and has persisted the flow's slot
// binding before it returns.
func (p *providerAuth) authorize(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params,
		authFieldSessionID, authFieldProviderID, authFieldConnectionID,
		authFieldMethodsGeneration, authFieldMethod, authFieldAuthorizeRequestID, authFieldInputs)
	if err != nil {
		return nil, err
	}

	request, err := decodeAuthorizeRequest(fields)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(request.sessionID)
	if err != nil {
		return nil, err
	}

	key := authFlowKey{sessionID: session.id, providerID: request.providerID}

	if replay, ok := p.replayAuthorize(key, request.authorizeRequestID); ok {
		return replay, nil
	}

	method, err := p.resolveMethod(request)
	if err != nil {
		return nil, err
	}

	if inputErr := validateAuthInputs(request.providerID, method, request.inputs); inputErr != nil {
		return nil, inputErr
	}

	flowID, err := newAuthToken()
	if err != nil {
		return nil, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

	p.supersede(key, authReasonSuperseded)

	now := authNow()
	record := authLedgerRecord{
		ProviderID:         request.providerID,
		ConnectionID:       request.connectionID,
		Revision:           1,
		BindingGeneration:  1,
		FlowID:             flowID,
		AuthorizeRequestID: request.authorizeRequestID,
		State:              authLedgerIntent,
		CreatedAt:          now.UnixMilli(),
		UpdatedAt:          now.UnixMilli(),
	}

	if prior, ok, readErr := p.ledger.read(request.providerID); readErr == nil && ok {
		record.Revision = prior.Revision + 1
		record.BindingGeneration = prior.BindingGeneration
		record.CreatedAt = prior.CreatedAt
	}

	if writeErr := p.ledger.write(record); writeErr != nil {
		return nil, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

	flow := &authFlow{
		id:                 flowID,
		sessionID:          session.id,
		providerID:         request.providerID,
		connectionID:       request.connectionID,
		revision:           record.Revision,
		bindingGeneration:  record.BindingGeneration,
		method:             method,
		authorizeRequestID: request.authorizeRequestID,
		inputs:             request.inputs,
		createdAt:          record.CreatedAt,
		state:              authStatePending,
		expiresAt:          now.Add(authSafetyDeadline),
		probeInterval:      authPollFloor,
		disarm:             make(chan struct{}),
	}

	// The flow is registered against the ledger entry that already names it, so
	// the flowId every later answer carries — a mint failure's included —
	// addresses a real record.
	p.mu.Lock()
	p.flows[key] = flow
	p.byID[flowID] = flow
	p.mu.Unlock()

	presentation, cause := p.mintPresentation(ctx, flow)
	if cause != "" {
		return nil, p.fail(flow, cause, false)
	}

	p.mu.Lock()
	flow.presentation = presentation
	flow.presented = true
	p.mu.Unlock()

	p.armCompleter(flow)

	return presentation, nil
}

type authorizeRequest struct {
	sessionID    string
	providerID   string
	connectionID string
	generation   string
	method       string
	// authorizeRequestID is the caller-minted idempotency key. authorize is the
	// only leg that takes one because it is the most destructive leg here.
	authorizeRequestID string
	inputs             map[string]string
}

func decodeAuthorizeRequest(fields map[string]json.RawMessage) (authorizeRequest, error) {
	request := authorizeRequest{}

	var err error
	if request.sessionID, err = authRequiredString(fields, authFieldSessionID); err != nil {
		return request, err
	}

	if request.providerID, err = authRequiredString(fields, authFieldProviderID); err != nil {
		return request, err
	}

	if request.connectionID, err = authRequiredString(fields, authFieldConnectionID); err != nil {
		return request, err
	}

	if request.generation, err = authRequiredString(fields, authFieldMethodsGeneration); err != nil {
		return request, err
	}

	if request.method, err = authRequiredString(fields, authFieldMethod); err != nil {
		return request, err
	}

	if request.authorizeRequestID, err = authRequiredString(fields, authFieldAuthorizeRequestID); err != nil {
		return request, err
	}

	if raw, ok := fields[authFieldInputs]; ok {
		if err := json.Unmarshal(raw, &request.inputs); err != nil {
			return request, invalidAuthField(authFieldInputs)
		}
	}

	return request, nil
}

// replayAuthorize answers a repeated idempotency key verbatim from memory: no
// supersede, no completer disarm, no destruction of flow or broker state, and
// no native call. The record it answers from survives every terminal
// transition and is dropped only when the session closes, so a repeat after
// completion returns what the first call returned instead of driving a second
// login. A record whose mint never published a presentation has nothing to
// replay.
func (p *providerAuth) replayAuthorize(key authFlowKey, requestID string) (authAuthorizeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.flows[key]
	if !ok || !flow.presented || flow.authorizeRequestID != requestID {
		return authAuthorizeResult{}, false
	}

	return flow.presentation, true
}

// resolveMethod fences a method id against the generation that produced it. A
// native id is an array index, so it means nothing against a later catalog.
func (p *providerAuth) resolveMethod(request authorizeRequest) (authCatalogMethod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.generation == "" || p.generation != request.generation {
		return authCatalogMethod{}, invalidAuthField(authFieldMethodsGeneration)
	}

	for _, method := range p.catalog[request.providerID] {
		if method.ID == request.method {
			return method, nil
		}
	}

	return authCatalogMethod{}, invalidAuthField(authFieldMethod)
}

// mintPresentation performs the native mint for an oauth method and builds the
// wire presentation. An api method has nothing to mint: its value is submitted
// through callback and applied natively there. A non-empty cause is the leg's
// failure, and the flow it names owns the transition and the broker teardown.
func (p *providerAuth) mintPresentation(ctx context.Context, flow *authFlow) (authAuthorizeResult, string) {
	result := authAuthorizeResult{
		FlowID:        flow.id,
		FlowExpiresAt: flow.expiresAt.UnixMilli(),
		Message:       flow.method.Label,
	}

	if flow.method.Type == authMethodTypeAPI {
		result.Interaction = authInteractionSecret

		return result, ""
	}

	broker, err := p.startBroker(ctx)
	if err != nil {
		return authAuthorizeResult{}, authCauseProcess
	}

	p.mu.Lock()
	flow.broker = broker
	p.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	authorization, err := broker.client.ProviderAuthorize(callCtx, flow.providerID, flow.method.Index, flow.inputs)
	if err != nil {
		return authAuthorizeResult{}, authNativeCause(err)
	}

	if authLoopbackHost(authorization.URL) {
		return authAuthorizeResult{}, authCauseUnsupportedVariant
	}

	authorizeURL, ok := authDisplayURL(authorization.URL)
	if !ok {
		return authAuthorizeResult{}, authCauseNativeVeto
	}

	message, ok := authDisplayText(authorization.Instructions, authMaxMessageBytes)
	if !ok {
		return authAuthorizeResult{}, authCauseNativeVeto
	}

	result.URL = authorizeURL
	result.Message = message

	if code, ok := authUserCodeFromURL(authorizeURL); ok {
		result.UserCode = code
	}

	if authorization.Method == opencode.ProviderAuthNativeMethodCode {
		result.Interaction = authInteractionCallback
		result.CallbackInput = authCallbackInputCode
	} else {
		result.Interaction = authInteractionWait
	}

	return result, ""
}

// authUserCodeFromURL reads the code out of the one machine-readable place the
// harness puts it. It is never parsed out of the human instructions, and the
// whole url stays code-bearing for the flow's life because the code is inside
// it.
func authUserCodeFromURL(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}

	value := parsed.Query().Get("user_code")
	if value == "" {
		return "", false
	}

	return authDisplayUserCode(value)
}

// armCompleter bounds the flow by its effective deadline. It is armed exactly
// once, at authorize, and status never starts, extends, or rearms it.
func (p *providerAuth) armCompleter(flow *authFlow) {
	deadline := time.Until(flow.expiresAt)
	disarm := flow.disarm

	p.goSafe("provider auth completer", func() {
		timer := time.NewTimer(deadline)
		defer timer.Stop()

		select {
		case <-disarm:
			return
		case <-timer.C:
			p.expire(flow)
		}
	})
}

func (p *providerAuth) expire(flow *authFlow) {
	p.mu.Lock()

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return
	}

	flow.state = authStateExpired
	flow.reason = authReasonDeadline
	broker := flow.takeBroker()

	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	broker.destroy(ctx)
}

// supersede terminalizes the flow a new authorize replaces. The abandoned
// broker home is destroyed without installing anything from it, so a stale
// native approval completes into a store that no longer exists. A flow that
// already terminalized was not superseded by anything and keeps both its record
// and its id.
func (p *providerAuth) supersede(key authFlowKey, reason string) {
	p.mu.Lock()

	flow, ok := p.flows[key]
	if !ok || authTerminal(flow.state) {
		p.mu.Unlock()

		return
	}

	delete(p.flows, key)
	delete(p.byID, flow.id)

	flow.state = authStateCancelled
	flow.reason = reason
	broker := flow.takeBroker()

	flow.stopCompleter()
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	broker.destroy(ctx)
}

func (f *authFlow) stopCompleter() {
	select {
	case <-f.disarm:
	default:
		close(f.disarm)
	}
}

func (f *authFlow) takeBroker() *authBroker {
	broker := f.broker
	f.broker = nil

	return broker
}

func (f *authFlow) destroyBroker(ctx context.Context) {
	f.takeBroker().destroy(ctx)
}

// callback submits the flow's expected value. For an oauth flow it is the
// completion driver: the native endpoint blocks until the provider settles,
// after which the completed credential is read out of the broker home's own
// store and installed once into the durable runtime store.
func (p *providerAuth) callback(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldMethod, authFieldFlowID, authFieldInput)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	method, err := authRequiredString(fields, authFieldMethod)
	if err != nil {
		return nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	input, err := authString(fields, authFieldInput)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	flow, err := p.addressFlow(session.id, providerID, flowID)
	if err != nil {
		return nil, err
	}

	if flow.method.ID != method {
		return nil, invalidAuthField(authFieldMethod)
	}

	if authTerminal(flow.state) {
		return nil, authFailed(authCauseFlowState, providerID, method, flowID)
	}

	if flow.method.Type == authMethodTypeAPI {
		return p.applySecret(ctx, session, flow, input)
	}

	return p.completeOAuth(ctx, session, flow, input)
}

// applySecret writes an operator-supplied key into the durable store. No
// harness validates a secret at write time, so the flow reaches saved rather
// than authenticated.
func (p *providerAuth) applySecret(ctx context.Context, session *session, flow *authFlow, input string) (any, error) {
	if input == "" || len(input) > authMaxTextInputBytes {
		return nil, invalidAuthField(authFieldInput)
	}

	credential := opencode.ProviderAuthCredential{
		Type:     opencode.ProviderAuthTypeAPI,
		Key:      input,
		Metadata: flow.inputs,
	}

	if err := p.install(ctx, session, flow, credential); err != nil {
		return nil, err
	}

	p.terminalize(flow, authStateSaved, "", 0)

	return authFlowIDResult{FlowID: flow.id}, nil
}

func (p *providerAuth) completeOAuth(ctx context.Context, session *session, flow *authFlow, input string) (any, error) {
	if flow.presentation.CallbackInput == "" && input != "" {
		return nil, invalidAuthField(authFieldInput)
	}

	if flow.presentation.CallbackInput != "" && input == "" {
		return nil, invalidAuthField(authFieldInput)
	}

	if len(input) > authMaxTextInputBytes {
		return nil, invalidAuthField(authFieldInput)
	}

	broker := flow.broker
	if broker == nil {
		return nil, p.fail(flow, authCauseFlowState, false)
	}

	if err := broker.client.ProviderAuthCallback(ctx, flow.providerID, flow.method.Index, input); err != nil {
		return nil, p.fail(flow, authNativeCause(err), input != "")
	}

	credential, ok, err := broker.client.StoredProviderAuth(ctx, flow.providerID)
	if err != nil || !ok {
		return nil, p.fail(flow, authCauseHarvestFailed, false)
	}

	if err := p.install(ctx, session, flow, credential); err != nil {
		return nil, err
	}

	p.terminalize(flow, authStateAuthenticated, "", credential.Expires)

	destroyCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	flow.destroyBroker(destroyCtx)

	return authFlowIDResult{FlowID: flow.id}, nil
}

// install performs the single fenced write into the durable runtime store and
// publishes it without a restart, then records the post-mutation confirmation.
func (p *providerAuth) install(ctx context.Context, session *session, flow *authFlow, credential opencode.ProviderAuthCredential) error {
	client := session.nativeClient()
	if client == nil {
		return p.fail(flow, authCauseTransport, true)
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	if err := client.SetProviderAuth(callCtx, flow.providerID, credential); err != nil {
		return p.fail(flow, authNativeCause(err), true)
	}

	if err := client.DisposeInstance(callCtx); err != nil {
		return p.fail(flow, authNativeCause(err), true)
	}

	now := authNow().UnixMilli()

	record := authLedgerRecord{
		ProviderID:         flow.providerID,
		ConnectionID:       flow.connectionID,
		Revision:           flow.revision,
		BindingGeneration:  flow.bindingGeneration,
		FlowID:             flow.id,
		AuthorizeRequestID: flow.authorizeRequestID,
		State:              authLedgerConfirmed,
		CreatedAt:          flow.createdAt,
		UpdatedAt:          now,
	}

	if err := p.ledger.write(record); err != nil {
		return p.fail(flow, authCauseProcess, true)
	}

	return nil
}

// fail returns the leg's closed error and performs the transition its cause
// pairs with. A cause with no transition consumes nothing.
func (p *providerAuth) fail(flow *authFlow, cause string, materialInFlight bool) error {
	if state, reason := authFlowTransition(cause, materialInFlight); state != "" {
		p.terminalize(flow, state, reason, 0)

		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		flow.destroyBroker(ctx)
	}

	return authFailed(cause, flow.providerID, flow.method.ID, flow.id)
}

func (p *providerAuth) terminalize(flow *authFlow, state string, reason string, credentialExpiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow.state = state
	flow.reason = reason
	flow.credentialExpiresAt = credentialExpiresAt

	flow.stopCompleter()
}

// addressFlow resolves a flowId a caller supplied. A missing, unknown,
// superseded, or cross-session id is a caller addressing failure and never a
// flow failure.
func (p *providerAuth) addressFlow(sessionID acp.SessionId, providerID string, flowID string) (*authFlow, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.byID[flowID]
	if !ok || flow.sessionID != sessionID || flow.providerID != providerID {
		return nil, invalidAuthField(authFieldFlowID)
	}

	return flow, nil
}

// status reports the flow, not the connection. Its expiresAt is credential
// expiry and never flow expiry.
func (p *providerAuth) status(ctx context.Context, params json.RawMessage) (any, error) {
	flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.probe(ctx, flow)

	p.mu.Lock()
	defer p.mu.Unlock()

	result := authStatusResult{FlowID: flow.id, State: flow.state, Reason: flow.reason}
	if flow.state == authStateAuthenticated {
		result.ExpiresAt = flow.credentialExpiresAt
	}

	return result, nil
}

// probe refreshes a pending flow from the native store behind the adapter's own
// interval floor, serving the cached state in between so a consumer's poll
// cadence never reaches the provider. A native rate-limit refusal adds five
// seconds to the interval, reported on the next call through the slower cadence
// it produces.
func (p *providerAuth) probe(ctx context.Context, flow *authFlow) {
	p.mu.Lock()

	now := authNow()
	if authTerminal(flow.state) || flow.broker == nil || now.Before(flow.nextProbeAt) {
		p.mu.Unlock()

		return
	}

	flow.nextProbeAt = now.Add(flow.probeInterval)
	broker := flow.broker
	p.mu.Unlock()

	credential, ok, err := broker.client.StoredProviderAuth(ctx, flow.providerID)
	if err != nil {
		if opencode.IsRateLimited(err) {
			p.mu.Lock()
			flow.probeInterval += authSlowDownStep
			flow.nextProbeAt = now.Add(flow.probeInterval)
			p.mu.Unlock()
		}

		return
	}

	if !ok {
		return
	}

	session, err := p.agent.session(flow.sessionID)
	if err != nil {
		return
	}

	if err := p.install(ctx, session, flow, credential); err != nil {
		return
	}

	p.terminalize(flow, authStateAuthenticated, "", credential.Expires)

	destroyCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	flow.destroyBroker(destroyCtx)
}

// cancel is adapter-owned: OpenCode has no native cancel route, so the leg does
// everything the adapter owns and claims nothing about the provider. An issued
// device code stays valid there until it expires.
func (p *providerAuth) cancel(ctx context.Context, params json.RawMessage) (any, error) {
	flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return authFlowIDResult{FlowID: flow.id}, nil
	}

	flow.state = authStateCancelled
	flow.reason = authReasonOwnerCancel
	broker := flow.takeBroker()

	flow.stopCompleter()
	p.mu.Unlock()

	broker.destroy(ctx)

	return authFlowIDResult{FlowID: flow.id}, nil
}

func (p *providerAuth) addressedFlowLeg(params json.RawMessage) (*authFlow, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	flowID, err := authRequiredString(fields, authFieldFlowID)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	return p.addressFlow(session.id, providerID, flowID)
}

// disconnect bumps the binding generation before it touches anything else, then
// removes only the exactly-fenced slot and verifies absence. It never removes a
// differently fenced entry and promises no provider-side revocation.
func (p *providerAuth) disconnect(ctx context.Context, params json.RawMessage) (any, error) {
	fields, err := authParamFields(params, authFieldSessionID, authFieldProviderID, authFieldConnectionID, authFieldBindingGeneration)
	if err != nil {
		return nil, err
	}

	sessionID, err := authRequiredString(fields, authFieldSessionID)
	if err != nil {
		return nil, err
	}

	providerID, err := authRequiredString(fields, authFieldProviderID)
	if err != nil {
		return nil, err
	}

	connectionID, err := authRequiredString(fields, authFieldConnectionID)
	if err != nil {
		return nil, err
	}

	bindingGeneration, err := authRequiredInt64(fields, authFieldBindingGeneration)
	if err != nil {
		return nil, err
	}

	session, err := p.authSession(sessionID)
	if err != nil {
		return nil, err
	}

	record, ok, err := p.ledger.read(providerID)
	if err != nil {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	if !ok || record.ConnectionID != connectionID || record.BindingGeneration != bindingGeneration {
		return nil, authFailed(authCauseBindingConflict, providerID, "", "")
	}

	record.BindingGeneration++
	record.UpdatedAt = authNow().UnixMilli()
	record.State = authLedgerIntent

	if err := p.ledger.write(record); err != nil {
		return nil, authFailed(authCauseProcess, providerID, "", "")
	}

	client := session.nativeClient()
	if client == nil {
		return nil, authFailed(authCauseTransport, providerID, "", "")
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	if err := client.RemoveProviderAuth(callCtx, providerID); err != nil {
		return nil, authFailed(authNativeCause(err), providerID, "", "")
	}

	if _, present, err := client.StoredProviderAuth(callCtx, providerID); err != nil || present {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	record.State = authLedgerRemoved
	record.UpdatedAt = authNow().UnixMilli()

	if err := p.ledger.write(record); err != nil {
		return nil, authFailed(authCauseProcess, providerID, "", "")
	}

	return struct{}{}, nil
}

// closeSession cancels every pending flow the session owns, terminalizing each
// as cancelled/session_closed and destroying its broker home, and drops every
// record the session could still replay an idempotency key from. It runs before
// the native interrupt, so a flow is never abandoned to a process already being
// torn down.
func (p *providerAuth) closeSession(ctx context.Context, sessionID acp.SessionId) {
	p.mu.Lock()

	brokers := make([]*authBroker, 0, len(p.flows))

	for key, flow := range p.flows {
		if key.sessionID != sessionID {
			continue
		}

		delete(p.flows, key)
		delete(p.byID, flow.id)

		if authTerminal(flow.state) {
			continue
		}

		flow.state = authStateCancelled
		flow.reason = authReasonSessionClosed

		flow.stopCompleter()
		brokers = append(brokers, flow.takeBroker())
	}

	p.mu.Unlock()

	for _, broker := range brokers {
		broker.destroy(ctx)
	}
}

// authNativeCause classifies a native failure without forwarding any of its
// text. A native message can carry an entire upstream HTTP response, headers
// included.
func authNativeCause(err error) string {
	switch {
	case opencode.IsBadRequest(err):
		return authCauseProviderRefused
	default:
		return authCauseTransport
	}
}
