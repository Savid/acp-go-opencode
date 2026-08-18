//nolint:tagliatelle // OpenCode native event payloads use sessionID wire names.
package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/observer"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

var errPromptCancelled = errors.New("prompt cancelled")

const (
	eventServerConnected    = "server.connected"
	eventPermissionV2Asked  = "permission.v2.asked"
	eventPermissionAsked    = "permission.asked"
	eventMessagePartCreated = "message.part.created"
	eventMessagePartUpdated = "message.part.updated"
	eventMessageUpdated     = "message.updated"
	eventSessionError       = "session.error"
	eventQuestionAsked      = "question.asked"
	eventQuestionV2Asked    = "question.v2.asked"

	fieldPrompt         = "prompt"
	fieldPromptResource = "prompt.resource"

	updateUserMessageChunk  = "user_message_chunk"
	updateAgentMessageChunk = "agent_message_chunk"
	partTypeText            = "text"
	partTypeReasoning       = "reasoning"
	partTypeFile            = "file"
	partTypeTool            = "tool"
	partTypeStepFinish      = "step-finish"
	contentTypeAudio        = "audio"
	mediaTypeImage          = "image"
	roleUser                = "user"
	defaultMimeType         = "application/octet-stream"

	permissionReplyOnce   = "once"
	permissionReplyAlways = "always"
	permissionReplyReject = "reject"
	reasonCancelled       = "cancelled"

	questionWrapperKey      = "question"
	fallbackQuestionID      = "question_1"
	schemaTypeString        = "string"
	questionFallbackMessage = "OpenCode needs input"

	finishReasonLength    = "length"
	nativeStatusPending   = "pending"
	nativeStatusCompleted = "completed"
	nativeStatusSuccess   = "success"
	nativeStatusError     = "error"
	toolNameRead          = "read"
	toolNameEdit          = "edit"
	toolNameDelete        = "delete"
	toolNameBash          = "bash"
	priorityHigh          = "high"
	priorityLow           = "low"
)

const (
	turnFailedErrorTag = "opencode_turn_failed"
	causeProvider      = "provider"
	causeTransport     = "transport"
	causeTimeout       = "timeout"
)

