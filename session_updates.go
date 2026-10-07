package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	statusComplete    = "complete"
	statusInterrupted = "interrupted"
	fieldCwd          = "cwd"
	fieldValue        = "value"
	fieldType         = "type"
	fieldText         = "text"
	roleUser          = "user"
	roleAssistant     = "assistant"
)

const (
	nativeToolBash       = "bash"
	nativeCommandPath    = "/command"
	partFile             = "file"
	partTool             = "tool"
	eventPermissionAsked = "permission.asked"
	eventQuestionAsked   = "question.asked"
	approvalReject       = "reject"
	approvalOnce         = "once"
	approvalAlways       = "always"
	statusIdle           = "idle"
	modeBuild            = "build"
	partReasoning        = "reasoning"
	partStepFinish       = "step-finish"
	eventMessageUpdated  = "message.updated"
	eventPartUpdated     = "message.part.updated"
	fieldID              = "id"
)

type cycleState struct {
	text          map[string]string
	roles         map[string]string
	tools         map[string]bool
	terminalTools map[string]bool
	files         map[string]bool
	// parents are the user messages whose generations the cycle carried.
	parents map[string]bool
	// usage sums the tokens of every call the cycle reported; lastStep is the
	// step-finish part of the latest one.
	usage         *acp.Usage
	lastStep      string
	stopReason    string
	errorMessage  string
	structured    json.RawMessage
	imagesEmitted bool
}

