package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// heldEventCapacity bounds the events one dispatch hold may accumulate. The hold
// exists for the length of one native dispatch acknowledgement, so reaching this
// bound means the native stream outran a boundary that should have been
// instantaneous. Overflow fails the session's stream closed rather than dropping
// an event: a dropped native event is indistinguishable from a native event that
// never happened.
const heldEventCapacity = 4096

// sessionPump is the session's own consumer of the native event stream. It is
// created with the native session's runtime binding and lives until that binding
// ends, so events arriving between prompts are routed rather than queued behind a
// prompt that may never come.
type sessionPump struct {
	session *session
	binding *nativeIncarnationBinding
	cancel  context.CancelFunc
	done    chan struct{}
	// released wakes the loop when a dispatch hold ends, so held events are
	// routed as soon as the boundary they were held for has passed.
	released chan struct{}
	// hold is the dispatch barrier. The pump acknowledges it only after every
	// event it has already received has immutable ownership, then stops receiving
	// until the dispatch gate is released.
	hold chan chan error
	held []queuedNativeEvent
	// afterReceive is a deterministic test barrier at the only scheduler gap the
	// dispatch barrier is required to close.
	afterReceive func()
	afterObserve func(opencode.Event, *nativeEventObservation)
	beforePause  func()
}

type queuedNativeEvent struct {
	event       opencode.Event
	observation *nativeEventObservation
}

type nativeEventOwnership uint8

const (
	nativeEventAgent nativeEventOwnership = iota
	nativeEventPromptBeforeEvidence
	nativeEventCycle
	// nativeEventInert marks an event this adapter has no structured reading
	// for. It is still delivered raw, but it owns nothing and settles nothing.
	nativeEventInert
)

// nativeEventObservation fixes ownership at the instant the pump accepts an
// event from the native stream. Later prompt admission can never rewrite an
// agent-owned observation, and an event seen before request-specific dispatch
// evidence cannot finish the newly reserved prompt.
type nativeEventObservation struct {
	ownership nativeEventOwnership
	binding   *nativeIncarnationBinding
	cycle     *foregroundCycle
}

// startPump binds a pump to the session's current runtime client. Each runtime
// generation gets its own pump: a recovered session installs a new client, a new
// lifecycle incarnation, and a new pump together.
func (s *session) startPump() {
	binding := s.currentIncarnation()
	s.mu.Lock()
	previous := s.pump
	s.mu.Unlock()

	if previous != nil {
		previous.stop()
	}

	if binding == nil || binding.client == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	pump := &sessionPump{
		session: s, binding: binding,
		cancel: cancel, done: make(chan struct{}), released: make(chan struct{}, 1),
		hold: make(chan chan error),
	}

	s.mu.Lock()
	s.pump = pump
	s.mu.Unlock()

	go pump.run(ctx)
}

// stopPump ends the session's pump and waits for it to finish, so no event is
// routed against a client this session no longer owns.
func (s *session) stopPump() {
	s.mu.Lock()
	pump := s.pump
	s.pump = nil
	s.mu.Unlock()

	pump.stop()
}

func (p *sessionPump) stop() {
	if p == nil {
		return
	}

	p.cancel()
	<-p.done
}

// wake tells the loop that a dispatch hold has ended.
func (p *sessionPump) wake() {
	if p == nil {
		return
	}

	select {
	case p.released <- struct{}{}:
	default:
	}
}

