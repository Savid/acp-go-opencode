package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

	// nativeCauseMaxBytes bounds the native cause text a failure carries.
	nativeCauseMaxBytes = 2048
	// processExitGrace is how long failure classification waits for a dead
	// child to be reaped after its stdout closed.
	processExitGrace = 2 * time.Second
)

// nativePrompt is one mapped prompt: the message text plus attached images.
type nativePrompt struct {
	message string
	images  []map[string]any
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

	textParts := make([]string, 0, len(blocks))
	contextParts := make([]string, 0)

	for _, block := range blocks {
		switch {
		case block.Text != nil:
			if audienceIsUserOnly(block.Text.Annotations) {
				continue
			}

			textParts = append(textParts, block.Text.Text)
		case block.Image != nil:
		case block.ResourceLink != nil:
			textParts = append(textParts, strings.TrimSpace(block.ResourceLink.Uri))
		case block.Resource != nil:
			if blob := block.Resource.Resource.BlobResourceContents; blob != nil {
				textParts = append(textParts, strings.TrimSpace(blob.Uri))
			}

			if text := block.Resource.Resource.TextResourceContents; text != nil {
				textParts = append(textParts, strings.TrimSpace(text.Uri))
				contextParts = append(contextParts, contextResourceText(text.Uri, text.Text))
			}
		default:
			return nativePrompt{}, wire.Unsupported("prompt")
		}
	}

	prompt := nativePrompt{
		message: strings.Join(append(textParts, contextParts...), "\n"),
		images:  make([]map[string]any, 0, len(decoded)),
	}

	for index := range decoded {
		if decoded[index].Field == image.FieldPromptResource && !image.IsImageMIME(decoded[index].MIME) {
			continue
		}

		if supported := s.modelImageCapability(); supported != nil && !*supported {
			return nativePrompt{}, image.UnsupportedByModel(decoded[index].Field, decoded[index].Index).InvalidParams()
		}

		prompt.images = append(prompt.images, map[string]any{fieldType: partFile, "mime": decoded[index].MIME, "url": "data:" + decoded[index].MIME + ";base64," + base64.StdEncoding.EncodeToString(decoded[index].Data)})
	}

	if strings.TrimSpace(prompt.message) == "" && len(prompt.images) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	return prompt, nil
}

func audienceIsUserOnly(annotations *acp.Annotations) bool {
	return annotations != nil && len(annotations.Audience) == 1 && annotations.Audience[0] == acp.RoleUser
}

func contextResourceText(uri string, text string) string {
	escape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

	return "\n<context ref=\"" + escape.Replace(uri) + "\">\n" + escape.Replace(text) + "\n</context>"
}

// prompt sends one turn to opencode and streams updates until the run settles.
func (s *session) prompt(ctx context.Context, params acp.PromptRequest, raw json.RawMessage) (acp.PromptResponse, error) {
	meta := lifecycle.RetainRequestMetadata(params.Meta, raw)

	submission, paramErr := lifecycle.DecodePromptCorrelation(meta, s.lifecycleNegotiated())
	if paramErr != nil {
		return acp.PromptResponse{}, invalidParam(paramErr)
	}

	if err := s.admissionError(); err != nil {
		return acp.PromptResponse{}, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	s.mu.Lock()
	busy := s.cycle != nil
	s.mu.Unlock()

	if busy {
		return acp.PromptResponse{}, wire.Backpressure(limitSessionPrompt)
	}

	mapped, err := s.mapPrompt(ctx, params.Prompt)
	if err != nil {
		if ctx.Err() != nil {
			return cancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	// session/cancel cancels this request's context through the SDK. A cancel
	// that lands before native dispatch creates neither submission nor turn and
	// answers cancelled.
	if ctx.Err() != nil {
		return cancelledResponse(params), nil
	}

	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	t := &turn{
		cycle:      cycle{origin: lifecycle.CauseSubmission, state: cycleState{}},
		submission: submission,
		settled:    make(chan struct{}),
		finished:   make(chan struct{}),
		messageID:  opencode.NewMessageID(),
	}
	defer close(t.finished)

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

	if timeout := s.agent.options.TurnTimeout; timeout > 0 {
		timer := time.AfterFunc(timeout, func() { s.timeout(context.WithoutCancel(ctx), t) })
		defer timer.Stop()
	}

	requestCtx, requestCancel := context.WithCancel(context.WithoutCancel(ctx))
	requestDone := make(chan struct{})

	defer func() { requestCancel(); <-requestDone }()

	path, body := s.promptRequest(mapped, t.messageID)

	go func() {
		defer close(requestDone)

		var message opencode.NativeMessage

		err := rt.client.Do(requestCtx, s.cwd, http.MethodPost, opencode.SessionPath(string(s.id))+path, body, &message)
		select {
		case rt.results <- nativePromptResult{turn: t, message: message, err: err}:
		case <-requestCtx.Done():
		case <-rt.done:
		}
	}()

	select {
	case <-t.settled:
	case <-ctx.Done():
		s.cancel(ctx)

		select {
		case <-t.settled:
		case <-time.After(sessionAbortTimeout):
			s.stopRuntime(context.WithoutCancel(ctx), rt)
			t.settle(turnTransportEnded)
		}
	}

	return s.settleTurn(ctx, rt, t, params)
}

func cancelledResponse(params acp.PromptRequest) acp.PromptResponse {
	return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}
}

// dispatchFailure classifies a prompt command opencode never accepted: a native
// rejection carries its text as a provider failure, a dead child is a
// process exit, and everything else is transport.
func (s *session) dispatchFailure(ctx context.Context, rt *binding, err error) error {
	var commandErr *opencode.HTTPError
	if errors.As(err, &commandErr) {
		return turnFailure(wire.CauseProvider, nativeErrorText(&commandErr.Native))
	}

	return s.transportFailure(ctx, rt, err)
}

// transportFailure recovers the real cause behind a lost native stream: the
// child's exit status and last stderr line where it died, otherwise the
// transport error.
func (s *session) transportFailure(ctx context.Context, rt *binding, err error) error {
	waitCtx, cancel := context.WithTimeout(ctx, processExitGrace)
	defer cancel()

	if result, waitErr := rt.server.proc.Wait(waitCtx); waitErr == nil {
		message := fmt.Sprintf("opencode process exited with status %d", result.ExitCode)
		if result.Signal != 0 {
			message = fmt.Sprintf("opencode process was killed by signal %d", result.Signal)
		}

		if line := rt.server.stderr.lastLine(); line != "" {
			message += ": " + line
		}

		return turnFailure(wire.CauseProcessExit, message)
	}

	if err == nil {
		err = rt.server.stream.Err()
	}

	if err == nil {
		err = errors.New("opencode event stream closed mid-turn")
	}

	return turnFailure(wire.CauseTransport, err.Error())
}

func turnFailure(cause string, message string) *acp.RequestError {
	return wire.TurnFailed(vendor, wire.TurnFailure{Cause: cause, Message: boundNativeCause(message)})
}

// boundNativeCause is the single gate every native cause text passes through
// before it reaches a client.
func boundNativeCause(message string) string {
	if len(message) > nativeCauseMaxBytes {
		message = message[:nativeCauseMaxBytes]
	}

	return strings.TrimSpace(strings.ToValidUTF8(message, ""))
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
func judgeCycle(c *cycle, cancelled bool) cycleVerdict {
	switch {
	case cancelled:
		return cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case c.failure != nil:
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: c.failure}
	case c.state.stopReason == stopReasonError:
		message := strings.TrimSpace(c.state.errorMessage)
		if message == "" {
			message = "opencode reported a turn error"
		}

		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: turnFailure(wire.CauseProvider, message)}
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
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: turnFailure(wire.CauseProvider, "unknown native finish status: "+c.state.stopReason)}
	}

	return cycleVerdict{outcome: outcome, stopReason: string(stop)}
}