func (s *session) handleEvent(ctx context.Context, rt *binding, event opencode.Event) {
	s.emitRawEvent(ctx, event)

	var props eventProperties
	if err := json.Unmarshal(event.Properties, &props); err != nil {
		rt.cancel()

		return
	}

	s.mu.Lock()
	t, c, closing := s.turn, s.cycle, s.closing
	current := s.runtime == rt
	s.mu.Unlock()

	if !current || closing && t == nil && c == nil {
		return
	}

	if event.Type == "todo.updated" {
		_ = s.emitPlan(ctx, event.Properties)

		return
	}

	info, valid := s.eventInfo(event, props)
	if !valid {
		rt.cancel()

		return
	}

	if event.Type == "session.deleted" {
		s.poisonSession(ctx, "native_session_identity_drift")
		rt.cancel()

		return
	}

	if err := s.projectCompaction(ctx, event, props); err != nil {
		s.agent.log.WarnContext(ctx, "compaction notification failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	if info.ID != "" && s.parentCompleted(info.ID, info.ParentID) {
		return
	}

	if t != nil {
		if info.ID != "" {
			if info.ID != t.messageID && info.ParentID != t.messageID {
				// A native steer or compaction continues the turn's run under
				// another user message. Its calls fill this session's context,
				// so their usage joins the turn.
				if event.Type == eventPartUpdated && props.Part.Type == partStepFinish && s.turnAccepted(t) {
					s.recordFailure(&t.cycle, s.emitResponseUsage(ctx, &t.cycle, props.Part, info))
				}

				return
			}

			s.acceptTurn(ctx, t)
		} else if event.Type != "session.error" {
			return
		}

		c = &t.cycle
	} else if c == nil && info.ID != "" {
		c = s.openAgentCycle(ctx, rt)
	}

	if c == nil {
		return
	}

	s.recordFailure(c, s.projectEvent(ctx, rt, c, event, props, info))

	if t == nil && (event.Type == "session.idle" || (event.Type == "session.status" && props.Status.Type == statusIdle)) {
		s.settleAgentCycle(ctx, rt, c)
	}
}

// openAgentCycle reserves the foreground before publishing native work.
func (s *session) openAgentCycle(ctx context.Context, rt *binding) *cycle {
	c := &cycle{Cycle: s.lc.NewAgentCycle(), done: make(chan struct{})}
	s.mu.Lock()
	if s.turn != nil || s.cycle != nil || s.closing || s.runtime != rt {
		s.mu.Unlock()

		return nil
	}

	s.cycle = c
	s.mu.Unlock()
	s.recordFailure(c, s.lc.OpenAgentCycle(ctx, c.Cycle))
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runtime != rt || s.cycle != c || s.closing {
		return nil
	}

	return c
}

func nativeErrorText(native *opencode.NativeError) string {
	if native == nil {
		return "native request failed"
	}

	if native.Data.Message != "" {
		return native.Data.Message
	}

	if native.Message != "" {
		return native.Message
	}

	if native.Name != "" {
		return native.Name
	}

	return native.Type
}

// recordParent notes the user message whose generation a message belongs to.
func (state *cycleState) recordParent(info opencode.NativeMessageInfo) {
	if state.parents == nil {
		state.parents = map[string]bool{}
	}

	if info.Role == roleUser {
		state.parents[info.ID] = true
	} else if info.ParentID != "" {
		state.parents[info.ParentID] = true
	}
}

func (s *session) projectInfo(_ context.Context, state *cycleState, info opencode.NativeMessageInfo) error {
	state.recordParent(info)

	if info.Role != roleAssistant {
		return nil
	}

	if info.Error != nil {
		state.stopReason = stopReasonError
		state.errorMessage = nativeErrorText(info.Error)
	} else if info.Finish != "" {
		state.stopReason = info.Finish
	}

	if len(info.Structured) > 0 {
		state.structured = append(json.RawMessage(nil), info.Structured...)
	}

	if info.ProviderID != "" && info.ModelID != "" {
		s.mu.Lock()
		s.model = info.ProviderID + "/" + info.ModelID
		s.mu.Unlock()
	}

	return nil
}

// contextTokens is the context a finished call leaves occupied, counted as
// opencode's own context display counts it: the call's input, cached input,
// output, and reasoning tokens.
func contextTokens(tokens opencode.NativeTokens) int {
	return int(tokens.Input + tokens.Output + tokens.Reasoning + tokens.Cache.Read + tokens.Cache.Write)
}

// callUsage is one call's token breakdown. opencode's input already excludes
// the tokens read from and written to the prompt cache, and its output
// excludes reasoning, so output and reasoning together are what the call
// generated.
func callUsage(tokens opencode.NativeTokens) wire.CallUsage {
	return wire.CallUsage{
		InputTokens:       new(int(tokens.Input)),
		CachedReadTokens:  new(int(tokens.Cache.Read)),
		CachedWriteTokens: new(int(tokens.Cache.Write)),
		OutputTokens:      new(int(tokens.Output + tokens.Reasoning)),
	}
}

// emitResponseUsage reports one finished call from its step-finish part,
// which carries that call's own tokens and is the call's only usage report.
// Aborted and failed calls end without one. A step-finish without any token
// is unknown, as a gateway's response cache reports a replayed call, so it
// neither reports nor counts. Every other call counts toward the cycle's
// consumption; a compaction summary reports no context, since its input is
// the conversation it replaces.
func (s *session) emitResponseUsage(ctx context.Context, c *cycle, part opencode.NativePart, info opencode.NativeMessageInfo) error {
	if s.cycleCancelled(c) || info.Role != roleAssistant {
		return nil
	}

	c.state.lastStep = part.ID

	call := callUsage(part.Tokens)
	if !call.Known() {
		return nil
	}

	c.state.usage = addUsage(c.state.usage, part.Tokens)

	if info.CompactionSummary() {
		return nil
	}

	return s.emit(ctx, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{
		Size: s.knownContextWindow(info.ProviderID, info.ModelID),
		Used: contextTokens(part.Tokens),
		Meta: call.Apply(nil),
	}})
}

// addUsage adds one call's tokens to the cycle's consumption.
func addUsage(total *acp.Usage, tokens opencode.NativeTokens) *acp.Usage {
	if total == nil {
		total = &acp.Usage{CachedReadTokens: new(0), CachedWriteTokens: new(0), ThoughtTokens: new(0)}
	}

	total.InputTokens += int(tokens.Input)
	total.OutputTokens += int(tokens.Output)
	*total.CachedReadTokens += int(tokens.Cache.Read)
	*total.CachedWriteTokens += int(tokens.Cache.Write)
	*total.ThoughtTokens += int(tokens.Reasoning)
	total.TotalTokens = total.InputTokens + total.OutputTokens + *total.CachedReadTokens + *total.CachedWriteTokens + *total.ThoughtTokens

	return total
}