// pauseForDispatch establishes the receive-side half of prompt admission. Its
// acknowledgement means every event the pump received earlier has already
// fixed its owner, and the pump will receive nothing else until wake.
func (p *sessionPump) pauseForDispatch(ctx context.Context) error {
	if p == nil {
		return nil
	}

	if p.beforePause != nil {
		p.beforePause()
	}

	ack := make(chan error, 1)
	select {
	case p.hold <- ack:
	case <-p.done:
		return errors.New("native event pump stopped before dispatch")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-ack:
		return err
	case <-p.done:
		return errors.New("native event pump stopped during dispatch barrier")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run drains the native stream for the life of the binding. Draining and routing
// are separate: the channel is always read, so a dispatch hold never stalls the
// native connection, and held events are routed in arrival order the moment the
// hold ends.
func (p *sessionPump) run(ctx context.Context) {
	defer func() {
		close(p.done)

		if recover() != nil {
			failure := acp.NewInternalError(turnFailedData(causeTransport, "native event pump panicked", 0, ""))
			p.session.failNativeIncarnationCause(
				p.binding, "native event pump panicked", failure,
			)
		}
	}()

	stream := p.binding.client.EventStream()
	paused := false

	for {
		if paused {
			select {
			case <-ctx.Done():
				return
			case <-p.released:
				paused = false
			}

			continue
		}

		if len(p.held) > 0 && p.session.dispatchGate.TryLock() {
			event := p.held[0]
			p.held = p.held[1:]

			err := p.session.routeNativeEventForIncarnation(
				ctx, p.binding, event.event, event.observation,
			)
			p.session.dispatchGate.Unlock()

			if err != nil {
				p.session.failNativeIncarnation(p.binding, err)

				return
			}

			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-p.released:
		case ack := <-p.hold:
			paused = true

			ack <- nil
		case item := <-stream:
			if p.afterReceive != nil {
				p.afterReceive()
			}

			if item.Terminal != nil {
				p.session.handleStreamError(p.binding, item.Terminal)

				return
			}

			if item.Event == nil {
				p.session.handleStreamError(p.binding, errors.New("OpenCode event stream delivered an empty item"))

				return
			}

			event := *item.Event

			sessionID, named := opencode.EventSessionID(event)
			if !named || sessionID != p.session.idmap.NativeSessionID {
				continue
			}

			observation, current := p.session.observeNativeEvent(p.binding, event)
			if !current {
				continue
			}

			if p.afterObserve != nil {
				p.afterObserve(event, observation)
			}

			if len(p.held) >= heldEventCapacity {
				p.session.failNativeIncarnation(p.binding,
					fmt.Errorf("held OpenCode event queue exceeded %d events", heldEventCapacity))

				return
			}

			p.held = append(p.held, queuedNativeEvent{event: event, observation: observation})

			// Observation already claimed any authority this event carries. Only
			// the exact user-message identity can release a completion-reporting
			// dispatch; unrelated events merely remain ordered for later routing.
		}
	}
}

// handleStreamError fences the exact native incarnation whose ordered channel
// ended. A terminal marker follows every event the SSE reader accepted, so the
// marker proves where delivery stopped but never that the native work settled.
func (s *session) handleStreamError(binding *nativeIncarnationBinding, _ error) {
	failure := acp.NewInternalError(turnFailedData(causeTransport, "native event stream failed", 0, ""))
	s.failNativeIncarnationCause(binding, "native event stream failed", failure)
}

// routeNativeEvent routes one native event. Every event is filtered by the
// structured native session id it names, which is what keeps a directory-scoped
// stream from delivering another session's work — including a forked child in the
// same directory.
func (s *session) routeNativeEvent(ctx context.Context, event opencode.Event) {
	binding := s.currentIncarnation()

	observation, current := s.observeNativeEvent(binding, event)
	if !current {
		return
	}

	if err := s.routeNativeEventForIncarnation(ctx, binding, event, observation); err != nil {
		s.failNativeIncarnation(binding, err)
	}
}

func (s *session) routeNativeEventForIncarnation(
	ctx context.Context,
	binding *nativeIncarnationBinding,
	event opencode.Event,
	observations ...*nativeEventObservation,
) error {
	if !s.incarnationIsCurrent(binding) {
		return nil
	}

	var observation *nativeEventObservation
	if len(observations) > 0 {
		observation = observations[0]
	} else {
		observation, _ = s.observeNativeEvent(binding, event)
	}

	ctx = context.WithValue(withNativeIncarnationBinding(ctx, binding), nativeEventObservationKey{}, observation)
	if observation != nil && observation.cycle != nil && observation.cycle.origin == lifecycle.CauseSubmission {
		ctx = withTurnRoute(ctx, observation.cycle.turnNonce)
	}

	if observation != nil && observation.ownership == nativeEventAgent {
		defer s.completeAgentObservation(observation)
	}

	if sessionID, named := opencode.EventSessionID(event); named && sessionID != s.idmap.NativeSessionID {
		return nil
	}

	_ = s.emitRawOpenCodeEvent(ctx, event)

	if err := s.applyNativeEvent(ctx, event); err != nil {
		return err
	}

	return nil
}

type nativeEventObservationKey struct{}

func (s *session) observeNativeEvent(
	binding *nativeIncarnationBinding,
	event opencode.Event,
) (*nativeEventObservation, bool) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if binding == nil || s.incarnation != binding {
		return nil, false
	}

	observation := &nativeEventObservation{binding: binding}

	if !nativeEventDecoded(event.Type) {
		observation.ownership = nativeEventInert

		return observation, true
	}

	cycle := s.cycle
	if cycle == nil {
		observation.ownership = nativeEventAgent
		s.pendingAgentObservations++

		return observation, true
	}

	if cycle.origin == lifecycle.CauseSubmission && nativeEventProvesPrompt(event, cycle.nativeMessageID) &&
		!cycle.dispatchProven {
		cycle.dispatchProven = true
		close(cycle.dispatchEvidence)
	}

	if cycle.origin != lifecycle.CauseSubmission {
		observation.ownership = nativeEventCycle
		observation.cycle = cycle

		return observation, true
	}

	if nativeEventCausallyMatchesCycleLocked(event, cycle) {
		observation.ownership = nativeEventCycle
		observation.cycle = cycle

		return observation, true
	}

	if !cycle.dispatchProven {
		observation.ownership = nativeEventPromptBeforeEvidence

		return observation, true
	}

	// Every remaining same-session event has no identity tying it to the active
	// prompt and therefore cannot mutate or settle that prompt's cycle.
	observation.ownership = nativeEventAgent
	s.pendingAgentObservations++

	return observation, true
}

func nativeEventProvesPrompt(event opencode.Event, messageID string) bool {
	if messageID == "" || event.Type != opencode.EventMessageUpdated {
		return false
	}

	info, ok := eventMessageInfo(event.Properties)

	return ok && info.Role == roleUser && info.ID == messageID
}

func nativeEventCausallyMatchesCycleLocked(event opencode.Event, cycle *foregroundCycle) bool {
	if cycle == nil || cycle.origin != lifecycle.CauseSubmission {
		return false
	}

	if nativeEventProvesPrompt(event, cycle.nativeMessageID) {
		return true
	}

	switch event.Type {
	case opencode.EventSessionStatus, opencode.EventTodoUpdated:
		return cycle.dispatchProven
	case opencode.EventMessageUpdated:
		info, ok := eventMessageInfo(event.Properties)
		if !ok || info.Role != roleAssistant ||
			(info.ParentID != cycle.nativeMessageID && !cycle.ownsAssistant(info.ID)) {
			return false
		}

		cycle.adoptAssistant(info)

		return true
	case opencode.EventMessagePartCreated, opencode.EventMessagePartUpdated:
		part, _, ok := eventPartUpdate(event.Properties)
		if !ok {
			return false
		}

		// OpenCode echoes the parts of the dispatched user message back over the
		// stream after accepting it, so the prompt's own message addresses this
		// cycle just as much as any step the answer is built from.
		return part.MessageID == cycle.nativeMessageID || cycle.ownsAssistant(part.MessageID)
	case opencode.EventSessionIdle:
		return cycle.assistantTerminal || cycle.interrupted
	case opencode.EventSessionError:
		return cycle.assistantID != ""
	case opencode.EventPermissionV2Asked, opencode.EventPermissionAsked:
		var request opencode.PermissionRequest
		if json.Unmarshal(event.Properties, &request) != nil {
			return false
		}

		return cycle.claimsRequest(request.ToolCall().MessageID)
	case opencode.EventQuestionV2Asked, opencode.EventQuestionAsked:
		request, ok := eventQuestion(event.Properties)

		return ok && cycle.claimsRequest(request.Tool.MessageID)
	case opencode.EventPermissionV2Replied, opencode.EventPermissionReplied,
		opencode.EventQuestionV2Replied, opencode.EventQuestionReplied,
		opencode.EventQuestionV2Rejected, opencode.EventQuestionRejected:
		replied, ok := opencode.DecodeActionReplied(event.Properties)
		if !ok || cycle == nil {
			return false
		}

		return cycleOwnsAction(cycle, replied.RequestID)
	default:
		return false
	}
}

func cycleOwnsAction(cycle *foregroundCycle, actionID string) bool {
	if cycle == nil || actionID == "" {
		return false
	}

	_, ok := cycle.blockers[actionID]

	return ok
}

func (s *session) completeAgentObservation(observation *nativeEventObservation) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if observation != nil && observation.binding == s.incarnation && s.pendingAgentObservations > 0 {
		s.pendingAgentObservations--
	}
}

