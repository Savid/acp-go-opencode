package opencodeacp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/observer"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	fieldPrompt         = "prompt"
	fieldPromptResource = "prompt.resource"

	updateUserMessageChunk  = "user_message_chunk"
	updateAgentMessageChunk = "agent_message_chunk"
	partTypeText            = "text"
	partTypeReasoning       = "reasoning"
	partTypeFile            = "file"
	partTypeTool            = "tool"
	partTypeStepFinish      = "step-finish"
	mediaTypeImage          = "image"
	roleUser                = "user"
	roleAssistant           = "assistant"
	defaultMimeType         = "application/octet-stream"

	permissionReplyOnce   = "once"
	permissionReplyAlways = "always"
	permissionReplyReject = "reject"
	reasonCancelled       = "cancelled"

	questionWrapperKey      = "question"
	fallbackQuestionID      = "question_1"
	schemaTypeString        = "string"
	questionFallbackMessage = "OpenCode needs input"

	finishReasonLength     = "length"
	nativeStatusPending    = "pending"
	nativeStatusInProgress = "in_progress"
	nativeStatusCompleted  = "completed"
	nativeStatusSuccess    = "success"
	nativeStatusFailed     = "failed"
	nativeStatusError      = "error"
	nativeToolFailed       = "native tool failed"
	toolNameRead           = "read"
	toolNameEdit           = "edit"
	toolNameDelete         = "delete"
	toolNameBash           = "bash"
	priorityHigh           = "high"
	priorityLow            = "low"
)

var errTurnAssistantIdentityMissing = errors.New("OpenCode turn assistant identity is missing")

const (
	turnFailedErrorTag = "opencode_turn_failed"
	causeProvider      = "provider"
	causeTransport     = "transport"
	causeTimeout       = "timeout"
)

// turnFailedData builds the uniform ACP error Data payload for a native
// OpenCode turn failure. Only closed classifications cross the ACP boundary;
// native/provider bodies and arbitrary handler causes stay internal.
func turnFailedData(cause, _ string, statusCode int, _ string) map[string]any {
	message := "OpenCode turn failed"

	switch cause {
	case causeProvider:
		message = "OpenCode provider request failed"
	case causeTransport:
		message = "OpenCode transport failed"
	case causeTimeout:
		message = "OpenCode turn timed out"
	}

	data := map[string]any{
		jsonFieldError:   turnFailedErrorTag,
		jsonFieldCause:   cause,
		jsonFieldMessage: message,
	}
	if statusCode > 0 {
		data[jsonFieldStatusCode] = statusCode
	}

	return data
}

// assistantErrorData builds the uniform turn-failure Data payload for a native
// OpenCode assistant/provider failure (cause "provider").
// structuredOutputRequested reports whether this turn sent a native
// output-format schema.
func assistantErrorData(err *opencode.AssistantError, structuredOutputRequested bool) map[string]any {
	data := turnFailedData(causeProvider, err.Detail(), err.StatusCode(), err.ProviderCode())
	data[jsonFieldStructuredOutputRequested] = structuredOutputRequested

	return data
}

// mergeAssistantErrorFields adds the closed machine-readable assistant-error
// status and structured-output marker onto an existing ACP error Data map.
func mergeAssistantErrorFields(data map[string]any, err *opencode.AssistantError, structuredOutputRequested bool) {
	data[jsonFieldStructuredOutputRequested] = structuredOutputRequested
	if err.StatusCode() > 0 {
		data[jsonFieldStatusCode] = err.StatusCode()
	}
}

