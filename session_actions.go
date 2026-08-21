package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const maxACPFrameBytes = 10 * 1024 * 1024

var actionSettlementTimeout = closeTimeout

// actionKind names the two native request classes this adapter routes to a host.
type actionKind int

const (
	actionPermission actionKind = iota
	actionElicitation
)

// nativeActionOutcome is how one action was answered. It exists so the native
// reply, the lifecycle terminal state, and the resolver's own decision are all
// derived from one value rather than recomputed at three sites.
type nativeActionOutcome struct {
	// state is the one terminal lifecycle state this action reaches.
	state lifecycle.ActionState
	// permissionReply is the native permission answer, empty for elicitations.
	permissionReply string
	// message annotates the native reply with why the harness answered itself.
	message string
	// answers are the native question answers, nil for a rejection.
	answers [][]string
}

// pendingAction is one registered action awaiting a host answer. A record is
// created before the announcing lifecycle event and removed only when exactly one
// terminal state has been emitted for it, so a host can never observe an action
// this adapter cannot answer and OpenCode is never left blocked on one this
// adapter already reported finished.
type pendingAction struct {
	id        string
	kind      actionKind
	turnNonce string
	cycle     *foregroundCycle
	owner     lifecycle.Owner
	cancel    context.CancelFunc
	binding   *nativeIncarnationBinding
	runID     string
	// admissionDone closes after registration either fails or the pending
	// action has been announced. Settlement waits for it, so neither an
	// immediate host response nor cancellation can resolve an action before its
	// first ordered sight.
	admissionMu        sync.Mutex
	admissionDone      chan struct{}
	admissionOnce      sync.Once
	admissionCancelled bool
	announced          bool
	// terminal fires once. The winner of response-versus-cancellation
	// arbitration writes the outcome and closes it.
	terminal sync.Once
	// permission and question hold the native request one of them describes.
	permission opencode.PermissionRequest
	question   opencode.QuestionRequest
}

// actionRegistry holds every action registered against this session. It is the
// single authority on what is pending, so the snapshot, the blocked-cycle
// projection, cancellation, and native-side resolutions all read one set.
type actionRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingAction
	// claimed records every action id this session has ever registered. A native
	// stream can re-announce a request it already announced, and a second
	// registration of a resolved id would report a resolved action as pending.
	claimed map[string]struct{}
}

func newActionRegistry() *actionRegistry {
	return &actionRegistry{pending: map[string]*pendingAction{}, claimed: map[string]struct{}{}}
}

// claim registers one action id exactly once for the life of the session.
func (r *actionRegistry) claim(action *pendingAction) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, seen := r.claimed[action.id]; seen {
		return false, errors.New("native action id was reused in one incarnation")
	}

	if len(r.claimed) >= lifecycle.EmitterActionLimit {
		return false, errors.New("native action registry capacity exceeded")
	}

	r.claimed[action.id] = struct{}{}
	r.pending[action.id] = action

	return true, nil
}

// take removes one pending action and reports whether this caller is the one that
// removed it.
func (r *actionRegistry) take(id string) (*pendingAction, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	action, ok := r.pending[id]
	if ok {
		delete(r.pending, id)
	}

	return action, ok
}

// takeIf removes id only when it is still the exact action incarnation the
// caller observed. Reusing an id on a replacement registry can never let a late
// resolver remove the replacement's request.
func (r *actionRegistry) takeIf(id string, expected *pendingAction) (*pendingAction, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	action, ok := r.pending[id]
	if !ok || action != expected {
		return nil, false
	}

	delete(r.pending, id)

	return action, true
}

// snapshot lists every pending action.
func (r *actionRegistry) snapshot() []*pendingAction {
	r.mu.Lock()
	defer r.mu.Unlock()

	actions := make([]*pendingAction, 0, len(r.pending))
	for _, action := range r.pending {
		actions = append(actions, action)
	}

	return actions
}

// blocked reports whether any action is pending.
func (r *actionRegistry) blocked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.pending) > 0
}