func nativeObservation(ctx context.Context) *nativeEventObservation {
	observation, _ := ctx.Value(nativeEventObservationKey{}).(*nativeEventObservation)

	return observation
}

// nativeEventDecoded reports whether one native event type has a structured
// reading below. OpenCode's bus carries far more than an ACP adapter reads — a
// single turn is punctuated by session.updated, and the bus also publishes
// session, project, catalog and integration notifications — so membership here is
// what separates an event that can name an owner from one that only passes
// through. A type applyNativeEvent reads without naming here is observed inert,
// so its reading runs against no cycle — for a settle-shaped type that silently
// discards the evidence that ends a turn. The two sets are therefore held equal
// by test rather than by care.
func nativeEventDecoded(eventType string) bool {
	switch eventType {
	case opencode.EventSessionIdle, opencode.EventSessionStatus, opencode.EventSessionError,
		opencode.EventMessageUpdated, opencode.EventMessagePartCreated, opencode.EventMessagePartUpdated,
		opencode.EventTodoUpdated,
		opencode.EventPermissionV2Asked, opencode.EventPermissionAsked,
		opencode.EventQuestionV2Asked, opencode.EventQuestionAsked,
		opencode.EventPermissionV2Replied, opencode.EventPermissionReplied,
		opencode.EventQuestionV2Replied, opencode.EventQuestionReplied,
		opencode.EventQuestionV2Rejected, opencode.EventQuestionRejected:
		return true
	default:
		return false
	}
}

