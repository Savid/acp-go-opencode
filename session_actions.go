package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

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
	id     string
	kind   actionKind
	cycle  *foregroundCycle
	owner  lifecycle.Owner
	cancel context.CancelFunc
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
func (r *actionRegistry) claim(action *pendingAction) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, seen := r.claimed[action.id]; seen {
		return false
	}

	r.claimed[action.id] = struct{}{}
	r.pending[action.id] = action

	return true
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
	if req.ID == "" || req.SessionID == "" {
		return nil
	}

	if req.SessionID != s.idmap.NativeSessionID {
		return nil
	}

	if callID := req.ToolCall().CallID; callID != "" && !s.publishedToolCall(callID) {
		replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		replyErr := s.client.ReplyPermission(replyCtx, req, permissionReplyReject, "unpublished tool call")

		cancel()

		return errors.Join(
			invalidRoute("permission request does not target a tool call this session published"),
			replyErr,
		)
	}

	return s.beginAction(ctx, &pendingAction{id: req.ID, kind: actionPermission, permission: req})
}

// routeNativeQuestion admits one native question. A question without a tool call
// is a session-level prompt and carries no tool-call fence; one that names a tool
// call is held to the same published-call rule as a permission.
func (s *session) routeNativeQuestion(ctx context.Context, req opencode.QuestionRequest) error {
	if req.ID == "" || req.SessionID == "" {
		return nil
	}

	if req.SessionID != s.idmap.NativeSessionID {
		return nil
	}

	if callID := req.Tool.CallID; callID != "" && !s.publishedToolCall(callID) {
		rejectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		rejectErr := s.client.RejectQuestion(rejectCtx, req)

		cancel()

		return errors.Join(
			invalidRoute("question request does not target a tool call this session published"),
			rejectErr,
		)
	}

	return s.beginAction(ctx, &pendingAction{id: req.ID, kind: actionElicitation, question: req})
}

// beginAction runs the one ordered admission sequence for every action: register
// the record, start the outbound host request, announce the action, then report
// the foreground it blocks. The host request starts before the announcement so an
// announced action is always one that is already in flight, and the arbitration
// that terminalizes it runs on its own goroutine so a blocked action never stalls
// the session's event pump.
func (s *session) beginAction(ctx context.Context, action *pendingAction) error {
	if !s.agent.lifecycleNegotiated().Present() && s.lifecycleStreamAbsent() {
		return s.resolveActionRequest(ctx, action)
	}

	s.lifecycleMu.Lock()

	cycle, err := s.openAgentCycleLocked(ctx)
	if err != nil {
		s.lifecycleMu.Unlock()

		return err
	}

	action.cycle = cycle
	action.owner = lifecycle.Owner{Type: lifecycle.OwnerTurn, ID: cycle.turnID}

	if !s.actions.claim(action) {
		s.lifecycleMu.Unlock()

		return nil
	}

	s.blockCycleLocked(action.id, cycle)
	s.lifecycleMu.Unlock()

	requestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	action.cancel = cancel

	answered := make(chan nativeActionOutcome, 1)

	go func() {
		defer handleAgentGoroutinePanicRecover(requestCtx, agentLogger(s.agent), "OpenCode action request", func(recovered any) {
			answered <- nativeActionOutcome{state: lifecycle.ActionFailed}

			s.recordActionFailure(fmt.Errorf("opencode action request panicked: %v", recovered))
		})

		outcome, err := s.askHost(requestCtx, action)
		// An ask this adapter abandoned is not a failure of the turn. The two
		// paths that abandon one — OpenCode resolving the action itself, and a
		// cycle ending under it — cancel this context first and then record
		// whatever actually went wrong, and a cancellation racing them to the
		// cycle's single failure slot would report the abandonment instead of
		// the reason for it.
		if err != nil && requestCtx.Err() == nil {
			s.recordActionFailure(err)
		}

		answered <- outcome
	}()

	announceErr := s.announceAction(ctx, action, cycle)
	if announceErr != nil {
		cancel()
		<-answered
		s.settleActionNatively(ctx, action, nativeActionOutcome{state: lifecycle.ActionFailed})

		return announceErr
	}

	go func() {
		defer handleAgentGoroutinePanicRecover(requestCtx, agentLogger(s.agent), "OpenCode action settlement", nil)

		s.awaitActionOutcome(action, answered)
	}()

	return nil
}