// routeNativePermission admits one native permission request. The fence is
// structural: the request must name this session's native id, and a request that
// names a tool call this session never published is refused rather than shown to
// a host that could not attribute it.
func (s *session) routeNativePermission(ctx context.Context, req opencode.PermissionRequest) error {
	if req.SessionID != s.idmap.NativeSessionID {
		return nil
	}

	action := &pendingAction{
		id: req.ID, kind: actionPermission, turnNonce: turnNonceFromContext(ctx), permission: req,
		binding: s.nativeIncarnationForContext(ctx),
	}
	if callID := req.ToolCall().CallID; callID != "" && !s.publishedToolCall(callID) {
		return s.refuseNativeAction(ctx, action, true,
			invalidRoute("permission request does not target a tool call this session published"))
	}

	if err := s.prepareAction(ctx, action); err != nil {
		return s.refuseNativeAction(ctx, action, true, err)
	}

	if answered, err := s.beginAction(ctx, action); err != nil {
		return s.refuseNativeAction(ctx, action, !answered, err)
	}

	return nil
}

// routeNativeQuestion admits one native question. A question without a tool call
// is a session-level prompt and carries no tool-call fence; one that names a tool
// call is held to the same published-call rule as a permission.
func (s *session) routeNativeQuestion(ctx context.Context, req opencode.QuestionRequest) error {
	if req.SessionID != s.idmap.NativeSessionID {
		return nil
	}

	action := &pendingAction{
		id: req.ID, kind: actionElicitation, turnNonce: turnNonceFromContext(ctx), question: req,
		binding: s.nativeIncarnationForContext(ctx),
	}
	if callID := req.Tool.CallID; callID != "" && !s.publishedToolCall(callID) {
		return s.refuseNativeAction(ctx, action, true,
			invalidRoute("question request does not target a tool call this session published"))
	}

	if err := s.prepareAction(ctx, action); err != nil {
		return s.refuseNativeAction(ctx, action, true, err)
	}

	if answered, err := s.beginAction(ctx, action); err != nil {
		return s.refuseNativeAction(ctx, action, !answered, err)
	}

	return nil
}

func (s *session) prepareAction(ctx context.Context, action *pendingAction) error {
	s.lifecycleMu.Lock()

	binding := s.incarnation
	if binding == nil || action.binding == nil || action.binding != binding {
		s.lifecycleMu.Unlock()

		return errors.New("native action incarnation binding is stale")
	}

	cycle, _ := s.observedCycleLocked(ctx, false)

	ownerID := ""
	if cycle != nil {
		ownerID = cycle.turnID
	} else {
		ownerID = fmt.Sprintf("agent-turn-%d", s.turnCounter+1)
	}

	action.owner = lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: ownerID}
	action.runID = s.submission.RunID

	if err := validateActionIdentifiers(action); err != nil {
		s.lifecycleMu.Unlock()

		return err
	}

	if binding.stream != nil {
		kind := lifecycle.ActionPermission
		if action.kind == actionElicitation {
			kind = lifecycle.ActionElicitation
		}

		if err := binding.stream.Preflight(lifecycle.ActionEvent(lifecycle.PendingAction(
			action.id, kind, action.owner, true,
		))); err != nil {
			s.lifecycleMu.Unlock()

			return err
		}
	}
	s.lifecycleMu.Unlock()

	if !s.hostCanAnswerAction(action) {
		return nil
	}

	return s.validateHostActionFrame(action)
}

func validateActionIdentifiers(action *pendingAction) error {
	identifier := func(name, value string, required bool) error {
		if value == "" {
			if required {
				return fmt.Errorf("%s is empty", name)
			}

			return nil
		}

		if len(value) > lifecycle.IdentifierBound {
			return fmt.Errorf("%s exceeds %d bytes", name, lifecycle.IdentifierBound)
		}

		return nil
	}

	if err := identifier("action id", action.id, true); err != nil {
		return err
	}

	if err := identifier("action owner id", action.owner.ID, true); err != nil {
		return err
	}

	tool := action.permission.ToolCall()
	if action.kind == actionElicitation {
		tool = opencode.PermissionTool{CallID: action.question.Tool.CallID, MessageID: action.question.Tool.MessageID}
	}

	if err := identifier("action tool-call id", tool.CallID, false); err != nil {
		return err
	}

	return identifier("action tool message id", tool.MessageID, false)
}