// applyNativeEvent is the session's structured reading of the native event set.
// Nothing here infers a foreground boundary from silence, elapsed time, or a
// status poll: the native idle event is the only completion authority.
func (s *session) applyNativeEvent(ctx context.Context, event opencode.Event) error {
	if observation := nativeObservation(ctx); observation != nil &&
		observation.ownership == nativeEventPromptBeforeEvidence {
		switch event.Type {
		case opencode.EventSessionError,
			opencode.EventPermissionV2Asked, opencode.EventPermissionAsked,
			opencode.EventQuestionV2Asked, opencode.EventQuestionAsked,
			opencode.EventPermissionV2Replied, opencode.EventPermissionReplied,
			opencode.EventQuestionV2Replied, opencode.EventQuestionReplied,
			opencode.EventQuestionV2Rejected, opencode.EventQuestionRejected:
		default:
			return nil
		}
	}

	switch event.Type {
	case opencode.EventSessionIdle:
		return s.markNativeTerminal(ctx)
	case opencode.EventSessionStatus:
		return s.applyNativeStatus(ctx, event)
	case opencode.EventSessionError:
		return s.applyNativeError(ctx, event)
	case opencode.EventMessageUpdated:
		return s.applyNativeMessageInfo(ctx, event)
	case opencode.EventMessagePartCreated, opencode.EventMessagePartUpdated:
		return s.applyNativePart(ctx, event)
	case opencode.EventTodoUpdated:
		return s.applyNativeTodos(ctx, event)
	case opencode.EventPermissionV2Asked, opencode.EventPermissionAsked:
		return s.applyNativePermissionAsked(ctx, event)
	case opencode.EventQuestionV2Asked, opencode.EventQuestionAsked:
		return s.applyNativeQuestionAsked(ctx, event)
	case opencode.EventPermissionV2Replied, opencode.EventPermissionReplied,
		opencode.EventQuestionV2Replied, opencode.EventQuestionReplied,
		opencode.EventQuestionV2Rejected, opencode.EventQuestionRejected:
		s.applyNativeActionReplied(ctx, event)

		return nil
	default:
		return nil
	}
}

// applyNativeStatus reads one native status transition. Busy with no open cycle is
// native work no submission caused, which opens an agent-origin turn; busy inside
// an open cycle is that cycle continuing.
func (s *session) applyNativeStatus(ctx context.Context, event opencode.Event) error {
	status, ok := opencode.DecodeSessionStatus(event.Properties)
	if !ok || status.Status.Type != opencode.SessionStatusBusy {
		return nil
	}

	s.lifecycleMu.Lock()

	cycle, err := s.observedCycleLocked(ctx, true)
	if err != nil {
		s.lifecycleMu.Unlock()

		return err
	}

	if cycle != nil {
		cycle.runStarted = true
		s.lifecycleMu.Unlock()

		return nil
	}
	s.lifecycleMu.Unlock()

	return nil
}