func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (resp acp.PromptResponse, err error) {
	// The route envelope is the anti-stale turn authenticator and the lifecycle
	// value is submission identity: neither is derived from the other, the route
	// is validated first so a prompt never reports two rejections, and both
	// verdicts are reached before anything is dispatched to the harness.
	route, err := parseInboundTurnRoute(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	submission, err := a.promptSubmission(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	ctx, finish := a.observe.StartPrompt(ctx, params.Meta, session.currentModel())
	defer func() { finish(promptResultForObserver(resp, err, session.currentModel())) }()

	resp, err = session.promptWithRoute(ctx, params, route.TurnNonce, submission)

	return resp, err
}

func promptResultForObserver(resp acp.PromptResponse, err error, model string) observer.PromptResult {
	result := observer.PromptResult{
		Err:        err,
		Model:      model,
		StopReason: string(resp.StopReason),
	}
	if resp.Usage == nil {
		return result
	}

	result.InputTokens = resp.Usage.InputTokens
	result.OutputTokens = resp.Usage.OutputTokens
	result.TotalTokens = resp.Usage.TotalTokens

	if resp.Usage.CachedReadTokens != nil {
		result.CachedReadTokens = *resp.Usage.CachedReadTokens
	}

	if resp.Usage.CachedWriteTokens != nil {
		result.CachedWriteTokens = *resp.Usage.CachedWriteTokens
	}

	if resp.Usage.ThoughtTokens != nil {
		result.ThoughtTokens = *resp.Usage.ThoughtTokens
	}

	return result
}

func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	route, err := parseInboundTurnRoute(params.Meta)
	if err != nil {
		return err
	}

	// A cancel carries no lifecycle value. The refusal lands before the native
	// interrupt and before any local turn state moves, so the cancel is never
	// applied; being a notification it carries no response frame, which makes the
	// refusal wire-silent rather than absent.
	if refusal := refuseLifecycleMeta(params.Meta); refusal != nil {
		return refusal
	}

	session, err := a.session(params.SessionId)
	if err != nil {
		return err
	}

	if poisonErr := session.ensureNotPoisoned(); poisonErr != nil {
		return poisonErr
	}

	if err := session.requireActiveTurn(route.TurnNonce); err != nil {
		return err
	}

	// Cancellation is session-scoped. It interrupts the addressed native session
	// and returns; the turn's own settlement — every blocking action cancelled,
	// the native-safe prefix committed, the terminal transition emitted — is
	// reported by the prompt this cancel addressed, on its own response.
	cancelCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	return session.cancelTurn(cancelCtx)
}

func (s *session) promptWithRoute(
	ctx context.Context,
	params acp.PromptRequest,
	turnNonce string,
	submission lifecycle.Submission,
) (acp.PromptResponse, error) {
	if err := s.requireEstablished(ctx); err != nil {
		return acp.PromptResponse{}, err
	}

	if err := s.ensureNotPoisoned(); err != nil {
		return acp.PromptResponse{}, err
	}

	acquire := s.acquireTurn

	_, slashCandidate := slashCommandInvocation(params.Prompt)
	if slashCandidate {
		acquire = s.acquireCommandTurn
	}

	release, err := acquire(ctx)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	// A crashed runtime retires its exact generation asynchronously. Recover
	// before publishing this turn's cancellation handle so the retiring
	// generation cannot cancel a prompt that has not touched it.
	if recoveryErr := s.ensureRuntime(ctx); recoveryErr != nil {
		return acp.PromptResponse{}, recoveryErr
	}

	prepareCtx := withTurnRoute(ctx, turnNonce)

	invocation, command, matchedCommand, err := s.resolvePromptCommand(prepareCtx, params)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	var dispatch nativeDispatch

	if matchedCommand {
		dispatch, err = s.commandDispatch(prepareCtx, params, invocation, command)
	} else {
		dispatch, err = s.messageDispatch(prepareCtx, params)
	}

	if err != nil {
		return acp.PromptResponse{}, err
	}

	if err := s.refreshLifecycleMCP(prepareCtx); err != nil {
		return acp.PromptResponse{}, err
	}

	return s.runPromptTurn(ctx, params, turnNonce, submission, dispatch)
}

func (s *session) resolvePromptCommand(ctx context.Context, params acp.PromptRequest) (slashCommandPrompt, opencode.NativeCommand, bool, error) {
	invocation, slashCandidate := slashCommandInvocation(params.Prompt)

	_, matchedBeforeRefresh := s.cachedCommand(invocation.name)
	if slashCandidate {
		if err := s.refreshCommands(ctx); err != nil && s.agent != nil && s.agent.log != nil {
			s.agent.log.DebugContext(ctx, "refresh OpenCode commands before prompt failed", slog.String("session_id", string(s.id)))
		}
	}

	command, matchedCommand := s.cachedCommand(invocation.name)
	if slashCandidate && matchedBeforeRefresh && !matchedCommand {
		return slashCommandPrompt{}, opencode.NativeCommand{}, false, acp.NewInvalidParams(map[string]any{
			jsonFieldError:   "opencode_command_removed",
			jsonFieldMessage: "The selected OpenCode command is no longer available",
		})
	}

	return invocation, command, matchedCommand, nil
}

// nativeDispatch is one prepared native frame and the boundary its route
// acknowledges at. The async prompt route answers when the native dispatcher has
// accepted the frame, which is an acknowledgement; the command route answers only
// when the run it started has finished, which is a completion report. The
// difference is stated here rather than inferred at the call site.
type nativeDispatch struct {
	send    func(context.Context) error
	command opencode.NativeCommand
	matched bool
	// reportsCompletion marks a route whose response is the run's outcome. Such
	// a route cannot acknowledge admission, so acceptance comes from the first
	// native event the addressed session publishes for the frame.
	reportsCompletion bool
	// messageID is the exact native user-message identity carried by this
	// request. Only an observed native event naming this identity can prove a
	// completion-reporting route accepted this particular frame.
	messageID string
}

func (s *session) commandDispatch(ctx context.Context, params acp.PromptRequest, invocation slashCommandPrompt, command opencode.NativeCommand) (nativeDispatch, error) {
	handoff, err := s.validatePromptMedia(ctx, params.Prompt[1:])
	if err != nil {
		return nativeDispatch{}, err
	}

	parts, err := commandPromptParts(params.Prompt[1:], handoff)
	if err != nil {
		return nativeDispatch{}, err
	}

	agent, model := s.commandContext()

	req := opencode.CommandRequest{
		Agent:     agent,
		Model:     model,
		Command:   command.Name,
		Arguments: invocation.arguments,
		Parts:     parts,
	}

	messageID, err := nativePromptMessageID()
	if err != nil {
		return nativeDispatch{}, err
	}

	req.MessageID = messageID

	return nativeDispatch{
		send: func(turnCtx context.Context) error {
			return s.client.DispatchCommand(turnCtx, s.idmap.NativeSessionID, req)
		},
		command:           command,
		matched:           true,
		reportsCompletion: true,
		messageID:         messageID,
	}, nil
}

func (s *session) messageDispatch(ctx context.Context, params acp.PromptRequest) (nativeDispatch, error) {
	handoff, err := s.validatePromptMedia(ctx, params.Prompt)
	if err != nil {
		return nativeDispatch{}, err
	}

	parts, err := promptToOpenCodeParts(params.Prompt, handoff)
	if err != nil {
		return nativeDispatch{}, err
	}

	modelSelector, hasModel := s.modelSelector()

	req := opencode.MessageRequest{
		Parts: parts,
		Agent: s.currentMode(),
	}
	if len(s.outputSchema) > 0 {
		req.Format = &opencode.OutputFormat{
			Type:   opencode.OutputFormatJSONSchema,
			Schema: cloneAnyMap(s.outputSchema),
		}
	}

	if hasModel {
		req.Model = &modelSelector
	}

	messageID, err := nativePromptMessageID()
	if err != nil {
		return nativeDispatch{}, err
	}

	req.MessageID = messageID

	return nativeDispatch{
		send: func(turnCtx context.Context) error {
			return s.client.DispatchMessage(turnCtx, s.idmap.NativeSessionID, req)
		},
		messageID: messageID,
	}, nil
}

// nativeMessageIDEntropy is the randomness the native user-message identifier
// of a prompt is minted from.
var nativeMessageIDEntropy io.Reader = rand.Reader

func nativePromptMessageID() (string, error) {
	id, err := opencode.NewMessageID(nativeMessageIDEntropy)
	if err != nil {
		return "", fmt.Errorf("create native prompt message id: %w", err)
	}

	return id, nil
}

// nativeTurnEnd is the terminal evidence one prompt-origin turn ended on. Exactly
// one of its fields decides the outcome, and none of them is a timer over silence:
// cancellation and the deadline are this adapter's own decisions, and everything
// else came from the native stream.
type nativeTurnEnd struct {
	cancelled bool
	timedOut  bool
	// lost records that the incarnation ended without the native terminal signal
	// the turn needed, so nothing further may be emitted on its stream.
	lost error
}

func (s *session) runPromptTurn(
	ctx context.Context,
	params acp.PromptRequest,
	turnNonce string,
	submission lifecycle.Submission,
	dispatch nativeDispatch,
) (acp.PromptResponse, error) {
	cycle, turnCtx, completion, err := s.dispatchAndAccept(ctx, turnNonce, submission, dispatch)
	if err != nil {
		// A dying host request context withdraws the frame before acceptance:
		// there is nobody left to answer, and the withdrawal is reported as the
		// cancellation it is rather than as a failure of the route.
		if !s.wasCancelled() && s.takePendingDispatchFailure() == nil && ctx.Err() != nil {
			s.finishTurn()

			return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
		}

		// Nothing was accepted, so no submission and no turn exist. The native
		// refusal is reported as it is, and if the frame was in fact admitted
		// without this call learning so, the session's own event stream opens an
		// agent-origin turn for it rather than a silently accepted one.
		classified := s.classifyDispatchFailure(turnCtx, err, dispatch)
		s.finishTurn()

		return acp.PromptResponse{}, classified
	}
	defer s.finishTurn()

	end := s.awaitPromptTerminal(ctx, turnCtx, cycle, completion)

	return s.completePromptTurn(ctx, turnCtx, params, cycle, end, dispatch)
}

// dispatchAndAccept posts the frame and publishes acceptance. The event gate is
// held across both, so every native event caused by the frame is routed after the
// acceptance that explains it and none is dropped waiting for it.
func (s *session) dispatchAndAccept(
	ctx context.Context,
	turnNonce string,
	submission lifecycle.Submission,
	dispatch nativeDispatch,
) (*foregroundCycle, context.Context, <-chan error, error) {
	s.dispatchGate.Lock()

	s.mu.Lock()
	pump := s.pump
	s.mu.Unlock()

	if err := pump.pauseForDispatch(ctx); err != nil {
		s.releaseDispatchGate()

		return nil, withTurnRoute(ctx, turnNonce), nil, err
	}

	reservationCtx := withTurnRoute(ctx, turnNonce)

	cycle, turnCtx, err := s.reservePromptCycle(reservationCtx, dispatch.messageID, submission)
	if err != nil {
		s.releaseDispatchGate()

		return nil, reservationCtx, nil, err
	}

	// The reservation now owns every request-specific event the native POST can
	// publish synchronously. Resume receiving while routing remains held behind
	// dispatchGate, otherwise an unbuffered native stream could deadlock the POST
	// before it acknowledges admission.
	pump.wake()

	completion, err := s.sendReservedNativeFrame(turnCtx, cycle, dispatch)
	if err != nil {
		s.abandonPromptCycle(cycle)
		s.releaseDispatchGate()

		return nil, turnCtx, nil, err
	}

	err = s.acceptPromptCycle(turnCtx, cycle, submission)
	s.releaseDispatchGate()

	if err != nil {
		s.finishTurn()

		return nil, turnCtx, nil, err
	}

	return cycle, turnCtx, completion, nil
}

// releaseDispatchGate ends the hold and wakes the pump, so events held for the
// dispatch boundary are routed immediately rather than at the next arrival.
func (s *session) releaseDispatchGate() {
	s.dispatchGate.Unlock()

	s.mu.Lock()
	pump := s.pump
	s.mu.Unlock()

	pump.wake()
}

// sendReservedNativeFrame posts the frame and returns once the harness has taken
// it. On a completion-reporting route the post runs on its own goroutine and only
// observation of the exact user-message identity proves admission; the returned
// channel then carries that route's completion report.
func (s *session) sendReservedNativeFrame(
	turnCtx context.Context,
	cycle *foregroundCycle,
	dispatch nativeDispatch,
) (<-chan error, error) {
	// The turn deadline bounds the dispatch boundary too: a route that never
	// acknowledges admission would otherwise hold the prompt past the deadline
	// the turn was configured with.
	timeout := s.turnTimeout()

	var timeoutC <-chan time.Time

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		timeoutC = timer.C
	}

	result := make(chan error, 1)

	go func() {
		result <- sendRecoveringPanic(turnCtx, dispatch)
	}()

	for {
		select {
		case err := <-result:
			s.lifecycleMu.Lock()
			proven := s.cycle == cycle && cycle.dispatchProven
			s.lifecycleMu.Unlock()

			if err != nil {
				if proven {
					completion := make(chan error, 1)
					completion <- err

					return completion, nil
				}

				return nil, err
			}

			if !dispatch.reportsCompletion {
				// The async route's complete successful response is its durable
				// admission acknowledgement. Native idle remains terminal authority.
				return make(chan error), nil
			}

			// A command response is completion, never admission. If observation of
			// its exact user-message identity lost the select race, use that already
			// recorded proof; otherwise keep waiting for that request-specific event.
			if proven {
				completion := make(chan error, 1)
				completion <- nil

				return completion, nil
			}

			result = nil
		case <-cycle.dispatchEvidence:
			failures := make(chan error, 1)

			if result != nil {
				go func(result <-chan error, reportsCompletion bool) {
					err := <-result
					if err != nil || reportsCompletion {
						failures <- err
					}
				}(result, dispatch.reportsCompletion)
			}

			return failures, nil
		case <-turnCtx.Done():
			return nil, turnCtx.Err()
		case <-timeoutC:
			return nil, fmt.Errorf("native dispatch acknowledged nothing before the turn deadline: %w", context.DeadlineExceeded)
		}
	}
}