func (s *session) validateHostActionFrame(action *pendingAction) error {
	method := acp.ClientMethodSessionRequestPermission

	var params any = s.permissionHostRequest(action)

	if action.kind == actionElicitation {
		method = acp.ClientMethodElicitationCreate
		request, _, scope := s.elicitationHostRequest(action)

		raw, err := scopedElicitationParams(request, scope)
		if err != nil {
			return fmt.Errorf("marshal complete host action request: %w", err)
		}

		params = raw
	}

	frame := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  any             `json:"params"`
	}{
		JSONRPC: jsonRPCVersion,
		ID:      json.RawMessage("18446744073709551615"),
		Method:  method,
		Params:  params,
	}

	encoded, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal complete host action request: %w", err)
	}

	if len(encoded)+1 > maxACPFrameBytes {
		return fmt.Errorf("complete host action request exceeds %d bytes", maxACPFrameBytes)
	}

	return nil
}

// declineNativePermission answers a permission this session cannot route and
// forgets it. A request that arrives before the prompt it might belong to has
// proved its own dispatch names no turn here, and leaving it unanswered would
// strand the harness waiting on a decision nobody will make. Nothing about an
// unroutable request is a fact about the runtime's health.
func (s *session) declineNativePermission(ctx context.Context, req opencode.PermissionRequest) {
	binding := s.nativeIncarnationForContext(ctx)
	if binding == nil || binding.client == nil || !s.incarnationIsCurrent(binding) {
		return
	}

	_ = binding.client.ReplyPermission(ctx, req, permissionReplyReject, "unroutable native permission")
}

// declineNativeQuestion is declineNativePermission for a question: the harness is
// answered, the request is dropped, and the incarnation lives on.
func (s *session) declineNativeQuestion(ctx context.Context, req opencode.QuestionRequest) {
	binding := s.nativeIncarnationForContext(ctx)
	if binding == nil || binding.client == nil || !s.incarnationIsCurrent(binding) {
		return
	}

	_ = binding.client.RejectQuestion(ctx, req)
}

func (s *session) refuseNativeAction(
	ctx context.Context,
	action *pendingAction,
	answerNative bool,
	err error,
) error {
	if err == nil {
		return nil
	}

	binding := action.binding
	if binding == nil {
		binding = s.nativeIncarnationForContext(ctx)
	}

	if answerNative && binding != nil && binding.client != nil && s.incarnationIsCurrent(binding) {
		if action.kind == actionPermission {
			_ = binding.client.ReplyPermission(ctx, action.permission, permissionReplyReject, "invalid native action")
		} else {
			_ = binding.client.RejectQuestion(ctx, action.question)
		}
	}

	s.failNativeIncarnation(binding, err)

	return err
}

// beginAction runs the one ordered admission sequence for every action: register
// the record, start the outbound host request, announce the action, then report
// the foreground it blocks. The host request starts before the announcement so an
// announced action is always one that is already in flight, and the arbitration
// that terminalizes it runs on its own goroutine so a blocked action never stalls
// the session's event pump.
func (s *session) beginAction(ctx context.Context, action *pendingAction) (bool, error) {
	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	action.cancel = cancel
	action.admissionDone = make(chan struct{})

	s.lifecycleMu.Lock()
	if action.binding == nil || s.incarnation != action.binding {
		s.lifecycleMu.Unlock()
		cancel()

		return false, errors.New("native action incarnation binding is stale")
	}

	cycle, err := s.openAgentCycleLocked(ctx)
	if err != nil {
		s.lifecycleMu.Unlock()
		cancel()

		return false, err
	}

	if cycle == nil {
		s.lifecycleMu.Unlock()
		cancel()

		return false, errors.New("native action requires an active lifecycle incarnation")
	}

	action.cycle = cycle
	if action.owner.ID != cycle.turnID {
		s.lifecycleMu.Unlock()
		cancel()

		return false, errors.New("native action owner changed before admission")
	}

	if stream := action.binding.stream; stream != nil {
		kind := lifecycle.ActionPermission
		if action.kind == actionElicitation {
			kind = lifecycle.ActionElicitation
		}

		if err := stream.Preflight(
			lifecycle.ActionEvent(lifecycle.PendingAction(action.id, kind, action.owner, true)),
			lifecycle.TransitionEvent(
				lifecycle.ForegroundRequiresAction, cycle.id, cycle.turnID, cycle.origin,
			),
		); err != nil {
			s.lifecycleMu.Unlock()
			cancel()

			return false, err
		}
	}

	if _, claimErr := action.binding.registry.claim(action); claimErr != nil {
		s.lifecycleMu.Unlock()
		cancel()

		return false, claimErr
	}

	s.blockCycleLocked(action.id, cycle)
	s.lifecycleMu.Unlock()

	var registrationErr error

	request := s.beginHostActionRequest(requestCtx, action)
	select {
	case registrationErr = <-request.registered:
	case <-requestCtx.Done():
		registrationErr = requestCtx.Err()
	}

	if registrationErr != nil {
		cancelled := action.admissionIsCancelled()
		state := lifecycle.ActionFailed

		action.completeAdmission(false)
		cancel()

		if cancelled {
			state = lifecycle.ActionCancelled
		}

		s.finishAction(action, nativeActionOutcome{state: state})

		if cancelled {
			return true, nil
		}

		return true, fmt.Errorf("register host action request: %w", registrationErr)
	}

	action.admissionMu.Lock()
	if action.admissionCancelled {
		action.completeAdmissionLocked(false)
		action.admissionMu.Unlock()
		s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})

		return true, nil
	}

	announced, announceErr := s.announceAction(ctx, action, cycle)
	action.completeAdmissionLocked(announced)
	action.admissionMu.Unlock()

	if announceErr != nil {
		cancel()
		s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionFailed})
		s.failNativeIncarnation(action.binding, announceErr)

		return true, announceErr
	}

	go func() {
		defer handleAgentGoroutinePanicRecover(requestCtx, agentLogger(s.agent), "OpenCode action settlement", nil)

		s.awaitActionOutcome(action, request.answered)
	}()

	return false, nil
}

