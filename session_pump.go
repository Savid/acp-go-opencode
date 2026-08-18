package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

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
	session    *session
	client     opencode.Client
	generation uint64
	cancel     context.CancelFunc
	done       chan struct{}
	// released wakes the loop when a dispatch hold ends, so held events are
	// routed as soon as the boundary they were held for has passed.
	released chan struct{}
	held     []opencode.Event
}

// startPump binds a pump to the session's current runtime client. Each runtime
// generation gets its own pump: a recovered session installs a new client, a new
// lifecycle incarnation, and a new pump together.
func (s *session) startPump() {
	s.mu.Lock()
	client := s.client
	generation := s.runtimeGeneration
	previous := s.pump
	s.mu.Unlock()

	if previous != nil {
		previous.stop()
	}

	if client == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	pump := &sessionPump{
		session: s, client: client, generation: generation,
		cancel: cancel, done: make(chan struct{}), released: make(chan struct{}, 1),
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

// run drains the native stream for the life of the binding. Draining and routing
// are separate: the channel is always read, so a dispatch hold never stalls the
// native connection, and held events are routed in arrival order the moment the
// hold ends.
func (p *sessionPump) run(ctx context.Context) {
	defer close(p.done)
	defer recoverAgentGoroutine(ctx, agentLogger(p.session.agent), "OpenCode session pump")

	events := p.client.Events()
	errs := p.client.EventErrors()

	for {
		if len(p.held) > 0 && p.session.dispatchGate.TryLock() {
			event := p.held[0]
			p.held = p.held[1:]

			p.session.routeNativeEvent(ctx, event)
			p.session.dispatchGate.Unlock()

			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-p.released:
		case event := <-events:
			if len(p.held) >= heldEventCapacity {
				p.session.failStream(fmt.Errorf("held OpenCode event queue exceeded %d events", heldEventCapacity))

				return
			}

			p.held = append(p.held, event)

			// A dispatch waiting on a completion-reporting route learns from
			// this that the harness has started speaking for the session, which
			// is the admission it cannot read off that route's response. The
			// event itself is still routed in order, after the acceptance.
			if sessionID, named := opencode.EventSessionID(event); named && sessionID == p.session.idmap.NativeSessionID {
				p.session.noteDispatchEvidence()
			}
		case err := <-errs:
			p.session.handleStreamError(ctx, err)
		}
	}
}

// handleStreamError records one native stream failure. The epoch it names is
// suppressed so a reconnected stream cannot replay the failed epoch's parts, and
// an open cycle fails: the transport that would have proven its completion is
// gone.
func (s *session) handleStreamError(ctx context.Context, err error) {
	s.markStreamFailed(opencode.StreamErrorEpoch(err))

	s.lifecycleMu.Lock()
	cycle := s.cycle

	if cycle != nil && cycle.failure == nil {
		cycle.failure = acp.NewInternalError(turnFailedData(causeTransport, err.Error(), 0, ""))
	}
	s.lifecycleMu.Unlock()

	if cycle == nil {
		s.failPendingDispatch(acp.NewInternalError(turnFailedData(causeTransport, err.Error(), 0, "")))

		return
	}

	s.markNativeTerminal(ctx)
}

// failStream latches a delivery failure this session cannot report as an ordered
// event. It fences the incarnation, because a stream that lost an event can no
// longer prove its own contiguity.
func (s *session) failStream(err error) {
	s.fenceLifecycle(err.Error())
}

// routeNativeEvent routes one native event. Every event is filtered by the
// structured native session id it names, which is what keeps a directory-scoped
// stream from delivering another session's work — including a forked child in the
// same directory.
func (s *session) routeNativeEvent(ctx context.Context, event opencode.Event) {
	if s.shouldSuppressEvent(event) {
		return
	}

	// A notification caused by work a prompt submitted carries that prompt's
	// route envelope, which is the host's anti-stale turn authenticator. Work no
	// prompt submitted carries none: there is no client turn to authenticate
	// against, and inventing one would name a turn the host never opened.
	ctx = withTurnRoute(ctx, s.currentTurnNonce())

	// Raw events are non-authoritative debug output: a failed emit is recorded
	// internally and the authoritative session stream continues regardless.
	if err := s.emitRawOpenCodeEvent(ctx, event); err != nil && s.agent != nil && s.agent.log != nil {
		s.agent.log.DebugContext(ctx, "emit opencode raw event failed",
			slog.String("session_id", string(s.id)),
			slog.String("error", err.Error()),
		)
	}

	if sessionID, named := opencode.EventSessionID(event); named && sessionID != s.idmap.NativeSessionID {
		return
	}

	if err := s.applyNativeEvent(ctx, event); err != nil {
		s.failCycleDelivery(ctx, err)
	}
}

// failCycleDelivery ends the open cycle on an event the session could not apply
// or deliver. The ordered representation of the turn is broken at that point:
// the native work is interrupted, because output the session cannot report is
// work nobody asked to continue, and the failure is the cycle's terminal
// evidence rather than an idle the broken order would misreport.
func (s *session) failCycleDelivery(ctx context.Context, err error) {
	s.lifecycleMu.Lock()
	cycle := s.cycle

	if cycle == nil || cycle.settled {
		s.lifecycleMu.Unlock()
		s.recordNativeFailure(err)

		return
	}

	if cycle.failure == nil {
		cycle.failure = err
	}

	interrupt := !cycle.interrupted
	cycle.interrupted = true
	s.lifecycleMu.Unlock()

	if interrupt {
		s.interruptNativeWork(ctx)
	}

	s.markNativeTerminal(ctx)
}

// applyNativeEvent is the session's structured reading of the native event set.
// Nothing here infers a foreground boundary from silence, elapsed time, or a
// status poll: the native idle event is the only completion authority.
func (s *session) applyNativeEvent(ctx context.Context, event opencode.Event) error {
	switch event.Type {
	case opencode.EventServerConnected:
		return s.reconcileNativeActions(ctx)
	case opencode.EventSessionIdle:
		s.markNativeTerminal(ctx)

		return nil
	case opencode.EventSessionStatus:
		return s.applyNativeStatus(ctx, event)
	case opencode.EventSessionError:
		return s.applyNativeError(event)
	case opencode.EventMessageUpdated:
		s.applyNativeMessageInfo(event)

		return nil
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
	defer s.lifecycleMu.Unlock()

	if s.cycle != nil {
		return nil
	}

	_, err := s.openAgentCycleLocked(ctx)

	return err
}

// applyNativeError records one native turn failure against the open cycle. A
// failure arriving with no open cycle names no turn to fail, and the native stream
// re-publishes a turn's error after its idle, so a settled cycle keeps the outcome
// it already reported.
func (s *session) applyNativeError(event opencode.Event) error {
	var nativeError opencode.SessionError
	if err := json.Unmarshal(event.Properties, &nativeError); err != nil {
		return err
	}

	if nativeError.SessionID != s.idmap.NativeSessionID || nativeError.Error == nil {
		return nil
	}

	s.lifecycleMu.Lock()

	if s.cycle == nil {
		s.lifecycleMu.Unlock()

		// The error precedes acceptance: the run the pending frame started is
		// the only work it can name, so the awaiting dispatch fails with it.
		s.failPendingDispatch(opencode.AssistantErrorFromNativeError(nativeError.Error))

		return nil
	}

	defer s.lifecycleMu.Unlock()

	if s.cycle.settled || s.cycle.failure != nil {
		return nil
	}

	s.cycle.failure = opencode.AssistantErrorFromNativeError(nativeError.Error)

	return nil
}

// applyNativeMessageInfo records the role a message declared and, for an
// assistant message, the identity the settling turn reads its usage and stop
// reason from. OpenCode publishes this before any part event for that message.
func (s *session) applyNativeMessageInfo(event opencode.Event) {
	info, ok := eventMessageInfo(event.Properties)
	if !ok || info.SessionID != s.idmap.NativeSessionID {
		return
	}

	s.recordMessageRole(info)

	if info.Role != roleAssistant {
		return
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.cycle != nil {
		s.cycle.assistantID = info.ID
	}
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

// reconcileNativeActions re-reads the native pending sets. It runs when a stream
// connects, which is the one moment an action may have been asked while no
// consumer was attached.
func (s *session) reconcileNativeActions(ctx context.Context) error {
	client := s.currentClient()
	if client == nil {
		return nil
	}

	permissions, err := client.PendingPermissions(ctx)
	if err != nil {
		return err
	}

	for i := range permissions {
		if routeErr := s.routeNativePermission(ctx, permissions[i]); routeErr != nil {
			return routeErr
		}
	}

	questions, err := client.PendingQuestions(ctx)
	if err != nil {
		return err
	}

	for i := range questions {
		if err := s.routeNativeQuestion(ctx, questions[i]); err != nil {
			return err
		}
	}

	return nil
}

// markNativeTerminal records the terminal native evidence for the open cycle and
// hands settlement to whoever owns it: a foreground prompt settles its own turn
// after committing its prefix, and an agent-origin turn is settled here.
func (s *session) markNativeTerminal(ctx context.Context) {
	s.lifecycleMu.Lock()
	cycle := s.cycle

	if cycle == nil || cycle.settled {
		s.lifecycleMu.Unlock()

		return
	}

	cycle.idle = true
	cycle.wake()
	origin := cycle.origin
	s.lifecycleMu.Unlock()

	if origin == lifecycle.CauseSubmission {
		return
	}

	if err := s.settleAgentCycle(ctx, cycle); err != nil {
		s.recordNativeFailure(err)
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

// recordNativeFailure records a pump-side failure. The pump has no caller to
// answer, so a failure it cannot attribute to an open cycle is logged and a
// failure it can attribute settles that cycle as failed.
func (s *session) recordNativeFailure(err error) {
	if err == nil {
		return
	}

	s.lifecycleMu.Lock()
	cycle := s.cycle

	if cycle != nil && cycle.failure == nil {
		cycle.failure = err
	}
	s.lifecycleMu.Unlock()

	if cycle != nil || s.agent == nil || s.agent.log == nil {
		return
	}

	s.agent.log.DebugContext(context.Background(), "OpenCode session event failed outside a turn",
		slog.String("session_id", string(s.id)),
		slog.String("error", err.Error()),
	)
}