// sendRecoveringPanic posts one frame on the caller's goroutine, converting a
// panic in the native route into the dispatch failure it is: the prompt must
// answer its caller, and a crashed route answers nothing on its own.
func sendRecoveringPanic(turnCtx context.Context, dispatch nativeDispatch) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.New("native dispatch panicked")
		}
	}()

	return dispatch.send(turnCtx)
}

// classifyDispatchFailure maps a pre-acceptance failure onto its ACP error. A
// refusal from the native route is the caller's answer, and any failure observed
// after the host cancelled is reported as the cancellation: the interrupt is
// what broke the dispatch, and the route's own error text merely describes how.
func (s *session) classifyDispatchFailure(ctx context.Context, err error, dispatch nativeDispatch) error {
	var requestErr *acp.RequestError
	if errors.As(err, &requestErr) {
		return err
	}

	if s.wasCancelled() {
		return acp.NewInvalidRequest(map[string]any{
			jsonFieldError:   "opencode_prompt_cancelled_before_dispatch",
			jsonFieldMessage: "OpenCode prompt was cancelled before dispatch",
		})
	}

	// A native failure that arrived while the frame awaited acceptance is the
	// real cause; the route's own error is just the released context.
	if pending := s.takePendingDispatchFailure(); pending != nil {
		return s.classifyTurnFailure(context.WithoutCancel(ctx), pending, dispatch)
	}

	if s.turnTimeout() > 0 && errors.Is(err, context.DeadlineExceeded) {
		s.interruptNativeWork(context.WithoutCancel(ctx))

		return acp.NewInternalError(turnFailedData(
			causeTimeout, fmt.Sprintf("turn exceeded %s deadline", s.turnTimeout()), 0, "",
		))
	}

	if runtimeErr := s.runtimeFailure(); runtimeErr != nil {
		return runtimeErr
	}

	if dispatch.matched && opencode.IsBadRequest(err) {
		return s.classifyTurnFailure(context.WithoutCancel(ctx), err, dispatch)
	}

	var assistantErr *opencode.AssistantError
	if errors.As(err, &assistantErr) {
		return acp.NewInternalError(assistantErrorData(assistantErr, len(s.outputSchema) > 0))
	}

	if opencode.IsBadRequest(err) {
		return acp.NewInvalidParams(map[string]any{
			jsonFieldError:   "opencode_prompt_rejected",
			jsonFieldMessage: "OpenCode rejected the prompt",
		})
	}

	return acp.NewInternalError(turnFailedData(causeTransport, err.Error(), 0, ""))
}

// awaitPromptTerminal waits for the evidence that ends the accepted turn. The
// native idle event is the completion authority; the other three cases are the
// adapter's own decisions — a cancellation, a configured deadline, and the loss of
// the incarnation that would have reported completion.
func (s *session) awaitPromptTerminal(
	ctx context.Context,
	turnCtx context.Context,
	cycle *foregroundCycle,
	completion <-chan error,
) nativeTurnEnd {
	timeout := s.turnTimeout()

	var timeoutC <-chan time.Time

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		timeoutC = timer.C
	}

	select {
	case <-cycle.signal:
		// The cancel guard runs here too: a cancel and the terminal evidence its
		// own interrupt produced can arrive together, and the turn the host
		// cancelled resolves to cancelled no matter which wakes the select.
		if s.wasCancelled() {
			return s.settleCancelledTurn(ctx, cycle)
		}

		return s.observedCycleEnd(cycle)
	case err := <-completion:
		if err != nil {
			s.recordCycleFailure(cycle, err)
		}

		return s.observedCycleEnd(cycle)
	case <-turnCtx.Done():
		// Only a cancellation this host asked for settles as cancelled. The turn
		// context also dies when the runtime binding does, and that cycle ends on
		// the transport failure the detach recorded.
		if !s.wasCancelled() {
			return s.observedCycleEnd(cycle)
		}

		return s.settleCancelledTurn(ctx, cycle)
	case <-timeoutC:
		// The cancel guard runs before the deadline is applied: when a user
		// cancel and the turn deadline coincide, the turn resolves to cancelled
		// rather than to a timeout it did not lose on.
		if s.wasCancelled() || ctx.Err() != nil {
			return s.settleCancelledTurn(ctx, cycle)
		}

		end := s.settleCancelledTurn(ctx, cycle)
		end.cancelled = false
		end.timedOut = true

		return end
	}
}

// observedCycleEnd reads the evidence the cycle already holds.
func (s *session) observedCycleEnd(cycle *foregroundCycle) nativeTurnEnd {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if cycle.failure != nil {
		return nativeTurnEnd{}
	}

	return nativeTurnEnd{lost: cycle.lost}
}

// settleCancelledTurn interrupts the native session and waits for it to report
// that the work stopped. The acknowledgement is the cancellation boundary: without
// it the turn reports a loss rather than a clean cancellation.
func (s *session) settleCancelledTurn(ctx context.Context, cycle *foregroundCycle) nativeTurnEnd {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settlementTimeout)
	defer cancel()

	if err := s.cancelTurn(waitCtx); err != nil {
		return nativeTurnEnd{cancelled: true, lost: err}
	}

	if err := s.awaitNativeSettlement(waitCtx, cycle); err != nil {
		return nativeTurnEnd{cancelled: true, lost: err}
	}

	return nativeTurnEnd{cancelled: true}
}

// recordCycleFailure records a native failure this turn ends on.
func (s *session) recordCycleFailure(cycle *foregroundCycle, err error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if cycle.failure == nil {
		cycle.failure = err
	}
}

// completePromptTurn is the one settlement path for an accepted turn. It runs the
// same order on success, failure, and cancellation: every blocking action
// terminalizes, the native-safe prefix commits, the single ending transition goes
// out, and only then does the ACP response or error follow. A prefix that cannot
// be committed fences the stream instead of reporting an idle the store cannot
// back.
//
// The answer is decided before the ending transition is emitted, and no step
// after that emission may change it. That is what keeps the response and the
// settlement one account of the same turn: an affirmative end that reached the
// carrier may be durable at the host even if its delivery reported failure, so
// the turn answers what it said rather than contradicting it with an error.
func (s *session) completePromptTurn(
	ctx context.Context,
	turnCtx context.Context,
	params acp.PromptRequest,
	cycle *foregroundCycle,
	end nativeTurnEnd,
	dispatch nativeDispatch,
) (acp.PromptResponse, error) {
	settleCtx := context.WithoutCancel(turnCtx)

	s.cancelActions(settleCtx)

	if end.lost != nil {
		s.fenceLifecycle("native incarnation lost")

		return acp.PromptResponse{}, acp.NewInternalError(map[string]any{
			jsonFieldError: "opencode_turn_settlement_unproven",
			jsonFieldCause: "native_incarnation_lost",
		})
	}

	response, outcome, stopReason, turnErr := s.promptOutcome(settleCtx, turnCtx, params, cycle, end, dispatch)

	if commitErr := s.commitForegroundPrefix(settleCtx); commitErr != nil {
		s.fenceLifecycle(fmt.Sprintf("native-safe prefix commit failed: %v", commitErr))

		return acp.PromptResponse{}, errors.Join(turnErr, commitErr)
	}

	settled := s.settleCycle(settleCtx, cycle, outcome, stopReason)
	if settled.failsTurn() {
		return acp.PromptResponse{}, s.classifyTurnFailure(settleCtx, settled.err, dispatch)
	}

	s.reportAffirmedSettlementFailure(settleCtx, cycle, settled)

	if turnErr != nil {
		return acp.PromptResponse{}, turnErr
	}

	return response, nil
}