// applyNativeError records one native turn failure against the open cycle. A
// failure arriving with no open cycle names no turn to fail, and the native stream
// re-publishes a turn's error after its idle, so a settled cycle keeps the outcome
// it already reported.
func (s *session) applyNativeError(ctx context.Context, event opencode.Event) error {
	var nativeError opencode.SessionError
	if err := json.Unmarshal(event.Properties, &nativeError); err != nil {
		return err
	}

	if nativeError.SessionID != s.idmap.NativeSessionID || nativeError.Error == nil {
		return nil
	}

	s.lifecycleMu.Lock()

	cycle, _ := s.observedCycleLocked(ctx, false)

	if cycle == nil {
		s.lifecycleMu.Unlock()

		// The error precedes acceptance: the run the pending frame started is
		// the only work it can name, so the awaiting dispatch fails with it.
		s.failPendingDispatch(opencode.AssistantErrorFromNativeError(nativeError.Error))

		return nil
	}

	defer s.lifecycleMu.Unlock()

	if cycle.settled || cycle.failure != nil {
		return nil
	}

	cycle.failure = opencode.AssistantErrorFromNativeError(nativeError.Error)

	// A failure against a run the native session never started is the whole of
	// that frame's story. OpenCode refuses an agent it cannot resolve before it
	// creates the message for the frame: it publishes no message, no status, and
	// so it never idles the cycle either. Waiting for an idle that cannot come
	// would hold the turn open forever, so the cycle ends on the evidence it has.
	// This is not a boundary read out of silence — the native runtime spoke, and
	// what it said was that nothing is running.
	cycle.refused = true
	cycle.wake()

	return nil
}

// markCycleRunning records that the native session has begun speaking for the
// open cycle: the frame became a message, or the run it scheduled reported
// itself busy. Either one proves there is a run for a later failure to name.
func (s *session) markCycleRunning(ctx context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	cycle, err := s.observedCycleLocked(ctx, true)
	if err != nil {
		return err
	}

	if cycle != nil {
		cycle.runStarted = true
	}

	return nil
}