// knownContextWindow is the catalog context window of the model a call ran
// on, else 0.
func (s *session) knownContextWindow(providerID, modelID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, provider := range s.models.Providers {
		if provider.ID == providerID {
			if n, ok := provider.Models[modelID].Limit["context"].(float64); ok {
				return int(n)
			}
		}
	}

	return 0
}

func (s *session) projectMessage(ctx context.Context, c *cycle, message opencode.NativeMessage) error {
	if err := s.projectInfo(ctx, &c.state, message.Info); err != nil {
		return err
	}

	for index := range message.Parts {
		part := &message.Parts[index]
		if err := s.projectPart(ctx, &c.state, *part, message.Info.Role); err != nil {
			return err
		}
	}

	return nil
}

func (s *session) projectPart(ctx context.Context, state *cycleState, part opencode.NativePart, role string) error {
	if part.ID == "" {
		return errors.New("native part identity missing")
	}

	if state.roles == nil {
		state.roles = map[string]string{}
		state.text = map[string]string{}
		state.tools = map[string]bool{}
		state.terminalTools = map[string]bool{}
		state.files = map[string]bool{}
	}

	state.roles[part.ID] = part.Type

	if role != roleAssistant {
		return nil
	}

	switch part.Type {
	case fieldText, partReasoning:
		previous := state.text[part.ID]

		suffix, ok := strings.CutPrefix(part.Text, previous)
		if !ok {
			return errors.New("native text changed after publication")
		}

		state.text[part.ID] = part.Text

		if suffix == "" {
			return nil
		}

		if part.Type == partReasoning {
			return s.emit(ctx, acp.UpdateAgentThoughtText(suffix))
		}

		return s.emit(ctx, acp.UpdateAgentMessageText(suffix))
	case partTool:
		return s.emitTool(ctx, state, part)
	case partFile:
		blocks := s.outputFile(opencode.NativeAttachment{ID: part.ID, Mime: part.Mime, URL: part.URL, Filename: part.Filename}, nil)
		for _, block := range blocks {
			encoded, err := json.Marshal(block)
			if err != nil {
				return err
			}

			key := part.ID + "/" + imageReference(string(encoded))
			if state.files[key] {
				continue
			}

			state.files[key] = true

			if err := s.emit(ctx, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: block}}); err != nil {
				return err
			}

			if block.Image != nil {
				state.imagesEmitted = true
			}
		}
	}

	return nil
}

func (s *session) emitTool(ctx context.Context, state *cycleState, part opencode.NativePart) error {
	if part.CallID == "" {
		return errors.New("native tool identity missing")
	}

	var native struct {
		Status      string                      `json:"status"`
		Input       any                         `json:"input"`
		Output      string                      `json:"output"`
		Error       string                      `json:"error"`
		Title       string                      `json:"title"`
		Attachments []opencode.NativeAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(part.State, &native); err != nil {
		return err
	}

	id := acp.ToolCallId(part.CallID)
	if state.terminalTools[part.CallID] {
		return nil
	}

	if !state.tools[part.CallID] {
		title := native.Title
		if title == "" {
			title = part.Tool
		}

		if err := s.emit(ctx, acp.StartToolCall(id, title, acp.WithStartKind(toolKind(part.Tool)), acp.WithStartStatus(acp.ToolCallStatusInProgress), acp.WithStartRawInput(native.Input))); err != nil {
			return err
		}

		state.tools[part.CallID] = true
	}

	if native.Status != "completed" && native.Status != stopReasonError {
		if native.Input != nil {
			return s.emit(ctx, acp.UpdateToolCall(id, acp.WithUpdateRawInput(native.Input)))
		}

		return nil
	}

	state.terminalTools[part.CallID] = true
	status := acp.ToolCallStatusCompleted

	output := native.Output
	if native.Status == stopReasonError {
		status = acp.ToolCallStatusFailed
		output = native.Error
	}

	content := []acp.ToolCallContent{}
	if output != "" {
		content = append(content, acp.ToolContent(acp.TextBlock(output)))
	}

	used := int64(0)

	for index, file := range native.Attachments {
		file.ID = attachmentID(part.ID, index)
		for _, block := range s.outputFile(file, &used) {
			if block.Text != nil {
				status = acp.ToolCallStatusFailed
			}

			if block.Image != nil {
				state.imagesEmitted = true
			}

			content = append(content, acp.ToolContent(block))
		}
	}

	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateStatus(status), acp.WithUpdateRawInput(native.Input), acp.WithUpdateRawOutput(output)}
	if len(content) > 0 {
		opts = append(opts, acp.WithUpdateContent(content))
	}

	return s.emit(ctx, acp.UpdateToolCall(id, opts...))
}