// reportAffirmedSettlementFailure records the one delivery failure this adapter
// answers past. An affirmed end binds the response, so its send error is neither
// returned nor joined anywhere, and without this it would leave no trace at all:
// an operator reading a successful turn would have no way to know the success
// answer stood on a send that reported failure and fenced the generation. The
// error itself is not logged, only that it happened and to which turn.
func (s *session) reportAffirmedSettlementFailure(
	ctx context.Context,
	cycle *foregroundCycle,
	settled cycleSettlement,
) {
	if settled.err == nil || s.agent == nil || s.agent.log == nil {
		return
	}

	s.agent.log.WarnContext(ctx, "OpenCode turn answered success on an errored settlement delivery",
		slog.String("session_id", string(s.id)),
		slog.String("turn_id", cycle.turnID),
		slog.String("cycle_id", cycle.id),
	)
}

// promptOutcome decides what this turn ended as. Cancellation wins over every
// native failure, a recorded native failure wins over the transcript, and the
// success path reads its stop reason and usage from the native assistant message
// the turn produced.
func (s *session) promptOutcome(
	settleCtx context.Context,
	turnCtx context.Context,
	params acp.PromptRequest,
	cycle *foregroundCycle,
	end nativeTurnEnd,
	dispatch nativeDispatch,
) (acp.PromptResponse, lifecycle.Outcome, string, error) {
	if end.cancelled {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId},
			lifecycle.OutcomeCancelled, string(acp.StopReasonCancelled), nil
	}

	if end.timedOut {
		return acp.PromptResponse{}, lifecycle.OutcomeFailed, "", acp.NewInternalError(turnFailedData(
			causeTimeout, fmt.Sprintf("turn exceeded %s deadline", s.turnTimeout()), 0, "",
		))
	}

	if failure := s.cycleFailure(cycle); failure != nil {
		return acp.PromptResponse{}, lifecycle.OutcomeFailed, "", s.classifyTurnFailure(settleCtx, failure, dispatch)
	}

	final, err := s.finalAssistantMessage(settleCtx, cycle)
	if err != nil {
		return acp.PromptResponse{}, lifecycle.OutcomeFailed, "", s.classifyTurnFailure(settleCtx, err, dispatch)
	}

	if err := s.emitMessage(turnCtx, final, false); err != nil {
		return acp.PromptResponse{}, lifecycle.OutcomeFailed, "", err
	}

	stopReason := stopReasonFromOpenCode(final.Info.Finish)

	return acp.PromptResponse{
		StopReason:    stopReason,
		Usage:         usageFromTokens(final.Info.Tokens),
		UserMessageId: params.MessageId,
		Meta:          s.structuredOutputMeta(final),
	}, lifecycle.OutcomeSuccess, string(stopReason), nil
}

// cycleFailure reports the native failure recorded against this cycle.
func (s *session) cycleFailure(cycle *foregroundCycle) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	return cycle.failure
}

// finalAssistantMessage reads the native assistant message this turn ends on.
// OpenCode answers one turn in steps, each its own assistant message, and only
// the last of them carries the turn's stop reason, usage and structured output.
// The identities come from the ordered stream rather than from a guess, and the
// read happens once, after the native idle event, rather than on a poll.
func (s *session) finalAssistantMessage(ctx context.Context, cycle *foregroundCycle) (opencode.NativeMessage, error) {
	s.lifecycleMu.Lock()
	steps := make(map[string]struct{}, len(cycle.assistantIDs))

	for id := range cycle.assistantIDs {
		steps[id] = struct{}{}
	}
	s.lifecycleMu.Unlock()

	client := s.currentClient()
	if client == nil {
		return opencode.NativeMessage{}, errors.New("OpenCode turn has no runtime client")
	}

	messages, err := client.Messages(ctx, s.idmap.NativeSessionID)
	if err != nil {
		return opencode.NativeMessage{}, fmt.Errorf("read OpenCode turn messages: %w", err)
	}

	var final opencode.NativeMessage

	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Info.Role != roleAssistant {
			continue
		}

		if _, owned := steps[message.Info.ID]; owned {
			final = message

			break
		}
	}

	if len(steps) == 0 || final.Info.ID == "" {
		return opencode.NativeMessage{}, errTurnAssistantIdentityMissing
	}

	return final, opencode.AssistantMessageError(final)
}

// classifyTurnFailure maps one post-acceptance native failure onto its ACP error.
// A command whose route refused the frame reports the command error, and the
// command catalog is re-read so a stale entry cannot be offered again.
func (s *session) classifyTurnFailure(ctx context.Context, err error, dispatch nativeDispatch) error {
	if errors.Is(err, errTurnAssistantIdentityMissing) {
		return acp.NewInternalError(map[string]any{
			jsonFieldError: "opencode_turn_assistant_identity_missing",
			jsonFieldCause: causeProvider,
		})
	}

	var assistantErr *opencode.AssistantError

	isAssistantErr := errors.As(err, &assistantErr)

	if dispatch.matched && opencode.IsBadRequest(err) {
		refreshCtx, cancel := context.WithTimeout(ctx, closeTimeout)
		if refreshErr := s.refreshCommands(refreshCtx); refreshErr != nil && s.agent != nil && s.agent.log != nil {
			s.agent.log.DebugContext(refreshCtx, "refresh OpenCode commands after command bad request failed",
				slog.String("session_id", string(s.id)))
		}

		cancel()

		data := map[string]any{
			jsonFieldError:   "opencode_command_bad_request",
			jsonFieldMessage: "OpenCode rejected the command",
		}
		if isAssistantErr {
			mergeAssistantErrorFields(data, assistantErr, false)
		}

		return acp.NewInvalidParams(data)
	}

	if isAssistantErr {
		return acp.NewInternalError(assistantErrorData(assistantErr, !dispatch.matched && len(s.outputSchema) > 0))
	}

	if acpErr := new(acp.RequestError); errors.As(err, &acpErr) {
		return err
	}

	return acp.NewInternalError(turnFailedData(causeTransport, err.Error(), 0, ""))
}

func (s *session) refreshLifecycleMCP(ctx context.Context) error {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()

	s.mu.Lock()
	if !s.mcpRefreshPending {
		s.mu.Unlock()

		return nil
	}

	client := s.client
	servers := cloneNativeMCPServerConfigs(s.mcpServers)
	s.mu.Unlock()

	if client == nil {
		return acp.NewInternalError(turnFailedData(causeTransport, "OpenCode MCP refresh has no runtime client", 0, ""))
	}

	if err := client.RefreshMCP(ctx, servers); err != nil {
		return acp.NewInternalError(turnFailedData(causeTransport, fmt.Sprintf("refresh OpenCode MCP catalog: %v", err), 0, ""))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != client || s.runtimeLostCause != "" {
		return acp.NewInternalError(turnFailedData(causeTransport, "OpenCode runtime changed during MCP refresh", 0, ""))
	}

	s.mcpRefreshPending = false

	return nil
}

// turnTimeout returns the configured per-turn deadline, or 0 when no deadline
// is set (the default).
func (s *session) turnTimeout() time.Duration {
	if s.agent == nil {
		return 0
	}

	return s.agent.options.TurnTimeout
}

func (s *session) structuredOutputMeta(final opencode.NativeMessage) map[string]any {
	if len(s.outputSchema) == 0 || len(final.Info.Structured) == 0 {
		return nil
	}

	var value any
	if err := json.Unmarshal(final.Info.Structured, &value); err != nil || value == nil {
		return nil
	}

	return map[string]any{opencodeMetaKey: map[string]any{structuredOutputMetaKey: value}}
}

type slashCommandPrompt struct {
	name      string
	arguments string
}

func slashCommandInvocation(blocks []acp.ContentBlock) (slashCommandPrompt, bool) {
	if len(blocks) == 0 || blocks[0].Text == nil {
		return slashCommandPrompt{}, false
	}

	text := blocks[0].Text.Text
	if text == "" || text[0] != '/' {
		return slashCommandPrompt{}, false
	}

	rest := text[1:]
	for i, r := range rest {
		if unicode.IsSpace(r) {
			return slashCommandPrompt{name: rest[:i], arguments: rest[i+len(string(r)):]}, true
		}
	}

	return slashCommandPrompt{name: rest}, true
}

func promptToOpenCodeParts(blocks []acp.ContentBlock, handoff resolvedPromptMedia) ([]map[string]any, error) {
	parts := make([]map[string]any, 0, len(blocks))
	for position, block := range blocks {
		switch {
		case block.Text != nil:
			parts = append(parts, map[string]any{jsonFieldType: partTypeText, partTypeText: block.Text.Text})
		case block.ResourceLink != nil:
			parts = append(parts, map[string]any{jsonFieldType: partTypeText, partTypeText: block.ResourceLink.Uri})
		case block.Resource != nil:
			part, err := embeddedResourceOpenCodePart(position, block.Resource.Resource, handoff)
			if err != nil {
				return nil, err
			}

			parts = append(parts, part)
		case block.Image != nil:
			parts = append(parts, handoff.imagePart(position, block.Image))
		default:
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: fieldPrompt})
		}
	}

	if len(parts) == 0 {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: fieldPrompt})
	}

	return parts, nil
}

