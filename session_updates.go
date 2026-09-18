package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const (
	statusComplete     = "complete"
	statusInterrupted  = "interrupted"
	fieldCwd           = "cwd"
	nativeSessionIDKey = "session_id"
	fieldValue         = "value"
	fieldType          = "type"
	fieldText          = "text"
	roleUser           = "user"
	roleAssistant      = "assistant"
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
	fieldID              = "id"
)

type cycleState struct {
	text          map[string]string
	roles         map[string]string
	tools         map[string]bool
	terminalTools map[string]bool
	files         map[string]bool
	messages      map[string]opencode.NativeMessageInfo
	stopReason    string
	errorMessage  string
	contextUsed   int64
	contextMax    int64
	structured    json.RawMessage
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
		s.poisonSession(ctx, "native_session_id_drift")
		rt.cancel()

		return
	}

	if info.ID != "" && s.parentCompleted(info.ID, info.ParentID) {
		return
	}

	if t != nil {
		if info.ID != "" {
			if info.ID != t.messageID && info.ParentID != t.messageID {
				return
			}

			s.acceptTurn(ctx, t)
		} else if event.Type != "session.error" {
			return
		}

		c = &t.cycle
	} else if c == nil && info.ID != "" {
		c = &cycle{Cycle: lifecycle.Cycle{Origin: lifecycle.CauseActivity}, done: make(chan struct{})}
		s.recordFailure(c, s.lc.OpenAgentCycle(ctx, &c.Cycle))

		s.mu.Lock()
		s.cycle = c
		s.mu.Unlock()
	}

	if c == nil {
		return
	}

	s.recordFailure(c, s.projectEvent(ctx, rt, c, event, props, info))

	if t == nil && (event.Type == "session.idle" || (event.Type == "session.status" && props.Status.Type == statusIdle)) {
		s.settleAgentCycle(ctx, rt, c)
	}
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
func (s *session) projectInfo(_ context.Context, state *cycleState, info opencode.NativeMessageInfo) error {
	if state.messages == nil {
		state.messages = map[string]opencode.NativeMessageInfo{}
	}

	state.messages[info.ID] = info
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

	state.contextUsed = int64(info.Tokens.Input + info.Tokens.Cache.Read + info.Tokens.Cache.Write)

	s.mu.Lock()
	defer s.mu.Unlock()

	if info.ProviderID != "" && info.ModelID != "" {
		s.model = info.ProviderID + "/" + info.ModelID
	}

	for _, provider := range s.models.Providers {
		if provider.ID == info.ProviderID {
			if n, ok := provider.Models[info.ModelID].Limit["context"].(float64); ok {
				state.contextMax = int64(n)
			}
		}
	}

	return nil
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

	if native.Status != "completed" && native.Status != "error" {
		if native.Input != nil {
			return s.emit(ctx, acp.UpdateToolCall(id, acp.WithUpdateRawInput(native.Input)))
		}

		return nil
	}

	state.terminalTools[part.CallID] = true
	status := acp.ToolCallStatusCompleted

	output := native.Output
	if native.Status == "error" {
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

			content = append(content, acp.ToolContent(block))
		}
	}

	return s.emit(ctx, acp.UpdateToolCall(id, acp.WithUpdateStatus(status), acp.WithUpdateRawInput(native.Input), acp.WithUpdateRawOutput(output), acp.WithUpdateContent(content)))
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

func (s *session) emitUsage(ctx context.Context, state *cycleState) {
	_ = s.emit(ctx, acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Size: int(state.contextMax), Used: int(state.contextUsed)}})
}

func (s *session) emitRawEvent(ctx context.Context, event opencode.Event) {
	if s.rawEvents == nil || !s.rawEvents.Enabled() {
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
	case "message.updated":
		info = props.Info
		s.rememberMessage(info)
	case "message.part.updated":
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
	var err error

	switch event.Type {
	case "message.updated":
		err = s.projectInfo(ctx, &c.state, info)
	case "message.part.updated":
		err = s.projectPart(ctx, &c.state, props.Part, info.Role)
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

	s.cancelDialogs()
	s.callbacks.Wait()

	if c.state.stopReason == "" {
		c.state.stopReason = statusComplete
	}

	s.emitUsage(settleCtx, &c.state)

	if err := s.commitMirror(settleCtx, rt); err != nil {
		s.recordFailure(c, s.mirrorFailure(err))
		s.lc.Fence()
		// A fenced incarnation is terminal, so the binding ends with it and the
		// next operation relaunches and opens a new one. This runs on the
		// binding's own pump, which joins itself, so the cancel is the drop.
		rt.cancel()
	}

	verdict := judgeCycle(c, s.cycleFailure(c), false)
	_ = s.lc.Idle(settleCtx, c.Cycle, verdict.stopReason, verdict.outcome)
	close(c.done)

	for id := range c.state.messages {
		m := c.state.messages[id]
		if m.Role == roleUser {
			s.completeParent(id)
		} else {
			s.completeParent(m.ParentID)
		}
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
		case "cancelled":
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