// announceAction emits the action's first sight and the foreground state it
// blocks. The two are one step: a blocking action that announced no transition
// would leave a host unable to explain why the foreground stopped.
func (s *session) announceAction(ctx context.Context, action *pendingAction, cycle *foregroundCycle) (bool, error) {
	kind := lifecycle.ActionPermission
	if action.kind == actionElicitation {
		kind = lifecycle.ActionElicitation
	}

	s.lifecycleMu.Lock()

	if s.incarnation != action.binding {
		s.lifecycleMu.Unlock()

		return false, errors.New("action lifecycle binding is stale")
	}

	receipts := make([]deliveryReceipt, 0, 2)

	receipt, err := s.emitLifecycleLockedWithFailure(ctx, lifecycle.ActionEvent(lifecycle.PendingAction(
		action.id, kind, action.owner, true,
	)), false)
	if err != nil {
		s.lifecycleMu.Unlock()

		return false, err
	}

	receipts = append(receipts, receipt)

	receipt, err = s.emitLifecycleLockedWithFailure(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRequiresAction, cycle.id, cycle.turnID, cycle.origin,
	), false)
	if err == nil {
		receipts = append(receipts, receipt)
	}

	s.lifecycleMu.Unlock()

	if err != nil {
		return true, err
	}

	for _, receipt := range receipts {
		if err := waitDelivery(ctx, receipt); err != nil {
			return true, err
		}
	}

	return true, nil
}

func (a *pendingAction) completeAdmission(announced bool) {
	a.admissionMu.Lock()
	defer a.admissionMu.Unlock()

	a.completeAdmissionLocked(announced)
}

func (a *pendingAction) completeAdmissionLocked(announced bool) {
	a.announced = announced
	a.admissionOnce.Do(func() { close(a.admissionDone) })
}

func (a *pendingAction) cancelAdmission() {
	a.admissionMu.Lock()

	a.admissionCancelled = true
	if a.cancel != nil {
		a.cancel()
	}
	a.admissionMu.Unlock()
}

func (a *pendingAction) admissionIsCancelled() bool {
	a.admissionMu.Lock()
	defer a.admissionMu.Unlock()

	return a.admissionCancelled
}

func (a *pendingAction) waitForAdmission() {
	if a.admissionDone != nil {
		<-a.admissionDone
	}
}

// awaitActionOutcome arbitrates the host answer against cancellation. Cancellation
// wins: a cycle that is being torn down reports its blockers cancelled, replies to
// OpenCode so the harness is never left blocked, and only then terminalizes.
func (s *session) awaitActionOutcome(action *pendingAction, answered <-chan nativeActionOutcome) {
	select {
	case outcome := <-answered:
		if action.admissionIsCancelled() {
			s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})

			return
		}

		select {
		case <-action.cycle.signal:
			action.cancelAdmission()
			s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})

			return
		default:
		}

		s.finishAction(action, outcome)
	case <-action.cycle.signal:
		action.cancelAdmission()
		s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})
	}
}