func commandPromptParts(blocks []acp.ContentBlock, handoff resolvedPromptMedia) ([]map[string]any, error) {
	if len(blocks) == 0 {
		return nil, nil
	}

	parts := make([]map[string]any, 0, len(blocks))
	for position, block := range blocks {
		part, ok, err := commandFilePart(position, block, handoff)
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError: errValueUnsupported,
				jsonFieldField: fieldPrompt,
			})
		}

		parts = append(parts, part)
	}

	return parts, nil
}

func commandFilePart(position int, block acp.ContentBlock, handoff resolvedPromptMedia) (map[string]any, bool, error) {
	switch {
	case block.Image != nil:
		return handoff.imagePart(position, block.Image), true, nil
	case block.ResourceLink != nil:
		return resourceLinkOpenCodePart(block.ResourceLink), true, nil
	case block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil:
		part, err := handoff.blobPart(position, block.Resource.Resource.BlobResourceContents)

		return part, true, err
	default:
		return nil, false, nil
	}
}

func resourceLinkOpenCodePart(resource *acp.ContentBlockResourceLink) map[string]any {
	mimeType := defaultMimeType
	if resource.MimeType != nil && *resource.MimeType != "" {
		mimeType = *resource.MimeType
	}

	part := map[string]any{
		jsonFieldType: partTypeFile,
		jsonFieldMime: mimeType,
		jsonFieldURL:  resource.Uri,
	}
	if resource.Name != "" {
		part["filename"] = resource.Name
	} else if filename := filenameFromURI(resource.Uri); filename != "" {
		part["filename"] = filename
	}

	return part
}

// blobResourceOpenCodePart maps one embedded blob resource to its native file
// part. The base64 it inlines is passed in rather than read off the block, so the
// caller decides whether the harness sees the host's spelling or the re-encoding
// of the bytes the gates measured.
func blobResourceOpenCodePart(resource *acp.BlobResourceContents, blob string) (map[string]any, error) {
	mimeType := defaultMimeType
	if resource.MimeType != nil && *resource.MimeType != "" {
		mimeType = *resource.MimeType
	}

	nativeURL := resource.Uri
	if blob != "" {
		nativeURL = "data:" + mimeType + ";base64," + blob
	}

	if nativeURL == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: "missing resource data or uri"})
	}

	part := map[string]any{
		jsonFieldType: partTypeFile,
		jsonFieldMime: mimeType,
		jsonFieldURL:  nativeURL,
	}
	if filename := filenameFromURI(resource.Uri); filename != "" {
		part["filename"] = filename
	}

	return part, nil
}

// imageOpenCodePart maps one ACP image block to its native file part. No name
// is derived from the block URI: the handoff form has no name it is allowed to
// pass on, so neither form contributes one and both build the same native part
// for the same image.
func imageOpenCodePart(image *acp.ContentBlockImage) map[string]any {
	return map[string]any{
		jsonFieldType: partTypeFile,
		jsonFieldMime: image.MimeType,
		jsonFieldURL:  "data:" + image.MimeType + ";base64," + image.Data,
	}
}

func filenameFromURI(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil {
		return ""
	}

	name := filepath.Base(parsed.Path)
	if name == "." || name == "/" {
		return ""
	}

	return name
}

func embeddedResourceOpenCodePart(
	position int,
	resource acp.EmbeddedResourceResource,
	media resolvedPromptMedia,
) (map[string]any, error) {
	if text := resource.TextResourceContents; text != nil {
		value := firstNonEmpty(text.Text, text.Uri)
		if value == "" {
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: "missing resource text or uri"})
		}

		return map[string]any{jsonFieldType: partTypeText, partTypeText: value}, nil
	}

	if blob := resource.BlobResourceContents; blob != nil {
		declared := ""
		if blob.MimeType != nil {
			declared = *blob.MimeType
		}

		if isImageMediaType(declared) {
			return media.blobPart(position, blob)
		}

		if blob.Uri != "" {
			return map[string]any{jsonFieldType: partTypeText, partTypeText: blob.Uri}, nil
		}
	}

	return nil, acp.NewInvalidParams(map[string]any{jsonFieldField: fieldPromptResource, jsonFieldError: "missing supported resource content"})
}

func (s *session) replayMessages(ctx context.Context) error {
	if err := s.ensureNotPoisoned(); err != nil {
		return err
	}

	messages, err := s.client.Messages(ctx, s.idmap.NativeSessionID)
	if err != nil {
		return err
	}

	for i := range messages {
		if err := s.emitMessage(ctx, messages[i], true); err != nil {
			return err
		}
	}

	return nil
}

// emitMessage projects one native message. replay marks stored-transcript
// emission: user parts are included and image artifacts must come from the
// canonical store instead of fresh local reads.
func (s *session) emitMessage(ctx context.Context, message opencode.NativeMessage, replay bool) error {
	if err := s.validateNativeMessageSession(ctx, message); err != nil {
		return err
	}

	isUser := message.Info.Role == roleUser
	if isUser && !replay {
		return nil
	}

	for i := range message.Parts {
		part := &message.Parts[i]
		if err := s.emitPartUpdates(ctx, message.Info.Role, *part, "", replay); err != nil {
			return err
		}

		if part.Type == partTypeStepFinish {
			size := s.contextWindow(ctx, message.Info.ProviderID, message.Info.ModelID)
			if err := s.emitUsageUpdate(ctx, part.MessageID, part.Tokens, size); err != nil {
				return err
			}
		}
	}

	if message.Info.Tokens.Total > 0 {
		size := s.contextWindow(ctx, message.Info.ProviderID, message.Info.ModelID)

		return s.emitUsageUpdate(ctx, message.Info.ID, message.Info.Tokens, size)
	}

	return nil
}

func (s *session) validateNativeMessageSession(ctx context.Context, message opencode.NativeMessage) error {
	expected := s.idmap.NativeSessionID
	if expected == "" {
		return nil
	}

	if message.Info.SessionID != "" && message.Info.SessionID != expected {
		return s.poisonNativeSessionDrift(ctx, "message info.sessionID", message.Info.SessionID)
	}

	for i := range message.Parts {
		part := &message.Parts[i]
		if part.SessionID == "" || part.SessionID == expected {
			continue
		}

		return s.poisonNativeSessionDrift(ctx, "message part sessionID", part.SessionID)
	}

	return nil
}

type emittedToolState struct {
	status    acp.ToolCallStatus
	title     string
	input     any
	output    any
	hasInput  bool
	hasOutput bool
}

type emittedUsageState struct {
	used int
	size int
}

type nativeToolState struct {
	status    acp.ToolCallStatus
	title     string
	input     any
	output    any
	hasInput  bool
	hasOutput bool
}

// emitPartUpdates maps one native part and emits its updates. A mapping
// error still emits the attribution updates built alongside it (the failed
// tool state) and commits their bookkeeping before the error fails the turn.
func (s *session) emitPartUpdates(
	ctx context.Context,
	role string,
	part opencode.NativePart,
	nativeDelta string,
	replay bool,
) error {
	s.updateMu.Lock()

	updates, commit, mapErr := s.partUpdates(ctx, role, part, nativeDelta, replay)
	s.updateMu.Unlock()

	receipts := make([]deliveryReceipt, 0, len(updates))
	for index, update := range updates {
		var delivered func()
		if index == len(updates)-1 && commit != nil {
			delivered = func() {
				s.updateMu.Lock()
				commit()
				s.updateMu.Unlock()
			}
		}

		receipt, err := s.enqueueUpdateCommitted(ctx, update, delivered)
		if err != nil {
			s.failUpdateDelivery(ctx, err)

			return errors.Join(mapErr, err)
		}

		receipts = append(receipts, receipt)
	}

	for _, receipt := range receipts {
		if err := waitDelivery(ctx, receipt); err != nil {
			s.failUpdateDelivery(ctx, err)

			return errors.Join(mapErr, err)
		}
	}

	return mapErr
}