// turnFailedData builds the uniform ACP error Data payload for a native
// OpenCode turn failure. cause is the machine-readable failure class
// (provider/transport/timeout); message carries the real native cause text and
// is never a fixed placeholder or bare transport string like "EOF".
// statusCode/providerCode are included only when the harness supplies them.
func turnFailedData(cause, message string, statusCode int, providerCode string) map[string]any {
	data := map[string]any{
		jsonFieldError:   turnFailedErrorTag,
		jsonFieldCause:   cause,
		jsonFieldMessage: message,
	}
	if statusCode > 0 {
		data[jsonFieldStatusCode] = statusCode
	}

	if providerCode != "" {
		data[jsonFieldProviderCode] = providerCode
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

// mergeAssistantErrorFields adds the optional machine-readable assistant-error
// fields (statusCode, providerCode) and structuredOutputRequested onto an
// existing ACP error Data map. Optional fields are included only when present.
func mergeAssistantErrorFields(data map[string]any, err *opencode.AssistantError, structuredOutputRequested bool) {
	data[jsonFieldStructuredOutputRequested] = structuredOutputRequested
	if err.StatusCode() > 0 {
		data[jsonFieldStatusCode] = err.StatusCode()
	}

	if err.ProviderCode() != "" {
		data[jsonFieldProviderCode] = err.ProviderCode()
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

	poisonErr := session.ensureNotPoisoned()
	if poisonErr != nil {
		return poisonErr
	}

	epoch, err := session.beginCancellation(route.TurnNonce, true, true)
	if err != nil {
		return err
	}

	cancelCtx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
	defer cancel()

	return session.resolveCancellation(cancelCtx, epoch)
}

// Prompt is the internal session seam used by deterministic unit tests. The
// public Agent path always calls promptWithRoute after strict route validation.
// The lifecycle correlation is read through the same helper the public path uses,
// so the seam can never accept a submission identity the wire would refuse.
func (s *session) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	nonce := internalSeamTurnNonce
	if route, err := parseInboundTurnRoute(params.Meta); err == nil {
		nonce = route.TurnNonce
	}

	submission, err := s.agent.promptSubmission(params.Meta)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	return s.promptWithRoute(ctx, params, nonce, submission)
}

func (s *session) promptWithRoute(
	ctx context.Context,
	params acp.PromptRequest,
	turnNonce string,
	submission lifecycle.Submission,
) (acp.PromptResponse, error) {
	if err := s.ensureNotPoisoned(); err != nil {
		return acp.PromptResponse{}, err
	}

	releaseNativeTurn, err := s.agent.acquireNativeTurn(ctx)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer releaseNativeTurn()

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

	turnCtx := s.beginTurn(ctx, turnNonce)
	defer s.finishTurn()

	s.recordSubmission(submission)

	invocation, command, matchedCommand, err := s.resolvePromptCommand(turnCtx, params)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	var runNative func(context.Context) (opencode.NativeMessage, error)

	if matchedCommand {
		runNative, err = s.commandNativeRun(turnCtx, params, invocation, command)
	} else {
		runNative, err = s.messageNativeRun(turnCtx, params)
	}

	if err != nil {
		return acp.PromptResponse{}, err
	}

	if err := s.drainClientBacklog(turnCtx); err != nil {
		return acp.PromptResponse{}, err
	}

	return s.runPromptTurn(ctx, turnCtx, params, runNative, command, matchedCommand)
}

func (s *session) resolvePromptCommand(ctx context.Context, params acp.PromptRequest) (slashCommandPrompt, opencode.NativeCommand, bool, error) {
	invocation, slashCandidate := slashCommandInvocation(params.Prompt)

	_, matchedBeforeRefresh := s.cachedCommand(invocation.name)
	if slashCandidate {
		if err := s.refreshCommands(ctx); err != nil && s.agent != nil && s.agent.log != nil {
			s.agent.log.DebugContext(ctx, "refresh OpenCode commands before prompt failed", slog.String("session_id", string(s.id)), slog.String("error", err.Error()))
		}
	}

	command, matchedCommand := s.cachedCommand(invocation.name)
	if slashCandidate && matchedBeforeRefresh && !matchedCommand {
		return slashCommandPrompt{}, opencode.NativeCommand{}, false, acp.NewInvalidParams(map[string]any{
			jsonFieldError:   "opencode_command_removed",
			jsonFieldMessage: fmt.Sprintf("OpenCode command %q is no longer available", invocation.name),
			jsonFieldCommand: invocation.name,
		})
	}

	return invocation, command, matchedCommand, nil
}

func (s *session) commandNativeRun(ctx context.Context, params acp.PromptRequest, invocation slashCommandPrompt, command opencode.NativeCommand) (func(context.Context) (opencode.NativeMessage, error), error) {
	handoff, err := s.validatePromptMedia(ctx, params.Prompt[1:])
	if err != nil {
		return nil, err
	}

	parts, err := commandPromptParts(params.Prompt[1:], handoff)
	if err != nil {
		return nil, err
	}

	agent, model := s.commandContext()
	if err := s.validateModel(ctx, model, modelFieldPrompt); err != nil {
		return nil, err
	}

	req := opencode.CommandRequest{
		Agent:     agent,
		Model:     model,
		Command:   command.Name,
		Arguments: invocation.arguments,
		Parts:     parts,
	}
	if params.MessageId != nil {
		req.MessageID = *params.MessageId
	}

	return func(turnCtx context.Context) (opencode.NativeMessage, error) {
		return s.client.RunCommand(turnCtx, s.idmap.NativeSessionID, req)
	}, nil
}

func (s *session) messageNativeRun(ctx context.Context, params acp.PromptRequest) (func(context.Context) (opencode.NativeMessage, error), error) {
	handoff, err := s.validatePromptMedia(ctx, params.Prompt)
	if err != nil {
		return nil, err
	}

	parts, err := promptToOpenCodeParts(params.Prompt, handoff)
	if err != nil {
		return nil, err
	}

	modelSelector, hasModel, err := s.validatedModelSelector(ctx, modelFieldPrompt)
	if err != nil {
		return nil, err
	}

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

	if params.MessageId != nil {
		req.MessageID = *params.MessageId
	}

	return func(turnCtx context.Context) (opencode.NativeMessage, error) {
		return s.client.SendMessage(turnCtx, s.idmap.NativeSessionID, req)
	}, nil
}

type promptTurnResult struct {
	message opencode.NativeMessage
	err     error
}

func (s *session) runPromptTurn(
	ctx context.Context,
	turnCtx context.Context,
	params acp.PromptRequest,
	runNative func(context.Context) (opencode.NativeMessage, error),
	command opencode.NativeCommand,
	matchedCommand bool,
) (acp.PromptResponse, error) {
	if err := s.refreshLifecycleMCP(turnCtx); err != nil {
		return acp.PromptResponse{}, err
	}

	return s.runPromptTurnWithRefreshedMCP(ctx, turnCtx, params, runNative, command, matchedCommand)
}

func (s *session) runPromptTurnWithRefreshedMCP(
	ctx context.Context,
	turnCtx context.Context,
	params acp.PromptRequest,
	runNative func(context.Context) (opencode.NativeMessage, error),
	command opencode.NativeCommand,
	matchedCommand bool,
) (response acp.PromptResponse, returnErr error) {
	var fenceOnce sync.Once

	var fenceErr error

	fenceTurn := func(markCancelled bool) error {
		fenceOnce.Do(func() {
			epoch, _ := s.beginCancellation("", false, markCancelled)

			fenceCtx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
			fenceErr = s.resolveCancellation(fenceCtx, epoch)

			cancel()
		})

		return fenceErr
	}

	failTurn := func(err error) (acp.PromptResponse, error) {
		return s.settlePromptFailure(err, fenceTurn, params)
	}
	if err := s.reconcilePermissions(turnCtx); err != nil {
		return failTurn(err)
	}

	if err := s.reconcileQuestions(turnCtx); err != nil {
		return failTurn(err)
	}

	// The stream route exists before native dispatch. Once validation,
	// reconciliation, and admission have completed, publish acceptance and
	// running before any event caused by the native frame can be forwarded.
	if err := s.beginLifecycleTurn(turnCtx); err != nil {
		return failTurn(err)
	}
	defer func() {
		outcome := lifecycle.OutcomeSuccess
		stopReason := string(response.StopReason)
		if returnErr != nil {
			outcome, stopReason = lifecycle.OutcomeFailed, ""
		} else if response.StopReason == acp.StopReasonCancelled {
			outcome = lifecycle.OutcomeCancelled
		}
		if settleErr := s.settleLifecycleTurn(context.WithoutCancel(turnCtx), stopReason, outcome); settleErr != nil {
			returnErr = errors.Join(returnErr, settleErr)
		}
	}()

	done := make(chan promptTurnResult, 1)

	go func() {
		// A panic in the native turn is logged and converted into a turn
		// result so the prompt select loop can never block forever.
		defer func() {
			handleAgentGoroutinePanic(turnCtx, agentLogger(s.agent), "OpenCode native turn", func(recovered any) {
				done <- promptTurnResult{err: fmt.Errorf("opencode native turn panicked: %v", recovered)}
			}, recover())
		}()

		message, err := runNative(turnCtx)
		done <- promptTurnResult{message: message, err: err}
	}()

	timeout := s.turnTimeout()

	var timeoutC <-chan time.Time

	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		timeoutC = timer.C
	}

	for {
		select {
		case event := <-s.client.Events():
			if event.Type == eventServerConnected {
				if err := s.reconcilePermissions(turnCtx); err != nil {
					return failTurn(err)
				}

				if err := s.reconcileQuestions(turnCtx); err != nil {
					return failTurn(err)
				}

				continue
			}

			if err := s.handleEvent(turnCtx, event); err != nil {
				var assistantErr *opencode.AssistantError
				if errors.As(err, &assistantErr) {
					if response, cancelErr, cancelled := s.settlePromptCancellation(ctx, fenceTurn, params); cancelled {
						return response, cancelErr
					}

					return s.finishPromptTurn(ctx, turnCtx, params, promptTurnResult{err: err}, command, matchedCommand)
				}

				return failTurn(err)
			}
		case err := <-s.client.EventErrors():
			s.markStreamFailed(opencode.StreamErrorEpoch(err))

			if response, cancelErr, cancelled := s.settlePromptCancellation(ctx, fenceTurn, params); cancelled {
				return response, cancelErr
			}

			if fenceErr := fenceTurn(false); fenceErr != nil {
				return acp.PromptResponse{}, errors.Join(acp.NewInternalError(turnFailedData(causeTransport, err.Error(), 0, "")), fenceErr)
			}

			return acp.PromptResponse{}, acp.NewInternalError(turnFailedData(causeTransport, err.Error(), 0, ""))
		case result := <-done:
			if response, cancelErr, cancelled := s.settlePromptCancellation(ctx, fenceTurn, params); cancelled {
				return response, cancelErr
			}

			if runtimeErr := s.runtimeFailure(); runtimeErr != nil {
				return acp.PromptResponse{}, runtimeErr
			}

			return s.finishPromptTurn(ctx, turnCtx, params, result, command, matchedCommand)
		case <-timeoutC:
			// The cancel guard runs before all failure mapping: when a user
			// cancel and the turn deadline coincide, the turn resolves
			// deterministically to cancelled, never cause "timeout".
			cancelWon := s.wasCancelled() || ctx.Err() != nil

			if fenceErr := fenceTurn(false); fenceErr != nil {
				return acp.PromptResponse{}, fenceErr
			}

			if cancelWon || s.wasCancelled() {
				return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
			}

			return acp.PromptResponse{}, acp.NewInternalError(turnFailedData(causeTimeout, fmt.Sprintf("turn exceeded %s deadline", timeout), 0, ""))
		case <-turnCtx.Done():
			if response, cancelErr, cancelled := s.settlePromptCancellation(ctx, fenceTurn, params); cancelled {
				return response, cancelErr
			}

			if runtimeErr := s.runtimeFailure(); runtimeErr != nil {
				return acp.PromptResponse{}, runtimeErr
			}

			return acp.PromptResponse{}, acp.NewInternalError(turnFailedData(causeTransport, "OpenCode turn context ended without a cancellation route", 0, ""))
		}
	}
}

func (s *session) settlePromptFailure(
	err error,
	fenceTurn func(bool) error,
	params acp.PromptRequest,
) (acp.PromptResponse, error) {
	if errors.Is(err, errPromptCancelled) {
		if fenceErr := fenceTurn(true); fenceErr != nil {
			return acp.PromptResponse{}, fenceErr
		}

		return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
	}

	if runtimeErr := s.runtimeFailure(); runtimeErr != nil {
		return acp.PromptResponse{}, runtimeErr
	}

	if fenceErr := fenceTurn(false); fenceErr != nil {
		return acp.PromptResponse{}, errors.Join(err, fenceErr)
	}

	return acp.PromptResponse{}, err
}

func (s *session) settlePromptCancellation(
	ctx context.Context,
	fenceTurn func(bool) error,
	params acp.PromptRequest,
) (acp.PromptResponse, error, bool) {
	if !s.wasCancelled() && ctx.Err() == nil {
		return acp.PromptResponse{}, nil, false
	}

	if fenceErr := fenceTurn(true); fenceErr != nil {
		return acp.PromptResponse{}, fenceErr, true
	}

	return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil, true
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

func (s *session) finishPromptTurn(
	ctx context.Context,
	turnCtx context.Context,
	params acp.PromptRequest,
	result promptTurnResult,
	command opencode.NativeCommand,
	matchedCommand bool,
) (acp.PromptResponse, error) {
	if result.err != nil {
		if s.wasCancelled() || ctx.Err() != nil {
			//nolint:nilerr // The native error is intentionally suppressed for caller cancellation.
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
		}

		var assistantErr *opencode.AssistantError

		isAssistantErr := errors.As(result.err, &assistantErr)
		if matchedCommand && opencode.IsBadRequest(result.err) {
			refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
			if err := s.refreshCommands(refreshCtx); err != nil && s.agent != nil && s.agent.log != nil {
				s.agent.log.DebugContext(refreshCtx, "refresh OpenCode commands after command bad request failed", slog.String("session_id", string(s.id)), slog.String("error", err.Error()))
			}

			cancel()

			data := map[string]any{
				jsonFieldError:   "opencode_command_bad_request",
				jsonFieldMessage: result.err.Error(),
				jsonFieldCommand: command.Name,
			}
			if isAssistantErr {
				mergeAssistantErrorFields(data, assistantErr, false)
			}

			return acp.PromptResponse{}, acp.NewInvalidParams(data)
		}

		if isAssistantErr {
			structuredOutputRequested := !matchedCommand && len(s.outputSchema) > 0

			return acp.PromptResponse{}, acp.NewInternalError(assistantErrorData(assistantErr, structuredOutputRequested))
		}

		return acp.PromptResponse{}, acp.NewInternalError(turnFailedData(causeTransport, result.err.Error(), 0, ""))
	}

	final := result.message
	if err := s.emitMessage(turnCtx, final, false); err != nil {
		return acp.PromptResponse{}, err
	}

	usage := usageFromTokens(final.Info.Tokens)

	stopReason := stopReasonFromOpenCode(final.Info.Finish)
	if s.wasCancelled() || ctx.Err() != nil {
		stopReason = acp.StopReasonCancelled
	}

	s.finishTurn()

	if err := s.snapshotToStore(context.WithoutCancel(ctx)); err != nil {
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{
		StopReason:    stopReason,
		Usage:         usage,
		UserMessageId: params.MessageId,
		Meta:          s.structuredOutputMeta(final),
	}, nil
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
	defer s.updateMu.Unlock()

	updates, commit, mapErr := s.partUpdates(ctx, role, part, nativeDelta, replay)
	for _, update := range updates {
		if err := s.emitUpdate(ctx, update); err != nil {
			return errors.Join(mapErr, err)
		}
	}

	if commit != nil {
		commit()
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

			s.markActiveToolCallID(string(id))
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

		s.markActiveToolCallID(string(id))
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
			s.markActiveToolCallID(string(id))
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
		s.markActiveToolCallID(string(id))
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
		state.output = map[string]any{jsonFieldError: failure}
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

func (s *session) handleEvent(ctx context.Context, event opencode.Event) error {
	if s.shouldSuppressEvent(event) {
		return nil
	}

	// Raw events are non-authoritative debug output: a failed emit is recorded
	// internally and the authoritative prompt turn continues regardless.
	if err := s.emitRawOpenCodeEvent(ctx, event); err != nil && s.agent != nil && s.agent.log != nil {
		s.agent.log.DebugContext(ctx, "emit opencode raw event failed",
			slog.String("session_id", string(s.id)),
			slog.String("error", err.Error()),
		)
	}

	switch event.Type {
	case eventPermissionV2Asked, eventPermissionAsked:
		var req opencode.PermissionRequest
		if err := json.Unmarshal(event.Properties, &req); err != nil {
			return err
		}

		if event.Type == eventPermissionV2Asked {
			req.ReplyRoute = opencode.PermissionRouteAPI
		} else {
			req.ReplyRoute = opencode.PermissionRouteSession
		}

		if req.SessionID == s.idmap.NativeSessionID {
			return s.dispatchPermission(ctx, req)
		}
	case "todo.updated":
		var payload struct {
			SessionID string                `json:"sessionID"`
			Todos     []opencode.NativeTodo `json:"todos"`
		}
		if err := json.Unmarshal(event.Properties, &payload); err == nil && payload.SessionID == s.idmap.NativeSessionID {
			return s.emitPlan(ctx, payload.Todos)
		}
	case eventMessageUpdated:
		if info, ok := eventMessageInfo(event.Properties); ok && info.SessionID == s.idmap.NativeSessionID {
			s.recordMessageRole(info)
		}
	case eventSessionError:
		var nativeError opencode.SessionError
		if err := json.Unmarshal(event.Properties, &nativeError); err != nil {
			return err
		}

		if nativeError.SessionID == s.idmap.NativeSessionID && nativeError.Error != nil {
			return opencode.AssistantErrorFromNativeError(nativeError.Error)
		}
	case eventMessagePartUpdated, eventMessagePartCreated:
		part, delta, ok := eventPartUpdate(event.Properties)
		if ok && part.SessionID == s.idmap.NativeSessionID {
			// The native stream echoes the just-posted user message parts; the
			// ACP client already owns that content, so only non-user parts are
			// forwarded. Roles arrive via message.updated before any part event.
			if s.messageRole(part.MessageID) == roleUser {
				return nil
			}

			s.markActiveMessageID(part.MessageID)

			return s.emitPartUpdates(ctx, "assistant", part, delta, false)
		}
	case eventQuestionV2Asked, eventQuestionAsked:
		req, ok := eventQuestion(event.Properties)
		if ok && req.SessionID == s.idmap.NativeSessionID {
			if event.Type == eventQuestionV2Asked {
				req.ReplyRoute = opencode.QuestionRouteAPI
			} else {
				req.ReplyRoute = opencode.QuestionRouteSession
			}

			return s.dispatchQuestion(ctx, req)
		}
	}

	return nil
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

func (s *session) reconcilePermissions(ctx context.Context) error {
	requests, err := s.client.PendingPermissions(ctx)
	if err != nil {
		return err
	}

	for i := range requests {
		req := &requests[i]
		if req.SessionID == s.idmap.NativeSessionID {
			if err := s.dispatchPermission(ctx, *req); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *session) dispatchPermission(ctx context.Context, req opencode.PermissionRequest) error {
	if req.Tool.CallID == "" || !s.ownsCurrentToolCall(req.Tool.CallID) {
		replyCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		replyErr := s.client.ReplyPermission(replyCtx, req, permissionReplyReject, "stale or unknown tool call")

		cancel()

		return errors.Join(invalidRoute("permission request does not target a tool call published in the active turn"), replyErr)
	}

	return s.handlePermission(ctx, req)
}

func (s *session) reconcileQuestions(ctx context.Context) error {
	requests, err := s.client.PendingQuestions(ctx)
	if err != nil {
		return err
	}

	for _, req := range requests {
		if req.SessionID == s.idmap.NativeSessionID {
			if err := s.dispatchQuestion(ctx, req); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *session) dispatchQuestion(ctx context.Context, req opencode.QuestionRequest) error {
	if req.Tool.CallID == "" || s.ownsCurrentToolCall(req.Tool.CallID) {
		return s.handleQuestion(ctx, req)
	}

	rejectCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	rejectErr := s.client.RejectQuestion(rejectCtx, req)

	cancel()

	return errors.Join(
		invalidRoute("question request does not target a tool call published in the active turn"),
		rejectErr,
	)
}

func (s *session) handlePermission(ctx context.Context, req opencode.PermissionRequest) error {
	if req.ID == "" || req.SessionID == "" {
		return nil
	}

	if !s.claimPermissionRequest(req.ID) {
		return nil
	}

	s.addPendingPermission(req)
	owner := s.currentLifecycleOwner()
	if err := s.lifecycleActionPending(ctx, lifecycle.PendingAction(
		req.ID, lifecycle.ActionPermission, owner, true,
	)); err != nil {
		return err
	}

	conn := s.agent.connection()
	if conn == nil {
		_, _, cancelled := s.takePendingPermission(req.ID)

		replyCtx := ctx
		if cancelled || ctx.Err() != nil {
			backgroundCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			defer cancel()

			replyCtx = backgroundCtx
		}

		return s.client.ReplyPermission(replyCtx, req, permissionReplyReject, "client unavailable")
	}

	title := req.ActionName()
	if title == "" {
		title = "OpenCode permission"
	}

	status := acp.ToolCallStatusPending
	kind := acp.ToolKindOther

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: s.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(req.Tool.CallID),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			RawInput: map[string]any{
				"action":              req.ActionName(),
				"resources":           req.ResourceList(),
				"metadata":            req.Metadata,
				jsonFieldSource:       req.Source,
				"save":                req.Save,
				permissionReplyAlways: req.Always,
				"toolCallId":          req.Tool.CallID,
				jsonFieldMessageID:    req.Tool.MessageID,
			},
		},
		Options: []acp.PermissionOption{
			{OptionId: permissionReplyOnce, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: permissionReplyAlways, Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: permissionReplyReject, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
		Meta: mergeMeta(map[string]any{opencodeMetaKey: map[string]any{routeFieldRequestID: req.ID, opencodeNativeIDMetaKey: req.SessionID}}, s.lifecycleActionMeta(req.ID, owner)),
	})
	if err != nil {
		_, ok, cancelled := s.takePendingPermission(req.ID)
		if !ok {
			return errPromptCancelled
		}

		if cancelled || s.wasCancelled() || ctx.Err() != nil {
			replyCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			_ = s.client.ReplyPermission(replyCtx, req, permissionReplyReject, reasonCancelled)

			cancel()

			return errPromptCancelled
		}

		replyCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		replyErr := s.client.ReplyPermission(replyCtx, req, permissionReplyReject, "client permission request failed")

		cancel()

		if replyErr != nil {
			return errors.Join(err, replyErr)
		}

		return err
	}

	reply := permissionReplyReject

	if resp.Outcome.Selected != nil {
		switch resp.Outcome.Selected.OptionId {
		case permissionReplyOnce, permissionReplyAlways, permissionReplyReject:
			reply = string(resp.Outcome.Selected.OptionId)
		}
	}

	if resp.Outcome.Cancelled != nil {
		reply = permissionReplyReject
	}
	actionState := lifecycle.ActionDeclined
	if reply == permissionReplyOnce || reply == permissionReplyAlways {
		actionState = lifecycle.ActionAccepted
	} else if resp.Outcome.Cancelled != nil {
		actionState = lifecycle.ActionCancelled
	}
	if err := s.lifecycleActionResolved(ctx, req.ID, actionState); err != nil {
		return err
	}

	_, ok, cancelled := s.takePendingPermission(req.ID)
	if !ok {
		return errPromptCancelled
	}

	if cancelled || ctx.Err() != nil {
		replyCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		if err := s.client.ReplyPermission(replyCtx, req, permissionReplyReject, reasonCancelled); err != nil {
			return err
		}

		return errPromptCancelled
	}

	return s.client.ReplyPermission(ctx, req, reply, "")
}

func (s *session) handleQuestion(ctx context.Context, req opencode.QuestionRequest) error {
	if req.ID == "" || req.SessionID == "" {
		return nil
	}

	if !s.claimQuestionRequest(req.ID) {
		return nil
	}

	s.addPendingQuestion(req)
	owner := s.currentLifecycleOwner()
	if err := s.lifecycleActionPending(ctx, lifecycle.PendingAction(req.ID, lifecycle.ActionElicitation, owner, true)); err != nil {
		return err
	}

	conn := s.agent.connection()
	if conn == nil || !s.agent.clientSupportsFormElicitation() {
		_, _, cancelled := s.takePendingQuestion(req.ID)

		rejectCtx := ctx
		if cancelled || ctx.Err() != nil {
			backgroundCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			defer cancel()

			rejectCtx = backgroundCtx
		}

		return s.client.RejectQuestion(rejectCtx, req)
	}

	request, propertyIDs := questionElicitationRequest(req)
	if request.Form != nil {
		request.Form.Meta = mergeMeta(request.Form.Meta, s.lifecycleActionMeta(req.ID, owner))
	}

	scope := elicitationScope{SessionID: s.id, TurnNonce: s.currentTurnNonce()}
	if req.Tool.CallID != "" {
		scope.ToolCallID = acp.ToolCallId(req.Tool.CallID)
	} else {
		requestIDValue := acp.RequestIdStr(req.ID)
		requestID := acp.RequestId{Str: &requestIDValue}
		scope.RequestID = &requestID
	}

	resp, err := conn.CreateElicitation(ctx, request, scope)
	if err != nil {
		_, ok, cancelled := s.takePendingQuestion(req.ID)
		if !ok {
			return errPromptCancelled
		}

		if cancelled || s.wasCancelled() || ctx.Err() != nil {
			rejectCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			_ = s.client.RejectQuestion(rejectCtx, req)

			cancel()

			return errPromptCancelled
		}

		rejectCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		rejectErr := s.client.RejectQuestion(rejectCtx, req)

		cancel()

		if rejectErr != nil {
			return errors.Join(err, rejectErr)
		}

		return err
	}

	if resp.Accept == nil {
		if err := s.lifecycleActionResolved(ctx, req.ID, lifecycle.ActionDeclined); err != nil {
			return err
		}
		_, ok, cancelled := s.takePendingQuestion(req.ID)
		if !ok {
			return errPromptCancelled
		}

		rejectCtx := ctx
		if cancelled || ctx.Err() != nil {
			backgroundCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			defer cancel()

			rejectCtx = backgroundCtx
		}

		if err := s.client.RejectQuestion(rejectCtx, req); err != nil {
			return err
		}

		if cancelled || ctx.Err() != nil {
			return errPromptCancelled
		}

		return nil
	}

	_, ok, cancelled := s.takePendingQuestion(req.ID)
	if !ok {
		return errPromptCancelled
	}

	if cancelled || ctx.Err() != nil {
		rejectCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()

		if err := s.client.RejectQuestion(rejectCtx, req); err != nil {
			return err
		}

		return errPromptCancelled
	}

	if err := s.lifecycleActionResolved(ctx, req.ID, lifecycle.ActionAccepted); err != nil {
		return err
	}
	return s.client.ReplyQuestion(ctx, req, questionAnswersFromContent(resp.Accept.Content, propertyIDs))
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

func (s *session) drainClientBacklog(ctx context.Context) error {
	for {
		select {
		case event := <-s.client.Events():
			switch event.Type {
			case eventPermissionV2Asked, eventPermissionAsked:
				var req opencode.PermissionRequest
				if err := json.Unmarshal(event.Properties, &req); err != nil {
					return err
				}

				if req.SessionID != s.idmap.NativeSessionID {
					continue
				}

				replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
				replyErr := s.client.ReplyPermission(replyCtx, req, permissionReplyReject, "stale callback from a completed turn")

				cancel()

				return errors.Join(invalidRoute("permission callback arrived outside its originating turn"), replyErr)
			case eventQuestionV2Asked, eventQuestionAsked:
				req, ok := eventQuestion(event.Properties)
				if !ok {
					return errors.New("invalid OpenCode question callback in turn backlog")
				}

				if req.SessionID != s.idmap.NativeSessionID {
					continue
				}

				replyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
				replyErr := s.client.RejectQuestion(replyCtx, req)

				cancel()

				return errors.Join(invalidRoute("question callback arrived outside its originating turn"), replyErr)
			}
		case <-s.client.EventErrors():
			continue
		default:
			return nil
		}
	}
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
	s.agent.observe.ObserveFirstPromptUpdate(ctx)

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	return conn.SessionUpdate(ctx, acp.SessionNotification{
		Meta:      turnRouteMetaFromContext(ctx),
		SessionId: s.id,
		Update:    update,
	})
}

func (s *session) emitRawOpenCodeEvent(ctx context.Context, event opencode.Event) error {
	if !s.rawMessages.Enabled() || len(event.Raw) == 0 {
		return nil
	}

	conn := s.agent.connection()
	if conn == nil {
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

	s.rawEventMu.Lock()
	defer s.rawEventMu.Unlock()

	sequence := s.rawSeq + 1

	payload := map[string]any{
		jsonFieldSessionID: s.id,
		jsonFieldSequence:  sequence,
		jsonFieldSource:    rawEventSource,
		jsonFieldEvent:     raw,
	}
	if meta := turnRouteMetaFromContext(ctx); meta != nil {
		payload["_meta"] = meta
	}

	capped, err := capRawEventPayload(payload)
	if err != nil {
		return err
	}

	if err := conn.NotifyExtension(ctx, RawEventMethod, capped); err != nil {
		return err
	}

	s.rawSeq = sequence

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
	defer s.updateMu.Unlock()

	current := emittedUsageState{used: update.UsageUpdate.Used, size: update.UsageUpdate.Size}
	if messageID != "" {
		if previous, ok := s.emittedUsage[messageID]; ok && previous == current {
			return nil
		}
	}

	if err := s.emitUpdate(ctx, *update); err != nil {
		return err
	}

	if messageID != "" {
		s.emittedUsage[messageID] = current
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
	case "failed", nativeStatusError:
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
	case "in_progress", "running":
		return acp.PlanEntryStatusInProgress
	default:
		return acp.PlanEntryStatusPending
	}
}