func (s *session) emit(ctx context.Context, updates ...acp.SessionUpdate) error {
	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	for _, update := range updates {
		if err := conn.SessionUpdate(context.WithoutCancel(ctx), acp.SessionNotification{SessionId: s.id, Update: update}); err != nil {
			return err
		}
	}

	return nil
}

func (s *session) emitRawEvent(ctx context.Context, event opencode.Event) {
	if !s.rawEvents.Enabled() {
		return
	}

	conn := s.agent.connection()
	if conn == nil {
		return
	}

	var payload map[string]any
	if json.Unmarshal(event.Raw, &payload) != nil {
		return
	}

	redactImageURLs(payload)

	if err := s.rawEvents.Emit(ctx, func(ctx context.Context, method string, params map[string]any) error {
		return conn.NotifyExtension(ctx, method, params)
	}, payload); err != nil {
		s.agent.observe.RecordRawMessageEmitFailure(ctx, err)
	}
}

func (s *session) emitSessionInfo(ctx context.Context, prompt []acp.ContentBlock) {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	update := acp.SessionSessionInfoUpdate{UpdatedAt: &updatedAt}

	s.mu.Lock()
	s.updatedAt = updatedAt

	if s.title == "" {
		if title := wire.PromptTitle(prompt); title != "" {
			s.title = title
			update.Title = &title
		}
	}
	s.mu.Unlock()

	_ = s.emit(ctx, acp.SessionUpdate{SessionInfoUpdate: &update})
}

func (s *session) sessionInfo() acp.SessionInfo {
	s.mu.Lock()
	title := s.title
	updatedAt := s.updatedAt
	s.mu.Unlock()

	if title == "" {
		title = string(s.id)
	}

	info := acp.SessionInfo{
		Meta:                  wire.NativeSessionMeta(vendor, s.nativeID),
		SessionId:             s.id,
		Title:                 &title,
		Cwd:                   s.cwd,
		AdditionalDirectories: append([]string(nil), s.additionalDirectories...),
	}
	if updatedAt != "" {
		info.UpdatedAt = &updatedAt
	}

	return info
}

//nolint:tagliatelle // Native event properties use uppercase ID suffixes.
type eventProperties struct {
	Info      opencode.NativeMessageInfo   `json:"info"`
	Part      opencode.NativePart          `json:"part"`
	MessageID string                       `json:"messageID"`
	PartID    string                       `json:"partID"`
	Field     string                       `json:"field"`
	Delta     string                       `json:"delta"`
	Status    opencode.NativeSessionStatus `json:"status"`
	Error     *opencode.NativeError        `json:"error"`
}

func (s *session) eventInfo(event opencode.Event, props eventProperties) (opencode.NativeMessageInfo, bool) {
	var info opencode.NativeMessageInfo

	switch event.Type {
	case eventMessageUpdated:
		info = props.Info
		s.rememberMessage(info)
	case eventPartUpdated:
		info = s.nativeMessage(props.Part.MessageID)
	case "message.part.delta":
		info = s.nativeMessage(props.MessageID)
	case eventPermissionAsked:
		var request opencode.PermissionRequest
		if json.Unmarshal(event.Properties, &request) != nil {
			return opencode.NativeMessageInfo{}, false
		}

		info = s.nativeMessage(request.Tool.MessageID)
	case eventQuestionAsked:
		var request opencode.QuestionRequest
		if json.Unmarshal(event.Properties, &request) != nil {
			return opencode.NativeMessageInfo{}, false
		}

		info = s.nativeMessage(request.Tool.MessageID)
	}

	return info, true
}