func (s *session) partUpdates(
	ctx context.Context,
	role string,
	part opencode.NativePart,
	nativeDelta string,
	replay bool,
) ([]acp.SessionUpdate, func(), error) {
	messageID := part.MessageID
	switch part.Type {
	case partTypeText:
		text, commit := s.partTextDelta(part, nativeDelta)
		if text == "" {
			return nil, commit, nil
		}

		if role == roleUser {
			return []acp.SessionUpdate{{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{
				SessionUpdate: updateUserMessageChunk,
				MessageId:     &messageID,
				Content:       acp.TextBlock(text),
			}}}, commit, nil
		}

		return []acp.SessionUpdate{{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			SessionUpdate: updateAgentMessageChunk,
			MessageId:     &messageID,
			Content:       acp.TextBlock(text),
		}}}, commit, nil
	case partTypeReasoning:
		text, commit := s.partTextDelta(part, nativeDelta)
		if text == "" {
			return nil, commit, nil
		}

		return []acp.SessionUpdate{{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
			SessionUpdate: "agent_thought_chunk",
			MessageId:     &messageID,
			Content:       acp.TextBlock(text),
		}}}, commit, nil
	case partTypeFile:
		return s.filePartUpdates(ctx, role, part, replay)
	case partTypeTool:
		return s.toolPartUpdates(ctx, part, replay)
	default:
		return nil, nil, nil
	}
}

// filePartUpdates maps a standalone native file part. User file parts are
// replayed as user message content; assistant file parts are mapped
// defensively as agent-provenance image output, each image alone in its own
// chunk.
func (s *session) filePartUpdates(
	ctx context.Context,
	role string,
	part opencode.NativePart,
	replay bool,
) ([]acp.SessionUpdate, func(), error) {
	messageID := part.MessageID

	if role == roleUser {
		block, ok := userFilePartBlock(part)
		if !ok {
			return nil, nil, nil
		}

		dedupeKey := "user/" + part.ID
		if _, emitted := s.emittedFileParts[dedupeKey]; emitted {
			return nil, nil, nil
		}

		return []acp.SessionUpdate{{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{
				SessionUpdate: updateUserMessageChunk,
				MessageId:     &messageID,
				Content:       block,
			}}}, func() {
				s.emittedFileParts[dedupeKey] = struct{}{}
			}, nil
	}

	artifact := opencode.NativeAttachment{
		ID:       part.ID,
		Type:     partTypeFile,
		Mime:     part.Mime,
		Filename: part.Filename,
		URL:      part.URL,
	}

	item, mapped, err := s.mapOutputArtifact(ctx, artifact, "file/"+part.ID, provenanceAgent, replay)
	if err != nil {
		// An assistant file part has no tool call to attribute to, so the
		// guidance takes the image's place rather than the image vanishing.
		guidance, recoverable := imageOutputGuidance(err)
		if !recoverable {
			return nil, nil, err
		}

		return []acp.SessionUpdate{{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			SessionUpdate: updateAgentMessageChunk,
			MessageId:     &messageID,
			Content:       acp.TextBlock(guidance),
		}}}, nil, nil
	}

	if !mapped {
		return nil, nil, nil
	}

	dedupeKey := "agent/" + part.ID + "/" + item.key()
	if _, emitted := s.emittedFileParts[dedupeKey]; emitted {
		return nil, nil, nil
	}

	return []acp.SessionUpdate{{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			SessionUpdate: updateAgentMessageChunk,
			MessageId:     &messageID,
			Content:       item.block,
		}}}, func() {
			s.emittedFileParts[dedupeKey] = struct{}{}
		}, nil
}

// userFilePartBlock restores the prompt content a user file part carries: an
// embedded image for an image data URL, a resource link for a remote URI.
func userFilePartBlock(part opencode.NativePart) (acp.ContentBlock, bool) {
	if _, mime, payload, ok := parseImageDataURL(part.URL); ok {
		if !isImageMediaType(firstNonEmpty(mime, part.Mime)) {
			return acp.ContentBlock{}, false
		}

		if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
			return acp.ContentBlock{}, false
		}

		return acp.ImageBlock(payload, firstNonEmpty(mime, part.Mime)), true
	}

	if remoteArtifactURL(part.URL) {
		name := firstNonEmpty(part.Filename, filenameFromURI(part.URL), part.URL)

		return acp.ResourceLinkBlock(name, part.URL), true
	}

	return acp.ContentBlock{}, false
}

func (s *session) partTextDelta(part opencode.NativePart, nativeDelta string) (string, func()) {
	if part.ID == "" {
		return firstNonEmpty(part.Text, nativeDelta), nil
	}

	previous := s.emittedPartText[part.ID]
	if part.Text == "" {
		if nativeDelta == "" {
			return "", nil
		}

		next := previous + nativeDelta

		return nativeDelta, func() { s.emittedPartText[part.ID] = next }
	}

	if part.Text == previous {
		return "", nil
	}

	if previous == "" {
		return part.Text, func() { s.emittedPartText[part.ID] = part.Text }
	}

	if strings.HasPrefix(part.Text, previous) {
		return part.Text[len(previous):], func() { s.emittedPartText[part.ID] = part.Text }
	}

	if strings.HasPrefix(previous, part.Text) {
		return "", nil
	}

	s.logNonAppendPartRewrite(part, len(previous))

	return "", nil
}

func (s *session) logNonAppendPartRewrite(part opencode.NativePart, previousLength int) {
	if s.agent == nil || s.agent.log == nil {
		return
	}

	s.agent.log.Debug("ignored non-append OpenCode part rewrite",
		slog.String("session_id", string(s.id)),
		slog.String("part_id", part.ID),
		slog.String("part_type", part.Type),
		slog.Int("previous_length", previousLength),
		slog.Int("current_length", len(part.Text)),
	)
}

func (s *session) toolPartUpdates(ctx context.Context, part opencode.NativePart, replay bool) ([]acp.SessionUpdate, func(), error) {
	id := acp.ToolCallId(firstNonEmpty(part.CallID, part.ID, "opencode-tool"))
	current := nativeToolPartState(part, id)

	var (
		content        []imageOutputItem
		contentChanged bool
		mapErr         error
	)

	if current.status == acp.ToolCallStatusCompleted {
		if attachments := nativeToolAttachments(part.State); len(attachments) > 0 {
			content, contentChanged, mapErr = s.toolContentSnapshot(ctx, string(id), part, attachments, replay)
		}
	}

	previous, seen := s.emittedTools[string(id)]

	// An attachment the adapter will not ship fails the tool call. A verdict
	// the model can act on carries its guidance there and the turn runs on;
	// only the artifact store breaking is still turn-fatal.
	if mapErr != nil {
		return s.failedToolAttribution(id, part, current, previous, seen, mapErr)
	}

	if !seen {
		opts := []acp.ToolCallStartOpt{
			acp.WithStartKind(toolKind(part.Tool)),
			acp.WithStartStatus(current.status),
		}
		if current.hasInput {
			opts = append(opts, acp.WithStartRawInput(current.input))
		}

		if current.hasOutput {
			opts = append(opts, acp.WithStartRawOutput(current.output))
		}

		if contentChanged {
			opts = append(opts, acp.WithStartContent(toolCallContentFromItems(content)))
		}

		return []acp.SessionUpdate{acp.StartToolCall(id, current.title, opts...)}, func() {
			s.emittedTools[string(id)] = emittedToolState(current)
			if contentChanged {
				s.emittedToolContent[string(id)] = content
			}

			s.markPublishedToolCall(string(id))
		}, nil
	}

	if current.status != previous.status && !toolStatusCanAdvance(previous.status, current.status) {
		return nil, nil, nil
	}

	next := previous
	opts := make([]acp.ToolCallUpdateOpt, 0, 4)

	if toolStatusCanAdvance(previous.status, current.status) && current.status != previous.status {
		next.status = current.status
		opts = append(opts, acp.WithUpdateStatus(current.status))
	}

	if current.title != previous.title {
		next.title = current.title
		opts = append(opts, acp.WithUpdateTitle(current.title))
	}

	if current.hasInput && (!previous.hasInput || !reflect.DeepEqual(current.input, previous.input)) {
		next.input = current.input
		next.hasInput = true

		opts = append(opts, acp.WithUpdateRawInput(current.input))
	}

	if current.hasOutput && (!previous.hasOutput || !reflect.DeepEqual(current.output, previous.output)) {
		next.output = current.output
		next.hasOutput = true

		opts = append(opts, acp.WithUpdateRawOutput(current.output))
	}

	if contentChanged {
		opts = append(opts, acp.WithUpdateContent(toolCallContentFromItems(content)))
	}

	if len(opts) == 0 {
		return nil, nil, nil
	}

	return []acp.SessionUpdate{acp.UpdateToolCall(id, opts...)}, func() {
		s.emittedTools[string(id)] = next
		if contentChanged {
			s.emittedToolContent[string(id)] = content
		}

		s.markPublishedToolCall(string(id))
	}, nil
}

