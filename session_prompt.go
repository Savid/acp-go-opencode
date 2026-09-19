package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	limitSessionPrompt = "session_prompt"

	stopReasonLength    = "length"
	stopReasonMaxTokens = "max_tokens"
	stopReasonError     = "error"
)

// nativePrompt retains ordered native parts and the text used for command routing.
type nativePrompt struct {
	message string
	images  []map[string]any
	parts   []map[string]any
}

// mapPrompt converts ACP prompt content to opencode's prompt shape. Embedded
// context is appended to the message text; images run the core input gates
// and travel as inline base64.
func (s *session) mapPrompt(ctx context.Context, blocks []acp.ContentBlock) (nativePrompt, error) {
	if len(blocks) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	decoded, refusal, err := image.ValidatePrompt(ctx, blocks, image.Options{
		Limits:      s.agent.options.ImageLimits.core(),
		HandoffRoot: s.agent.options.InputHandoffRoot,
		Blobs:       func(string) image.BlobDisposition { return image.BlobGate },
	})
	if err != nil {
		return nativePrompt{}, err
	}

	if refusal != nil {
		return nativePrompt{}, refusal.InvalidParams()
	}

	prompt := nativePrompt{}
	textParts := make([]string, 0, len(blocks))
	appendText := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}

		textParts = append(textParts, text)
		prompt.parts = append(prompt.parts, map[string]any{fieldType: fieldText, fieldText: text})
	}

	media := make(map[int]image.Decoded, len(decoded))
	for _, item := range decoded {
		media[item.Block] = item
	}

	for index, block := range blocks {
		if item, gated := media[index]; gated {
			if !image.IsImageMIME(item.MIME) {
				if block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil {
					appendText(block.Resource.Resource.BlobResourceContents.Uri)
				}

				continue
			}

			if supported := s.modelImageCapability(); supported != nil && !*supported {
				return nativePrompt{}, image.UnsupportedByModel(item.Field, item.Index).InvalidParams()
			}

			part := map[string]any{fieldType: partFile, "mime": item.MIME, "url": "data:" + item.MIME + ";base64," + base64.StdEncoding.EncodeToString(item.Data)}
			prompt.images = append(prompt.images, part)
			prompt.parts = append(prompt.parts, part)

			continue
		}

		switch {
		case block.Text != nil:
			if !wire.AudienceIsUserOnly(block.Text.Annotations) {
				appendText(block.Text.Text)
			}
		case block.ResourceLink != nil:
			appendText(block.ResourceLink.Uri)
		case block.Resource != nil:
			if text := block.Resource.Resource.TextResourceContents; text != nil {
				appendText(wire.ContextResourceText(text.Uri, text.Text))
			}
		default:
			return nativePrompt{}, wire.Unsupported("prompt")
		}
	}

	prompt.message = strings.Join(textParts, "\n")
	if len(prompt.parts) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	return prompt, nil
}

