//nolint:tagliatelle // OpenCode native event payloads use sessionID wire names.
package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

var errPromptCancelled = errors.New("prompt cancelled")

const (
	eventServerConnected    = "server.connected"
	eventPermissionV2Asked  = "permission.v2.asked"
	eventMessagePartCreated = "message.part.created"
	eventQuestionAsked      = "question.asked"
	eventQuestionV2Asked    = "question.v2.asked"

	partTypeText       = "text"
	partTypeFile       = "file"
	partTypeTool       = "tool"
	partTypeStepFinish = "step-finish"
	contentTypeAudio   = "audio"
	mediaTypeImage     = "image"
	blockTypeUnknown   = "unknown"
	roleUser           = "user"
	defaultMimeType    = "application/octet-stream"

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
	priorityHigh          = "high"
	priorityLow           = "low"
)

func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	session, err := a.session(params.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	return session.Prompt(ctx, params)
}

func (a *Agent) Cancel(ctx context.Context, params acp.CancelNotification) error {
	session, err := a.session(params.SessionId)
	if err != nil {
		return err
	}

	if err := session.ensureNotPoisoned(); err != nil {
		return err
	}

	session.cancelTurn()

	cancelCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()

	return session.client.Abort(cancelCtx, session.idmap.NativeSessionID)
}

func (s *session) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	if err := s.ensureNotPoisoned(); err != nil {
		return acp.PromptResponse{}, err
	}

	invocation, command, matchedCommand, err := s.resolvePromptCommand(ctx, params)
	if err != nil {
		return acp.PromptResponse{}, err
	}

	acquire := s.acquireTurn
	if matchedCommand {
		acquire = s.acquireCommandTurn
	}

	release, err := acquire(ctx)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	var runNative func(context.Context) (opencode.NativeMessage, error)

	if matchedCommand {
		runNative, err = s.commandNativeRun(ctx, params, invocation, command)
	} else {
		runNative, err = s.messageNativeRun(ctx, params)
	}

	if err != nil {
		return acp.PromptResponse{}, err
	}

	if err := s.drainClientBacklog(ctx); err != nil {
		if errors.Is(err, errPromptCancelled) {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
		}

		return acp.PromptResponse{}, err
	}

	turnCtx := s.beginTurn(ctx)
	defer s.finishTurn()

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
	parts, err := commandPromptParts(params.Prompt[1:])
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
	parts, err := promptToOpenCodeParts(params.Prompt)
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
	var abortOnce sync.Once

	abortTurn := func() {
		abortOnce.Do(func() {
			abortCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
			_ = s.client.Abort(abortCtx, s.idmap.NativeSessionID)

			cancel()
		})
	}

	failTurn := func(err error) (acp.PromptResponse, error) {
		if errors.Is(err, errPromptCancelled) {
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
		}

		abortTurn()

		return acp.PromptResponse{}, err
	}
	if err := s.reconcilePermissions(turnCtx); err != nil {
		return failTurn(err)
	}

	if err := s.reconcileQuestions(turnCtx); err != nil {
		return failTurn(err)
	}

	done := make(chan promptTurnResult, 1)

	go func() {
		message, err := runNative(turnCtx)
		done <- promptTurnResult{message: message, err: err}
	}()

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
				return failTurn(err)
			}
		case err := <-s.client.EventErrors():
			s.markStreamFailed(opencode.StreamErrorEpoch(err))
			abortTurn()

			return acp.PromptResponse{}, acp.NewInternalError(map[string]any{jsonFieldError: "opencode_sse_disconnect", jsonFieldMessage: err.Error()})
		case result := <-done:
			return s.finishPromptTurn(ctx, turnCtx, params, result, command, matchedCommand)
		case <-turnCtx.Done():
			abortTurn()

			return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
		}
	}
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
		if s.wasCancelled() || turnCtx.Err() != nil {
			//nolint:nilerr // The native error is intentionally suppressed for caller cancellation.
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled, UserMessageId: params.MessageId}, nil
		}

		if matchedCommand && opencode.IsBadRequest(result.err) {
			refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
			if err := s.refreshCommands(refreshCtx); err != nil && s.agent != nil && s.agent.log != nil {
				s.agent.log.DebugContext(refreshCtx, "refresh OpenCode commands after command bad request failed", slog.String("session_id", string(s.id)), slog.String("error", err.Error()))
			}

			cancel()

			return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{
				jsonFieldError:   "opencode_command_bad_request",
				jsonFieldMessage: result.err.Error(),
				jsonFieldCommand: command.Name,
			})
		}

		return acp.PromptResponse{}, result.err
	}

	final := result.message
	if err := s.emitMessage(turnCtx, final, false); err != nil {
		return acp.PromptResponse{}, err
	}

	usage := usageFromTokens(final.Info.Tokens)

	stopReason := stopReasonFromOpenCode(final.Info.Finish)
	if s.wasCancelled() || turnCtx.Err() != nil {
		stopReason = acp.StopReasonCancelled
	}

	if err := s.snapshotToStore(context.WithoutCancel(ctx)); err != nil {
		return acp.PromptResponse{}, err
	}

	return acp.PromptResponse{StopReason: stopReason, Usage: usage, UserMessageId: params.MessageId}, nil
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