// failedToolAttribution builds the failed tool state for an attachment the
// adapter will not ship. A recoverable verdict carries its guidance as the
// tool call's own content and returns no error, so the turn keeps its context
// and can write the image somewhere readable; the artifact store breaking
// returns the error and stays turn-fatal, with content already delivered for
// the tool call left intact by omitting the content field.
func (s *session) failedToolAttribution(
	id acp.ToolCallId,
	part opencode.NativePart,
	current nativeToolState,
	previous emittedToolState,
	seen bool,
	mapErr error,
) ([]acp.SessionUpdate, func(), error) {
	guidance, recoverable := imageOutputGuidance(mapErr)

	turnErr := mapErr
	if recoverable {
		turnErr = nil
	}

	refusalContent := func() []acp.ToolCallContent {
		if !recoverable {
			return nil
		}

		return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(guidance))}
	}()

	if !seen {
		failed := current
		failed.status = acp.ToolCallStatusFailed

		opts := []acp.ToolCallStartOpt{
			acp.WithStartKind(toolKind(part.Tool)),
			acp.WithStartStatus(acp.ToolCallStatusFailed),
		}
		if refusalContent != nil {
			opts = append(opts, acp.WithStartContent(refusalContent))
		}

		return []acp.SessionUpdate{acp.StartToolCall(id, current.title, opts...)}, func() {
			s.emittedTools[string(id)] = emittedToolState(failed)
			s.markPublishedToolCall(string(id))
		}, turnErr
	}

	if !toolStatusCanAdvance(previous.status, acp.ToolCallStatusFailed) {
		return nil, nil, turnErr
	}

	next := previous
	next.status = acp.ToolCallStatusFailed

	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(acp.ToolCallStatusFailed)}
	if refusalContent != nil {
		opts = append(opts, acp.WithUpdateContent(refusalContent))
	}

	return []acp.SessionUpdate{acp.UpdateToolCall(id, opts...)}, func() {
		s.emittedTools[string(id)] = next
		s.markPublishedToolCall(string(id))
	}, turnErr
}

func nativeToolPartState(part opencode.NativePart, id acp.ToolCallId) nativeToolState {
	state := nativeToolState{
		status: acp.ToolCallStatusInProgress,
		title:  firstNonEmpty(part.Tool, string(id)),
	}
	if len(part.State) == 0 {
		return state
	}

	var values map[string]any
	if json.Unmarshal(part.State, &values) != nil {
		return state
	}

	if value, _ := values["status"].(string); value != "" {
		state.status = toolStatus(value)
	}

	if value, _ := values[jsonFieldTitle].(string); value != "" {
		state.title = value
	}

	state.input, state.hasInput = values["input"]

	state.output, state.hasOutput = values["output"]

	if failure, ok := values[jsonFieldError]; ok && failure != nil {
		state.output = map[string]any{jsonFieldError: nativeToolFailed}
		state.hasOutput = true
	}

	return state
}

func toolStatusCanAdvance(previous, current acp.ToolCallStatus) bool {
	if previous == current {
		return false
	}

	if previous == acp.ToolCallStatusCompleted || previous == acp.ToolCallStatusFailed {
		return false
	}

	return toolStatusRank(current) >= toolStatusRank(previous)
}

func toolStatusRank(status acp.ToolCallStatus) int {
	switch status {
	case acp.ToolCallStatusPending:
		return 1
	case acp.ToolCallStatusInProgress:
		return 2
	case acp.ToolCallStatusCompleted, acp.ToolCallStatusFailed:
		return 3
	default:
		return 0
	}
}

func eventMessageInfo(data json.RawMessage) (opencode.NativeMessageInfo, bool) {
	var wrapper struct {
		Info opencode.NativeMessageInfo `json:"info"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Info.ID != "" {
		return wrapper.Info, true
	}

	var info opencode.NativeMessageInfo
	if err := json.Unmarshal(data, &info); err == nil && info.ID != "" {
		return info, true
	}

	return opencode.NativeMessageInfo{}, false
}

func eventPart(data json.RawMessage) (opencode.NativePart, bool) {
	part, _, ok := eventPartUpdate(data)

	return part, ok
}

func eventPartUpdate(data json.RawMessage) (opencode.NativePart, string, bool) {
	var wrapper struct {
		Part  opencode.NativePart `json:"part"`
		Delta string              `json:"delta"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Part.Type != "" {
		return wrapper.Part, wrapper.Delta, true
	}

	var part opencode.NativePart
	if err := json.Unmarshal(data, &part); err == nil && part.Type != "" {
		return part, "", true
	}

	return opencode.NativePart{}, "", false
}

func eventQuestion(data json.RawMessage) (opencode.QuestionRequest, bool) {
	var req opencode.QuestionRequest
	if err := json.Unmarshal(data, &req); err == nil && req.ID != "" {
		return req, true
	}

	for _, key := range []string{questionWrapperKey, jsonFieldRequest, "data"} {
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal(data, &wrapper); err != nil {
			continue
		}

		raw := wrapper[key]
		if len(raw) == 0 {
			continue
		}

		if err := json.Unmarshal(raw, &req); err == nil && req.ID != "" {
			return req, true
		}
	}

	return opencode.QuestionRequest{}, false
}

func mergeMeta(left, right map[string]any) map[string]any {
	out := make(map[string]any, len(left)+len(right))
	for key, value := range left {
		out[key] = value
	}

	for key, value := range right {
		out[key] = value
	}

	return out
}

func questionElicitationRequest(req opencode.QuestionRequest) (acp.UnstableCreateElicitationRequest, []string) {
	properties := make(map[string]any, len(req.Questions))
	required := make([]string, 0, len(req.Questions))

	propertyIDs := make([]string, 0, len(req.Questions))
	for index, question := range req.Questions {
		id := fmt.Sprintf("question_%d", index+1)
		propertyIDs = append(propertyIDs, id)
		required = append(required, id)
		properties[id] = questionPropertySchema(index, question)
	}

	if len(properties) == 0 {
		propertyIDs = []string{fallbackQuestionID}
		required = []string{fallbackQuestionID}
		properties[fallbackQuestionID] = map[string]any{jsonFieldType: schemaTypeString, jsonFieldTitle: "Question 1"}
	}

	title := "OpenCode question"

	return acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: questionElicitationMessage(req.Questions),
			Mode:    elicitationModeForm,
			RequestedSchema: acp.UnstableElicitationSchema{
				Title:      &title,
				Type:       acp.UnstableElicitationSchemaTypeObject,
				Properties: properties,
				Required:   required,
			},
			Meta: map[string]any{opencodeMetaKey: map[string]any{
				routeFieldRequestID:     req.ID,
				opencodeNativeIDMetaKey: req.SessionID,
				jsonFieldTool: map[string]any{
					"messageId": req.Tool.MessageID,
					"callId":    req.Tool.CallID,
				},
			}},
		},
	}, propertyIDs
}

func questionPropertySchema(index int, question opencode.QuestionInfo) map[string]any {
	title := firstNonEmpty(question.Header, fmt.Sprintf("Question %d", index+1))

	description := question.Question
	if question.Multiple {
		items := map[string]any{jsonFieldType: schemaTypeString}

		if !question.Custom {
			if options := questionOptionSchemas(question.Options); len(options) > 0 {
				items["anyOf"] = options
			}
		}

		return map[string]any{
			jsonFieldType:  "array",
			jsonFieldTitle: title,
			"description":  description,
			"items":        items,
		}
	}

	property := map[string]any{
		jsonFieldType:  schemaTypeString,
		jsonFieldTitle: title,
		"description":  description,
	}

	if !question.Custom {
		if options := questionOptionSchemas(question.Options); len(options) > 0 {
			property["oneOf"] = options
		}
	}

	return property
}

func questionOptionSchemas(options []opencode.QuestionOption) []map[string]any {
	out := make([]map[string]any, 0, len(options))
	for _, option := range options {
		label := strings.TrimSpace(option.Label)
		if label == "" {
			continue
		}

		item := map[string]any{
			"const":        label,
			jsonFieldTitle: label,
		}
		if option.Description != "" {
			item["description"] = option.Description
		}

		out = append(out, item)
	}

	return out
}

func questionElicitationMessage(questions []opencode.QuestionInfo) string {
	if len(questions) == 1 && questions[0].Question != "" {
		return questions[0].Question
	}

	return questionFallbackMessage
}

func questionAnswersFromContent(content map[string]any, propertyIDs []string) [][]string {
	answers := make([][]string, len(propertyIDs))
	for index, id := range propertyIDs {
		answers[index] = stringAnswersFromAny(content[id])
	}

	return answers
}