// applyNativeMessageInfo records the role a message declared and, for an
// assistant message, adopts it as a step of the open cycle. OpenCode publishes
// this before any part event for that message.
func (s *session) applyNativeMessageInfo(ctx context.Context, event opencode.Event) error {
	info, ok := eventMessageInfo(event.Properties)
	if !ok || info.SessionID != s.idmap.NativeSessionID {
		return nil
	}

	s.recordMessageRole(info)

	// The message OpenCode created for the frame is the first proof that the
	// frame became a run.
	if err := s.markCycleRunning(ctx); err != nil {
		return err
	}

	if info.Role != roleAssistant {
		return nil
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	cycle, _ := s.observedCycleLocked(ctx, false)

	if cycle != nil {
		cycle.adoptAssistant(info)
	}

	return nil
}

func (s *session) applyNativePart(ctx context.Context, event opencode.Event) error {
	part, delta, ok := eventPartUpdate(event.Properties)
	if !ok || part.SessionID != s.idmap.NativeSessionID {
		return nil
	}

	// The native stream echoes the just-posted user message parts; the ACP
	// client already owns that content, so only non-user parts are forwarded.
	if s.messageRole(part.MessageID) == roleUser {
		return nil
	}

	s.markActiveMessageID(part.MessageID)

	return s.emitPartUpdates(ctx, roleAssistant, part, delta, false)
}

func (s *session) applyNativeTodos(ctx context.Context, event opencode.Event) error {
	var payload struct {
		SessionID string                `json:"sessionID"` //nolint:tagliatelle // OpenCode native event payloads use sessionID wire names.
		Todos     []opencode.NativeTodo `json:"todos"`
	}
	if err := json.Unmarshal(event.Properties, &payload); err != nil || payload.SessionID != s.idmap.NativeSessionID {
		return nil //nolint:nilerr // a malformed or foreign todo payload is skipped, never fatal
	}

	return s.emitPlan(ctx, payload.Todos)
}

func (s *session) applyNativePermissionAsked(ctx context.Context, event opencode.Event) error {
	var req opencode.PermissionRequest
	if err := json.Unmarshal(event.Properties, &req); err != nil {
		return err
	}

	if event.Type == opencode.EventPermissionV2Asked {
		req.ReplyRoute = opencode.PermissionRouteAPI
	} else {
		req.ReplyRoute = opencode.PermissionRouteSession
	}

	if observation := nativeObservation(ctx); observation != nil && observation.ownership == nativeEventPromptBeforeEvidence {
		s.declineNativePermission(ctx, req)

		return nil
	}

	return s.routeNativePermission(ctx, req)
}

func (s *session) applyNativeQuestionAsked(ctx context.Context, event opencode.Event) error {
	req, ok := eventQuestion(event.Properties)
	if !ok {
		return errors.New("invalid OpenCode question event")
	}

	if event.Type == opencode.EventQuestionV2Asked {
		req.ReplyRoute = opencode.QuestionRouteAPI
	} else {
		req.ReplyRoute = opencode.QuestionRouteSession
	}

	if observation := nativeObservation(ctx); observation != nil && observation.ownership == nativeEventPromptBeforeEvidence {
		s.declineNativeQuestion(ctx, req)

		return nil
	}

	return s.routeNativeQuestion(ctx, req)
}

// applyNativeActionReplied terminalizes an action the harness resolved itself.
// Consuming it is what keeps a policy answer, a native client's answer, or a
// withdrawal from leaving the foreground blocked on a request nothing is waiting
// for any more.
func (s *session) applyNativeActionReplied(ctx context.Context, event opencode.Event) {
	replied, ok := opencode.DecodeActionReplied(event.Properties)
	if !ok || replied.SessionID != s.idmap.NativeSessionID {
		return
	}

	s.nativeActionResolved(ctx, replied, nativeRepliedActionState(event.Type, replied))
}

// nativeRepliedActionState maps one native resolution onto the terminal action
// state it proves. A withdrawn question was never answered, which is a
// cancellation rather than a refusal.
func nativeRepliedActionState(eventType string, replied opencode.ActionRepliedEvent) lifecycle.ActionState {
	switch eventType {
	case opencode.EventQuestionV2Rejected, opencode.EventQuestionRejected:
		return lifecycle.ActionCancelled
	case opencode.EventPermissionV2Replied, opencode.EventPermissionReplied:
		if replied.Accepted() {
			return lifecycle.ActionAccepted
		}

		return lifecycle.ActionDeclined
	default:
		return lifecycle.ActionAccepted
	}
}

// markNativeTerminal records the terminal native evidence for the open cycle and
// hands settlement to whoever owns it: a foreground prompt settles its own turn
// after committing its prefix, and an agent-origin turn is settled here.
func (s *session) markNativeTerminal(ctx context.Context) error {
	s.lifecycleMu.Lock()

	cycle, _ := s.observedCycleLocked(ctx, false)

	if cycle == nil || cycle.settled {
		s.lifecycleMu.Unlock()

		return nil
	}

	cycle.idle = true
	cycle.wake()
	origin := cycle.origin
	s.lifecycleMu.Unlock()

	if origin == lifecycle.CauseSubmission {
		return nil
	}

	if err := s.settleAgentCycle(ctx, cycle); err != nil {
		s.recordCycleFailure(cycle, err)

		return err
	}

	return nil
}

//nolint:nilnil // A fixed observation may intentionally own no foreground cycle.
func (s *session) observedCycleLocked(ctx context.Context, openAgent bool) (*foregroundCycle, error) {
	observation := nativeObservation(ctx)
	if observation == nil {
		return s.cycle, nil
	}

	if observation.binding != s.incarnation {
		return nil, nil
	}

	switch observation.ownership {
	case nativeEventPromptBeforeEvidence, nativeEventInert:
		return nil, nil
	case nativeEventCycle:
		if observation.cycle != nil && s.cycle == observation.cycle {
			return observation.cycle, nil
		}

		return nil, nil
	case nativeEventAgent:
		if observation.cycle != nil {
			if s.cycle == observation.cycle {
				return observation.cycle, nil
			}

			return nil, nil
		}

		if s.cycle != nil && s.cycle.origin == lifecycle.CauseActivity {
			observation.cycle = s.cycle

			return s.cycle, nil
		}

		if !openAgent {
			return nil, nil
		}

		if s.cycle != nil {
			return nil, nil
		}

		cycle, err := s.openAgentCycleLocked(ctx)
		if err != nil {
			return nil, err
		}

		observation.cycle = cycle

		return cycle, nil
	default:
		return nil, nil
	}
}

// settleAgentCycle ends an agent-origin turn. It obeys the same ordering a
// prompt-origin turn does: blockers terminalize, the native-safe prefix commits,
// and only then does the one ending transition go out.
func (s *session) settleAgentCycle(ctx context.Context, cycle *foregroundCycle) error {
	s.cancelActions(ctx)

	outcome, stopReason := lifecycle.OutcomeSuccess, string(acp.StopReasonEndTurn)
	if cycle.failure != nil {
		outcome, stopReason = lifecycle.OutcomeFailed, ""
	}

	if err := s.commitForegroundPrefix(ctx); err != nil {
		s.fenceLifecycle(fmt.Sprintf("native-safe prefix commit failed: %v", err))

		return err
	}

	return s.settleCycle(ctx, cycle, outcome, stopReason)
}