func promptToOpenCodeParts(blocks []acp.ContentBlock) ([]map[string]any, error) {
	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			parts = append(parts, map[string]any{jsonFieldType: partTypeText, partTypeText: block.Text.Text})
		case block.ResourceLink != nil:
			parts = append(parts, map[string]any{jsonFieldType: partTypeText, partTypeText: block.ResourceLink.Uri})
		case block.Resource != nil:
			text := embeddedResourceText(block.Resource.Resource)
			if text != "" {
				parts = append(parts, map[string]any{jsonFieldType: partTypeText, partTypeText: text})
			}
		case block.Image != nil:
			part, err := imageOpenCodePart(block.Image)
			if err != nil {
				return nil, err
			}

			parts = append(parts, part)
		default:
			return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: "prompt"})
		}
	}

	if len(parts) == 0 {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldField: "prompt"})
	}

	return parts, nil
}

func commandPromptParts(blocks []acp.ContentBlock) ([]map[string]any, error) {
	if len(blocks) == 0 {
		return nil, nil
	}

	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		part, ok, err := commandFilePart(block)
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, acp.NewInvalidParams(map[string]any{
				jsonFieldError: errValueUnsupported,
				"blockType":    contentBlockType(block),
			})
		}

		parts = append(parts, part)
	}

	return parts, nil
}

func commandFilePart(block acp.ContentBlock) (map[string]any, bool, error) {
	switch {
	case block.Image != nil:
		part, err := imageOpenCodePart(block.Image)

		return part, true, err
	case block.ResourceLink != nil:
		return resourceLinkOpenCodePart(block.ResourceLink), true, nil
	case block.Resource != nil && block.Resource.Resource.BlobResourceContents != nil:
		part, err := blobResourceOpenCodePart(block.Resource.Resource.BlobResourceContents)

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

func blobResourceOpenCodePart(resource *acp.BlobResourceContents) (map[string]any, error) {
	mimeType := defaultMimeType
	if resource.MimeType != nil && *resource.MimeType != "" {
		mimeType = *resource.MimeType
	}

	nativeURL := resource.Uri
	if resource.Blob != "" {
		nativeURL = "data:" + mimeType + ";base64," + resource.Blob
	}

	if nativeURL == "" {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldField: "prompt.resource", jsonFieldError: "missing resource data or uri"})
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

func contentBlockType(block acp.ContentBlock) string {
	switch {
	case block.Text != nil:
		return firstNonEmpty(block.Text.Type, partTypeText)
	case block.Image != nil:
		return firstNonEmpty(block.Image.Type, mediaTypeImage)
	case block.Audio != nil:
		return firstNonEmpty(block.Audio.Type, contentTypeAudio)
	case block.ResourceLink != nil:
		return firstNonEmpty(block.ResourceLink.Type, "resource_link")
	case block.Resource != nil:
		return firstNonEmpty(block.Resource.Type, "resource")
	default:
		return blockTypeUnknown
	}
}

func imageOpenCodePart(image *acp.ContentBlockImage) (map[string]any, error) {
	mimeType := image.MimeType
	if mimeType == "" {
		mimeType = defaultMimeType
	}

	part := map[string]any{
		jsonFieldType: partTypeFile,
		jsonFieldMime: mimeType,
	}

	switch {
	case image.Data != "":
		part[jsonFieldURL] = "data:" + mimeType + ";base64," + image.Data
	case image.Uri != nil && *image.Uri != "":
		part[jsonFieldURL] = *image.Uri
	default:
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldField: "prompt.image", jsonFieldError: "missing image data or uri"})
	}

	if filename := imageFilename(image); filename != "" {
		part["filename"] = filename
	}

	return part, nil
}

