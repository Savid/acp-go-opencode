package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/observer"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

func (s *session) handleControl(ctx context.Context, rt *binding, c *cycle, event opencode.Event) {
	var (
		permission opencode.PermissionRequest
		question   opencode.QuestionRequest
		id, toolID string
	)

	switch event.Type {
	case eventPermissionAsked:
		if json.Unmarshal(event.Properties, &permission) != nil {
			return
		}

		id = permission.ID
		toolID = permission.Tool.CallID
	case eventQuestionAsked:
		if json.Unmarshal(event.Properties, &question) != nil {
			return
		}

		id = question.ID
		toolID = question.Tool.CallID
	}

	if id == "" {
		rt.cancel()

		return
	}

	owned := toolID != "" && c.state.tools[toolID]
	callbackCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))

	release, admitted := s.registerDialog(id, cancel)
	if !admitted {
		return
	}

	go func() {
		defer release()
		defer cancel(nil)

		var (
			path string
			body any
		)

		if event.Type == eventPermissionAsked {
			choice := approvalReject
			if owned {
				choice = s.requestPermission(callbackCtx, c, permission)
			}

			path = "/permission/" + url.PathEscape(id) + "/reply"
			body = map[string]string{"reply": choice}
		} else {
			var answers [][]string
			if owned {
				answers = s.elicit(callbackCtx, c, question)
			}

			path = "/question/" + url.PathEscape(id) + "/reject"
			if answers != nil {
				path = "/question/" + url.PathEscape(id) + "/reply"
				body = map[string]any{"answers": answers}
			}
		}

		replyCtx, replyCancel := context.WithTimeout(context.WithoutCancel(ctx), sessionAbortTimeout)
		defer replyCancel()

		if err := rt.client.Do(replyCtx, s.cwd, http.MethodPost, path, body, nil); err != nil && !opencode.IsMissing(err) {
			rt.cancel()
		}
	}()
}
func (s *session) requestPermission(ctx context.Context, c *cycle, request opencode.PermissionRequest) string {
	conn := s.agent.connection()
	if conn == nil || ctx.Err() != nil {
		return approvalReject
	}

	options := []acp.PermissionOption{
		{OptionId: approvalOnce, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: approvalAlways, Name: "Allow for session", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: approvalReject, Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}

	title := request.Permission
	if len(request.Patterns) > 0 {
		title += " " + strings.Join(request.Patterns, ", ")
	}

	ctx, finish := s.agent.observe.StartPermission(ctx, title, "native")
	response, err := announcedRequest(ctx, s, c, lifecycle.ActionPermission, func(ctx context.Context, meta map[string]any) (acp.RequestPermissionResponse, error) {
		return conn.RequestPermission(ctx, acp.RequestPermissionRequest{Meta: meta, SessionId: s.id, ToolCall: acp.ToolCallUpdate{ToolCallId: acp.ToolCallId(request.Tool.CallID), Title: &title}, Options: options})
	}, func(response acp.RequestPermissionResponse, err error) lifecycle.ActionState {
		if err != nil {
			return lifecycle.ActionFailed
		}

		if response.Outcome.Selected == nil {
			return lifecycle.ActionCancelled
		}

		if response.Outcome.Selected.OptionId == approvalOnce || response.Outcome.Selected.OptionId == approvalAlways {
			return lifecycle.ActionAccepted
		}

		return lifecycle.ActionDeclined
	})

	choice := approvalReject
	if err == nil && ctx.Err() == nil && response.Outcome.Selected != nil && slices.Contains([]string{approvalOnce, approvalAlways}, string(response.Outcome.Selected.OptionId)) {
		choice = string(response.Outcome.Selected.OptionId)
	}

	finish(observer.PermissionResult{Behavior: choice, Mode: "native", ToolName: request.Permission})

	return choice
}
func (s *session) elicit(ctx context.Context, c *cycle, request opencode.QuestionRequest) [][]string {
	conn := s.agent.connection()
	if conn == nil || !s.agent.clientSupportsFormElicitation() || ctx.Err() != nil {
		return nil
	}

	schema := acp.UnstableElicitationSchema{Type: acp.UnstableElicitationSchemaTypeObject, Properties: map[string]any{}}

	for index, question := range request.Questions {
		key := strconv.Itoa(index)
		schema.Required = append(schema.Required, key)

		choices := make([]string, 0, len(question.Options))
		for _, option := range question.Options {
			choices = append(choices, option.Label)
		}

		value := map[string]any{fieldType: "string", "title": question.Question}
		if len(choices) > 0 {
			value["examples"] = choices
		}

		if !question.AllowsCustom() && len(choices) > 0 {
			value["enum"] = choices
		}

		if question.Multiple {
			value = map[string]any{fieldType: "array", "title": question.Question, "minItems": 1, "uniqueItems": true, "items": value}
		}

		schema.Properties[key] = value
	}

	ctx, finish := s.agent.observe.StartElicitation(ctx)
	response, err := announcedRequest(ctx, s, c, lifecycle.ActionElicitation, func(ctx context.Context, meta map[string]any) (acp.UnstableCreateElicitationResponse, error) {
		return conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Meta: meta, Mode: "form", Message: "OpenCode needs input", RequestedSchema: schema}})
	}, func(response acp.UnstableCreateElicitationResponse, err error) lifecycle.ActionState {
		if err != nil {
			return lifecycle.ActionFailed
		}

		if response.Accept != nil {
			return lifecycle.ActionAccepted
		}

		if response.Decline != nil {
			return lifecycle.ActionDeclined
		}

		return lifecycle.ActionCancelled
	})
	finish(observer.ElicitationResult{Accepted: err == nil && response.Accept != nil, Err: err})

	if err != nil || ctx.Err() != nil || response.Accept == nil {
		return nil
	}

	return questionAnswers(request.Questions, response.Accept.Content)
}