// prompt sends one turn to opencode and streams updates until the run settles.
func (s *session) prompt(ctx context.Context, params acp.PromptRequest, raw json.RawMessage) (acp.PromptResponse, error) {
	meta := lifecycle.RetainRequestMetadata(params.Meta, raw)

	submission, paramErr := lifecycle.DecodePromptCorrelation(meta, s.lifecycleNegotiated())
	if paramErr != nil {
		return acp.PromptResponse{}, wire.ParamRefusal(paramErr)
	}

	if err := s.admissionError(); err != nil {
		return acp.PromptResponse{}, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	// The turn runs under its own cancellation, not the request's: the SDK
	// cancels a prompt's context when the next prompt for the session arrives,
	// and a refused peer prompt must not end this turn.
	turnCtx, cancelTurn := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelTurn()

	t := &turn{
		cycle:      cycle{Cycle: lifecycle.Cycle{Origin: lifecycle.CauseSubmission}, state: cycleState{}},
		submission: submission,
		cancelTurn: cancelTurn,
		settled:    make(chan struct{}),
		finished:   make(chan struct{}),
		messageID:  opencode.NewMessageID(),
	}
	defer close(t.finished)

	// The turn is installed before the request-scoped work a cancel has to be
	// able to interrupt, and the busy check shares its critical section so a
	// cycle the pump opens can neither be missed nor wedge the session.
	s.mu.Lock()
	if s.cycle != nil || s.closing {
		s.mu.Unlock()

		return acp.PromptResponse{}, wire.Backpressure(limitSessionPrompt)
	}

	s.turn = t
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		s.mu.Unlock()
	}()

	mapped, err := s.mapPrompt(turnCtx, params.Prompt)
	if err != nil {
		if turnCtx.Err() != nil {
			return wire.CancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	rt, err := s.ensureRuntime(turnCtx)
	if err != nil {
		if turnCtx.Err() != nil {
			return wire.CancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	// A cancel that lands before native dispatch answers cancelled with no
	// native work.
	if turnCtx.Err() != nil {
		return wire.CancelledResponse(params), nil
	}

	s.mu.Lock()
	if s.runtime != rt || !rt.alive() {
		s.mu.Unlock()

		return acp.PromptResponse{}, s.transportFailure(context.WithoutCancel(ctx), rt, nil)
	}

	t.runtime = rt
	s.mu.Unlock()

	requestCtx, requestCancel := context.WithCancel(context.WithoutCancel(ctx))
	requestDone := make(chan struct{})

	defer func() { requestCancel(); <-requestDone }()

	path, body := s.promptRequest(mapped, t.messageID)

	go func() {
		defer close(requestDone)

		var message opencode.NativeMessage

		err := rt.client.Do(requestCtx, s.cwd, http.MethodPost, opencode.SessionPath(s.nativeID)+path, body, &message)
		select {
		case rt.results <- nativePromptResult{turn: t, message: message, err: err}:
		case <-requestCtx.Done():
		case <-rt.done:
		}
	}()

	select {
	case <-t.settled:
	case <-turnCtx.Done():
		// Whoever ended the turn already interrupted opencode; this bounds how
		// long settlement may take before the binding is dropped.
		select {
		case <-t.settled:
		case <-time.After(sessionAbortTimeout):
			s.stopRuntime(context.WithoutCancel(ctx), rt)
			t.settle(turnTransportEnded)
		}
	}

	return s.settleTurn(ctx, rt, t, params)
}

// dispatchFailure classifies a prompt command opencode never accepted: a native
// rejection carries its text as a provider failure, a dead child is a
// process exit, and everything else is transport.
func (s *session) dispatchFailure(ctx context.Context, rt *binding, err error) error {
	var commandErr *opencode.HTTPError
	if errors.As(err, &commandErr) {
		return wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: nativeErrorText(&commandErr.Native)})
	}

	return s.transportFailure(ctx, rt, err)
}

// transportFailure recovers the real cause behind a lost native stream: the
// child's exit status and stderr tail where it died, otherwise the
// transport error.
func (s *session) transportFailure(ctx context.Context, rt *binding, err error) error {
	return wire.TurnFailed(vendor, wire.TransportFailure(ctx, rt.server.proc, "opencode process", err, rt.server.stream.Err))
}

// cycleVerdict is how one cycle ended, in the terms the lifecycle stream and
// the prompt response need.
type cycleVerdict struct {
	outcome    lifecycle.Outcome
	stopReason string
	failure    error
}

// judgeCycle records how a natively settled cycle finished. The cancel guard
// runs before every failure mapping.
func judgeCycle(c *cycle, failure error, cancelled bool) cycleVerdict {
	switch {
	case cancelled:
		return cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case failure != nil:
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: failure}
	case c.state.stopReason == stopReasonError:
		message := strings.TrimSpace(c.state.errorMessage)
		if message == "" {
			message = "opencode reported a turn error"
		}

		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: message})}
	}

	stop := acp.StopReasonEndTurn
	outcome := lifecycle.OutcomeSuccess

	switch c.state.stopReason {
	case stopReasonLength, stopReasonMaxTokens:
		stop = acp.StopReasonMaxTokens
		outcome = lifecycle.OutcomeLimit
	case statusInterrupted:
		stop = acp.StopReasonCancelled
		outcome = lifecycle.OutcomeCancelled
	case statusComplete, "stop", "tool-calls":
	default:
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: "unknown native finish status: " + c.state.stopReason})}
	}

	return cycleVerdict{outcome: outcome, stopReason: string(stop)}
}