// finishAction performs the one terminal settlement of an action: reply to
// OpenCode, obtain its acknowledgement, then emit exactly one terminal action
// state and the transition that unblocks the cycle.
func (s *session) finishAction(action *pendingAction, outcome nativeActionOutcome) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	s.finishActionWithContext(ctx, action, outcome)
}

func (s *session) finishActionWithContext(ctx context.Context, action *pendingAction, outcome nativeActionOutcome) {
	action.terminal.Do(func() { s.settleActionNatively(ctx, action, outcome) })
}

// settleActionNatively answers OpenCode and then reports the action terminal. The
// native acknowledgement is ordered first: a lifecycle terminal state emitted
// before it would report an action finished while the harness still waits on it,
// and a failed acknowledgement is reported as a failed action rather than hidden.
func (s *session) settleActionNatively(ctx context.Context, action *pendingAction, outcome nativeActionOutcome) {
	if action.binding == nil {
		return
	}

	s.lifecycleMu.Lock()

	if s.incarnation != action.binding {
		s.lifecycleMu.Unlock()

		return
	}

	_, held := action.binding.registry.takeIf(action.id, action)
	s.lifecycleMu.Unlock()

	if !held {
		return
	}

	action.waitForAdmission()

	state := outcome.state
	if err := s.replyNative(ctx, action, outcome); err != nil {
		state = lifecycle.ActionFailed

		s.recordActionFailure(action, err)
	}

	if !action.announced {
		s.releaseUnannouncedAction(action)

		return
	}

	if err := s.terminalizeAction(ctx, action, state); err != nil {
		s.recordActionFailure(action, err)
		s.failNativeIncarnation(action.binding, err)
	}
}

func (s *session) releaseUnannouncedAction(action *pendingAction) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if action.cycle != nil {
		delete(action.cycle.blockers, action.id)
	}
}

// replyNative sends the answer OpenCode is blocked on.
func (s *session) replyNative(ctx context.Context, action *pendingAction, outcome nativeActionOutcome) error {
	if action == nil || action.binding == nil {
		return errors.New("OpenCode action reply has no incarnation binding")
	}

	client := action.binding.client
	if client == nil {
		return errors.New("OpenCode action reply has no runtime client")
	}

	if !s.actionBindingCurrent(action) {
		return nil
	}

	if action.kind == actionPermission {
		reply := outcome.permissionReply
		if reply == "" {
			reply = permissionReplyReject
		}

		message := outcome.message
		if message == "" && outcome.state == lifecycle.ActionCancelled {
			message = reasonCancelled
		}

		return client.ReplyPermission(ctx, action.permission, reply, message)
	}

	if outcome.state == lifecycle.ActionAccepted {
		return client.ReplyQuestion(ctx, action.question, outcome.answers)
	}

	return client.RejectQuestion(ctx, action.question)
}

// terminalizeAction emits the action's single terminal state and then the
// transition that releases its cycle. Running resumes only when the last blocker
// clears; a cycle whose blockers are gone because it is ending reports its
// terminal idle from the settlement path instead.
func (s *session) terminalizeAction(ctx context.Context, action *pendingAction, state lifecycle.ActionState) error {
	s.lifecycleMu.Lock()

	if s.incarnation != action.binding {
		s.lifecycleMu.Unlock()

		return nil
	}

	cycle := action.cycle
	if cycle == nil {
		s.lifecycleMu.Unlock()

		return nil
	}

	delete(cycle.blockers, action.id)

	receipts := make([]deliveryReceipt, 0, 2)

	receipt, err := s.emitLifecycleLocked(ctx, lifecycle.ActionEvent(
		lifecycle.ResolvedAction(action.id, state),
	))
	if err != nil {
		s.lifecycleMu.Unlock()

		return err
	}

	receipts = append(receipts, receipt)

	if len(cycle.blockers) > 0 || cycle.settled || cycle.terminalEvidence() {
		s.lifecycleMu.Unlock()

		return waitDelivery(ctx, receipt)
	}

	receipt, err = s.emitLifecycleLocked(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, cycle.id, cycle.turnID, cycle.origin,
	))
	if err == nil {
		receipts = append(receipts, receipt)
	}

	s.lifecycleMu.Unlock()

	if err != nil {
		return err
	}

	for _, receipt := range receipts {
		if err := waitDelivery(ctx, receipt); err != nil {
			return err
		}
	}

	return nil
}