func imageFilename(image *acp.ContentBlockImage) string {
	if image.Uri == nil || *image.Uri == "" {
		return ""
	}

	return filenameFromURI(*image.Uri)
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

func embeddedResourceText(resource acp.EmbeddedResourceResource) string {
	data, _ := json.Marshal(resource)

	var raw map[string]any

	_ = json.Unmarshal(data, &raw)
	if text, _ := raw[partTypeText].(string); text != "" {
		return text
	}

	if uri, _ := raw["uri"].(string); uri != "" {
		return uri
	}

	return ""
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

func (s *session) emitMessage(ctx context.Context, message opencode.NativeMessage, includeUser bool) error {
	if err := s.validateNativeMessageSession(ctx, message); err != nil {
		return err
	}

	isUser := message.Info.Role == roleUser
	if isUser && !includeUser {
		return nil
	}

	for i := range message.Parts {
		part := &message.Parts[i]
		if !s.markPart(*part) {
			continue
		}

		for _, update := range partUpdates(message.Info.Role, *part) {
			if err := s.emitUpdate(ctx, update); err != nil {
				return err
			}
		}

		if part.Type == partTypeStepFinish {
			if update := usageUpdateFromTokens(part.MessageID, part.Tokens); update != nil {
				if err := s.emitUpdate(ctx, *update); err != nil {
					return err
				}
			}
		}
	}

	if message.Info.Tokens.Total > 0 {
		if update := usageUpdateFromTokens(message.Info.ID, message.Info.Tokens); update != nil {
			return s.emitUpdate(ctx, *update)
		}
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

func partUpdates(role string, part opencode.NativePart) []acp.SessionUpdate {
	messageID := part.MessageID
	switch part.Type {
	case partTypeText:
		if part.Text == "" {
			return nil
		}

		if role == roleUser {
			return []acp.SessionUpdate{{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{
				SessionUpdate: "user_message_chunk",
				MessageId:     &messageID,
				Content:       acp.TextBlock(part.Text),
			}}}
		}

		return []acp.SessionUpdate{{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			SessionUpdate: "agent_message_chunk",
			MessageId:     &messageID,
			Content:       acp.TextBlock(part.Text),
		}}}
	case "reasoning":
		if part.Text == "" {
			return nil
		}

		return []acp.SessionUpdate{{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
			SessionUpdate: "agent_thought_chunk",
			MessageId:     &messageID,
			Content:       acp.TextBlock(part.Text),
		}}}
	case partTypeTool:
		return toolPartUpdates(part)
	default:
		return nil
	}
}

func toolPartUpdates(part opencode.NativePart) []acp.SessionUpdate {
	id := acp.ToolCallId(firstNonEmpty(part.CallID, part.ID, "opencode-tool"))
	title := firstNonEmpty(part.Tool, string(id))
	status := acp.ToolCallStatusInProgress

	if len(part.State) > 0 {
		var state map[string]any

		_ = json.Unmarshal(part.State, &state)
		if stateStatus, _ := state["status"].(string); stateStatus != "" {
			status = toolStatus(stateStatus)
		}

		if titleValue, _ := state[jsonFieldTitle].(string); titleValue != "" {
			title = titleValue
		}
	}

	return []acp.SessionUpdate{acp.StartToolCall(
		id,
		title,
		acp.WithStartKind(toolKind(part.Tool)),
		acp.WithStartStatus(status),
		acp.WithStartRawInput(part.Raw),
	)}
}

func (s *session) handleEvent(ctx context.Context, event opencode.Event) error {
	if s.shouldSuppressEvent(event) {
		return nil
	}

	if err := s.emitRawOpenCodeEvent(ctx, event); err != nil {
		return err
	}

	switch event.Type {
	case eventPermissionV2Asked, "permission.asked":
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
			return s.handlePermission(ctx, req)
		}
	case "todo.updated":
		var payload struct {
			SessionID string                `json:"sessionID"`
			Todos     []opencode.NativeTodo `json:"todos"`
		}
		if err := json.Unmarshal(event.Properties, &payload); err == nil && payload.SessionID == s.idmap.NativeSessionID {
			return s.emitPlan(ctx, payload.Todos)
		}
	case "message.part.updated", eventMessagePartCreated:
		part, ok := eventPart(event.Properties)
		if ok && part.SessionID == s.idmap.NativeSessionID && s.markPart(part) {
			s.markActiveMessageID(part.MessageID)

			for _, update := range partUpdates("assistant", part) {
				if err := s.emitUpdate(ctx, update); err != nil {
					return err
				}
			}
		}
	case eventQuestionV2Asked, eventQuestionAsked:
		req, ok := eventQuestion(event.Properties)
		if ok && req.SessionID == s.idmap.NativeSessionID {
			if event.Type == eventQuestionV2Asked {
				req.ReplyRoute = opencode.QuestionRouteAPI
			} else {
				req.ReplyRoute = opencode.QuestionRouteSession
			}

			return s.handleQuestion(ctx, req)
		}
	}

	return nil
}

func eventPart(data json.RawMessage) (opencode.NativePart, bool) {
	var part opencode.NativePart
	if err := json.Unmarshal(data, &part); err == nil && part.Type != "" {
		return part, true
	}

	var wrapper struct {
		Part opencode.NativePart `json:"part"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Part.Type != "" {
		return wrapper.Part, true
	}

	return opencode.NativePart{}, false
}

func eventQuestion(data json.RawMessage) (opencode.QuestionRequest, bool) {
	var req opencode.QuestionRequest
	if err := json.Unmarshal(data, &req); err == nil && req.ID != "" {
		return req, true
	}

	for _, key := range []string{questionWrapperKey, "request", "data"} {
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
			if err := s.handlePermission(ctx, *req); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *session) reconcileQuestions(ctx context.Context) error {
	requests, err := s.client.PendingQuestions(ctx)
	if err != nil {
		return err
	}

	for _, req := range requests {
		if req.SessionID == s.idmap.NativeSessionID {
			if err := s.handleQuestion(ctx, req); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *session) handlePermission(ctx context.Context, req opencode.PermissionRequest) error {
	if req.ID == "" || req.SessionID == "" {
		return nil
	}

	if !s.claimPermissionRequest(req.ID) {
		return nil
	}

	s.addPendingPermission(req)

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
			ToolCallId: acp.ToolCallId(firstNonEmpty(req.ID, "opencode-permission")),
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
		Meta: map[string]any{opencodeMetaKey: map[string]any{"requestId": req.ID, "nativeSessionId": req.SessionID}},
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

	resp, err := conn.CreateElicitation(ctx, request, elicitationScope{
		SessionID:  s.id,
		ToolCallID: acp.ToolCallId(firstNonEmpty(req.Tool.CallID, req.ID)),
	})
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

	return s.client.ReplyQuestion(ctx, req, questionAnswersFromContent(resp.Accept.Content, propertyIDs))
}

func (s *session) drainClientBacklog(ctx context.Context) error {
	suppress := s.suppressBacklog()
	defer s.clearSuppressBacklog()

	for {
		select {
		case event := <-s.client.Events():
			if suppress || event.Type == eventServerConnected || s.shouldSuppressEvent(event) {
				continue
			}

			if err := s.handleEvent(ctx, event); err != nil {
				return err
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
				"requestId":       req.ID,
				"nativeSessionId": req.SessionID,
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
	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	return conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: s.id, Update: update})
}

func (s *session) emitRawOpenCodeEvent(ctx context.Context, event opencode.Event) error {
	if !s.rawMessages.Enabled() {
		return nil
	}

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	var raw map[string]any
	if len(event.Raw) > 0 {
		_ = json.Unmarshal(event.Raw, &raw)
	}

	payload := map[string]any{
		"sessionId": s.id,
		"sequence":  s.nextRawEventSequence(),
		"source":    "opencode-serve",
		"event":     raw,
	}

	return conn.NotifyExtension(ctx, RawEventMethod, capRawEventPayload(payload))
}

func usageUpdateFromTokens(messageID string, tokens opencode.NativeTokens) *acp.SessionUpdate {
	used := int(tokens.Total)
	if used <= 0 {
		used = int(tokens.Input + tokens.Output + tokens.Reasoning)
	}

	if used <= 0 {
		return nil
	}

	size := used
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
	case "bash", "shell", "run":
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