func stringAnswersFromAny(value any) []string {
	switch typed := value.(type) {
	case nil:
		return []string{}
	case string:
		return []string{typed}
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if item == nil {
				continue
			}

			if str, ok := item.(string); ok {
				out = append(out, str)

				continue
			}

			out = append(out, fmt.Sprint(item))
		}

		return out
	default:
		return []string{fmt.Sprint(value)}
	}
}

func (s *session) emitPlan(ctx context.Context, todos []opencode.NativeTodo) error {
	entries := make([]acp.PlanEntry, 0, len(todos))
	for _, todo := range todos {
		if todo.Content == "" {
			continue
		}

		entries = append(entries, acp.PlanEntry{
			Content:  todo.Content,
			Priority: planPriority(todo.Priority),
			Status:   planStatus(todo.Status),
		})
	}

	if len(entries) == 0 {
		return nil
	}

	return s.emitUpdate(ctx, acp.UpdatePlan(entries...))
}

func (s *session) emitUpdate(ctx context.Context, update acp.SessionUpdate) error {
	receipt, err := s.enqueueUpdate(ctx, update)
	if err == nil {
		err = waitDelivery(ctx, receipt)
	}

	if err != nil {
		s.failUpdateDelivery(ctx, err)
	}

	return err
}

func (s *session) enqueueUpdate(ctx context.Context, update acp.SessionUpdate) (deliveryReceipt, error) {
	return s.enqueueUpdateCommitted(ctx, update, nil)
}

func (s *session) enqueueUpdateCommitted(
	ctx context.Context,
	update acp.SessionUpdate,
	onDelivered func(),
) (deliveryReceipt, error) {
	s.agent.observe.ObserveFirstPromptUpdate(ctx)
	binding := s.nativeIncarnationForContext(ctx)

	return s.delivery.enqueueUpdateCommitted(ctx, acp.SessionNotification{
		Meta:      turnRouteMetaFromContext(ctx),
		SessionId: s.id,
		Update:    update,
	}, onDelivered, func(err error) { s.failNativeIncarnation(binding, err) })
}

func (s *session) failUpdateDelivery(ctx context.Context, err error) {
	binding := s.nativeIncarnationForContext(ctx)
	s.failNativeIncarnation(binding, err)
}

func (s *session) emitRawOpenCodeEvent(ctx context.Context, event opencode.Event) error {
	if !s.rawMessages.Enabled() || len(event.Raw) == 0 {
		return nil
	}

	// A payload that decodes to nothing is skipped without consuming a
	// sequence number, so the per-session sequence stays contiguous over the
	// notifications that are actually emitted.
	var raw map[string]any

	_ = json.Unmarshal(event.Raw, &raw)

	if raw == nil {
		return nil
	}

	sanitizeRawEventValue(raw)

	payload := map[string]any{
		jsonFieldSessionID: s.id,
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     raw,
	}
	if meta := turnRouteMetaFromContext(ctx); meta != nil {
		payload["_meta"] = meta
	}

	preflight := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		preflight[key] = value
	}

	preflight[jsonFieldSequence] = int64(^uint64(0) >> 1)
	if _, err := capRawEventPayload(preflight); err != nil {
		return err
	}

	s.delivery.enqueueRaw(ctx, payload)

	return nil
}

// sanitizeRawEventValue keeps raw diagnostic events free of image payloads
// and signed locations: base64 data URLs collapse to a size marker and
// remote URL fields lose their query and fragment.
func sanitizeRawEventValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok {
				typed[key] = sanitizeRawEventString(key, text)

				continue
			}

			sanitizeRawEventValue(child)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				typed[index] = sanitizeRawEventString("", text)

				continue
			}

			sanitizeRawEventValue(child)
		}
	}
}

func sanitizeRawEventString(key, value string) string {
	if _, mime, payload, ok := parseImageDataURL(value); ok {
		return fmt.Sprintf("data:%s;base64,[redacted %d bytes]", mime, len(payload))
	}

	if key == jsonFieldURL || key == "uri" {
		parsed, err := url.Parse(value)
		if err == nil && (parsed.Scheme == schemeHTTP || parsed.Scheme == schemeHTTPS) {
			parsed.RawQuery = ""
			parsed.Fragment = ""

			return parsed.String()
		}
	}

	return value
}

func (s *session) emitUsageUpdate(
	ctx context.Context,
	messageID string,
	tokens opencode.NativeTokens,
	size int,
) error {
	update := usageUpdateFromTokens(messageID, tokens, size)
	if update == nil || update.UsageUpdate == nil {
		return nil
	}

	s.updateMu.Lock()

	current := emittedUsageState{used: update.UsageUpdate.Used, size: update.UsageUpdate.Size}
	if messageID != "" {
		if previous, ok := s.emittedUsage[messageID]; ok && previous == current {
			s.updateMu.Unlock()

			return nil
		}
	}
	s.updateMu.Unlock()

	var delivered func()
	if messageID != "" {
		delivered = func() {
			s.updateMu.Lock()
			s.emittedUsage[messageID] = current
			s.updateMu.Unlock()
		}
	}

	receipt, err := s.enqueueUpdateCommitted(ctx, *update, delivered)
	if err != nil {
		s.failUpdateDelivery(ctx, err)

		return err
	}

	if err := waitDelivery(ctx, receipt); err != nil {
		s.failUpdateDelivery(ctx, err)

		return err
	}

	return nil
}

func usageUpdateFromTokens(messageID string, tokens opencode.NativeTokens, size int) *acp.SessionUpdate {
	used := int(tokens.Total)
	if used <= 0 {
		used = int(tokens.Input + tokens.Output + tokens.Reasoning)
	}

	if used <= 0 {
		return nil
	}

	meta := map[string]any{opencodeMetaKey: map[string]any{"messageId": messageID}}

	return &acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{
		SessionUpdate: "usage_update",
		Used:          used,
		Size:          size,
		Meta:          meta,
	}}
}

func usageFromTokens(tokens opencode.NativeTokens) *acp.Usage {
	used := int(tokens.Total)
	if used <= 0 {
		used = int(tokens.Input + tokens.Output + tokens.Reasoning)
	}

	if used <= 0 {
		return nil
	}

	thought := int(tokens.Reasoning)
	cacheRead := int(tokens.Cache.Read)
	cacheWrite := int(tokens.Cache.Write)

	return &acp.Usage{
		InputTokens:       int(tokens.Input),
		OutputTokens:      int(tokens.Output),
		ThoughtTokens:     &thought,
		CachedReadTokens:  &cacheRead,
		CachedWriteTokens: &cacheWrite,
		TotalTokens:       used,
	}
}

func stopReasonFromOpenCode(reason string) acp.StopReason {
	switch strings.ToLower(reason) {
	case finishReasonLength, "max_tokens":
		return acp.StopReasonMaxTokens
	case reasonCancelled, "canceled":
		return acp.StopReasonCancelled
	case "refusal":
		return acp.StopReasonRefusal
	default:
		return acp.StopReasonEndTurn
	}
}

func toolStatus(value string) acp.ToolCallStatus {
	switch strings.ToLower(value) {
	case nativeStatusPending:
		return acp.ToolCallStatusPending
	case nativeStatusCompleted, nativeStatusSuccess:
		return acp.ToolCallStatusCompleted
	case nativeStatusFailed, nativeStatusError:
		return acp.ToolCallStatusFailed
	default:
		return acp.ToolCallStatusInProgress
	}
}

func toolKind(tool string) acp.ToolKind {
	switch strings.ToLower(tool) {
	case toolNameRead, "view":
		return acp.ToolKindRead
	case toolNameEdit, "write":
		return acp.ToolKindEdit
	case toolNameDelete, "remove":
		return acp.ToolKindDelete
	case "move", "rename":
		return acp.ToolKindMove
	case "grep", "search", "find":
		return acp.ToolKindSearch
	case toolNameBash, "shell", "run":
		return acp.ToolKindExecute
	case "fetch", "webfetch":
		return acp.ToolKindFetch
	case "think":
		return acp.ToolKindThink
	default:
		return acp.ToolKindOther
	}
}

func planPriority(value string) acp.PlanEntryPriority {
	switch strings.ToLower(value) {
	case priorityHigh:
		return acp.PlanEntryPriorityHigh
	case priorityLow:
		return acp.PlanEntryPriorityLow
	default:
		return acp.PlanEntryPriorityMedium
	}
}

func planStatus(value string) acp.PlanEntryStatus {
	switch strings.ToLower(value) {
	case nativeStatusCompleted, "done":
		return acp.PlanEntryStatusCompleted
	case nativeStatusInProgress, "running":
		return acp.PlanEntryStatusInProgress
	default:
		return acp.PlanEntryStatusPending
	}
}