type registeredActionRequest struct {
	registered <-chan error
	answered   <-chan nativeActionOutcome
}

func (s *session) hostCanAnswerAction(action *pendingAction) bool {
	if s.agent == nil || s.agent.connection() == nil {
		return false
	}

	return action.kind != actionElicitation || s.agent.clientSupportsFormElicitation()
}

func (s *session) beginRegisteredHostRequest(ctx context.Context, action *pendingAction) registeredActionRequest {
	conn := s.agent.connection()
	answered := make(chan nativeActionOutcome, 1)

	if action.kind == actionPermission {
		request := s.permissionHostRequest(action)
		pending := conn.BeginRequestPermission(ctx, request,
			s.hostRequestKey(acp.ClientMethodSessionRequestPermission, action))

		go func() {
			result := <-pending.answered

			outcome := permissionHostOutcome(result.response)
			if result.err != nil {
				outcome = nativeActionOutcome{state: lifecycle.ActionFailed, permissionReply: permissionReplyReject}

				if ctx.Err() == nil {
					s.recordActionFailure(action, result.err)
				}
			}

			answered <- outcome
		}()

		return registeredActionRequest{registered: pending.registered, answered: answered}
	}

	request, propertyIDs, scope := s.elicitationHostRequest(action)
	pending := conn.BeginCreateElicitation(ctx, request, scope,
		s.hostRequestKey(acp.ClientMethodElicitationCreate, action))

	go func() {
		result := <-pending.answered
		outcome := elicitationHostOutcome(result.response, propertyIDs)

		if result.err != nil {
			outcome = nativeActionOutcome{state: lifecycle.ActionFailed}

			if ctx.Err() == nil {
				s.recordActionFailure(action, result.err)
			}
		}

		answered <- outcome
	}()

	return registeredActionRequest{registered: pending.registered, answered: answered}
}

func (s *session) beginHostActionRequest(ctx context.Context, action *pendingAction) registeredActionRequest {
	if s.hostCanAnswerAction(action) {
		return s.beginRegisteredHostRequest(ctx, action)
	}

	registered := make(chan error, 1)
	answered := make(chan nativeActionOutcome, 1)

	registered <- nil

	if action.kind == actionPermission {
		answered <- nativeActionOutcome{
			state: lifecycle.ActionDeclined, permissionReply: permissionReplyReject, message: "client unavailable",
		}
	} else {
		answered <- nativeActionOutcome{state: lifecycle.ActionDeclined}
	}

	return registeredActionRequest{registered: registered, answered: answered}
}

func (s *session) hostRequestKey(method string, action *pendingAction) hostRequestKey {
	streamID := ""
	if action.binding != nil && action.binding.stream != nil {
		streamID = action.binding.stream.ID()
	}

	return hostRequestKey{method: method, streamID: streamID, actionID: action.id}
}