// settleTurn is the one settlement point every accepted prompt reaches:
// usage and session info, the durable mirror commit, the terminal idle, and
// only then the response or error.
func (s *session) settleTurn(ctx context.Context, rt *binding, t *turn, params acp.PromptRequest) (acp.PromptResponse, error) {
	s.beginSettlement(&t.cycle)

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	s.mu.Lock()
	cancelled := t.cancelled
	s.mu.Unlock()

	var verdict cycleVerdict

	commitFailed := false

	switch {
	case cancelled:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case t.ended == turnTransportEnded:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: s.transportFailure(settleCtx, rt, nil)}
	default:
		verdict = judgeCycle(&t.cycle, s.cycleFailure(&t.cycle), false)
	}

	if t.ended == turnSettled {
		if !cancelled {
			s.emitUsage(settleCtx, &t.state)
			s.emitSessionInfo(settleCtx, params.Prompt)
		}

		if err := s.commitMirror(settleCtx, rt); err != nil {
			s.stopRuntime(settleCtx, rt)
			s.fenceStream()

			commitFailed = true
			verdict.failure = s.mirrorFailure(&t.state, err)
			verdict.outcome = lifecycle.OutcomeFailed
		}
	}

	if s.claimCancellation(&t.cycle) && !commitFailed {
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	}

	if err := s.lc.Idle(settleCtx, t.Cycle, verdict.stopReason, verdict.outcome); err != nil && verdict.failure == nil {
		verdict.failure = err
	}

	// The incarnation ends with the generation that ran the turn, not with how
	// the turn ended: a native exit after a settled turn must not leave the
	// next process publishing on this stream.
	if t.ended == turnTransportEnded || !s.boundTo(rt) {
		s.fenceStream()
	}

	if verdict.failure != nil {
		return acp.PromptResponse{}, verdict.failure
	}

	var meta map[string]any

	if len(t.state.structured) > 0 {
		var value any
		if json.Unmarshal(t.state.structured, &value) == nil {
			meta = map[string]any{vendor: map[string]any{metaStructuredOutputKey: value}}
		}
	}

	return acp.PromptResponse{
		Meta: meta, Usage: promptUsage(&t.state),
		StopReason:    acp.StopReason(verdict.stopReason),
		UserMessageId: params.MessageId,
	}, nil
}

// mirrorFailure maps a failed mirror commit onto the turn-failure shape. A
// turn that delivered image bytes lost their durable replay representation.
func (s *session) mirrorFailure(state *cycleState, err error) error {
	s.agent.log.Error("session mirror commit failed", slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

	failure := wire.TurnFailure{Cause: wire.CauseTransport, Message: "session mirror commit failed"}
	if state.imagesEmitted {
		failure.Message = "image output is no longer available from the artifact store"
		failure.Stage = image.OutputStage
		failure.Reason = image.ReasonStorageFailed
	}

	return wire.TurnFailed(vendor, failure)
}

// boundTo reports whether rt is still this session's live binding.
func (s *session) boundTo(rt *binding) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.runtime == rt && rt.alive()
}

func (s *session) promptRequest(mapped nativePrompt, id string) (string, any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	head, args, _ := strings.Cut(mapped.message, " ")

	if s.options.OutputSchema == nil {
		for _, command := range s.commands {
			// The catalog publishes only sanitized names, so only those route.
			if wire.ValidCommandName(command.Name) && head == "/"+command.Name {
				return nativeCommandPath, opencode.CommandRequest{MessageID: id, Agent: s.mode, Model: s.model, Variant: s.effort, Command: command.Name, Arguments: args, Parts: mapped.images}
			}
		}
	}

	req := opencode.MessageRequest{MessageID: id, Agent: s.mode, Variant: s.effort, Parts: mapped.parts}
	if s.model != "" {
		provider, model, _ := strings.Cut(s.model, "/")
		req.Model = &opencode.ModelSelector{ProviderID: provider, ModelID: model}
	}

	if s.options.OutputSchema != nil {
		req.Format = &opencode.OutputFormat{Type: opencode.OutputFormatJSONSchema, Schema: wire.CloneMap(s.options.OutputSchema)}
	}

	return "/message", req
}

func promptUsage(state *cycleState) *acp.Usage {
	if len(state.messages) == 0 {
		return nil
	}

	usage := &acp.Usage{}
	read, write, thought := 0, 0, 0

	for id := range state.messages {
		message := state.messages[id]
		if message.Role != roleAssistant {
			continue
		}

		usage.InputTokens += int(message.Tokens.Input)
		usage.OutputTokens += int(message.Tokens.Output)
		read += int(message.Tokens.Cache.Read)
		write += int(message.Tokens.Cache.Write)
		thought += int(message.Tokens.Reasoning)
	}

	usage.TotalTokens = usage.InputTokens + usage.OutputTokens + read + write + thought
	usage.CachedReadTokens = &read
	usage.CachedWriteTokens = &write
	usage.ThoughtTokens = &thought

	return usage
}