// settleTurn is the one settlement point every accepted prompt reaches:
// usage and session info, the durable mirror commit, the terminal idle, and
// only then the response or error.
func (s *session) settleTurn(ctx context.Context, rt *binding, t *turn, params acp.PromptRequest) (acp.PromptResponse, error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	s.mu.Lock()
	cancelled, timedOut := t.cancelled, t.timedOut
	s.mu.Unlock()

	var verdict cycleVerdict

	switch {
	case cancelled:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case timedOut:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: turnFailure(wire.CauseTimeout, fmt.Sprintf("opencode turn exceeded %s", s.agent.options.TurnTimeout))}
	case t.ended == turnTransportEnded:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: s.transportFailure(settleCtx, rt, nil)}
	default:
		verdict = judgeCycle(&t.cycle, false)
	}

	if t.ended == turnSettled {
		if !cancelled {
			s.emitUsage(settleCtx, &t.state)
			s.emitSessionInfo(settleCtx, params.Prompt)
		}

		if err := s.commitMirror(settleCtx); err != nil {
			s.stopRuntime(settleCtx, rt)
			s.lcFence()
			verdict.failure = s.mirrorFailure(err)
			verdict.outcome = lifecycle.OutcomeFailed
		}
	}

	if err := s.lcIdle(settleCtx, &t.cycle, verdict); err != nil && verdict.failure == nil {
		verdict.failure = err
	}

	if t.ended == turnTransportEnded {
		s.lcFence()
	}

	if verdict.failure != nil {
		return acp.PromptResponse{}, verdict.failure
	}

	var meta map[string]any

	if len(t.state.structured) > 0 {
		var value any
		if json.Unmarshal(t.state.structured, &value) == nil {
			meta = map[string]any{vendor: map[string]any{"structuredOutput": value}}
		}
	}

	return acp.PromptResponse{
		Meta: meta, Usage: promptUsage(&t.state),
		StopReason:    acp.StopReason(verdict.stopReason),
		UserMessageId: params.MessageId,
	}, nil
}

// mirrorFailure reports a failed durability boundary without exposing native content.
func (s *session) mirrorFailure(err error) error {
	s.agent.log.Error("session mirror commit failed", slog.String(nativeSessionIDKey, string(s.id)), slog.String("reason", err.Error()))

	return turnFailure(wire.CauseTransport, "session mirror commit failed")
}

func (s *session) promptRequest(mapped nativePrompt, id string) (string, any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts := []map[string]any{}
	if mapped.message != "" {
		parts = append(parts, map[string]any{fieldType: fieldText, fieldText: mapped.message})
	}

	parts = append(parts, mapped.images...)
	for _, command := range s.commands {
		head, args, _ := strings.Cut(mapped.message, " ")
		if head == "/"+command.Name && s.options.OutputSchema == nil {
			return nativeCommandPath, opencode.CommandRequest{MessageID: id, Agent: s.mode, Model: s.model, Variant: s.effort, Command: command.Name, Arguments: args, Parts: mapped.images}
		}
	}

	req := opencode.MessageRequest{MessageID: id, Agent: s.mode, Variant: s.effort, Parts: parts}
	if s.model != "" {
		provider, model, _ := strings.Cut(s.model, "/")
		req.Model = &opencode.ModelSelector{ProviderID: provider, ModelID: model}
	}

	if s.options.OutputSchema != nil {
		req.Format = &opencode.OutputFormat{Type: opencode.OutputFormatJSONSchema, Schema: cloneAnyMap(s.options.OutputSchema)}
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