// announceAction emits the action's first sight and the foreground state it
// blocks. The two are one step: a blocking action that announced no transition
// would leave a host unable to explain why the foreground stopped.
func (s *session) announceAction(ctx context.Context, action *pendingAction, cycle *foregroundCycle) error {
	kind := lifecycle.ActionPermission
	if action.kind == actionElicitation {
		kind = lifecycle.ActionElicitation
	}

	if err := s.emitLifecycle(ctx, lifecycle.ActionEvent(lifecycle.PendingAction(
		action.id, kind, action.owner, true,
	))); err != nil {
		return err
	}

	return s.emitLifecycle(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRequiresAction, cycle.id, cycle.turnID, cycle.origin,
	))
}

// awaitActionOutcome arbitrates the host answer against cancellation. Cancellation
// wins: a cycle that is being torn down reports its blockers cancelled, replies to
// OpenCode so the harness is never left blocked, and only then terminalizes.
func (s *session) awaitActionOutcome(action *pendingAction, answered <-chan nativeActionOutcome) {
	select {
	case outcome := <-answered:
		s.finishAction(action, outcome)
	case <-action.cycle.signal:
		action.cancel()
		<-answered
		s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})
	}
}

// finishAction performs the one terminal settlement of an action: reply to
// OpenCode, obtain its acknowledgement, then emit exactly one terminal action
// state and the transition that unblocks the cycle.
func (s *session) finishAction(action *pendingAction, outcome nativeActionOutcome) {
	action.terminal.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		s.settleActionNatively(ctx, action, outcome)
	})
}

// settleActionNatively answers OpenCode and then reports the action terminal. The
// native acknowledgement is ordered first: a lifecycle terminal state emitted
// before it would report an action finished while the harness still waits on it,
// and a failed acknowledgement is reported as a failed action rather than hidden.
func (s *session) settleActionNatively(ctx context.Context, action *pendingAction, outcome nativeActionOutcome) {
	if _, held := s.actions.take(action.id); !held {
		return
	}

	state := outcome.state
	if err := s.replyNative(ctx, action, outcome); err != nil {
		state = lifecycle.ActionFailed

		s.recordActionFailure(err)
	}

	if err := s.terminalizeAction(ctx, action, state); err != nil {
		s.recordActionFailure(err)
	}
}

// replyNative sends the answer OpenCode is blocked on.
func (s *session) replyNative(ctx context.Context, action *pendingAction, outcome nativeActionOutcome) error {
	client := s.currentClient()
	if client == nil {
		return errors.New("OpenCode action reply has no runtime client")
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
	defer s.lifecycleMu.Unlock()

	cycle := action.cycle
	if cycle == nil {
		return nil
	}

	delete(cycle.blockers, action.id)

	if err := s.emitLifecycleLocked(ctx, lifecycle.ActionEvent(
		lifecycle.ResolvedAction(action.id, state),
	)); err != nil {
		return err
	}

	if len(cycle.blockers) > 0 || cycle.settled || cycle.terminalEvidence() {
		return nil
	}

	return s.emitLifecycleLocked(ctx, lifecycle.TransitionEvent(
		lifecycle.ForegroundRunning, cycle.id, cycle.turnID, cycle.origin,
	))
}

// askHost issues the outbound request and maps its answer onto one native
// outcome. It never replies to OpenCode itself: settlement is one path.
func (s *session) askHost(ctx context.Context, action *pendingAction) (nativeActionOutcome, error) {
	if action.kind == actionPermission {
		return s.askPermission(ctx, action)
	}

	return s.askElicitation(ctx, action)
}

func (s *session) askPermission(ctx context.Context, action *pendingAction) (nativeActionOutcome, error) {
	conn := s.agent.connection()
	if conn == nil {
		// A host that cannot be asked declines the permission, exactly as an
		// unanswerable elicitation declines: an error here would fail a turn
		// over a condition the native session can simply be answered about.
		return nativeActionOutcome{
			state: lifecycle.ActionDeclined, permissionReply: permissionReplyReject, message: "client unavailable",
		}, nil
	}

	req := action.permission
	title := req.ActionName()

	if title == "" {
		title = "OpenCode permission"
	}

	status := acp.ToolCallStatusPending
	kind := acp.ToolKindOther
	tool := req.ToolCall()

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
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
			s.lifecycleActionMeta(req.ID, action.owner),
		),
	})
	if err != nil {
		// The request itself failed: the host never decided, so the action
		// failed — it was not declined.
		return nativeActionOutcome{state: lifecycle.ActionFailed, permissionReply: permissionReplyReject}, err
	}

	if resp.Outcome.Cancelled != nil {
		return nativeActionOutcome{state: lifecycle.ActionCancelled, permissionReply: permissionReplyReject}, nil
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

	return nativeActionOutcome{state: state, permissionReply: reply}, nil
}