// announcedRequest sends one client request that holds native work, announces
// the action it answers once the request is on the wire, and resolves that
// action exactly once.
func announcedRequest[T any](
	ctx context.Context,
	s *session,
	c *cycle,
	kind lifecycle.ActionKind,
	send func(context.Context, map[string]any) (T, error),
	resolved func(T, error) lifecycle.ActionState,
) (T, error) {
	var zero T

	releaseCall, err := s.agent.acquireClientCall()
	if err != nil {
		return zero, err
	}
	defer releaseCall()

	actionID, err := s.reserveAction(c)
	if err != nil {
		return zero, err
	}

	if actionID == "" {
		return send(ctx, nil)
	}

	type answer struct {
		value T
		err   error
	}

	answers := make(chan answer, 1)

	var written <-chan struct{}
	if t := s.agent.transportRef(); t != nil {
		written = t.AwaitRequestWrite(actionID)
	}

	go func() {
		value, err := send(ctx, s.actionCorrelation(c, actionID))
		answers <- answer{value: value, err: err}
	}()

	if written != nil {
		select {
		case <-written:
		case result := <-answers:
			answers <- result
		}
	}

	if err := s.lcActionPendingWithID(ctx, c, actionID, kind); err != nil {
		s.agent.log.ErrorContext(ctx, "announce lifecycle action failed",
			slog.String(nativeSessionIDKey, string(s.id)), slog.String("reason", err.Error()))
	}

	result := <-answers
	state := resolved(result.value, result.err)

	if result.err != nil && errors.Is(context.Cause(ctx), errDialogCancelled) {
		state = lifecycle.ActionCancelled
	}

	if err := s.lcActionResolved(context.WithoutCancel(ctx), c, actionID, state); err != nil {
		s.agent.log.ErrorContext(ctx, "resolve lifecycle action failed",
			slog.String(nativeSessionIDKey, string(s.id)), slog.String("reason", err.Error()))
	}

	return result.value, result.err
}

func (s *session) reserveAction(c *cycle) (string, error) {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	if s.lc.stream == nil || s.lc.stream.Fenced() || c.turnID == "" {
		return "", nil
	}

	return s.nextLifecycleID("action"), nil
}

func questionAnswers(questions []opencode.QuestionInfo, content map[string]any) [][]string {
	answers := make([][]string, 0, len(questions))
	for index, question := range questions {
		value := content[strconv.Itoa(index)]
		values := []string{}

		if question.Multiple {
			items, ok := value.([]any)
			if !ok || len(items) == 0 {
				return nil
			}

			for _, item := range items {
				text, ok := item.(string)
				if !ok || slices.Contains(values, text) {
					return nil
				}

				values = append(values, text)
			}
		} else {
			text, ok := value.(string)
			if !ok {
				return nil
			}

			values = append(values, text)
		}

		for _, value := range values {
			if value == "" {
				return nil
			}

			if !question.AllowsCustom() {
				found := false
				for _, option := range question.Options {
					found = found || option.Label == value
				}

				if !found {
					return nil
				}
			}
		}

		answers = append(answers, values)
	}

	return answers
}
