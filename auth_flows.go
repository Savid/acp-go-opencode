package opencodeacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	// claimed is held by the one leg driving this flow's native completion.
	claimed bool

	// mintErr records why the native mint never produced a presentation, so a
	// repeated idempotency key is answered with the same failure rather than
	// driving a second native login.
	mintErr error

	broker *authBroker

	completionDone chan struct{}
	disarm         chan struct{}
}

type authAuthorizeResult struct {
	Interaction   string `json:"interaction"`
	URL           string `json:"url,omitempty"`
	Message       string `json:"message"`
	UserCode      string `json:"userCode,omitempty"`
	CallbackInput string `json:"callbackInput,omitempty"`
	FlowID        string `json:"flowId"`
	FlowExpiresAt int64  `json:"flowExpiresAt"`
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

	// The gate is held to the end of the mint, so everything below — the replay
	// check, the retired check, the supersede, the ledger intent, the
	// publication, and the native mint itself — settles as one step against
	// another authorize for the same key. Two requests that only compared
	// records before publishing would both miss and both mint, and the loser
	// would then be cancelled by the winner's supersede after the operator had
	// already been shown its code.
	release, admitted := p.admissions.admit(ctx, key)
	if !admitted {
		return nil, authFailed(authCauseTimeout, request.providerID, request.method, "")
	}

	defer release()

	replay, replayed, err := p.replayAuthorize(key, request.authorizeRequestID)
	if replayed {
		if err != nil {
			return nil, err
		}

		return replay, nil
	}

	if p.requestRetired(key, request.authorizeRequestID) {
		return nil, invalidAuthField(authFieldAuthorizeRequestID)
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

	if err := p.supersede(key, authReasonSuperseded); err != nil {
		return nil, authFailed(authCauseProcess, request.providerID, request.method, "")
	}

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

	record, cause := p.mintLedgerIntent(ctx, record)
	if cause != "" {
		return nil, authFailed(cause, request.providerID, request.method, "")
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
		disarm:             make(chan struct{}),
	}

	// The flow is registered against the ledger entry that already names it, so
	// the flowId every later answer carries — a mint failure's included —
	// addresses a real record. Nothing native has been started yet, so a
	// publication the session refuses leaves no process and no directory behind.
	if err := p.publishFlow(key, flow); err != nil {
		return nil, err
	}

	presentation, cause := p.mintPresentation(ctx, flow)
	if cause != "" {
		return nil, p.failMint(flow, cause)
	}

	p.mu.Lock()
	flow.presentation = presentation
	p.mu.Unlock()

	p.armCompleter(flow)

	if presentation.Interaction == authInteractionWait {
		p.driveWaitCompletion(flow, session)
	}

	return presentation, nil
}

// mintLedgerIntent claims the provider's next revision and persists the intent
// as one step, under the credential-slot gate authorize takes after the key
// gate and never the other way round. The claim is a read-modify-write of the
// same entry disconnect and install rewrite, so ungated it both loses and
// destroys: the generation a disconnect bumped in between is read back stale
// and overwritten by the revision this write carries, and the copy a disconnect
// read before this record existed is written back over it, leaving the login
// bound to a revision no entry names and unable to ever confirm.
func (p *providerAuth) mintLedgerIntent(ctx context.Context, record authLedgerRecord) (authLedgerRecord, string) {
	release, admitted := p.slots.admit(ctx, record.ProviderID)
	if !admitted {
		return record, authCauseTimeout
	}

	defer release()

	if prior, ok, err := p.ledger.read(record.ProviderID); err == nil && ok {
		record.Revision = prior.Revision + 1
		record.BindingGeneration = prior.BindingGeneration
		record.CreatedAt = prior.CreatedAt
	}

	if err := p.ledger.write(record); err != nil {
		return record, authCauseProcess
	}

	return record, ""
}

// failMint terminalizes a flow whose native mint never produced a presentation
// and records the failure the idempotency key replays. A repeat that re-drove
// the native login would mint a second flow at the provider and answer with a
// different cause and a different retryability than the call it repeats.
func (p *providerAuth) failMint(flow *authFlow, cause string) error {
	err := p.fail(flow, cause, false)

	p.mu.Lock()
	flow.mintErr = err
	p.mu.Unlock()

	return err
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

	if request.connectionID, err = authRequiredConnectionID(fields); err != nil {
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
// login. A record whose mint failed replays that failure for the same reason:
// a second native login would answer a different flowId under a different
// cause, and a repeat is never allowed to change the answer. Its caller holds
// the key's admission gate, so the record it finds has always settled.
func (p *providerAuth) replayAuthorize(key authFlowKey, requestID string) (authAuthorizeResult, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	flow, ok := p.flows[key]
	if !ok || flow.authorizeRequestID != requestID {
		return authAuthorizeResult{}, false, nil
	}

	if flow.mintErr != nil {
		return authAuthorizeResult{}, true, flow.mintErr
	}

	return flow.presentation, true, nil
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

	broker, err := p.startBroker(ctx, flow.sessionID)
	if err != nil {
		return authAuthorizeResult{}, authCauseProcess
	}

	p.mu.Lock()
	if authTerminal(flow.state) || !p.sessionAdmitted(flow.sessionID) {
		p.mu.Unlock()
		_ = p.retireBroker(ctx, broker)

		return authAuthorizeResult{}, authCauseFlowCancelled
	}

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

func (p *providerAuth) driveWaitCompletion(flow *authFlow, session *session) {
	done := make(chan struct{})

	p.mu.Lock()
	if err := p.claimFlowLocked(flow); err != nil {
		p.mu.Unlock()

		return
	}

	flow.completionDone = done
	p.mu.Unlock()

	p.goSafe("provider auth wait completion", func() {
		defer close(done)
		defer p.releaseFlow(flow)

		ctx, cancel := context.WithDeadline(context.Background(), flow.expiresAt.Add(closeTimeout))
		defer cancel()

		_, _ = p.completeOAuth(ctx, session, flow, "")
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

	_ = p.retireBroker(ctx, broker)
	p.waitCompletion(ctx, flow)
}

// supersede terminalizes the flow a new authorize replaces and retires the
// request id that named it, because from here on only the replacing record can
// be replayed. The abandoned broker home is destroyed without installing
// anything from it, so a stale native approval completes into a store that no
// longer exists. A flow that already terminalized was not superseded by
// anything and keeps both its record and its id.
func (p *providerAuth) supersede(key authFlowKey, reason string) error {
	p.mu.Lock()

	flow, ok := p.flows[key]
	if !ok {
		p.mu.Unlock()

		return nil
	}

	p.retire(key, flow.authorizeRequestID)

	if authTerminal(flow.state) {
		p.mu.Unlock()

		return nil
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

	err := p.retireBroker(ctx, broker)
	p.waitCompletion(ctx, flow)

	return err
}

func (f *authFlow) stopCompleter() {
	select {
	case <-f.disarm:
	default:
		close(f.disarm)
	}
}

// takeBroker claims the flow's broker home for destruction. Callers hold p.mu:
// the claim is what decides which leg destroys, so a read that is not
// serialized with the matching write lets two legs both see the same live
// handle and close one process, remove one directory, and unlink one shim
// twice.
func (f *authFlow) takeBroker() *authBroker {
	broker := f.broker
	f.broker = nil

	return broker
}

// destroyBroker claims the flow's broker under p.mu and destroys it outside the
// lock, because destruction terminates a process and walks a directory tree.
func (p *providerAuth) destroyBroker(ctx context.Context, flow *authFlow) error {
	p.mu.Lock()
	broker := flow.takeBroker()
	p.mu.Unlock()

	return p.retireBroker(ctx, broker)
}

func (p *providerAuth) waitCompletion(ctx context.Context, flow *authFlow) {
	p.mu.Lock()
	done := flow.completionDone
	p.mu.Unlock()

	if done == nil {
		return
	}

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// callback submits the operator-supplied value for a code or API-key flow.
// Wait flows drive their no-code native callback as soon as authorize returns.
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

	if flow.method.Type != authMethodTypeAPI {
		if err := validateOAuthCallbackInput(flow, input); err != nil {
			return nil, err
		}
	}

	if err := p.claimFlow(flow); err != nil {
		return nil, err
	}

	defer p.releaseFlow(flow)

	if flow.method.Type == authMethodTypeAPI {
		return p.applySecret(ctx, session, flow, input)
	}

	return p.completeOAuth(ctx, session, flow, input)
}

// applySecret writes an operator-supplied key into the durable store. No
// harness validates a secret at write time, so the flow reaches saved rather
// than authenticated.
//
// The native apply blocks like the oauth leg's native callback does, so the
// flow can reach a terminal state underneath it here too. A write that failed
// is answered for that closed record by install itself; a write that landed is
// not, because the value is resident and a no-transition cause over a value
// the store now holds would hide it. Either way the terminal record belongs to
// whoever closed the flow first, and the resident credential is what inventory
// reports.
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
	if err := validateOAuthCallbackInput(flow, input); err != nil {
		return nil, err
	}

	p.mu.Lock()
	broker := flow.broker
	p.mu.Unlock()

	if broker == nil {
		return nil, p.fail(flow, authCauseFlowState, false)
	}

	if err := broker.client.ProviderAuthCallback(ctx, flow.providerID, flow.method.Index, input); err != nil {
		if cause, abandoned := p.abandonedCause(flow); abandoned {
			return nil, authFailed(cause, flow.providerID, flow.method.ID, flow.id)
		}

		return nil, p.fail(flow, authNativeCause(err), input != "")
	}

	if cause, abandoned := p.abandonedCause(flow); abandoned {
		return nil, authFailed(cause, flow.providerID, flow.method.ID, flow.id)
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

	if err := p.destroyBroker(destroyCtx, flow); err != nil {
		return nil, authFailed(authCauseProcess, flow.providerID, flow.method.ID, flow.id)
	}

	return authFlowIDResult{FlowID: flow.id}, nil
}

func validateOAuthCallbackInput(flow *authFlow, input string) error {
	if flow.presentation.CallbackInput == "" && input != "" {
		return invalidAuthField(authFieldInput)
	}

	if flow.presentation.CallbackInput != "" && input == "" {
		return invalidAuthField(authFieldInput)
	}

	if len(input) > authMaxTextInputBytes {
		return invalidAuthField(authFieldInput)
	}

	return nil
}

// install performs the single fenced write into the durable runtime store and
// publishes it without a restart, then records the post-mutation confirmation.
// It holds the provider's credential-slot gate across the whole sequence,
// because disconnect rewrites the same slot and the two must not interleave.
func (p *providerAuth) install(ctx context.Context, session *session, flow *authFlow, credential opencode.ProviderAuthCredential) error {
	client := session.nativeClient()
	if client == nil {
		return p.failInstall(flow, authCauseTransport)
	}

	release, admitted := p.slots.admit(ctx, flow.providerID)
	if !admitted {
		return p.fail(flow, authCauseTimeout, false)
	}

	defer release()

	record := authLedgerRecord{
		ProviderID:         flow.providerID,
		ConnectionID:       flow.connectionID,
		Revision:           flow.revision,
		BindingGeneration:  flow.bindingGeneration,
		FlowID:             flow.id,
		AuthorizeRequestID: flow.authorizeRequestID,
		State:              authLedgerConfirmed,
		CreatedAt:          flow.createdAt,
	}

	if cause := p.staleLineage(record); cause != "" {
		return p.failInstall(flow, cause)
	}

	callCtx, cancel := context.WithTimeout(ctx, authNativeCallTimeout)
	defer cancel()

	finishQuotaMutation := p.agent.beginQuotaAuthMutation()
	defer finishQuotaMutation()

	if err := client.SetProviderAuth(callCtx, flow.providerID, credential); err != nil {
		return p.failInstall(flow, authNativeCause(err))
	}

	if err := client.DisposeInstance(callCtx); err != nil {
		return p.failInstall(flow, authNativeCause(err))
	}

	record.UpdatedAt = authNow().UnixMilli()

	// Every writer of this entry — a fresh authorize's intent, a disconnect's
	// bump, and this confirmation — holds the credential-slot gate this leg took
	// above, so the lineage read before the native write is still the stored
	// lineage here. The write needs no compare of its own.
	if err := p.ledger.write(record); err != nil {
		return p.failInstall(flow, authCauseProcess)
	}

	return nil
}

// staleLineage reports the cause install must answer with before it writes,
// and the empty string when the entry still names this binding. The check runs
// ahead of the native write and not only after it: a disconnect that already
// bumped the generation removed this provider's credential and verified the
// slot empty, so writing would refill the slot it verified — and the
// confirmation the ledger then correctly refuses leaves the entry reading
// removed while the credential is resident, which makes it live and invisible
// on every residence answer this surface has.
//
// One read ahead of the write is the whole check only because of the
// credential-slot gate — p.slots, keyed by provider id — which install holds
// from before this read through the confirmation write, and which every other
// writer of the entry takes too: authorize's intent in mintLedgerIntent, and
// disconnect's generation bump and its removed-write. Shorten that hold and the
// entry can move between this read and the write again, and the write needs a
// compare of its own back.
func (p *providerAuth) staleLineage(record authLedgerRecord) string {
	current, ok, err := p.ledger.read(record.ProviderID)
	if err != nil {
		return authCauseProcess
	}

	if ok && !current.namesLineage(record) {
		return authCauseBindingConflict
	}

	return ""
}

// failInstall answers an install that could not complete. The transition it
// would otherwise perform is the leg's own, so terminality has to be read
// before it: a flow that closed while the write was in flight is answered for
// the record its owner already closed, and this leg consumes nothing.
func (p *providerAuth) failInstall(flow *authFlow, cause string) error {
	if abandonedCause, abandoned := p.abandonedCause(flow); abandoned {
		return authFailed(abandonedCause, flow.providerID, flow.method.ID, flow.id)
	}

	return p.fail(flow, cause, true)
}

// abandonedCause reports the cause a leg answers with when the flow reached a
// terminal state while the native call this leg started was still in flight.
// Such a leg owns no transition and installs nothing: the record it addressed
// is already closed, and the outcome it carries is no longer the flow's.
func (p *providerAuth) abandonedCause(flow *authFlow) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case !authTerminal(flow.state):
		return "", false
	case flow.state == authStateCancelled:
		return authCauseFlowCancelled, true
	default:
		return authCauseFlowState, true
	}
}

// fail returns the leg's closed error and performs the transition its cause
// pairs with. A cause with no transition consumes nothing.
func (p *providerAuth) fail(flow *authFlow, cause string, materialInFlight bool) error {
	if state, reason := authFlowTransition(cause, materialInFlight); state != "" {
		p.terminalize(flow, state, reason, 0)

		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		if err := p.destroyBroker(ctx, flow); err != nil {
			cause = authCauseProcess
		}
	}

	return authFailed(cause, flow.providerID, flow.method.ID, flow.id)
}

// terminalize records the flow's one terminal transition. A flow that already
// reached one keeps it: a native call still in flight when the owner cancelled
// answers into a record the owner already closed, and the answer it carries is
// no longer the flow's outcome.
func (p *providerAuth) terminalize(flow *authFlow, state string, reason string, credentialExpiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if authTerminal(flow.state) {
		return
	}

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
func (p *providerAuth) status(_ context.Context, params json.RawMessage) (any, error) {
	flow, err := p.addressedFlowLeg(params)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	result := authStatusResult{FlowID: flow.id, State: flow.state, Reason: flow.reason}
	if flow.state == authStateAuthenticated {
		result.ExpiresAt = flow.credentialExpiresAt
	}

	return result, nil
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

		if cleanupErr := p.retryBrokerCleanup(ctx, flow.sessionID); cleanupErr != nil {
			return nil, authFailed(authCauseProcess, flow.providerID, flow.method.ID, flow.id)
		}

		return authFlowIDResult{FlowID: flow.id}, nil
	}

	flow.state = authStateCancelled
	flow.reason = authReasonOwnerCancel
	broker := flow.takeBroker()

	flow.stopCompleter()
	p.mu.Unlock()

	err = p.retireBroker(ctx, broker)
	p.waitCompletion(ctx, flow)

	if err != nil {
		return nil, authFailed(authCauseProcess, flow.providerID, flow.method.ID, flow.id)
	}

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
// differently fenced entry and promises no provider-side revocation. The whole
// sequence — the read, the generation compare, the bump, the native removal,
// the absence check and the removed-write — is held under the provider's
// credential-slot gate, because a login completing into the same slot would
// otherwise refill it between the verify and the answer.
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

	connectionID, err := authRequiredConnectionID(fields)
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

	release, admitted := p.slots.admit(ctx, providerID)
	if !admitted {
		return nil, authFailed(authCauseTimeout, providerID, "", "")
	}

	defer release()

	record, ok, err := p.ledger.read(providerID)
	if err != nil {
		return nil, authFailed(authCauseHarvestFailed, providerID, "", "")
	}

	if !ok || record.ConnectionID != connectionID || record.BindingGeneration != bindingGeneration {
		return nil, authFailed(authCauseBindingConflict, providerID, "", "")
	}

	finishQuotaMutation := p.agent.beginQuotaAuthMutation()
	defer finishQuotaMutation()

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

// closeSession fences publication, retires flows, and joins their cleanup.
func (p *providerAuth) closeSession(ctx context.Context, sessionID acp.SessionId) error {
	return p.closeFlows(ctx, sessionID, false)
}

func (p *providerAuth) close(ctx context.Context) error {
	return p.closeFlows(ctx, "", true)
}

func (p *providerAuth) closeFlows(ctx context.Context, sessionID acp.SessionId, all bool) error {
	p.mu.Lock()
	if all {
		p.closed = true
	} else {
		p.closedSessions[sessionID] = struct{}{}
	}

	brokers := make([]*authBroker, 0, len(p.flows))
	if p.completionCleanup == nil {
		p.completionCleanup = make(map[chan struct{}]acp.SessionId)
	}

	for key := range p.retired {
		if all || key.sessionID == sessionID {
			delete(p.retired, key)
		}
	}

	for key, flow := range p.flows {
		if !all && key.sessionID != sessionID {
			continue
		}

		delete(p.flows, key)
		delete(p.byID, flow.id)

		if flow.completionDone != nil {
			p.completionCleanup[flow.completionDone] = flow.sessionID
		}

		if !authTerminal(flow.state) {
			flow.state = authStateCancelled
			flow.reason = authReasonSessionClosed
			flow.stopCompleter()
		}

		if broker := flow.takeBroker(); broker != nil {
			brokers = append(brokers, broker)
		}
	}

	completions := make([]chan struct{}, 0, len(p.completionCleanup))
	for done, owner := range p.completionCleanup {
		if all || owner == sessionID {
			completions = append(completions, done)
		}
	}
	p.mu.Unlock()

	p.brokerMu.Lock()
	if p.brokers == nil {
		p.brokers = make(map[*authBroker]bool)
	}

	for _, broker := range brokers {
		p.brokers[broker] = true
	}

	err := p.cleanupBrokersLocked(ctx, sessionID, true)
	p.brokerMu.Unlock()

	if err != nil {
		return err
	}

	for _, done := range completions {
		select {
		case <-done:
			p.mu.Lock()
			delete(p.completionCleanup, done)
			p.mu.Unlock()
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}

	return err
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