func (s *session) projectEvent(ctx context.Context, rt *binding, c *cycle, event opencode.Event, props eventProperties, info opencode.NativeMessageInfo) error {
	if s.cycleCancelled(c) {
		if info.ID != "" {
			c.state.recordParent(info)
		}

		if event.Type == eventPermissionAsked || event.Type == eventQuestionAsked {
			s.handleControl(ctx, rt, c, event)
		}

		return nil
	}

	var err error

	switch event.Type {
	case eventMessageUpdated:
		err = s.projectInfo(ctx, &c.state, info)
	case eventPartUpdated:
		if props.Part.Type == partStepFinish {
			err = s.emitResponseUsage(ctx, c, props.Part, info)
		} else {
			err = s.projectPart(ctx, &c.state, props.Part, info.Role)
		}
	case "message.part.delta":
		if props.Field == fieldText && props.Delta != "" && info.Role == roleAssistant {
			if c.state.text == nil {
				c.state.text = map[string]string{}
			}

			c.state.text[props.PartID] += props.Delta
			if c.state.roles[props.PartID] == partReasoning {
				err = s.emit(ctx, acp.UpdateAgentThoughtText(props.Delta))
			} else {
				err = s.emit(ctx, acp.UpdateAgentMessageText(props.Delta))
			}
		}
	case eventPermissionAsked, eventQuestionAsked:
		s.handleControl(ctx, rt, c, event)
	case "session.error":
		c.state.stopReason = stopReasonError
		c.state.errorMessage = nativeErrorText(props.Error)
	}

	return err
}

func (s *session) settleAgentCycle(ctx context.Context, rt *binding, c *cycle) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	s.beginSettlement(c)

	if c.state.stopReason == "" {
		c.state.stopReason = statusComplete
	}

	if err := s.commitMirror(settleCtx, rt); err != nil {
		s.recordFailure(c, s.mirrorFailure(&c.state, err))
		s.fenceStream()
		// A fenced incarnation is terminal, so the binding ends with it and the
		// next operation relaunches and opens a new one. This runs on the
		// binding's own pump, which joins itself, so the cancel is the drop.
		rt.cancel()
	}

	verdict := judgeCycle(c, s.cycleFailure(c), s.claimCancellation(c))
	_ = s.lc.Idle(settleCtx, c.Cycle, verdict.stopReason, verdict.outcome)
	close(c.done)

	for id := range c.state.parents {
		s.completeParent(id)
	}

	s.mu.Lock()
	if s.cycle == c {
		s.cycle = nil
	}
	s.mu.Unlock()
}

func redactImageURLs(value any) {
	switch object := value.(type) {
	case map[string]any:
		if raw, ok := object["url"].(string); ok && strings.HasPrefix(raw, "data:") {
			object["url"] = ""
			if _, data, ok := strings.Cut(raw, ","); ok {
				object["encodedBytes"] = len(data)
			}
		}

		for _, item := range object {
			redactImageURLs(item)
		}
	case []any:
		for _, item := range object {
			redactImageURLs(item)
		}
	}
}

func toolKind(name string) acp.ToolKind {
	switch name {
	case nativeToolBash:
		return acp.ToolKindExecute
	case "read":
		return acp.ToolKindRead
	case "edit", "write", "apply_patch":
		return acp.ToolKindEdit
	case "glob", "grep", "websearch":
		return acp.ToolKindSearch
	case "webfetch":
		return acp.ToolKindFetch
	default:
		return acp.ToolKindOther
	}
}

func (s *session) emitPlan(ctx context.Context, payload json.RawMessage) error {
	var event struct {
		Todos []opencode.NativeTodo `json:"todos"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return err
	}

	entries := make([]acp.PlanEntry, 0, len(event.Todos))
	for _, todo := range event.Todos {
		status := acp.PlanEntryStatusPending

		switch todo.Status {
		case "in_progress":
			status = acp.PlanEntryStatusInProgress
		case "completed":
			status = acp.PlanEntryStatusCompleted
		case lifecycle.StopReasonCancelled:
			continue
		}

		priority := acp.PlanEntryPriorityMedium

		switch todo.Priority {
		case "high":
			priority = acp.PlanEntryPriorityHigh
		case "low":
			priority = acp.PlanEntryPriorityLow
		}

		entries = append(entries, acp.PlanEntry{Content: todo.Content, Priority: priority, Status: status})
	}

	return s.emit(ctx, acp.UpdatePlan(entries...))
}