func (s *session) permissionHostRequest(action *pendingAction) acp.RequestPermissionRequest {
	req := action.permission
	title := req.ActionName()

	if title == "" {
		title = "OpenCode permission"
	}

	status := acp.ToolCallStatusPending
	kind := acp.ToolKindOther
	tool := req.ToolCall()

	return acp.RequestPermissionRequest{
		SessionId: s.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(tool.CallID),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			RawInput: map[string]any{
				jsonFieldAction:       req.ActionName(),
				jsonFieldResources:    req.ResourceList(),
				"metadata":            req.Metadata,
				jsonFieldSource:       req.Source,
				"save":                req.Save,
				permissionReplyAlways: req.Always,
				"toolCallId":          tool.CallID,
				jsonFieldMessageID:    tool.MessageID,
			},
		},
		Options: []acp.PermissionOption{
			{OptionId: permissionReplyOnce, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: permissionReplyAlways, Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: permissionReplyReject, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
		Meta: mergeMeta(
			map[string]any{opencodeMetaKey: map[string]any{routeFieldRequestID: req.ID, opencodeNativeIDMetaKey: req.SessionID}},
			s.lifecycleActionMeta(action),
		),
	}
}

func permissionHostOutcome(resp acp.RequestPermissionResponse) nativeActionOutcome {
	if resp.Outcome.Cancelled != nil {
		return nativeActionOutcome{state: lifecycle.ActionCancelled, permissionReply: permissionReplyReject}
	}

	reply := permissionReplyReject

	if resp.Outcome.Selected != nil {
		switch resp.Outcome.Selected.OptionId {
		case permissionReplyOnce, permissionReplyAlways, permissionReplyReject:
			reply = string(resp.Outcome.Selected.OptionId)
		}
	}

	state := lifecycle.ActionDeclined
	if reply == permissionReplyOnce || reply == permissionReplyAlways {
		state = lifecycle.ActionAccepted
	}

	return nativeActionOutcome{state: state, permissionReply: reply}
}

func (s *session) elicitationHostRequest(
	action *pendingAction,
) (acp.UnstableCreateElicitationRequest, []string, elicitationScope) {
	request, propertyIDs := questionElicitationRequest(action.question)
	if request.Form != nil {
		request.Form.Meta = mergeMeta(request.Form.Meta, s.lifecycleActionMeta(action))
	}

	scope := elicitationScope{SessionID: s.id, TurnNonce: action.turnNonce}
	if action.question.Tool.CallID != "" {
		scope.ToolCallID = acp.ToolCallId(action.question.Tool.CallID)
	} else {
		requestIDValue := acp.RequestIdStr(action.id)
		requestID := acp.RequestId{Str: &requestIDValue}
		scope.RequestID = &requestID
	}

	return request, propertyIDs, scope
}

func elicitationHostOutcome(
	resp acp.UnstableCreateElicitationResponse,
	propertyIDs []string,
) nativeActionOutcome {
	if resp.Accept == nil {
		return nativeActionOutcome{state: lifecycle.ActionDeclined}
	}

	return nativeActionOutcome{
		state:   lifecycle.ActionAccepted,
		answers: questionAnswersFromContent(resp.Accept.Content, propertyIDs),
	}
}

// nativeActionResolved terminalizes an action OpenCode resolved on its own — a
// policy answer, a TUI answer, or a withdrawal. The outbound host request is
// cancelled and the action reports the state the harness recorded, because the
// harness is no longer waiting for an answer this adapter has not sent.
func (s *session) nativeActionResolved(ctx context.Context, replied opencode.ActionRepliedEvent, state lifecycle.ActionState) {
	s.lifecycleMu.Lock()

	binding := s.incarnation
	if binding == nil {
		s.lifecycleMu.Unlock()

		return
	}

	action, held := binding.registry.take(replied.RequestID)
	s.lifecycleMu.Unlock()

	if !held {
		return
	}

	action.terminal.Do(func() {
		action.cancelAdmission()
		action.waitForAdmission()

		if !action.announced {
			s.releaseUnannouncedAction(action)

			return
		}

		if err := s.terminalizeAction(ctx, action, state); err != nil {
			s.recordActionFailure(action, err)
			s.failNativeIncarnation(action.binding, err)
		}
	})
}

// cancelActions terminalizes every pending action as cancelled. It runs before a
// cycle's ending transition, so OpenCode is answered and the stream reports every
// blocker terminal before the foreground moves.
func (s *session) cancelActions(ctx context.Context) {
	binding := s.currentIncarnation()
	if binding == nil {
		return
	}

	actions := binding.registry.snapshot()
	for _, action := range actions {
		action.cancelAdmission()
	}

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), actionSettlementTimeout)
	defer cancel()

	for _, action := range actions {
		s.finishActionWithContext(settleCtx, action, nativeActionOutcome{state: lifecycle.ActionCancelled})
	}
}

func (s *session) cancelActionAdmissions() {
	binding := s.currentIncarnation()
	if binding == nil {
		return
	}

	for _, action := range binding.registry.snapshot() {
		action.cancelAdmission()
	}
}

// recordActionFailure latches an action failure onto the session's lifecycle
// state. A failure here is not a prompt's own failure — the action may belong to
// an agent-origin turn — so it is recorded where the owning cycle settles. Every
// caller reports a failure it already holds, so there is no nil error to guard.
func (s *session) recordActionFailure(action *pendingAction, _ error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if action != nil && s.incarnation == action.binding &&
		s.cycle == action.cycle && s.cycle != nil && s.cycle.failure == nil {
		s.cycle.failure = errors.New("native action settlement failed")
	}
}

func (s *session) actionBindingCurrent(action *pendingAction) bool {
	return action != nil && action.binding != nil && s.incarnationIsCurrent(action.binding)
}