func (s *session) askElicitation(ctx context.Context, action *pendingAction) (nativeActionOutcome, error) {
	conn := s.agent.connection()
	if conn == nil || !s.agent.clientSupportsFormElicitation() {
		// A host that cannot be asked declines the question. Reporting the
		// action declined rather than leaving it pending is what keeps an
		// unanswerable elicitation from blocking the foreground forever.
		return nativeActionOutcome{state: lifecycle.ActionDeclined}, nil
	}

	request, propertyIDs := questionElicitationRequest(action.question)
	if request.Form != nil {
		request.Form.Meta = mergeMeta(request.Form.Meta, s.lifecycleActionMeta(action.id, action.owner))
	}

	scope := elicitationScope{SessionID: s.id, TurnNonce: s.currentTurnNonce()}
	if action.question.Tool.CallID != "" {
		scope.ToolCallID = acp.ToolCallId(action.question.Tool.CallID)
	} else {
		requestIDValue := acp.RequestIdStr(action.id)
		requestID := acp.RequestId{Str: &requestIDValue}
		scope.RequestID = &requestID
	}

	resp, err := conn.CreateElicitation(ctx, request, scope)
	if err != nil {
		return nativeActionOutcome{state: lifecycle.ActionFailed}, err
	}

	if resp.Accept == nil {
		return nativeActionOutcome{state: lifecycle.ActionDeclined}, nil
	}

	return nativeActionOutcome{
		state:   lifecycle.ActionAccepted,
		answers: questionAnswersFromContent(resp.Accept.Content, propertyIDs),
	}, nil
}

// nativeActionResolved terminalizes an action OpenCode resolved on its own — a
// policy answer, a TUI answer, or a withdrawal. The outbound host request is
// cancelled and the action reports the state the harness recorded, because the
// harness is no longer waiting for an answer this adapter has not sent.
func (s *session) nativeActionResolved(ctx context.Context, replied opencode.ActionRepliedEvent, state lifecycle.ActionState) {
	action, held := s.actions.take(replied.RequestID)
	if !held {
		return
	}

	action.terminal.Do(func() {
		if action.cancel != nil {
			action.cancel()
		}

		if err := s.terminalizeAction(ctx, action, state); err != nil {
			s.recordActionFailure(err)
		}
	})
}

// cancelActions terminalizes every pending action as cancelled. It runs before a
// cycle's ending transition, so OpenCode is answered and the stream reports every
// blocker terminal before the foreground moves.
func (s *session) cancelActions(ctx context.Context) {
	for _, action := range s.actions.snapshot() {
		if action.cancel != nil {
			action.cancel()
		}

		s.finishAction(action, nativeActionOutcome{state: lifecycle.ActionCancelled})
	}

	_ = ctx
}

// resolveActionRequest answers OpenCode when this connection carries no lifecycle
// stream. The permission flow is unchanged by the absence of the extension: the
// host is still asked, and a host that cannot be asked still declines.
func (s *session) resolveActionRequest(ctx context.Context, action *pendingAction) error {
	if !s.actions.claim(action) {
		return nil
	}

	outcome, err := s.askHost(ctx, action)

	replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
	defer cancel()

	if _, held := s.actions.take(action.id); !held {
		return err
	}

	return errors.Join(err, s.replyNative(replyCtx, action, outcome))
}

// recordActionFailure latches an action failure onto the session's lifecycle
// state. A failure here is not a prompt's own failure — the action may belong to
// an agent-origin turn — so it is recorded where the owning cycle settles. Every
// caller reports a failure it already holds, so there is no nil error to guard.
func (s *session) recordActionFailure(err error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.cycle != nil && s.cycle.failure == nil {
		s.cycle.failure = err
	}
}
