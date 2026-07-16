package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestQuestionToolElicitationAcceptDeclineAndNoCapability(t *testing.T) {
	ctx := context.Background()

	t.Run("accept replies to native question", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitation = acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{
				"question_1": "Yes",
				"question_2": []any{"Red", "Blue"},
			}},
		}
		agent := NewAgent()
		agent.setAgentClient(conn)
		if _, err := agent.Initialize(ctx, acp.InitializeRequest{ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		}}); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		session := testSession(agent, client)

		req := opencode.QuestionRequest{
			ID:        "question-1",
			SessionID: "native-1",
			Tool:      opencode.QuestionTool{MessageID: "message-1", CallID: "call-1"},
			Questions: []opencode.QuestionInfo{
				{
					Question: "Proceed?",
					Header:   "Decision",
					Options: []opencode.QuestionOption{
						{Label: "Yes", Description: "Continue"},
						{Label: "No"},
					},
				},
				{
					Question: "Colors?",
					Header:   "Palette",
					Multiple: true,
					Options: []opencode.QuestionOption{
						{Label: "Red"},
						{Label: "Blue"},
					},
				},
			},
		}
		if err := session.handleQuestion(ctx, req); err != nil {
			t.Fatalf("handleQuestion: %v", err)
		}
		if len(conn.elicitations) != 1 {
			t.Fatalf("elicitations = %d, want 1", len(conn.elicitations))
		}
		got := conn.elicitations[0]
		if got.Form == nil || got.Form.Mode != "form" || got.Form.Message != "OpenCode needs input" {
			t.Fatalf("elicitation form = %#v", got.Form)
		}
		if conn.scopes[0].SessionID != session.id || conn.scopes[0].ToolCallID != "call-1" {
			t.Fatalf("scope = %#v", conn.scopes[0])
		}
		if len(got.Form.RequestedSchema.Required) != 2 {
			t.Fatalf("schema required = %#v", got.Form.RequestedSchema.Required)
		}
		reply := client.questionReply(0)
		if reply.sessionID != "native-1" || reply.requestID != "question-1" {
			t.Fatalf("reply target = %#v", reply)
		}
		if !reflect.DeepEqual(reply.answers, [][]string{{"Yes"}, {"Red", "Blue"}}) {
			t.Fatalf("answers = %#v", reply.answers)
		}
	})

	t.Run("decline rejects native question", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
		agent := NewAgent()
		agent.setAgentClient(conn)
		if _, err := agent.Initialize(ctx, acp.InitializeRequest{ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		}}); err != nil {
			t.Fatalf("Initialize: %v", err)
		}
		session := testSession(agent, client)
		if err := session.handleQuestion(ctx, opencode.QuestionRequest{ID: "q", SessionID: "native-1"}); err != nil {
			t.Fatalf("handleQuestion: %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %d, want 1", client.questionRejectCount())
		}
	})

	t.Run("missing capability rejects without ACP request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		if err := session.handleQuestion(ctx, opencode.QuestionRequest{ID: "q", SessionID: "native-1"}); err != nil {
			t.Fatalf("handleQuestion: %v", err)
		}
		if len(conn.elicitations) != 0 {
			t.Fatalf("elicitation sent without capability: %#v", conn.elicitations)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %d, want 1", client.questionRejectCount())
		}
	})
}

func TestPromptRejectsInvalidCurrentModel(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID:     "openai",
		Models: map[string]opencode.ProviderModel{"other": {ID: "other"}},
	}}}
	client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		t.Fatal("SendMessage called after invalid model")

		return opencode.NativeMessage{}, nil
	}
	session := testSession(NewAgent(), client)

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "nonce", "hello"))
	assertInvalidModelField(t, err, modelFieldPrompt)
}

func TestCommandPromptRejectsInvalidCurrentModel(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review"}}
	client.providers = opencode.ProvidersResponse{Providers: []opencode.ProviderInfo{{
		ID:     "openai",
		Models: map[string]opencode.ProviderModel{"other": {ID: "other"}},
	}}}
	client.runCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
		t.Fatal("RunCommand called after invalid model")

		return opencode.NativeMessage{}, nil
	}
	session := testSession(NewAgent(), client)

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "nonce", "/review"))
	assertInvalidModelField(t, err, modelFieldPrompt)
}

func TestQuestionToolReconcileAndCancelRejectsPending(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.pendingQuestions = []opencode.QuestionRequest{
		{ID: "foreign", SessionID: "other"},
		{ID: "q1", SessionID: "native-1"},
	}
	agent := NewAgent()
	session := testSession(agent, client)
	if err := session.reconcileQuestions(ctx); err != nil {
		t.Fatalf("reconcileQuestions: %v", err)
	}
	if client.questionRejectCount() != 1 {
		t.Fatalf("question rejects = %d, want 1", client.questionRejectCount())
	}

	turnCtx := session.beginTurn(ctx)
	session.mu.Lock()
	session.questions["q2"] = opencode.QuestionRequest{ID: "q2", SessionID: "native-1"}
	session.pending["p1"] = opencode.PermissionRequest{ID: "p1", SessionID: "native-1"}
	session.mu.Unlock()
	session.cancelTurn()
	if turnCtx.Err() == nil {
		t.Fatal("turn context was not cancelled")
	}
	if client.questionRejectCount() != 2 {
		t.Fatalf("question rejects after cancel = %d, want 2", client.questionRejectCount())
	}
	reply := client.permissionReply(0)
	if reply.reply != "reject" || reply.message != "cancelled" {
		t.Fatalf("permission cancel reply = %#v", reply)
	}
	session.finishTurn()
}

func TestPermissionV2AskReplyReconcileAndCancelled(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)
	turnCtx := session.beginTurn(ctx)
	session.markActiveToolCallID("tool-current")
	defer func() {
		session.finishTurn()
		_ = turnCtx
	}()

	if err := session.handlePermission(ctx, opencode.PermissionRequest{
		ID:        "perm-1",
		SessionID: "native-1",
		Action:    "edit",
		Resources: []string{"file.txt"},
		Metadata:  map[string]any{"path": "file.txt"},
		Tool:      opencode.PermissionTool{CallID: "tool-current"},
	}); err != nil {
		t.Fatalf("handlePermission: %v", err)
	}
	reply := client.permissionReply(0)
	if reply.reply != "once" || reply.sessionID != "native-1" || reply.requestID != "perm-1" {
		t.Fatalf("permission reply = %#v", reply)
	}
	if len(conn.permissions) != 1 || conn.permissions[0].ToolCall.Title == nil || *conn.permissions[0].ToolCall.Title != "edit" {
		t.Fatalf("permission request = %#v", conn.permissions)
	}

	conn.permission = acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	if err := session.handlePermission(ctx, opencode.PermissionRequest{ID: "perm-2", SessionID: "native-1", Tool: opencode.PermissionTool{CallID: "tool-current"}}); err != nil {
		t.Fatalf("handlePermission cancelled: %v", err)
	}
	if got := client.permissionReply(1).reply; got != "reject" {
		t.Fatalf("cancelled reply = %q, want reject", got)
	}

	client.pendingPermissions = []opencode.PermissionRequest{
		{ID: "foreign", SessionID: "other"},
		{ID: "perm-3", SessionID: "native-1", Tool: opencode.PermissionTool{CallID: "tool-current"}},
	}
	if err := session.reconcilePermissions(ctx); err != nil {
		t.Fatalf("reconcilePermissions: %v", err)
	}
	if got := client.permissionReply(2).requestID; got != "perm-3" {
		t.Fatalf("reconciled request id = %q", got)
	}
}

func TestPermissionQuestionDuplicateRequestIDsAreFenced(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	agent.setAgentClient(conn)
	session := testSession(agent, client)
	session.beginTurn(ctx)
	session.markActiveToolCallID("tool-current")
	defer session.finishTurn()

	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "permission.v2.asked",
		Properties: json.RawMessage(`{"id":"perm-dup","sessionID":"native-1","action":"edit","tool":{"callID":"tool-current"}}`),
	}); err != nil {
		t.Fatalf("permission event: %v", err)
	}
	client.pendingPermissions = []opencode.PermissionRequest{{ID: "perm-dup", SessionID: "native-1", Action: "edit", Tool: opencode.PermissionTool{CallID: "tool-current"}}}
	if err := session.reconcilePermissions(ctx); err != nil {
		t.Fatalf("permission reconcile: %v", err)
	}
	if conn.permissionRequestCount() != 1 || client.permissionReplyCount() != 1 {
		t.Fatalf("duplicate permission was not fenced requests=%d replies=%d", conn.permissionRequestCount(), client.permissionReplyCount())
	}

	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "question.asked",
		Properties: json.RawMessage(`{"id":"question-dup","sessionID":"native-1","questions":[{"question":"Continue?"}]}`),
	}); err != nil {
		t.Fatalf("question event: %v", err)
	}
	client.pendingQuestions = []opencode.QuestionRequest{{ID: "question-dup", SessionID: "native-1"}}
	if err := session.reconcileQuestions(ctx); err != nil {
		t.Fatalf("question reconcile: %v", err)
	}
	if len(conn.elicitations) != 1 || client.questionReplyCount() != 1 {
		t.Fatalf("duplicate question was not fenced elicitations=%d replies=%d", len(conn.elicitations), client.questionReplyCount())
	}
}

func TestPermissionAndQuestionCallbacksFollowExactToolStartOnACPWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agent, _, wireClient, peer := newWireCoverageConnection(t)
	_, err := peer.Initialize(ctx, acp.InitializeRequest{ClientCapabilities: acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
	}})
	require.NoError(t, err)

	native := newFakeOpenCodeClient()
	session := testSession(agent, native)
	turnCtx := session.beginTurn(ctx, "wire-turn")
	defer session.finishTurn()

	emitToolStart := func(id string) {
		t.Helper()
		properties := mustJSON(t, opencode.NativePart{
			ID:        "part-" + id,
			SessionID: "native-1",
			MessageID: "message-1",
			Type:      partTypeTool,
			Tool:      "edit",
			CallID:    id,
			State:     json.RawMessage(`{"status":"pending"}`),
		})
		require.NoError(t, session.handleEvent(turnCtx, opencode.Event{
			Type: eventMessagePartCreated, Properties: properties,
		}))
	}

	emitToolStart("permission-tool")
	require.NoError(t, session.handleEvent(turnCtx, opencode.Event{
		Type: eventPermissionV2Asked,
		Properties: json.RawMessage(
			`{"id":"permission-1","sessionID":"native-1","action":"edit","tool":{"callID":"permission-tool"}}`,
		),
	}))

	emitToolStart("question-tool")
	require.NoError(t, session.handleEvent(turnCtx, opencode.Event{
		Type: eventQuestionV2Asked,
		Properties: json.RawMessage(
			`{"id":"question-1","sessionID":"native-1","tool":{"callID":"question-tool"},"questions":[{"question":"Proceed?"}]}`,
		),
	}))

	require.Eventually(t, func() bool {
		wireClient.mu.Lock()
		defer wireClient.mu.Unlock()

		return len(wireClient.order) == 4
	}, time.Second, time.Millisecond)
	wireClient.mu.Lock()
	require.Equal(t, []string{
		"tool_call:permission-tool",
		"permission:permission-tool",
		"tool_call:question-tool",
		"elicitation:question-tool",
	}, wireClient.order)
	wireClient.mu.Unlock()

	err = session.handleEvent(turnCtx, opencode.Event{
		Type: eventQuestionV2Asked,
		Properties: json.RawMessage(
			`{"id":"question-stale","sessionID":"native-1","tool":{"callID":"not-published"},"questions":[{"question":"Proceed?"}]}`,
		),
	})
	require.ErrorContains(t, err, "does not target a tool call published in the active turn")
	require.Equal(t, 2, native.questionRejectCount())
	wireClient.mu.Lock()
	require.Len(t, wireClient.elicitations, 1)
	wireClient.mu.Unlock()
}

func TestEventMappingMessagePartToolTodoUsageAndRaw(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)
	session.rawMessages = rawMessageConfig{enabled: true}

	todoProps := json.RawMessage(`{"sessionID":"native-1","todos":[{"content":"Ship it","status":"in_progress","priority":"high"}]}`)
	if err := session.handleEvent(ctx, eventFromJSON(t, `{"type":"todo.updated","properties":`+string(todoProps)+`}`)); err != nil {
		t.Fatalf("todo event: %v", err)
	}
	textProps := json.RawMessage(`{"id":"part-1","sessionID":"native-1","messageID":"message-1","type":"text","text":"hello"}`)
	if err := session.handleEvent(ctx, opencode.Event{Type: "message.part.created", Properties: textProps, Raw: json.RawMessage(`{"type":"message.part.created"}`)}); err != nil {
		t.Fatalf("text event: %v", err)
	}
	if err := session.handleEvent(ctx, opencode.Event{Type: "message.part.created", Properties: textProps}); err != nil {
		t.Fatalf("duplicate text event: %v", err)
	}
	reasoningProps := json.RawMessage(`{"part":{"id":"part-2","sessionID":"native-1","messageID":"message-1","type":"reasoning","text":"thinking"}}`)
	if err := session.handleEvent(ctx, opencode.Event{Type: "message.part.updated", Properties: reasoningProps}); err != nil {
		t.Fatalf("reasoning event: %v", err)
	}
	toolProps := json.RawMessage(`{"id":"part-3","sessionID":"native-1","messageID":"message-1","type":"tool","tool":"bash","callID":"call-1","state":{"status":"completed","title":"Run"}}`)
	if err := session.handleEvent(ctx, opencode.Event{Type: "message.part.created", Properties: toolProps}); err != nil {
		t.Fatalf("tool event: %v", err)
	}
	if err := session.emitMessage(ctx, opencode.NativeMessage{
		Info: opencode.NativeMessageInfo{ID: "message-1", SessionID: "native-1", Role: "assistant", Tokens: opencode.NativeTokens{Total: 6}},
		Parts: []opencode.NativePart{{
			SessionID: "native-1",
			MessageID: "message-1",
			Type:      "step-finish",
			Tokens:    opencode.NativeTokens{Input: 2, Output: 3, Reasoning: 1},
		}},
	}, false); err != nil {
		t.Fatalf("emitMessage: %v", err)
	}

	if conn.updateCount() != 5 {
		t.Fatalf("updates = %d, want 5: %#v", conn.updateCount(), conn.updates)
	}
	if conn.updates[0].Update.Plan == nil {
		t.Fatalf("first update = %#v, want plan", conn.updates[0].Update)
	}
	if conn.updates[1].Update.AgentMessageChunk == nil {
		t.Fatalf("second update = %#v, want agent chunk", conn.updates[1].Update)
	}
	if conn.updates[2].Update.AgentThoughtChunk == nil {
		t.Fatalf("third update = %#v, want thought", conn.updates[2].Update)
	}
	if conn.updates[3].Update.ToolCall == nil {
		t.Fatalf("fourth update = %#v, want tool", conn.updates[3].Update)
	}
	if conn.updates[4].Update.UsageUpdate == nil {
		t.Fatalf("usage update missing: %#v", conn.updates)
	}
	if len(conn.extensions) == 0 || conn.extensions[0].method != RawEventMethod {
		t.Fatalf("raw events = %#v", conn.extensions)
	}
}

func TestPartUpdatesReconcileCumulativeTextAndMetadataEchoes(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	part := opencode.NativePart{
		ID:        "reasoning-1",
		MessageID: "message-1",
		Type:      "reasoning",
		Text:      "The user",
	}

	updates := committedPartUpdates(session, "assistant", part, "")
	if len(updates) != 1 || updates[0].AgentThoughtChunk == nil {
		t.Fatalf("initial updates = %#v, want one thought chunk", updates)
	}
	if got := updates[0].AgentThoughtChunk.Content.Text.Text; got != "The user" {
		t.Fatalf("initial thought = %q", got)
	}

	part.Raw = json.RawMessage(`{"id":"reasoning-1","metadata":{"changed":true}}`)
	metadataUpdates := committedPartUpdates(session, "assistant", part, "")
	if metadataUpdates != nil {
		t.Fatalf("metadata-only echo updates = %#v, want nil", metadataUpdates)
	}

	part.Text = "The user wants"
	updates = committedPartUpdates(session, "assistant", part, " WRONG")
	if len(updates) != 1 || updates[0].AgentThoughtChunk == nil {
		t.Fatalf("cumulative updates = %#v, want one thought chunk", updates)
	}
	if got := updates[0].AgentThoughtChunk.Content.Text.Text; got != " wants" {
		t.Fatalf("cumulative delta = %q, want %q", got, " wants")
	}

	part.Text = "The user"
	regressedUpdates := committedPartUpdates(session, "assistant", part, "")
	if regressedUpdates != nil {
		t.Fatalf("regressed snapshot updates = %#v, want nil", regressedUpdates)
	}

	part.Text = "The rewritten user wants"
	rewrittenUpdates := committedPartUpdates(session, "assistant", part, " replacement")
	if rewrittenUpdates != nil {
		t.Fatalf("explicit rewrite delta updates = %#v, want nil", rewrittenUpdates)
	}
}

func TestPartUpdatesSuppressRepeatedAndLateExplicitDeltas(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	part := opencode.NativePart{
		ID:        "reasoning-late",
		MessageID: "message-late",
		Type:      partTypeReasoning,
		Text:      "First",
	}

	initial := committedPartUpdates(session, "assistant", part, "st")
	if len(initial) != 1 || initial[0].AgentThoughtChunk == nil {
		t.Fatalf("initial explicit delta updates = %#v", initial)
	}
	if got := initial[0].AgentThoughtChunk.Content.Text.Text; got != "First" {
		t.Fatalf("initial explicit delta text = %q, want full snapshot", got)
	}

	part.Text = "First second"
	live := committedPartUpdates(session, "assistant", part, " second")
	if len(live) != 1 || live[0].AgentThoughtChunk == nil {
		t.Fatalf("live explicit delta updates = %#v", live)
	}
	if got := live[0].AgentThoughtChunk.Content.Text.Text; got != " second" {
		t.Fatalf("live explicit delta text = %q", got)
	}

	repeated := committedPartUpdates(session, "assistant", part, " second")
	if repeated != nil {
		t.Fatalf("repeated explicit delta updates = %#v, want nil", repeated)
	}

	finalPart := part
	finalPart.Text = "First second final"
	final := committedPartUpdates(session, "assistant", finalPart, "")
	if len(final) != 1 || final[0].AgentThoughtChunk == nil {
		t.Fatalf("final REST reconciliation updates = %#v", final)
	}
	if got := final[0].AgentThoughtChunk.Content.Text.Text; got != " final" {
		t.Fatalf("final REST reconciliation text = %q", got)
	}

	late := committedPartUpdates(session, "assistant", part, " second")
	if late != nil {
		t.Fatalf("late buffered SSE updates = %#v, want nil", late)
	}
	if got := session.emittedPartText[part.ID]; got != finalPart.Text {
		t.Fatalf("late buffered SSE regressed state to %q, want %q", got, finalPart.Text)
	}
}

func TestToolPartUpdatesEmitOneStartThenMonotonicUpdates(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	part := opencode.NativePart{
		ID:        "part-1",
		CallID:    "call-1",
		MessageID: "message-1",
		Type:      partTypeTool,
		Tool:      "StructuredOutput",
		State:     json.RawMessage(`{"status":"pending","input":{}}`),
	}

	updates := committedPartUpdates(session, "assistant", part, "")
	if len(updates) != 1 || updates[0].ToolCall == nil {
		t.Fatalf("pending updates = %#v, want one tool start", updates)
	}
	if updates[0].ToolCall.Status != acp.ToolCallStatusPending {
		t.Fatalf("pending status = %q", updates[0].ToolCall.Status)
	}

	part.State = json.RawMessage(`{"status":"running","input":{"score":100}}`)
	updates = committedPartUpdates(session, "assistant", part, "")
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("running updates = %#v, want one tool update", updates)
	}
	if updates[0].ToolCallUpdate.Status == nil || *updates[0].ToolCallUpdate.Status != acp.ToolCallStatusInProgress {
		t.Fatalf("running status = %#v", updates[0].ToolCallUpdate.Status)
	}

	part.State = json.RawMessage(`{"status":"completed","title":"Structured Output","input":{"score":100},"output":"captured"}`)
	updates = committedPartUpdates(session, "assistant", part, "")
	if len(updates) != 1 || updates[0].ToolCallUpdate == nil {
		t.Fatalf("completed updates = %#v, want one tool update", updates)
	}
	if updates[0].ToolCallUpdate.Status == nil || *updates[0].ToolCallUpdate.Status != acp.ToolCallStatusCompleted {
		t.Fatalf("completed status = %#v", updates[0].ToolCallUpdate.Status)
	}
	if got := updates[0].ToolCallUpdate.RawOutput; got != "captured" {
		t.Fatalf("completed raw output = %#v", got)
	}

	completedEcho := committedPartUpdates(session, "assistant", part, "")
	if completedEcho != nil {
		t.Fatalf("completed echo updates = %#v, want nil", completedEcho)
	}
	part.State = json.RawMessage(`{"status":"pending","input":{}}`)
	regressedTool := committedPartUpdates(session, "assistant", part, "")
	if regressedTool != nil {
		t.Fatalf("regressed tool updates = %#v, want nil", regressedTool)
	}
}

func TestToolPartUpdatesPreserveFailedErrorOutput(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	part := opencode.NativePart{
		ID:     "part-failed",
		CallID: "call-failed",
		Type:   partTypeTool,
		Tool:   "bash",
		State:  json.RawMessage(`{"status":"pending","input":{"command":"false"}}`),
	}
	start := committedPartUpdates(session, "assistant", part, "")
	if len(start) != 1 || start[0].ToolCall == nil {
		t.Fatalf("failed tool start = %#v", start)
	}

	part.State = json.RawMessage(`{"status":"running","input":{"command":"false"}}`)
	running := committedPartUpdates(session, "assistant", part, "")
	if len(running) != 1 || running[0].ToolCallUpdate == nil {
		t.Fatalf("failed tool running = %#v", running)
	}

	part.State = json.RawMessage(`{"status":"error","input":{"command":"false"},"error":"exit status 1"}`)
	failed := committedPartUpdates(session, "assistant", part, "")
	if len(failed) != 1 || failed[0].ToolCallUpdate == nil {
		t.Fatalf("failed tool terminal = %#v", failed)
	}
	update := failed[0].ToolCallUpdate
	if update.Status == nil || *update.Status != acp.ToolCallStatusFailed {
		t.Fatalf("failed tool status = %#v", update.Status)
	}
	if !reflect.DeepEqual(update.RawOutput, map[string]any{"error": "exit status 1"}) {
		t.Fatalf("failed tool raw output = %#v", update.RawOutput)
	}

	replaySession := testSession(NewAgent(), newFakeOpenCodeClient())
	replayed := committedPartUpdates(replaySession, "assistant", part, "")
	if len(replayed) != 1 || replayed[0].ToolCall == nil {
		t.Fatalf("replayed failed tool = %#v, want completed start", replayed)
	}
	if replayed[0].ToolCall.Status != acp.ToolCallStatusFailed {
		t.Fatalf("replayed failed status = %q", replayed[0].ToolCall.Status)
	}
	if !reflect.DeepEqual(replayed[0].ToolCall.RawOutput, map[string]any{"error": "exit status 1"}) {
		t.Fatalf("replayed failed raw output = %#v", replayed[0].ToolCall.RawOutput)
	}
}

func TestUpdateReconciliationCommitsOnlyAfterDelivery(t *testing.T) {
	ctx := context.Background()
	conn := newRecordingAgentClient()
	conn.updateErr = errors.New("delivery failed")
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, newFakeOpenCodeClient())

	textPart := opencode.NativePart{
		ID:        "text-retry",
		MessageID: "message-retry",
		Type:      partTypeText,
		Text:      "delivered once",
	}
	if err := session.emitPartUpdates(ctx, "assistant", textPart, ""); err == nil {
		t.Fatal("text delivery unexpectedly succeeded")
	}
	if _, ok := session.emittedPartText[textPart.ID]; ok {
		t.Fatal("failed text delivery committed reconciliation state")
	}
	conn.updateErr = nil
	if err := session.emitPartUpdates(ctx, "assistant", textPart, ""); err != nil {
		t.Fatalf("text retry: %v", err)
	}
	if session.emittedPartText[textPart.ID] != textPart.Text {
		t.Fatal("successful text retry did not commit reconciliation state")
	}

	toolPart := opencode.NativePart{
		ID:     "tool-retry",
		CallID: "call-retry",
		Type:   partTypeTool,
		Tool:   "bash",
		State:  json.RawMessage(`{"status":"pending","input":{"command":"true"}}`),
	}
	conn.updateErr = errors.New("delivery failed")
	if err := session.emitPartUpdates(ctx, "assistant", toolPart, ""); err == nil {
		t.Fatal("tool delivery unexpectedly succeeded")
	}
	if _, ok := session.emittedTools[toolPart.CallID]; ok {
		t.Fatal("failed tool delivery committed reconciliation state")
	}
	conn.updateErr = nil
	if err := session.emitPartUpdates(ctx, "assistant", toolPart, ""); err != nil {
		t.Fatalf("tool retry: %v", err)
	}
	lastUpdate := conn.updates[len(conn.updates)-1].Update
	if lastUpdate.ToolCall == nil || lastUpdate.ToolCallUpdate != nil {
		t.Fatalf("tool retry = %#v, want tool start", lastUpdate)
	}

	usageTokens := opencode.NativeTokens{Total: 42}
	conn.updateErr = errors.New("delivery failed")
	if err := session.emitUsageUpdate(ctx, "usage-retry", usageTokens, 100); err == nil {
		t.Fatal("usage delivery unexpectedly succeeded")
	}
	if _, ok := session.emittedUsage["usage-retry"]; ok {
		t.Fatal("failed usage delivery committed reconciliation state")
	}
	conn.updateErr = nil
	if err := session.emitUsageUpdate(ctx, "usage-retry", usageTokens, 100); err != nil {
		t.Fatalf("usage retry: %v", err)
	}
	if _, ok := session.emittedUsage["usage-retry"]; !ok {
		t.Fatal("successful usage retry did not commit reconciliation state")
	}
}

func TestUpdateReconciliationEdgeBranches(t *testing.T) {
	ctx := context.Background()
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	deltaOnly := opencode.NativePart{
		ID:        "delta-only",
		MessageID: "message-delta",
		Type:      partTypeReasoning,
	}
	updates := committedPartUpdates(session, "assistant", deltaOnly, "tail")
	if len(updates) != 1 || updates[0].AgentThoughtChunk == nil {
		t.Fatalf("delta-only updates = %#v", updates)
	}
	if session.emittedPartText[deltaOnly.ID] != "tail" {
		t.Fatalf("delta-only committed text = %q", session.emittedPartText[deltaOnly.ID])
	}

	session.agent = nil
	session.emittedPartText["rewrite"] = "before"
	rewrite := opencode.NativePart{ID: "rewrite", Type: partTypeText, Text: "after"}
	if updates := committedPartUpdates(session, "assistant", rewrite, ""); updates != nil {
		t.Fatalf("rewrite without logger updates = %#v", updates)
	}

	noState := nativeToolPartState(opencode.NativePart{Tool: "bash"}, "tool")
	if noState.status != acp.ToolCallStatusInProgress {
		t.Fatalf("tool without state status = %q", noState.status)
	}
	malformed := nativeToolPartState(opencode.NativePart{State: json.RawMessage(`{`)}, "tool")
	if malformed.status != acp.ToolCallStatusInProgress {
		t.Fatalf("malformed tool state status = %q", malformed.status)
	}
	if got := toolStatusRank(acp.ToolCallStatus("unknown")); got != 0 {
		t.Fatalf("unknown tool status rank = %d", got)
	}
	if err := session.emitUsageUpdate(ctx, "empty", opencode.NativeTokens{}, 0); err != nil {
		t.Fatalf("empty usage update: %v", err)
	}
}

func committedPartUpdates(
	session *session,
	role string,
	part opencode.NativePart,
	nativeDelta string,
) []acp.SessionUpdate {
	updates, commit := session.partUpdates(role, part, nativeDelta)
	if commit != nil {
		commit()
	}

	return updates
}

func TestLiveUserMessagePartsAreNotEchoed(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)

	// The native stream declares the user message (wrapped payload) before its
	// part events; the prompt echo must not stream back as an agent chunk.
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       eventMessageUpdated,
		Properties: json.RawMessage(`{"info":{"id":"user-1","sessionID":"native-1","role":"user"}}`),
	}); err != nil {
		t.Fatalf("message.updated event: %v", err)
	}
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pu-1","sessionID":"native-1","messageID":"user-1","type":"text","text":"the prompt"}}`),
	}); err != nil {
		t.Fatalf("user part event: %v", err)
	}
	if conn.updateCount() != 0 {
		t.Fatalf("user prompt echoed: %#v", conn.updates)
	}

	// Bare (unwrapped) message.updated payloads are also recognized.
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       eventMessageUpdated,
		Properties: json.RawMessage(`{"id":"user-2","sessionID":"native-1","role":"user"}`),
	}); err != nil {
		t.Fatalf("bare message.updated event: %v", err)
	}
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pu-2","sessionID":"native-1","messageID":"user-2","type":"text","text":"another prompt"}}`),
	}); err != nil {
		t.Fatalf("second user part event: %v", err)
	}
	if conn.updateCount() != 0 {
		t.Fatalf("bare-role user prompt echoed: %#v", conn.updates)
	}

	// Assistant parts (role declared or unknown) still stream.
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       eventMessageUpdated,
		Properties: json.RawMessage(`{"info":{"id":"asst-1","sessionID":"native-1","role":"assistant"}}`),
	}); err != nil {
		t.Fatalf("assistant message.updated event: %v", err)
	}
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pa-1","sessionID":"native-1","messageID":"asst-1","type":"text","text":"reply"}}`),
	}); err != nil {
		t.Fatalf("assistant part event: %v", err)
	}
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pa-2","sessionID":"native-1","messageID":"unknown-role","type":"text","text":"more"}}`),
	}); err != nil {
		t.Fatalf("unknown-role part event: %v", err)
	}
	if conn.updateCount() != 2 ||
		conn.updates[0].Update.AgentMessageChunk == nil || conn.updates[1].Update.AgentMessageChunk == nil {
		t.Fatalf("assistant parts not streamed: %#v", conn.updates)
	}

	// Foreign-session and malformed message.updated payloads are ignored, as
	// are infos without an id or role.
	for _, properties := range []string{
		`{"info":{"id":"other","sessionID":"native-other","role":"user"}}`,
		`{"info":{"sessionID":"native-1","role":"user"}}`,
		`not json`,
	} {
		if err := session.handleEvent(ctx, opencode.Event{
			Type:       eventMessageUpdated,
			Properties: json.RawMessage(properties),
		}); err != nil {
			t.Fatalf("message.updated %q: %v", properties, err)
		}
	}

	session.recordMessageRole(opencode.NativeMessageInfo{ID: "no-role", SessionID: "native-1"})
	if role := session.messageRole("no-role"); role != "" {
		t.Fatalf("role recorded without value: %q", role)
	}
}

func TestPromptSSEDisconnectAbortsNativeTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	started := make(chan struct{})
	client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}
	agent := NewAgent()
	session := testSession(agent, client)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Meta: routeCarrier("nonce"), Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start native send")
	}
	client.errs <- errors.New("stream closed")
	select {
	case err := <-done:
		assertTurnFailed(t, err, causeTransport, "stream closed")
	case <-ctx.Done():
		t.Fatal("Prompt did not return")
	}
	if client.abortCount() != 1 {
		t.Fatalf("abort count = %d, want 1", client.abortCount())
	}
}

func TestPromptIdleSSEDisconnectDoesNotPoisonNextTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.errs <- errors.New("idle stream closed")
	client.events <- opencode.Event{Type: "server.connected"}
	client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{
			Info:  opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"},
			Parts: []opencode.NativePart{{ID: "final", SessionID: id, MessageID: "assistant", Type: "text", Text: "ok"}},
		}, nil
	}
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)

	resp, err := session.Prompt(context.Background(), acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("resp = %#v", resp)
	}
	if client.abortCount() != 0 {
		t.Fatalf("idle disconnect aborted native turn %d times", client.abortCount())
	}
}

func TestPromptSuppressesLateFailedEpochEvents(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.getSession = testNativeSession("native-1")
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)
	session.cwd = t.TempDir()
	require.NoError(t, session.snapshotToStore(context.Background()))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		return client, nil
	}
	started := make(chan struct{})
	client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start")
	}
	client.events <- opencode.Event{
		Type:        "message.part.created",
		StreamEpoch: 7,
		Properties:  json.RawMessage(`{"id":"stream-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
	}
	deadline := time.After(time.Second)
	for conn.updateCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("stream update was not emitted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	client.errs <- opencode.StreamError{Epoch: 7, Err: errors.New("stream failed")}
	select {
	case err := <-done:
		assertTurnFailed(t, err, causeTransport, "stream failed")
	case <-ctx.Done():
		t.Fatal("Prompt did not fail on stream error")
	}

	client.events <- opencode.Event{
		Type:        "message.part.created",
		StreamEpoch: 7,
		Properties:  json.RawMessage(`{"id":"late-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"late"}`),
	}
	client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-2", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
	}
	if _, err := session.Prompt(context.Background(), acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("again")}}); err != nil {
		t.Fatalf("second Prompt: %v", err)
	}
	if conn.updateCount() != 1 {
		t.Fatalf("late failed-epoch update was emitted: %#v", conn.updates)
	}
}

func TestPromptNativeTurnQueueHonorsContext(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	session := testSession(agent, client)
	release, err := agent.acquireNativeTurn(context.Background())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("queued")}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestPromptCancelledStreamErrorRetiresRuntime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		closeErr error
	}{
		{name: "contained"},
		{name: "containment failure", closeErr: errors.New("containment failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			client.closeErr = tc.closeErr
			agent := NewAgent()
			session := testSession(agent, client)
			turnCtx := session.beginTurn(context.Background(), "nonce")
			session.cancelled = true
			client.errs <- errors.New("stream failed while cancellation won")

			response, err := session.runPromptTurnWithRefreshedMCP(
				context.Background(), turnCtx,
				acp.PromptRequest{SessionId: session.id},
				func(ctx context.Context) (opencode.NativeMessage, error) {
					<-ctx.Done()

					return opencode.NativeMessage{}, ctx.Err()
				},
				opencode.NativeCommand{}, false,
			)
			if tc.closeErr != nil {
				require.ErrorContains(t, err, tc.closeErr.Error())

				return
			}
			require.NoError(t, err)
			require.Equal(t, acp.StopReasonCancelled, response.StopReason)
		})
	}
}

func TestPromptUnexpectedTurnContextEndIsTransportFailure(t *testing.T) {
	client := newFakeOpenCodeClient()
	session := testSession(NewAgent(), client)
	turnCtx := session.beginTurn(context.Background(), "nonce")
	session.mu.Lock()
	cancel := session.cancel
	session.mu.Unlock()
	cancel()
	release := make(chan struct{})
	defer close(release)

	_, err := session.runPromptTurnWithRefreshedMCP(
		context.Background(), turnCtx, acp.PromptRequest{SessionId: session.id},
		func(context.Context) (opencode.NativeMessage, error) {
			<-release

			return opencode.NativeMessage{}, nil
		},
		opencode.NativeCommand{}, false,
	)
	assertTurnFailed(t, err, causeTransport, "without a cancellation route")
}

func TestFinishPromptTurnCancellationBranches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	response, err := session.finishPromptTurn(
		ctx, context.Background(), acp.PromptRequest{},
		promptTurnResult{err: errors.New("native failed")}, opencode.NativeCommand{}, false,
	)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonCancelled, response.StopReason)

	session = testSession(NewAgent(), newFakeOpenCodeClient())
	response, err = session.finishPromptTurn(
		ctx, context.Background(), acp.PromptRequest{},
		promptTurnResult{message: opencode.NativeMessage{Info: opencode.NativeMessageInfo{
			ID: "assistant", SessionID: "native-1", Role: "assistant", Finish: "stop",
		}}},
		opencode.NativeCommand{}, false,
	)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonCancelled, response.StopReason)
}

func TestPromptCleanEOFSentinelDisconnectAbortsTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	agent := NewAgent()
	session := testSession(agent, client)
	started := make(chan struct{})
	client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start")
	}
	client.errs <- opencode.StreamError{Epoch: 11, Err: opencode.ErrSSEDisconnect}
	select {
	case err := <-done:
		assertTurnFailed(t, err, causeTransport, "opencode SSE disconnected")
	case <-ctx.Done():
		t.Fatal("Prompt did not fail on clean EOF disconnect")
	}
	if client.abortCount() != 1 {
		t.Fatalf("native aborts = %d, want 1", client.abortCount())
	}
}

func TestPromptServerReconnectReconcilesPendingPermissionAndQuestion(t *testing.T) {
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
	session := testSession(agent, client)
	started := make(chan struct{})
	release := make(chan struct{})
	client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-release

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start")
	}
	session.markActiveToolCallID("tool-current")
	client.pendingPermissions = []opencode.PermissionRequest{{ID: "perm", SessionID: "native-1", Action: "edit", Tool: opencode.PermissionTool{CallID: "tool-current"}}}
	client.pendingQuestions = []opencode.QuestionRequest{{
		ID:        "question",
		SessionID: "native-1",
		Questions: []opencode.QuestionInfo{{
			Question: "Pick?",
			Header:   "Pick",
			Options:  []opencode.QuestionOption{{Label: "A", Description: "A"}},
		}},
	}}
	client.events <- opencode.Event{Type: "server.connected"}
	deadline := time.After(time.Second)
	for client.permissionReplyCount() == 0 || client.questionReplyCount() == 0 {
		select {
		case <-deadline:
			t.Fatalf("pending queues not reconciled permissions=%d questions=%d", client.permissionReplyCount(), client.questionReplyCount())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Prompt did not finish")
	}
}

func TestPromptServerReconnectReconcileFailures(t *testing.T) {
	for _, tt := range []struct {
		name          string
		setup         func(*recordingAgentClient, *Agent) chan struct{}
		pending       func(*fakeOpenCodeClient)
		cancel        bool
		wantErr       string
		wantCancelled bool
	}{
		{
			name: "permission error",
			setup: func(conn *recordingAgentClient, _ *Agent) chan struct{} {
				conn.permErr = errors.New("permission failed")

				return nil
			},
			pending: func(client *fakeOpenCodeClient) {
				client.pendingPermissions = []opencode.PermissionRequest{{ID: "perm", SessionID: "native-1", Action: "edit", Tool: opencode.PermissionTool{CallID: "tool-current"}}}
			},
			wantErr: "permission failed",
		},
		{
			name: "permission cancelled",
			setup: func(conn *recordingAgentClient, _ *Agent) chan struct{} {
				conn.permissionStarted = make(chan struct{}, 1)
				conn.permissionRelease = make(chan struct{})

				return conn.permissionStarted
			},
			pending: func(client *fakeOpenCodeClient) {
				client.pendingPermissions = []opencode.PermissionRequest{{ID: "perm", SessionID: "native-1", Action: "edit", Tool: opencode.PermissionTool{CallID: "tool-current"}}}
			},
			cancel:        true,
			wantCancelled: true,
		},
		{
			name: "question error",
			setup: func(conn *recordingAgentClient, agent *Agent) chan struct{} {
				agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
				conn.elicitErr = errors.New("elicitation failed")

				return nil
			},
			pending: func(client *fakeOpenCodeClient) {
				client.pendingQuestions = []opencode.QuestionRequest{{
					ID:        "question",
					SessionID: "native-1",
					Questions: []opencode.QuestionInfo{{
						Question: "Pick?",
						Header:   "Pick",
						Options:  []opencode.QuestionOption{{Label: "A"}},
					}},
				}}
			},
			wantErr: "elicitation failed",
		},
		{
			name: "question cancelled",
			setup: func(conn *recordingAgentClient, agent *Agent) chan struct{} {
				agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
				conn.elicitationStarted = make(chan struct{}, 1)
				conn.elicitationRelease = make(chan struct{})

				return conn.elicitationStarted
			},
			pending: func(client *fakeOpenCodeClient) {
				client.pendingQuestions = []opencode.QuestionRequest{{
					ID:        "question",
					SessionID: "native-1",
					Questions: []opencode.QuestionInfo{{
						Question: "Pick?",
						Header:   "Pick",
						Options:  []opencode.QuestionOption{{Label: "A"}},
					}},
				}}
			},
			cancel:        true,
			wantCancelled: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			conn := newRecordingAgentClient()
			agent := NewAgent()
			agent.setAgentClient(conn)
			startedHook := tt.setup(conn, agent)
			session := testSession(agent, client)
			sendStarted := make(chan struct{})
			client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
				close(sendStarted)
				<-ctx.Done()

				return opencode.NativeMessage{}, ctx.Err()
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan struct {
				resp acp.PromptResponse
				err  error
			}, 1)
			go func() {
				resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
				done <- struct {
					resp acp.PromptResponse
					err  error
				}{resp: resp, err: err}
			}()
			select {
			case <-sendStarted:
			case <-ctx.Done():
				t.Fatal("Prompt did not start")
			}
			session.markActiveToolCallID("tool-current")
			tt.pending(client)
			client.events <- opencode.Event{Type: "server.connected"}
			if startedHook != nil {
				select {
				case <-startedHook:
				case <-ctx.Done():
					t.Fatal("reconcile request did not start")
				}
			}
			if tt.cancel {
				cancel()
			}
			select {
			case got := <-done:
				if tt.wantCancelled {
					if got.err != nil || got.resp.StopReason != acp.StopReasonCancelled {
						t.Fatalf("Prompt cancelled resp=%#v err=%v", got.resp, got.err)
					}

					return
				}
				if got.err == nil || !strings.Contains(got.err.Error(), tt.wantErr) {
					t.Fatalf("Prompt error = %v, want %q", got.err, tt.wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("Prompt did not finish")
			}
		})
	}
}

func TestPromptCancelDuringInFlightPermissionAndQuestion(t *testing.T) {
	for _, tt := range []struct {
		name       string
		setup      func(*Agent)
		sendEvent  func(*fakeOpenCodeClient)
		assertDone func(*testing.T, *fakeOpenCodeClient, *recordingAgentClient)
	}{
		{
			name: "permission",
			sendEvent: func(client *fakeOpenCodeClient) {
				client.events <- opencode.Event{
					Type:       "permission.v2.asked",
					Properties: json.RawMessage(`{"id":"perm","sessionID":"native-1","action":"edit","tool":{"callID":"tool-current"}}`),
				}
			},
			assertDone: func(t *testing.T, client *fakeOpenCodeClient, conn *recordingAgentClient) {
				t.Helper()
				if conn.permissionRequestCount() != 1 {
					t.Fatalf("permission requests = %#v", conn.permissions)
				}
				if client.permissionReplyCount() != 1 {
					t.Fatalf("permission replies = %#v", client.permissionReplies)
				}
				reply := client.permissionReply(0)
				if reply.reply != "reject" || reply.message != "cancelled" {
					t.Fatalf("permission cancel reply = %#v", reply)
				}
			},
		},
		{
			name: "question",
			setup: func(agent *Agent) {
				agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
			},
			sendEvent: func(client *fakeOpenCodeClient) {
				client.events <- opencode.Event{
					Type:       "question.asked",
					Properties: json.RawMessage(`{"id":"question","sessionID":"native-1","questions":[{"question":"Pick one","options":[{"label":"Yes"}]}]}`),
				}
			},
			assertDone: func(t *testing.T, client *fakeOpenCodeClient, conn *recordingAgentClient) {
				t.Helper()
				if len(conn.elicitations) != 1 {
					t.Fatalf("elicitations = %#v", conn.elicitations)
				}
				if client.questionRejectCount() != 1 {
					t.Fatalf("question rejects = %#v", client.questionRejects)
				}
				if client.questionRejects[0].route != opencode.QuestionRouteSession {
					t.Fatalf("question reject route = %#v", client.questionRejects[0])
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			conn := newRecordingAgentClient()
			conn.permissionStarted = make(chan struct{}, 1)
			conn.permissionRelease = make(chan struct{})
			conn.elicitationStarted = make(chan struct{}, 1)
			conn.elicitationRelease = make(chan struct{})
			agent := NewAgent()
			agent.setAgentClient(conn)
			if tt.setup != nil {
				tt.setup(agent)
			}
			session := testSession(agent, client)
			agent.mu.Lock()
			agent.sessions[session.id] = session
			agent.mu.Unlock()

			started := make(chan struct{})
			client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
				close(started)
				<-ctx.Done()

				return opencode.NativeMessage{}, ctx.Err()
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan acp.PromptResponse, 1)
			go func() {
				resp, _ := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Meta: routeCarrier("nonce"), Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
				done <- resp
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("Prompt did not start")
			}
			session.markActiveToolCallID("tool-current")
			tt.sendEvent(client)
			switch tt.name {
			case "permission":
				select {
				case <-conn.permissionStarted:
				case <-ctx.Done():
					t.Fatal("permission request did not start")
				}
			case "question":
				select {
				case <-conn.elicitationStarted:
				case <-ctx.Done():
					t.Fatal("elicitation request did not start")
				}
			}
			if err := agent.Cancel(ctx, CancelRequest(session.id, "nonce")); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
			select {
			case resp := <-done:
				if resp.StopReason != acp.StopReasonCancelled {
					t.Fatalf("prompt resp = %#v", resp)
				}
			case <-ctx.Done():
				t.Fatal("Prompt did not return after cancel")
			}
			tt.assertDone(t, client, conn)
		})
	}
}

func TestPromptBacklogPermissionWithoutCurrentToolFailsClosed(t *testing.T) {
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	conn.permErr = context.Canceled
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)
	session.mu.Lock()
	session.cancelled = true
	session.mu.Unlock()
	client.events <- opencode.Event{
		Type:       "permission.v2.asked",
		Properties: json.RawMessage(`{"id":"perm","sessionID":"native-1"}`),
	}
	_, err := session.Prompt(context.Background(), acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
	if err == nil || !strings.Contains(err.Error(), "outside its originating turn") {
		t.Fatalf("stale permission error=%v", err)
	}
}

func TestPromptBacklogErrorBeforeTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	session := testSession(NewAgent(), client)
	client.events <- opencode.Event{
		Type:       "permission.v2.asked",
		Properties: json.RawMessage(`{`),
	}
	if _, err := session.Prompt(context.Background(), acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
		t.Fatal("malformed backlog event was ignored")
	}
}

func TestTurnBacklogForeignCallbacksAreDiscardedAndMalformedQuestionFails(t *testing.T) {
	client := newFakeOpenCodeClient()
	session := testSession(NewAgent(), client)
	client.events <- opencode.Event{
		Type:       eventPermissionV2Asked,
		Properties: json.RawMessage(`{"id":"permission","sessionID":"foreign"}`),
	}
	client.events <- opencode.Event{
		Type:       eventQuestionAsked,
		Properties: json.RawMessage(`{"id":"question","sessionID":"foreign"}`),
	}
	require.NoError(t, session.drainClientBacklog(context.Background()))
	require.Zero(t, client.permissionReplyCount())
	require.Zero(t, client.questionRejectCount())

	client.events <- opencode.Event{Type: eventQuestionAsked, Properties: json.RawMessage(`{`)}
	require.ErrorContains(t, session.drainClientBacklog(context.Background()), "invalid OpenCode question callback")
}

func TestPromptReconcileCancelledBeforeSend(t *testing.T) {
	for _, tt := range []struct {
		name      string
		setup     func(*fakeOpenCodeClient, *recordingAgentClient, *Agent)
		waitStart func(context.Context, *testing.T, *recordingAgentClient)
	}{
		{
			name: "question",
			setup: func(client *fakeOpenCodeClient, conn *recordingAgentClient, agent *Agent) {
				client.pendingQuestions = []opencode.QuestionRequest{{ID: "question", SessionID: "native-1"}}
				conn.elicitationStarted = make(chan struct{}, 1)
				conn.elicitationRelease = make(chan struct{})
				agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
			},
			waitStart: func(ctx context.Context, t *testing.T, conn *recordingAgentClient) {
				t.Helper()
				select {
				case <-conn.elicitationStarted:
				case <-ctx.Done():
					t.Fatal("elicitation request did not start")
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			conn := newRecordingAgentClient()
			agent := NewAgent()
			agent.setAgentClient(conn)
			session := testSession(agent, client)
			agent.sessions[session.id] = session
			tt.setup(client, conn, agent)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan acp.PromptResponse, 1)
			go func() {
				resp, _ := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
				done <- resp
			}()
			tt.waitStart(ctx, t, conn)
			session.cancelTurn()
			select {
			case resp := <-done:
				if resp.StopReason != acp.StopReasonCancelled {
					t.Fatalf("resp = %#v", resp)
				}
			case <-ctx.Done():
				t.Fatal("prompt did not return")
			}
		})
	}
}

func TestPermissionQuestionCancelledReplyBranches(t *testing.T) {
	t.Run("permission without connection uses background when context cancelled", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		if err := session.handlePermission(turnCtx, opencode.PermissionRequest{ID: "perm", SessionID: "native-1"}); err != nil {
			t.Fatalf("handlePermission: %v", err)
		}
		if got := client.permissionReply(0).message; got != "client unavailable" {
			t.Fatalf("permission reply = %q", got)
		}
		session.finishTurn()
	})

	t.Run("permission client error after context cancellation resolves native request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.permErr = context.Canceled
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		err := session.handlePermission(turnCtx, opencode.PermissionRequest{ID: "perm", SessionID: "native-1"})
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handlePermission err = %v", err)
		}
		if got := client.permissionReply(0).message; got != "cancelled" {
			t.Fatalf("permission reply = %q", got)
		}
		session.finishTurn()
	})

	t.Run("permission client response after context cancellation is rejected", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		err := session.handlePermission(turnCtx, opencode.PermissionRequest{ID: "perm", SessionID: "native-1"})
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handlePermission err = %v", err)
		}
		if got := client.permissionReply(0).message; got != "cancelled" {
			t.Fatalf("permission reply = %q", got)
		}
		session.finishTurn()
	})

	t.Run("permission cancellation reply error is returned", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.replyErr = errors.New("reply failed")
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		if err := session.handlePermission(turnCtx, opencode.PermissionRequest{ID: "perm", SessionID: "native-1"}); err == nil {
			t.Fatal("reply error was ignored")
		}
		session.finishTurn()
	})

	t.Run("permission client error rejects native request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.permErr = errors.New("permission failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		err := session.handlePermission(context.Background(), opencode.PermissionRequest{ID: "perm", SessionID: "native-1", ReplyRoute: opencode.PermissionRouteSession})
		if err == nil || !strings.Contains(err.Error(), "permission failed") {
			t.Fatalf("handlePermission err = %v", err)
		}
		reply := client.permissionReply(0)
		if reply.route != opencode.PermissionRouteSession || reply.reply != "reject" || reply.message != "client permission request failed" {
			t.Fatalf("permission fail-closed reply = %#v", reply)
		}
	})

	t.Run("permission client error returns reject failure", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.replyErr = errors.New("reply failed")
		conn := newRecordingAgentClient()
		conn.permErr = errors.New("permission failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		err := session.handlePermission(context.Background(), opencode.PermissionRequest{ID: "perm", SessionID: "native-1"})
		if err == nil || !strings.Contains(err.Error(), "permission failed") || !strings.Contains(err.Error(), "reply failed") {
			t.Fatalf("handlePermission err = %v", err)
		}
	})

	t.Run("permission late response after cancel is not double-replied", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.permissionStarted = make(chan struct{}, 1)
		conn.permissionRelease = make(chan struct{})
		conn.permissionIgnoreContext = true
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		turnCtx := session.beginTurn(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- session.handlePermission(turnCtx, opencode.PermissionRequest{ID: "perm", SessionID: "native-1"})
		}()
		<-conn.permissionStarted
		session.cancelTurn()
		close(conn.permissionRelease)
		err := <-done
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handlePermission err = %v", err)
		}
		if client.permissionReplyCount() != 1 {
			t.Fatalf("permission replies = %#v", client.permissionReplies)
		}
		session.finishTurn()
	})

	t.Run("permission late client error after cancel is not double-replied", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.permErr = errors.New("permission failed")
		conn.permissionStarted = make(chan struct{}, 1)
		conn.permissionRelease = make(chan struct{})
		conn.permissionIgnoreContext = true
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		turnCtx := session.beginTurn(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- session.handlePermission(turnCtx, opencode.PermissionRequest{ID: "perm", SessionID: "native-1"})
		}()
		<-conn.permissionStarted
		session.cancelTurn()
		close(conn.permissionRelease)
		err := <-done
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handlePermission err = %v", err)
		}
		if client.permissionReplyCount() != 1 {
			t.Fatalf("permission replies = %#v", client.permissionReplies)
		}
		session.finishTurn()
	})
}

func TestQuestionCancelledReplyBranches(t *testing.T) {
	t.Run("question without form support uses background when context cancelled", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		if err := session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"}); err != nil {
			t.Fatalf("handleQuestion: %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %#v", client.questionRejects)
		}
		session.finishTurn()
	})

	t.Run("question client error after context cancellation rejects native request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitErr = context.Canceled
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		err := session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handleQuestion err = %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %#v", client.questionRejects)
		}
		session.finishTurn()
	})

	t.Run("question decline error is returned", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.replyErr = errors.New("reject failed")
		conn := newRecordingAgentClient()
		conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		if err := session.handleQuestion(context.Background(), opencode.QuestionRequest{ID: "question", SessionID: "native-1"}); err == nil {
			t.Fatal("reject error was ignored")
		}
	})

	t.Run("question decline after context cancellation returns cancelled", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		err := session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handleQuestion err = %v", err)
		}
		session.finishTurn()
	})

	t.Run("question late decline after cancel is not double-rejected", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
		conn.elicitationStarted = make(chan struct{}, 1)
		conn.elicitationRelease = make(chan struct{})
		conn.elicitationIgnoreContext = true
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		turnCtx := session.beginTurn(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		}()
		<-conn.elicitationStarted
		session.cancelTurn()
		close(conn.elicitationRelease)
		err := <-done
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handleQuestion err = %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %#v", client.questionRejects)
		}
		session.finishTurn()
	})

	t.Run("question client error rejects native request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitErr = errors.New("elicitation failed")
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		err := session.handleQuestion(context.Background(), opencode.QuestionRequest{ID: "question", SessionID: "native-1", ReplyRoute: opencode.QuestionRouteAPI})
		if err == nil || !strings.Contains(err.Error(), "elicitation failed") {
			t.Fatalf("handleQuestion err = %v", err)
		}
		if client.questionRejectCount() != 1 || client.questionRejects[0].route != opencode.QuestionRouteAPI {
			t.Fatalf("question fail-closed rejects = %#v", client.questionRejects)
		}
	})

	t.Run("question client error returns reject failure", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.replyErr = errors.New("reject failed")
		conn := newRecordingAgentClient()
		conn.elicitErr = errors.New("elicitation failed")
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		err := session.handleQuestion(context.Background(), opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		if err == nil || !strings.Contains(err.Error(), "elicitation failed") || !strings.Contains(err.Error(), "reject failed") {
			t.Fatalf("handleQuestion err = %v", err)
		}
	})

	t.Run("question accept after context cancellation rejects native request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		err := session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handleQuestion err = %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %#v", client.questionRejects)
		}
		session.finishTurn()
	})

	t.Run("question accept cancellation reject error is returned", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.replyErr = errors.New("reject failed")
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := session.beginTurn(ctx)
		cancel()
		if err := session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"}); err == nil {
			t.Fatal("reject error was ignored")
		}
		session.finishTurn()
	})

	t.Run("question late response after cancel is not double-rejected", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitationStarted = make(chan struct{}, 1)
		conn.elicitationRelease = make(chan struct{})
		conn.elicitationIgnoreContext = true
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		turnCtx := session.beginTurn(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		}()
		<-conn.elicitationStarted
		session.cancelTurn()
		close(conn.elicitationRelease)
		err := <-done
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handleQuestion err = %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %#v", client.questionRejects)
		}
		session.finishTurn()
	})

	t.Run("question late client error after cancel is not double-rejected", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.elicitErr = errors.New("elicitation failed")
		conn.elicitationStarted = make(chan struct{}, 1)
		conn.elicitationRelease = make(chan struct{})
		conn.elicitationIgnoreContext = true
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		turnCtx := session.beginTurn(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- session.handleQuestion(turnCtx, opencode.QuestionRequest{ID: "question", SessionID: "native-1"})
		}()
		<-conn.elicitationStarted
		session.cancelTurn()
		close(conn.elicitationRelease)
		err := <-done
		if !errors.Is(err, errPromptCancelled) {
			t.Fatalf("handleQuestion err = %v", err)
		}
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %#v", client.questionRejects)
		}
		session.finishTurn()
	})
}

func TestPromptHelpersAndAnswerMapping(t *testing.T) {
	imageMime := "image/png"
	parts, err := promptToOpenCodeParts([]acp.ContentBlock{
		acp.TextBlock("hello"),
		{ResourceLink: &acp.ContentBlockResourceLink{Name: "a", Type: "resource_link", Uri: "file:///tmp/a"}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: "embedded", Uri: "file:///tmp/b"},
		}}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "AA==", Uri: "file:///tmp/blob"},
		}}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "AA==", Uri: "file:///tmp/image.png", MimeType: &imageMime},
		}}},
		{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("promptToOpenCodeParts: %v", err)
	}
	if len(parts) != 6 || parts[0]["text"] != "hello" || parts[1]["text"] != "file:///tmp/a" ||
		parts[2]["text"] != "embedded" || parts[3]["text"] != "file:///tmp/blob" ||
		parts[4]["type"] != "file" || parts[4]["mime"] != "image/png" || parts[4]["url"] != "data:image/png;base64,AA==" ||
		parts[5]["type"] != "file" || parts[5]["mime"] != "image/png" || parts[5]["url"] != "data:image/png;base64,AA==" {
		t.Fatalf("parts = %#v", parts)
	}
	if _, err = promptToOpenCodeParts(nil); err == nil {
		t.Fatal("empty prompt accepted")
	}
	if _, err = promptToOpenCodeParts([]acp.ContentBlock{{Audio: &acp.ContentBlockAudio{Type: "audio", Data: "AA==", MimeType: "audio/wav"}}}); err == nil {
		t.Fatal("audio prompt accepted")
	}
	if _, err = promptToOpenCodeParts([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image"}}}); err == nil {
		t.Fatal("empty image prompt accepted")
	}
	if _, err = promptToOpenCodeParts([]acp.ContentBlock{{Resource: &acp.ContentBlockResource{
		Type: "resource", Resource: acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{}},
	}}}); err == nil {
		t.Fatal("empty embedded text resource accepted")
	}
	invalidURI := "%"
	parts, err = promptToOpenCodeParts([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Uri: &invalidURI}}})
	if err != nil || parts[0]["filename"] != nil || parts[0]["mime"] != "application/octet-stream" || parts[0]["url"] != invalidURI {
		t.Fatalf("invalid uri image parts = %#v err=%v", parts, err)
	}
	rootURI := "https://example.com"
	parts, err = promptToOpenCodeParts([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Uri: &rootURI}}})
	if err != nil || parts[0]["filename"] != nil || parts[0]["url"] != rootURI {
		t.Fatalf("root uri image parts = %#v err=%v", parts, err)
	}
	req, ids := questionElicitationRequest(opencode.QuestionRequest{ID: "q", SessionID: "s"})
	if req.Form == nil || req.Form.Message != "OpenCode needs input" || !reflect.DeepEqual(ids, []string{"question_1"}) {
		t.Fatalf("empty question elicitation = %#v ids=%#v", req, ids)
	}
	answers := questionAnswersFromContent(map[string]any{
		"question_1": nil,
		"question_2": []string{"a"},
		"question_3": []any{"b", float64(3), nil},
		"question_4": 4,
	}, []string{"question_1", "question_2", "question_3", "question_4"})
	if !reflect.DeepEqual(answers, [][]string{{}, {"a"}, {"b", "3"}, {"4"}}) {
		t.Fatalf("answers = %#v", answers)
	}
}

func TestPromptSendsNativeImageFileParts(t *testing.T) {
	client := newFakeOpenCodeClient()
	agent := NewAgent()
	session := testSession(agent, client)
	imageURI := "file:///tmp/screenshot.png"
	client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		want := []map[string]any{
			{"type": "text", "text": "look"},
			{"type": "file", "mime": "image/png", "url": "data:image/png;base64,AA=="},
			{"type": "file", "mime": "image/jpeg", "url": "file:///tmp/screenshot.png", "filename": "screenshot.png"},
		}
		if !reflect.DeepEqual(req.Parts, want) {
			t.Fatalf("native parts = %#v, want %#v", req.Parts, want)
		}

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
	}
	_, err := session.Prompt(context.Background(), acp.PromptRequest{
		SessionId: session.id,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("look"),
			{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png"}},
			{Image: &acp.ContentBlockImage{Type: "image", Uri: &imageURI, MimeType: "image/jpeg"}},
		},
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
}

func TestSlashCommandRefreshAdvertisesNativeListWithSanitizer(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{
		{Name: "init", Description: "Initialize", Source: "command", Template: "must not leak", Hints: []string{"$ARGUMENTS", "$1"}},
		{Name: "mcp:server:prompt", Source: "mcp"},
		{Name: ""},
		{Name: "/bad"},
		{Name: "bad/name"},
		{Name: "bad name"},
		{Name: "bad\nname"},
		{Name: "bad\u0007name"},
		{Name: "bad\u200dname"},
		{Name: string([]byte{0xff})},
	}
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)

	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("refreshCommands: %v", err)
	}
	if conn.updateCount() != 1 {
		t.Fatalf("updates = %#v", conn.updates)
	}
	update := conn.updates[0].Update.AvailableCommandsUpdate
	if update == nil {
		t.Fatalf("update = %#v", conn.updates[0].Update)
	}
	if got := commandNames(update.AvailableCommands); !reflect.DeepEqual(got, []string{"init", "mcp:server:prompt"}) {
		t.Fatalf("advertised names = %#v", got)
	}
	if update.AvailableCommands[0].Input == nil ||
		update.AvailableCommands[0].Input.Unstructured == nil ||
		update.AvailableCommands[0].Input.Unstructured.Hint != "$ARGUMENTS $1" {
		t.Fatalf("hint = %#v", update.AvailableCommands[0].Input)
	}
	if update.AvailableCommands[1].Description != "OpenCode mcp command" {
		t.Fatalf("synthesized description = %q", update.AvailableCommands[1].Description)
	}
	raw, err := json.Marshal(conn.updates)
	if err != nil {
		t.Fatalf("marshal updates: %v", err)
	}
	if strings.Contains(string(raw), "template") || strings.Contains(string(raw), "must not leak") {
		t.Fatalf("template leaked in update: %s", raw)
	}
	if _, ok := session.cachedCommand("mcp:server:prompt"); !ok {
		t.Fatal("colon command was not routable")
	}
	for _, invalid := range []string{"", "/bad", "bad/name", "bad name", "bad\nname", "bad\u0007name", "bad\u200dname", string([]byte{0xff})} {
		if _, ok := session.cachedCommand(invalid); ok {
			t.Fatalf("invalid command %q was routable", invalid)
		}
	}
	if got := availableCommandFromNative(opencode.NativeCommand{Name: "native"}).Description; got != "OpenCode command" {
		t.Fatalf("empty source description = %q", got)
	}
	cloned := cloneAvailableCommands([]acp.AvailableCommand{{Name: "meta", Description: "Meta", Meta: map[string]any{"k": "v"}}})
	cloned[0].Meta["k"] = "changed"
	if cloned[0].Meta["k"] == "v" {
		t.Fatal("metadata clone did not return mutable copy")
	}
}

func TestSlashCommandRefreshEmptyClearAndFailureKeepsCache(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)

	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if conn.updateCount() != 0 {
		t.Fatalf("initial empty update emitted: %#v", conn.updates)
	}

	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("non-empty refresh: %v", err)
	}
	if conn.updateCount() != 1 {
		t.Fatalf("updates after non-empty = %#v", conn.updates)
	}
	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("unchanged refresh: %v", err)
	}
	if conn.updateCount() != 1 {
		t.Fatalf("unchanged refresh emitted: %#v", conn.updates)
	}

	client.commandsErr = errors.New("commands failed")
	if err := session.refreshCommands(ctx); err == nil {
		t.Fatal("refresh failure returned nil")
	}
	if conn.updateCount() != 1 {
		t.Fatalf("failed refresh emitted update: %#v", conn.updates)
	}
	if _, ok := session.cachedCommand("review"); !ok {
		t.Fatal("failed refresh did not keep last good command")
	}

	client.commandsErr = nil
	client.commands = nil
	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("clear refresh: %v", err)
	}
	if conn.updateCount() != 2 {
		t.Fatalf("clear update missing: %#v", conn.updates)
	}
	clearUpdate := conn.updates[1].Update.AvailableCommandsUpdate
	if clearUpdate == nil || len(clearUpdate.AvailableCommands) != 0 {
		t.Fatalf("clear update = %#v", clearUpdate)
	}
	wire, err := json.Marshal(conn.updates[1])
	if err != nil {
		t.Fatalf("marshal clear update: %v", err)
	}
	if !strings.Contains(string(wire), `"availableCommands":[]`) {
		t.Fatalf("clear update JSON = %s", wire)
	}
	if strings.Contains(string(wire), `"availableCommands":null`) {
		t.Fatalf("clear update JSON used null: %s", wire)
	}
}

func TestPromptSlashCommandRouting(t *testing.T) {
	ctx := context.Background()
	t.Run("exact match routes to native command", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		agent := NewAgent()
		session := testSession(agent, client)
		messageID := "msg-user"
		client.runCommand = func(_ context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
			if id != "native-1" {
				t.Fatalf("native id = %q", id)
			}
			if req.MessageID != messageID || req.Agent != "build" || req.Model != "openai/gpt-test" ||
				req.Command != "review" || req.Arguments != " inspect this" || len(req.Parts) != 0 {
				t.Fatalf("command request = %#v", req)
			}

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		resp, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			MessageId: &messageID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("/review  inspect this")},
		})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if resp.StopReason != acp.StopReasonEndTurn || resp.UserMessageId == nil || *resp.UserMessageId != messageID {
			t.Fatalf("response = %#v", resp)
		}
	})

	t.Run("unmatched slash and leading whitespace are plain messages", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			text string
		}{
			{name: "unmatched", text: "/missing args"},
			{name: "escaped", text: " /review args"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				client := newFakeOpenCodeClient()
				client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
				agent := NewAgent()
				session := testSession(agent, client)
				client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
					if req.Parts[0]["text"] != tt.text {
						t.Fatalf("plain text part = %#v", req.Parts)
					}

					return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
				}
				if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock(tt.text)}}); err != nil {
					t.Fatalf("Prompt: %v", err)
				}
			})
		}
	})

	t.Run("refresh failure before slash prompt keeps plain path", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commandsErr = errors.New("commands failed")
		agent := NewAgent()
		session := testSession(agent, client)
		client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if req.Parts[0]["text"] != "/missing args" {
				t.Fatalf("plain text part = %#v", req.Parts)
			}

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/missing args")}}); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})

	t.Run("pre-prompt refresh removed cached command", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "stale", Description: "Stale", Source: "command"}}
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("initial refresh: %v", err)
		}
		if conn.updateCount() != 1 {
			t.Fatalf("initial updates = %#v", conn.updates)
		}
		client.commands = nil
		client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			t.Fatal("removed command fell back to plain message")

			return opencode.NativeMessage{}, nil
		}
		client.runCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
			t.Fatal("removed command was sent to native command endpoint")

			return opencode.NativeMessage{}, nil
		}

		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/stale now")}})
		if err == nil || !strings.Contains(err.Error(), "opencode_command_removed") || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("removed command error = %v", err)
		}
		if conn.updateCount() != 2 {
			t.Fatalf("updates = %#v", conn.updates)
		}
		clearUpdate := conn.updates[1].Update.AvailableCommandsUpdate
		if clearUpdate == nil || len(clearUpdate.AvailableCommands) != 0 {
			t.Fatalf("refreshed clear update = %#v", clearUpdate)
		}
	})

	t.Run("custom shadowing fixture routes exact name as data", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "init", Description: "Workspace init", Source: "command"}}
		session := testSession(NewAgent(), client)
		client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			t.Fatal("shadowing command fell back to plain message")

			return opencode.NativeMessage{}, nil
		}
		client.runCommand = func(_ context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
			if req.Command != "init" || req.Arguments != " custom args" {
				t.Fatalf("shadow command request = %#v", req)
			}

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/init  custom args")}}); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
}

func TestPromptSlashCommandMixedContent(t *testing.T) {
	ctx := context.Background()
	t.Run("matched command sends file parts", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		session := testSession(NewAgent(), client)
		imageURI := "file:///tmp/screenshot.png"
		resourceMime := "text/plain"
		blobMime := "application/octet-stream"
		client.runCommand = func(_ context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
			want := []map[string]any{
				{"type": "file", "mime": "image/png", "url": "data:image/png;base64,AA=="},
				{"type": "file", "mime": "image/jpeg", "url": "file:///tmp/screenshot.png", "filename": "screenshot.png"},
				{"type": "file", "mime": "text/plain", "url": "file:///tmp/notes.txt", "filename": "notes.txt"},
				{"type": "file", "mime": "application/octet-stream", "url": "data:application/octet-stream;base64,AA==", "filename": "blob.bin"},
			}
			if !reflect.DeepEqual(req.Parts, want) {
				t.Fatalf("command parts = %#v, want %#v", req.Parts, want)
			}

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt: []acp.ContentBlock{
				acp.TextBlock("/review"),
				{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png"}},
				{Image: &acp.ContentBlockImage{Type: "image", Uri: &imageURI, MimeType: "image/jpeg"}},
				{ResourceLink: &acp.ContentBlockResourceLink{Type: "resource_link", Uri: "file:///tmp/notes.txt", MimeType: &resourceMime}},
				{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
					BlobResourceContents: &acp.BlobResourceContents{Blob: "AA==", Uri: "file:///tmp/blob.bin", MimeType: &blobMime},
				}}},
			},
		})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})

	t.Run("matched command rejects unconvertible block type", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		session := testSession(NewAgent(), client)
		client.runCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
			t.Fatal("RunCommand called for unconvertible block")

			return opencode.NativeMessage{}, nil
		}
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt: []acp.ContentBlock{
				acp.TextBlock("/review"),
				{Audio: &acp.ContentBlockAudio{Type: "audio", Data: "AA==", MimeType: "audio/wav"}},
			},
		})
		requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: "prompt"})
	})

	t.Run("command part conversion errors use the uniform prompt shape", func(t *testing.T) {
		if _, err := commandPromptParts([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image"}}}); err == nil {
			t.Fatal("empty command image accepted")
		}
		for _, block := range []acp.ContentBlock{
			acp.TextBlock("extra text"),
			{Resource: &acp.ContentBlockResource{Type: "resource"}},
			{},
		} {
			_, err := commandPromptParts([]acp.ContentBlock{block})
			if err == nil {
				t.Fatalf("unconvertible command block accepted: %#v", block)
			}
			requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: "prompt"})
		}
		if _, err := blobResourceOpenCodePart(&acp.BlobResourceContents{}); err == nil {
			t.Fatal("empty blob resource accepted")
		}
		named := resourceLinkOpenCodePart(&acp.ContentBlockResourceLink{Name: "named.txt", Uri: "file:///tmp/ignored"})
		if named["mime"] != "application/octet-stream" || named["filename"] != "named.txt" {
			t.Fatalf("named resource link part = %#v", named)
		}
	})

	t.Run("unmatched slash keeps supported mixed content as plain message", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		session := testSession(NewAgent(), client)
		client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if len(req.Parts) != 2 || req.Parts[0]["text"] != "/missing" || req.Parts[1]["type"] != "file" {
				t.Fatalf("plain mixed parts = %#v", req.Parts)
			}

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt: []acp.ContentBlock{
				acp.TextBlock("/missing"),
				{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png"}},
			},
		})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
}

func TestPromptSlashCommandStaleRaceRefreshesWithoutPlainRetry(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name       string
		refreshErr bool
	}{
		{name: "refresh clears deleted command"},
		{name: "refresh failure is logged", refreshErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			client.commands = []opencode.NativeCommand{{Name: "stale", Description: "Stale", Source: "command"}}
			client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
				t.Fatal("stale command retried as plain prompt")

				return opencode.NativeMessage{}, nil
			}
			client.runCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
				if tt.refreshErr {
					client.commandsErr = errors.New("refresh failed")
				} else {
					client.commands = nil
				}

				return opencode.NativeMessage{}, &opencode.HTTPError{Method: "POST", Path: "/session/native-1/command", Status: "400 Bad Request", StatusCode: 400, Body: "unknown command"}
			}
			conn := newRecordingAgentClient()
			agent := NewAgent()
			agent.setAgentClient(conn)
			session := testSession(agent, client)

			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/stale now")}})
			if err == nil || !strings.Contains(err.Error(), "opencode_command_bad_request") {
				t.Fatalf("stale race error = %v", err)
			}
			if tt.refreshErr {
				if conn.updateCount() != 1 {
					t.Fatalf("failed refresh updates = %#v", conn.updates)
				}

				return
			}
			if conn.updateCount() != 2 {
				t.Fatalf("updates = %#v", conn.updates)
			}
			clearUpdate := conn.updates[1].Update.AvailableCommandsUpdate
			if clearUpdate == nil || len(clearUpdate.AvailableCommands) != 0 {
				t.Fatalf("refreshed clear update = %#v", clearUpdate)
			}
		})
	}
}

func TestPromptSlashCommandBadRequestMergesAssistantErrorFields(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "stale", Description: "Stale", Source: "command"}}
	client.runCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
		client.commands = nil

		nativeErr := &opencode.NativeError{Name: "APIError"}
		nativeErr.Data.StatusCode = 429
		nativeErr.Data.ResponseBody = `{"error":{"type":"rate_limit_exceeded"}}`
		assistantErr := opencode.AssistantMessageError(opencode.NativeMessage{Info: opencode.NativeMessageInfo{
			Role:   "assistant",
			Finish: "error",
			Error:  nativeErr,
		}})
		httpErr := &opencode.HTTPError{Method: "POST", Path: "/session/native-1/command", Status: "400 Bad Request", StatusCode: 400, Body: "unknown command"}

		return opencode.NativeMessage{}, errors.Join(httpErr, assistantErr)
	}
	session := testSession(NewAgent(), client)

	_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/stale now")}})
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected *acp.RequestError, got %T: %v", err, err)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data = %#v", reqErr.Data)
	}
	if data["structuredOutputRequested"] != false || data["statusCode"] != 429 || data["providerCode"] != "rate_limit_exceeded" {
		t.Fatalf("assistant error data = %#v", data)
	}
}

func TestPromptSlashCommandExclusiveTurn(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	agent := NewAgent()
	session := testSession(agent, client)
	release, err := session.acquireTurn(ctx)
	if err != nil {
		t.Fatalf("acquire normal turn: %v", err)
	}
	defer release()
	_, err = session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/review")}})
	if err == nil || !strings.Contains(err.Error(), "backpressure") {
		t.Fatalf("command during active prompt error = %v", err)
	}
}

func TestUsageUpdateSizeIsContextWindow(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name       string
		providerID string
		modelID    string
		wantSize   int
	}{
		{name: "known model reports context window", providerID: "openai", modelID: "gpt-test", wantSize: 1000},
		{name: "model without limit reports unknown", providerID: "openai", modelID: "gpt-other", wantSize: 0},
		{name: "missing model reports unknown", providerID: "", modelID: "", wantSize: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			conn := newRecordingAgentClient()
			agent := NewAgent()
			agent.setAgentClient(conn)
			session := testSession(agent, client)

			if err := session.emitMessage(ctx, opencode.NativeMessage{
				Info: opencode.NativeMessageInfo{
					ID:         "message-1",
					SessionID:  "native-1",
					Role:       "assistant",
					ProviderID: tt.providerID,
					ModelID:    tt.modelID,
					Tokens:     opencode.NativeTokens{Total: 42},
				},
			}, false); err != nil {
				t.Fatalf("emitMessage: %v", err)
			}

			var usage *acp.SessionUsageUpdate
			for _, update := range conn.updates {
				if update.Update.UsageUpdate != nil {
					usage = update.Update.UsageUpdate
				}
			}
			if usage == nil {
				t.Fatalf("no usage update emitted: %#v", conn.updates)
			}
			if usage.Used != 42 {
				t.Fatalf("usage used = %d, want 42", usage.Used)
			}
			if usage.Size != tt.wantSize {
				t.Fatalf("usage size = %d, want %d (context window, never fabricated from used)", usage.Size, tt.wantSize)
			}
		})
	}
}

func commandNames(commands []acp.AvailableCommand) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}

	return names
}

func TestPromptSuccessCancelAndErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("success through agent", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		messageID := "user-message"
		client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if id != "native-1" || req.MessageID != messageID || len(req.Parts) != 1 {
				t.Fatalf("SendMessage id=%q req=%#v", id, req)
			}
			msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{
				ID:        "assistant-1",
				SessionID: id,
				Role:      "assistant",
				Finish:    "length",
				Tokens:    opencode.NativeTokens{Total: 3, Input: 1, Output: 2},
			}}
			msg.Parts = []opencode.NativePart{{SessionID: id, MessageID: "assistant-1", Type: "text", Text: "hi"}}

			return msg, nil
		}
		resp, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Meta: routeCarrier("nonce"), MessageId: &messageID, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if resp.StopReason != acp.StopReasonMaxTokens || resp.UserMessageId == nil || *resp.UserMessageId != messageID || resp.Usage.TotalTokens != 3 {
			t.Fatalf("prompt resp = %#v", resp)
		}
		if conn.updateCount() != 2 {
			t.Fatalf("updates = %#v", conn.updates)
		}
	})

	t.Run("send error", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{}, errors.New("send failed")
		}
		session := testSession(NewAgent(), client)
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
			t.Fatal("send error prompt succeeded")
		}
	})

	t.Run("snapshot error after final message", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		agent := NewAgent(WithSessionStore(&errorSessionStore{err: errors.New("snapshot failed")}))
		session := testSession(agent, client)
		client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil ||
			!strings.Contains(err.Error(), "snapshot failed") {
			t.Fatalf("snapshot error = %v", err)
		}
	})

	t.Run("prompt validation and turn backpressure", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		session.turnQueue() <- struct{}{}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
			t.Fatal("prompt backpressure was ignored")
		}
		<-session.turnQueue()
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id}); err == nil {
			t.Fatal("empty prompt was accepted")
		}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{{Audio: &acp.ContentBlockAudio{Type: "audio", Data: "AA==", MimeType: "audio/wav"}}}}); err == nil {
			t.Fatal("unsupported audio prompt was accepted")
		}
	})

	t.Run("pending permission error", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.permissionsErr = errors.New("permissions failed")
		session := testSession(NewAgent(), client)
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
			t.Fatal("permission error prompt succeeded")
		}
	})

	t.Run("pending question error", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.questionsErr = errors.New("questions failed")
		session := testSession(NewAgent(), client)
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
			t.Fatal("question error prompt succeeded")
		}
	})

	t.Run("turn context cancellation", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		started := make(chan struct{})
		client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			close(started)
			<-ctx.Done()

			return opencode.NativeMessage{}, ctx.Err()
		}
		session := testSession(NewAgent(), client)
		promptCtx, cancel := context.WithCancel(context.Background())
		done := make(chan acp.PromptResponse, 1)
		go func() {
			resp, _ := session.Prompt(promptCtx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- resp
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("sendMessage did not start")
		}
		cancel()
		select {
		case resp := <-done:
			if resp.StopReason != acp.StopReasonCancelled {
				t.Fatalf("cancel resp = %#v", resp)
			}
		case <-time.After(time.Second):
			t.Fatal("cancelled prompt did not return")
		}
	})

	t.Run("unknown agent prompt and cancel", func(t *testing.T) {
		agent := NewAgent()
		if _, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: "missing", Meta: routeCarrier("nonce")}); err == nil {
			t.Fatal("unknown agent prompt succeeded")
		}
		if err := agent.Cancel(ctx, CancelRequest("missing", "nonce")); err == nil {
			t.Fatal("unknown agent cancel succeeded")
		}
	})
}

func TestNativeSessionIDDriftPoisonsSession(t *testing.T) {
	ctx := context.Background()

	t.Run("mismatched final message info session id", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		conn := newRecordingAgentClient()
		store := newCountingSessionStore()
		agent := NewAgent(WithSessionStore(store))
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("refreshCommands: %v", err)
		}
		client.sendMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{
				Info:  opencode.NativeMessageInfo{ID: "assistant", SessionID: "native-other", Role: "assistant", Finish: "stop"},
				Parts: []opencode.NativePart{{SessionID: "native-1", MessageID: "assistant", Type: "text", Text: "should not emit"}},
			}, nil
		}

		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		assertNativeSessionDriftPoison(t, session, conn, store, err, "native-other")
	})

	t.Run("mismatched final message part session id", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		conn := newRecordingAgentClient()
		store := newCountingSessionStore()
		agent := NewAgent(WithSessionStore(store))
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("refreshCommands: %v", err)
		}
		client.sendMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{
				Info:  opencode.NativeMessageInfo{ID: "assistant", SessionID: "native-1", Role: "assistant", Finish: "stop"},
				Parts: []opencode.NativePart{{SessionID: "native-other", MessageID: "assistant", Type: "text", Text: "should not emit"}},
			}, nil
		}

		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		assertNativeSessionDriftPoison(t, session, conn, store, err, "native-other")
	})

	t.Run("mismatched replay message session id", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		conn := newRecordingAgentClient()
		store := newCountingSessionStore()
		agent := NewAgent(WithSessionStore(store))
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("refreshCommands: %v", err)
		}
		client.messages = []opencode.NativeMessage{{
			Info: opencode.NativeMessageInfo{ID: "user", SessionID: "native-other", Role: "user"},
			Parts: []opencode.NativePart{{
				SessionID: "native-other",
				MessageID: "user",
				Type:      "text",
				Text:      "should not replay",
			}},
		}}

		err := session.replayMessages(ctx)
		assertNativeSessionDriftPoison(t, session, conn, store, err, "native-other")
	})
}

func TestPoisonedSessionRejectsFollowUpOperations(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	store := newCountingSessionStore()
	agent := NewAgent(WithSessionStore(store))
	agent.setAgentClient(conn)
	s := testSession(agent, client)
	agent.mu.Lock()
	agent.sessions[s.id] = s
	agent.mu.Unlock()

	if err := s.poison(ctx, "native drift without advertisement"); err == nil ||
		!strings.Contains(err.Error(), "opencode_native_session_id_drift") {
		t.Fatalf("poison error = %v", err)
	}
	if conn.updateCount() != 0 {
		t.Fatalf("poison without commands emitted updates: %#v", conn.updates)
	}
	if err := s.poison(ctx, "second poison"); err == nil ||
		!strings.Contains(err.Error(), "session_poisoned") ||
		!strings.Contains(err.Error(), "native drift without advertisement") {
		t.Fatalf("second poison error = %v", err)
	}
	if _, err := s.acquireTurn(ctx); err == nil || !strings.Contains(err.Error(), "session_poisoned") {
		t.Fatalf("acquire poisoned session error = %v", err)
	}
	if err := s.refreshCommands(ctx); err == nil || !strings.Contains(err.Error(), "session_poisoned") {
		t.Fatalf("refresh poisoned session error = %v", err)
	}
	if err := agent.Cancel(ctx, CancelRequest(s.id, "nonce")); err == nil || !strings.Contains(err.Error(), "session_poisoned") {
		t.Fatalf("cancel poisoned session error = %v", err)
	}
	if _, err := agent.SetSessionConfigOption(ctx, SetModelRequest(s.id, "openai/gpt-test")); err == nil ||
		!strings.Contains(err.Error(), "session_poisoned") {
		t.Fatalf("set config poisoned session error = %v", err)
	}
	if err := s.replayMessages(ctx); err == nil || !strings.Contains(err.Error(), "session_poisoned") {
		t.Fatalf("replay poisoned session error = %v", err)
	}
	if err := s.snapshotToStore(ctx); err == nil || !strings.Contains(err.Error(), "session_poisoned") {
		t.Fatalf("snapshot poisoned session error = %v", err)
	}
	if store.replaceCount() != 0 {
		t.Fatalf("store writes after poisoned follow-up = %d, want 0", store.replaceCount())
	}
	if err := (&session{}).validateNativeMessageSession(ctx, opencode.NativeMessage{Info: opencode.NativeMessageInfo{SessionID: "native-other"}}); err != nil {
		t.Fatalf("empty expected native id validation error = %v", err)
	}
}

func assertNativeSessionDriftPoison(
	t *testing.T,
	session *session,
	conn *recordingAgentClient,
	store *countingSessionStore,
	err error,
	gotNativeID string,
) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "opencode_native_session_id_drift") || !strings.Contains(err.Error(), gotNativeID) {
		t.Fatalf("drift error = %v", err)
	}
	if conn.updateCount() != 2 {
		t.Fatalf("updates after poison = %#v", conn.updates)
	}
	clearUpdate := conn.updates[1].Update.AvailableCommandsUpdate
	if clearUpdate == nil || clearUpdate.AvailableCommands == nil || len(clearUpdate.AvailableCommands) != 0 {
		t.Fatalf("poison clear update = %#v", clearUpdate)
	}
	if store.replaceCount() != 0 {
		t.Fatalf("store writes after poison = %d, want 0", store.replaceCount())
	}
	_, nextErr := session.Prompt(context.Background(), acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("again")}})
	if nextErr == nil || !strings.Contains(nextErr.Error(), "session_poisoned") || !strings.Contains(nextErr.Error(), gotNativeID) {
		t.Fatalf("subsequent poison error = %v", nextErr)
	}
}

type countingSessionStore struct {
	*InMemorySessionStore
	mu       sync.Mutex
	replaces int
}

func newCountingSessionStore() *countingSessionStore {
	return &countingSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
}

func (s *countingSessionStore) Replace(ctx context.Context, main SessionKey, replacements []SessionStoreReplacement) error {
	s.mu.Lock()
	s.replaces++
	s.mu.Unlock()

	return s.InMemorySessionStore.Replace(ctx, main, replacements)
}

func (s *countingSessionStore) replaceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.replaces
}

func TestPromptEventLoopAndEmitErrorBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("server connected is ignored before final message", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		started := make(chan struct{})
		release := make(chan struct{})
		client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			close(started)
			<-release

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- err
		}()
		<-started
		client.events <- opencode.Event{Type: "server.connected"}
		client.events <- opencode.Event{
			Type:       "message.part.created",
			Properties: json.RawMessage(`{"id":"event-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
		}
		deadline := time.After(time.Second)
		for conn.updateCount() == 0 {
			select {
			case <-deadline:
				t.Fatalf("stream update was not emitted: %#v", conn.updates)
			default:
				time.Sleep(time.Millisecond)
			}
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if conn.updateCount() != 1 {
			t.Fatalf("updates = %#v", conn.updates)
		}
	})

	t.Run("event update error returns", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.updateErr = errors.New("update failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		started := make(chan struct{})
		client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			close(started)
			<-ctx.Done()

			return opencode.NativeMessage{}, ctx.Err()
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- err
		}()
		<-started
		client.events <- opencode.Event{
			Type:       "message.part.created",
			Properties: json.RawMessage(`{"id":"event-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
		}
		if err := <-done; err == nil || !strings.Contains(err.Error(), "update failed") {
			t.Fatalf("event error = %v", err)
		}
		if client.abortCount() != 1 {
			t.Fatalf("event error aborts = %d, want 1", client.abortCount())
		}
	})

	t.Run("final message update error returns", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.updateErr = errors.New("final update failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{
				Info:  opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"},
				Parts: []opencode.NativePart{{ID: "final", SessionID: id, MessageID: "assistant", Type: "text", Text: "done", Raw: json.RawMessage(`{"id":"final"}`)}},
			}, nil
		}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil ||
			!strings.Contains(err.Error(), "final update failed") {
			t.Fatalf("final emit error = %v", err)
		}
	})
}

func TestReplayAndEventEdgeBranches(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(agent, client)

	client.messages = []opencode.NativeMessage{{
		Info: opencode.NativeMessageInfo{ID: "user-1", SessionID: "native-1", Role: "user"},
		Parts: []opencode.NativePart{{
			SessionID: "native-1",
			MessageID: "user-1",
			Type:      "text",
			Text:      "user text",
		}},
	}}
	if err := session.replayMessages(ctx); err != nil {
		t.Fatalf("replayMessages: %v", err)
	}
	if conn.updateCount() != 1 || conn.updates[0].Update.UserMessageChunk == nil {
		t.Fatalf("replay updates = %#v", conn.updates)
	}
	client.messagesErr = errors.New("messages failed")
	if err := session.replayMessages(ctx); err == nil {
		t.Fatal("replayMessages ignored client error")
	}
	if err := session.emitMessage(ctx, opencode.NativeMessage{Info: opencode.NativeMessageInfo{Role: "user"}}, false); err != nil {
		t.Fatalf("emitMessage skipped user: %v", err)
	}
	if err := session.emitUpdate(ctx, acp.UpdatePlan(acp.PlanEntry{Content: "no client"})); err != nil {
		t.Fatalf("emitUpdate with conn: %v", err)
	}
	nilConnSession := testSession(NewAgent(), newFakeOpenCodeClient())
	if err := nilConnSession.emitUpdate(ctx, acp.UpdatePlan(acp.PlanEntry{Content: "no client"})); err != nil {
		t.Fatalf("emitUpdate without conn: %v", err)
	}

	noConnClient := newFakeOpenCodeClient()
	noConnSession := testSession(NewAgent(), noConnClient)
	if err := noConnSession.handlePermission(ctx, opencode.PermissionRequest{}); err != nil {
		t.Fatalf("empty permission: %v", err)
	}
	noConnSession.pending = nil
	if err := noConnSession.handlePermission(ctx, opencode.PermissionRequest{ID: "p", SessionID: "native-1"}); err != nil {
		t.Fatalf("nil conn permission: %v", err)
	}
	if got := noConnClient.permissionReply(0).message; got != "client unavailable" {
		t.Fatalf("nil conn permission reply = %q", got)
	}
	session.questions = nil
	if err := session.handleQuestion(ctx, opencode.QuestionRequest{}); err != nil {
		t.Fatalf("empty question: %v", err)
	}
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	conn.elicitErr = errors.New("elicitation failed")
	if err := session.handleQuestion(ctx, opencode.QuestionRequest{ID: "q-error", SessionID: "native-1"}); err == nil {
		t.Fatal("elicitation error was ignored")
	}
	conn.elicitErr = nil
	if err := session.handleEvent(ctx, opencode.Event{Type: "permission.v2.asked", Properties: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed permission event succeeded")
	}
	conn.permission = acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("once")}
	session.beginTurn(ctx)
	session.markActiveToolCallID("c1")
	defer session.finishTurn()
	if err := session.handleEvent(ctx, opencode.Event{
		Type: "permission.asked",
		Properties: json.RawMessage(`{
			"id":"p-session",
			"sessionID":"native-1",
			"permission":"edit",
			"patterns":["acp-permission-probe.txt"],
			"metadata":{"filepath":"acp-permission-probe.txt"},
			"tool":{"messageID":"m1","callID":"c1"}
		}`),
	}); err != nil {
		t.Fatalf("permission.asked event: %v", err)
	}
	reply := client.permissionReply(client.permissionReplyCount() - 1)
	if reply.route != opencode.PermissionRouteSession || reply.requestID != "p-session" || reply.reply != "once" {
		t.Fatalf("permission.asked reply = %#v", reply)
	}
	permissionReq := conn.permissions[len(conn.permissions)-1]
	if permissionReq.ToolCall.Title == nil || *permissionReq.ToolCall.Title != "edit" {
		t.Fatalf("permission.asked ACP request = %#v", permissionReq)
	}
	rawInput, _ := permissionReq.ToolCall.RawInput.(map[string]any)
	resources, _ := rawInput["resources"].([]string)
	if len(resources) != 1 || resources[0] != "acp-permission-probe.txt" {
		t.Fatalf("permission.asked resources = %#v", permissionReq.ToolCall.RawInput)
	}
	if err := session.handleEvent(ctx, opencode.Event{
		Type:       "question.v2.asked",
		Properties: json.RawMessage(`{"id":"q-v2","sessionID":"native-1","questions":[{"question":"Continue?"}]}`),
	}); err != nil {
		t.Fatalf("question.v2.asked event: %v", err)
	}
	questionReply := client.questionReply(client.questionReplyCount() - 1)
	if questionReply.route != opencode.QuestionRouteAPI || questionReply.requestID != "q-v2" {
		t.Fatalf("question.v2 reply = %#v", questionReply)
	}
	assertEventEdgeAndHelperBranches(t, ctx, session)
}

func assertEventEdgeAndHelperBranches(t *testing.T, ctx context.Context, session *session) {
	t.Helper()
	if err := session.handleEvent(ctx, opencode.Event{Type: "todo.updated", Properties: json.RawMessage(`{"sessionID":"other","todos":[{"content":"x"}]}`)}); err != nil {
		t.Fatalf("foreign todo event: %v", err)
	}
	if err := session.handleEvent(ctx, opencode.Event{Type: "question.asked", Properties: json.RawMessage(`{"request":{"id":"q","sessionID":"other"}}`)}); err != nil {
		t.Fatalf("foreign question event: %v", err)
	}
	if part, ok := eventPart(json.RawMessage(`{"part":{"type":"text","text":"x"}}`)); !ok || part.Text != "x" {
		t.Fatalf("wrapped eventPart = %#v ok=%v", part, ok)
	}
	if _, ok := eventPart(json.RawMessage(`{`)); ok {
		t.Fatal("malformed eventPart succeeded")
	}
	part, delta, ok := eventPartUpdate(json.RawMessage(
		`{"part":{"id":"part-delta","type":"text","text":"hello"},"delta":"lo"}`,
	))
	if !ok || part.ID != "part-delta" || delta != "lo" {
		t.Fatalf("eventPartUpdate = %#v delta=%q ok=%v", part, delta, ok)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"question":{"id":"q1","sessionID":"s"}}`),
		json.RawMessage(`{"data":{"id":"q2","sessionID":"s"}}`),
		json.RawMessage(`{`),
	} {
		eventQuestion(raw)
	}
	if err := session.emitPlan(ctx, []opencode.NativeTodo{{Content: ""}}); err != nil {
		t.Fatalf("empty plan: %v", err)
	}
	rawSession := testSession(NewAgent(), newFakeOpenCodeClient())
	rawSession.rawMessages = rawMessageConfig{enabled: true}
	if err := rawSession.emitRawOpenCodeEvent(ctx, opencode.Event{Raw: json.RawMessage(`{"type":"x"}`)}); err != nil {
		t.Fatalf("raw event without conn: %v", err)
	}
	if usageFromTokens(opencode.NativeTokens{}) != nil {
		t.Fatal("empty tokens produced usage")
	}
	var emptyResource acp.EmbeddedResourceResource
	if _, err := embeddedResourceOpenCodePart(emptyResource); err == nil {
		t.Fatal("empty embedded resource accepted")
	}
	if updates := committedPartUpdates(rawSession, "assistant", opencode.NativePart{Type: "text"}, ""); updates != nil {
		t.Fatalf("empty text updates = %#v", updates)
	}
	if updates := committedPartUpdates(rawSession, "assistant", opencode.NativePart{Type: partTypeReasoning}, ""); updates != nil {
		t.Fatalf("empty reasoning updates = %#v", updates)
	}
}

func TestPromptRemainingErrorBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("send error after cancelled state", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			session.mu.Lock()
			session.cancelled = true
			session.mu.Unlock()

			return opencode.NativeMessage{}, errors.New("cancelled send")
		}
		resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil || resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancelled send resp=%#v err=%v", resp, err)
		}
	})

	t.Run("successful result marked cancelled", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			session.mu.Lock()
			session.cancelled = true
			session.mu.Unlock()

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: "native-1", Role: "assistant", Finish: "stop"}}, nil
		}
		resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil || resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancelled success resp=%#v err=%v", resp, err)
		}
	})

	t.Run("replay and emit update errors", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.updateErr = errors.New("update failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		client.messages = []opencode.NativeMessage{{
			Info: opencode.NativeMessageInfo{ID: "user", SessionID: "native-1", Role: "user"},
			Parts: []opencode.NativePart{{
				ID:        "user-part",
				SessionID: "native-1",
				MessageID: "user",
				Type:      "text",
				Text:      "hello",
				Raw:       json.RawMessage(`{"id":"user-part"}`),
			}},
		}}
		if err := session.replayMessages(ctx); err == nil {
			t.Fatal("replayMessages ignored emit error")
		}
		client = newFakeOpenCodeClient()
		conn = newRecordingAgentClient()
		conn.updateErr = errors.New("step update failed")
		agent = NewAgent()
		agent.setAgentClient(conn)
		session = testSession(agent, client)
		if err := session.emitMessage(ctx, opencode.NativeMessage{
			Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: "native-1", Role: "assistant"},
			Parts: []opencode.NativePart{{
				ID:        "usage",
				SessionID: "native-1",
				MessageID: "assistant",
				Type:      "step-finish",
				Tokens:    opencode.NativeTokens{Total: 1},
				Raw:       json.RawMessage(`{"id":"usage"}`),
			}},
		}, false); err == nil {
			t.Fatal("emitMessage ignored step-finish update error")
		}
	})

	t.Run("duplicate part and raw notify error", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		part := opencode.NativePart{ID: "dup", SessionID: "native-1", MessageID: "assistant", Type: "text", Text: "hello"}
		if err := session.emitMessage(ctx, opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", Role: "assistant"}, Parts: []opencode.NativePart{part, part}}, false); err != nil {
			t.Fatalf("duplicate emitMessage: %v", err)
		}
		// A raw-event emit failure is non-authoritative debug output and must
		// not abort the turn: handleEvent records it internally and continues.
		conn.notifyErr = errors.New("notify failed")
		session.rawMessages = rawMessageConfig{enabled: true}
		if err := session.handleEvent(ctx, opencode.Event{Type: "unknown", Raw: json.RawMessage(`{"type":"unknown"}`)}); err != nil {
			t.Fatalf("raw notify error aborted the turn: %v", err)
		}
	})

	t.Run("same-session events and reconcile errors", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		conn.permErr = errors.New("permission failed")
		if err := session.handleEvent(ctx, opencode.Event{
			Type:       "permission.v2.asked",
			Properties: json.RawMessage(`{"id":"p","sessionID":"native-1"}`),
		}); err == nil {
			t.Fatal("permission event ignored client error")
		}
		client.pendingPermissions = []opencode.PermissionRequest{{ID: "p2", SessionID: "native-1"}}
		if err := session.reconcilePermissions(ctx); err == nil {
			t.Fatal("reconcilePermissions ignored handle error")
		}
		conn.permErr = nil

		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		conn.elicitErr = errors.New("elicitation failed")
		if err := session.handleEvent(ctx, opencode.Event{
			Type:       "question.asked",
			Properties: json.RawMessage(`{"id":"q","sessionID":"native-1"}`),
		}); err == nil {
			t.Fatal("question event ignored client error")
		}
		client.pendingQuestions = []opencode.QuestionRequest{{ID: "q2", SessionID: "native-1"}}
		if err := session.reconcileQuestions(ctx); err == nil {
			t.Fatal("reconcileQuestions ignored handle error")
		}
		if req, ok := eventQuestion(json.RawMessage(`{"id":"direct","sessionID":"native-1"}`)); !ok || req.ID != "direct" {
			t.Fatalf("direct eventQuestion = %#v ok=%v", req, ok)
		}
	})
}

func TestLifecycleMCPRefreshRemainingRuntimeBranches(t *testing.T) {
	t.Run("missing client", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		session.mcpRefreshPending = true
		session.client = nil

		err := session.refreshLifecycleMCP(context.Background())
		require.ErrorContains(t, err, "no runtime client")
		require.True(t, session.mcpRefreshPending)
	})

	t.Run("runtime changes during refresh", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		session.mcpRefreshPending = true
		session.mcpServers = []opencode.MCPServerConfig{{Name: "wagie", URL: "https://mcp.test"}}
		client.refreshMCPFunc = func(context.Context, []opencode.MCPServerConfig) error {
			session.mu.Lock()
			session.runtimeLostCause = "runtime exited"
			session.mu.Unlock()

			return nil
		}

		err := session.refreshLifecycleMCP(context.Background())
		require.ErrorContains(t, err, "runtime changed during MCP refresh")
		require.True(t, session.mcpRefreshPending)
	})
}

func eventFromJSON(t *testing.T, raw string) opencode.Event {
	t.Helper()
	var event opencode.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}

	return event
}

func TestPromptStructuredOutput(t *testing.T) {
	ctx := context.Background()

	t.Run("schema forwarded and result surfaced", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		session.outputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string"}},
		}
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if req.Format == nil || req.Format.Type != opencode.OutputFormatJSONSchema || req.Format.Schema["type"] != "object" {
				t.Fatalf("format = %#v", req.Format)
			}

			msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{
				ID:         "assistant-1",
				SessionID:  id,
				Role:       "assistant",
				Finish:     "stop",
				Structured: json.RawMessage(`{"answer":"hi"}`),
			}}
			msg.Parts = []opencode.NativePart{{SessionID: id, MessageID: "assistant-1", Type: "text", Text: `{"answer":"hi"}`}}

			return msg, nil
		}
		resp, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Meta: routeCarrier("nonce"), Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		opencodeMeta, _ := resp.Meta[opencodeMetaKey].(map[string]any)
		structured, _ := opencodeMeta[structuredOutputMetaKey].(map[string]any)
		if structured["answer"] != "hi" {
			t.Fatalf("structured output meta = %#v", resp.Meta)
		}
	})

	t.Run("no schema sends no format and no meta", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		client.sendMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if req.Format != nil {
				t.Fatalf("format unexpectedly set: %#v", req.Format)
			}

			msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop"}}
			msg.Parts = []opencode.NativePart{{SessionID: id, MessageID: "assistant-1", Type: "text", Text: "hi"}}

			return msg, nil
		}
		resp, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Meta: routeCarrier("nonce"), Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if resp.Meta != nil {
			t.Fatalf("meta unexpectedly set: %#v", resp.Meta)
		}
	})

	t.Run("schema with invalid structured payload omits meta", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		session.outputSchema = map[string]any{"type": "object"}
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{
				ID:         "assistant-1",
				SessionID:  id,
				Role:       "assistant",
				Finish:     "stop",
				Structured: json.RawMessage(`not-json`),
			}}
			msg.Parts = []opencode.NativePart{{SessionID: id, MessageID: "assistant-1", Type: "text", Text: "hi"}}

			return msg, nil
		}
		resp, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Meta: routeCarrier("nonce"), Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil {
			t.Fatalf("Prompt: %v", err)
		}
		if resp.Meta != nil {
			t.Fatalf("meta unexpectedly set: %#v", resp.Meta)
		}
	})
}

func TestPromptAssistantErrorStructured(t *testing.T) {
	ctx := context.Background()

	const (
		providerDetail = "Error from provider (Console): Upstream request failed"
		responseBody   = `{"error":{"message":"Error from provider (Console): Upstream request failed","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`
	)
	apiError := func() *opencode.NativeError {
		e := &opencode.NativeError{Name: "APIError"}
		e.Data.Message = providerDetail
		e.Data.StatusCode = 400
		e.Data.ResponseBody = responseBody

		return e
	}

	for _, tt := range []struct {
		name       string
		withSchema bool
	}{
		{name: "schema requested surfaces structuredOutputRequested true", withSchema: true},
		{name: "no schema surfaces structuredOutputRequested false", withSchema: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			session := testSession(NewAgent(), client)
			if tt.withSchema {
				session.outputSchema = map[string]any{"type": "object"}
			}
			client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
				msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{
					ID:        "assistant-1",
					SessionID: id,
					Role:      "assistant",
					Finish:    "error",
					Error:     apiError(),
				}}
				// Route through the real translation so the test exercises the
				// full native-error decode and typed-error path.
				return opencode.NativeMessage{}, opencode.AssistantMessageError(msg)
			}

			_, err := session.Prompt(ctx, acp.PromptRequest{
				SessionId: session.id,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
			})
			data := assertTurnFailed(t, err, causeProvider, providerDetail)
			if data["structuredOutputRequested"] != tt.withSchema {
				t.Fatalf("structuredOutputRequested = %#v, want %v", data["structuredOutputRequested"], tt.withSchema)
			}
			if data["statusCode"] != 400 {
				t.Fatalf("statusCode = %#v", data["statusCode"])
			}
			if data["providerCode"] != "invalid_request_error" {
				t.Fatalf("providerCode = %#v", data["providerCode"])
			}
		})
	}

	t.Run("Error string carries opencode assistant error prefix", func(t *testing.T) {
		err := opencode.AssistantMessageError(opencode.NativeMessage{Info: opencode.NativeMessageInfo{
			Role:   "assistant",
			Finish: "error",
			Error:  apiError(),
		}})
		if err == nil || !strings.HasPrefix(err.Error(), "opencode assistant error:") {
			t.Fatalf("Error() = %v", err)
		}
	})
}

func TestPromptMappingHelpers(t *testing.T) {
	var resource acp.EmbeddedResourceResource
	if err := json.Unmarshal([]byte(`{"uri":"file:///tmp/a","text":"body"}`), &resource); err != nil {
		t.Fatal(err)
	}
	part, err := embeddedResourceOpenCodePart(resource)
	if err != nil || part[partTypeText] != "body" {
		t.Fatalf("embeddedResourceOpenCodePart = %#v, %v", part, err)
	}
	if update := usageUpdateFromTokens("m", opencode.NativeTokens{}, 0); update != nil {
		t.Fatalf("empty usage update = %#v", update)
	}
	usage := usageFromTokens(opencode.NativeTokens{Input: 1, Output: 2, Reasoning: 3})
	if usage == nil || usage.TotalTokens != 6 {
		t.Fatalf("usage = %#v", usage)
	}
	for _, reason := range []string{"length", "cancelled", "refusal", "stop"} {
		if stopReasonFromOpenCode(reason) == "" {
			t.Fatalf("empty stop reason for %q", reason)
		}
	}
	for _, status := range []string{"pending", "completed", "failed", "other"} {
		if toolStatus(status) == "" {
			t.Fatalf("empty tool status for %q", status)
		}
	}
	for _, tool := range []string{"read", "edit", "delete", "move", "grep", "bash", "fetch", "think", "other"} {
		if toolKind(tool) == "" {
			t.Fatalf("empty tool kind for %q", tool)
		}
	}
	for _, priority := range []string{"high", "low", "medium"} {
		if planPriority(priority) == "" {
			t.Fatalf("empty plan priority for %q", priority)
		}
	}
	for _, status := range []string{"completed", "in_progress", "pending"} {
		if planStatus(status) == "" {
			t.Fatalf("empty plan status for %q", status)
		}
	}
	if questionElicitationMessage([]opencode.QuestionInfo{{Question: "Only?"}}) != "Only?" {
		t.Fatal("single question message mismatch")
	}
	if got := questionOptionSchemas([]opencode.QuestionOption{{Label: ""}, {Label: "A"}}); len(got) != 1 {
		t.Fatalf("questionOptionSchemas = %#v", got)
	}
	if req, ok := eventQuestion(json.RawMessage(`{"request":{"id":"q","sessionID":"s"}}`)); !ok || req.ID != "q" {
		t.Fatalf("eventQuestion wrapper = %#v ok=%v", req, ok)
	}
	if _, ok := eventQuestion(json.RawMessage(`{}`)); ok {
		t.Fatal("empty event question parsed")
	}
}

// providerNativeError builds a native assistant provider error with the given
// detail, HTTP status, and provider code.
func providerNativeError(detail string, status int, code string) *opencode.NativeError {
	e := &opencode.NativeError{Name: "APIError"}
	e.Data.Message = detail
	e.Data.StatusCode = status
	e.Data.ResponseBody = `{"error":{"code":"` + code + `"}}`

	return e
}

func TestTurnTimeoutWithoutAgent(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	session.agent = nil
	if session.turnTimeout() != 0 {
		t.Fatal("nil agent turn timeout != 0")
	}
}

// A panic in the native turn goroutine is recovered, logged, and surfaced as a
// turn failure instead of crashing the agent or hanging the prompt.
func TestPromptNativeTurnPanicIsRecovered(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.sendMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		panic("native turn exploded")
	}
	session := testSession(NewAgent(), client)

	resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	if err == nil {
		t.Fatalf("panicking native turn returned no error: %#v", resp)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panic error = %v", err)
	}
}

// T1 — a native provider error terminates the turn with the uniform
// turn-failure JSON-RPC error (cause "provider"), never a PromptResponse and
// never StopReason end_turn.
func TestPromptProviderErrorSurfacesTurnFailed(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name   string
		detail string
		status int
		code   string
	}{
		{name: "auth", detail: "invalid api key", status: 401, code: "invalid_api_key"},
		{name: "rate limit", detail: "slow down", status: 429, code: "rate_limit_exceeded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
				msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{
					ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "error",
					Error: providerNativeError(tt.detail, tt.status, tt.code),
				}}

				return opencode.NativeMessage{}, opencode.AssistantMessageError(msg)
			}
			session := testSession(NewAgent(), client)

			resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
			if resp.StopReason != "" {
				t.Fatalf("StopReason = %q, want empty (no PromptResponse)", resp.StopReason)
			}
			data := assertTurnFailed(t, err, causeProvider, tt.detail)
			if data["statusCode"] != tt.status {
				t.Fatalf("statusCode = %#v, want %d", data["statusCode"], tt.status)
			}
			if data["providerCode"] != tt.code {
				t.Fatalf("providerCode = %#v, want %q", data["providerCode"], tt.code)
			}
		})
	}
}

// T3 — a transport failure from the native POST surfaces the uniform
// turn-failure error (cause "transport") with the real cause text, and leaves
// the session addressable and retriable (not poisoned, not unknown-session).
func TestPromptTransportErrorIsRetriable(t *testing.T) {
	ctx := context.Background()

	client := newFakeOpenCodeClient()
	client.sendMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{}, io.ErrUnexpectedEOF
	}
	session := testSession(NewAgent(), client)

	_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	assertTurnFailed(t, err, causeTransport, io.ErrUnexpectedEOF.Error())

	if poisonErr := session.ensureNotPoisoned(); poisonErr != nil {
		t.Fatalf("session poisoned after transport failure: %v", poisonErr)
	}

	// The next turn re-drives successfully: the session stayed addressable.
	client.sendMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-2", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
	}
	resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("again")}})
	if err != nil {
		t.Fatalf("retry Prompt after transport failure: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("retry StopReason = %q, want end_turn", resp.StopReason)
	}
}

// T4 — a mid-turn native stream error is surfaced as a structured transport
// failure (never misclassified) and does not poison the session.
func TestPromptStreamErrorIsStructuredTransportFailure(t *testing.T) {
	client := newFakeOpenCodeClient()
	started := make(chan struct{})
	client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}
	session := testSession(NewAgent(), client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start native send")
	}
	client.errs <- errors.New("connection reset by peer")
	select {
	case err := <-done:
		assertTurnFailed(t, err, causeTransport, "connection reset by peer")
	case <-ctx.Done():
		t.Fatal("Prompt did not return")
	}
	if poisonErr := session.ensureNotPoisoned(); poisonErr != nil {
		t.Fatalf("session poisoned after stream error: %v", poisonErr)
	}
}

// T5 — a native error observed while the turn is cancelled maps to StopReason
// cancelled with a nil error: the cancel guard runs before failure mapping.
func TestPromptCancelSuppressesNativeError(t *testing.T) {
	client := newFakeOpenCodeClient()
	started := make(chan struct{})
	client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, errors.New("provider error raised during cancel")
	}
	agent := NewAgent()
	session := testSession(agent, client)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct {
		resp acp.PromptResponse
		err  error
	}, 1)
	go func() {
		resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
		done <- struct {
			resp acp.PromptResponse
			err  error
		}{resp, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start")
	}
	if err := agent.Cancel(ctx, CancelRequest(session.id, "unit-test-turn")); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("cancelled turn returned error: %v", got.err)
		}
		if got.resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("StopReason = %q, want cancelled", got.resp.StopReason)
		}
	case <-ctx.Done():
		t.Fatal("Prompt did not return after cancel")
	}
	if !client.closed {
		t.Fatal("cancel did not retire the shared runtime")
	}
}

// T6 — with WithTurnTimeout set, a hanging native turn fails with cause
// "timeout" (not cancelled) after the exact shared runtime is retired.
func TestPromptTurnTimeoutFailsWithTimeoutCause(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.sendMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}
	agent := NewAgent(WithTurnTimeout(20 * time.Millisecond))
	session := testSession(agent, client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	if resp.StopReason != "" {
		t.Fatalf("StopReason = %q, want empty (no PromptResponse)", resp.StopReason)
	}
	assertTurnFailed(t, err, causeTimeout, "deadline")
	if client.abortCount() != 1 {
		t.Fatalf("abort count = %d, want 1 (timeout aborts the native turn)", client.abortCount())
	}
	if !client.closed {
		t.Fatal("timeout did not retire the shared runtime")
	}
}

// T7 — when a user cancel and the WithTurnTimeout deadline coincide, the cancel
// guard wins deterministically: the turn resolves to StopReason cancelled with a
// nil error (never cause "timeout"), and the native turn is aborted exactly once
// (no double-send). This pins the timeout branch's cancel re-check.
//
// The coincidence is reproduced deterministically by marking the cancel pending
// (the flag cancelTurn sets under the session lock) without yet cancelling
// turnCtx, so the fired deadline is the only ready select case: the timeout
// branch must observe the pending cancel and yield cancelled rather than a
// timeout failure.
func TestPromptCancelWinsCoincidentTimeout(t *testing.T) {
	client := newFakeOpenCodeClient()
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	client.sendMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-release

		return opencode.NativeMessage{}, errors.New("native error raised at coincident cancel+timeout")
	}
	agent := NewAgent(WithTurnTimeout(15 * time.Millisecond))
	session := testSession(agent, client)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct {
		resp acp.PromptResponse
		err  error
	}, 1)
	go func() {
		resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
		done <- struct {
			resp acp.PromptResponse
			err  error
		}{resp, err}
	}()

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("Prompt did not start")
	}

	// Cancel is pending (flag set under lock, as cancelTurn does) at the instant
	// the deadline fires; turnCtx stays live so the timeout branch is the case
	// that must honor the cancel guard.
	session.mu.Lock()
	session.cancelled = true
	session.mu.Unlock()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("coincident cancel+timeout returned error: %v", got.err)
		}
		if got.resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("StopReason = %q, want cancelled (cancel wins over coincident timeout)", got.resp.StopReason)
		}
	case <-ctx.Done():
		t.Fatal("Prompt did not return after coincident cancel+timeout")
	}

	if client.abortCount() != 1 {
		t.Fatalf("abort count = %d, want 1 (single abort, no double-send)", client.abortCount())
	}
}

// T8 — a double transport failure (blocking POST severed mid-body AND recovery
// re-fetch failed) surfaces as the uniform turn-failure error (cause
// "transport") whose message names both failures with context, never a bare
// stream error such as "unexpected EOF". The native client wraps the two
// failures (see TestOpenCodeBlockingPostDoubleFailureNamesBoth); this pins that
// the wrapped cause flows verbatim into the ACP error data.message.
func TestPromptDoubleTransportFailureNamesBoth(t *testing.T) {
	ctx := context.Background()

	client := newFakeOpenCodeClient()
	client.sendMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{}, fmt.Errorf(
			"opencode message POST: %w; message re-fetch failed: %v",
			io.ErrUnexpectedEOF,
			errors.New("opencode GET /session/native-1/message returned 502 Bad Gateway: gateway is down"),
		)
	}
	session := testSession(NewAgent(), client)

	_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	data := assertTurnFailed(t, err, causeTransport, "unexpected EOF")

	message, _ := data[jsonFieldMessage].(string)
	for _, want := range []string{"opencode message POST", "unexpected EOF", "message re-fetch failed", "502"} {
		if !strings.Contains(message, want) {
			t.Fatalf("data.message = %q, want substring %q (must name both failures)", message, want)
		}
	}
}
func promptCoverageParams(id acp.SessionId) acp.PromptRequest {
	return acp.PromptRequest{SessionId: id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}
}

func TestPromptCancelAndLoadRejectRemainingRouteAndMCPBranches(t *testing.T) {
	agent := NewAgent()
	_, err := agent.Prompt(context.Background(), acp.PromptRequest{})
	require.Error(t, err)

	client := newFakeOpenCodeClient()
	current := testSession(agent, client)
	agent.sessions[current.id] = current
	require.Error(t, agent.Cancel(context.Background(), CancelRequest(current.id, "stale")))

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId: "session", Cwd: t.TempDir(),
		McpServers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "bad"}}},
	})
	require.Error(t, err)
}

func TestPromptBacklogQuestionFailsClosedBeforeNativeTurn(t *testing.T) {
	agent := NewAgent()
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	client := newFakeOpenCodeClient()
	current := testSession(agent, client)
	client.events <- opencode.Event{
		Type:       eventQuestionAsked,
		Properties: json.RawMessage(`{"id":"question","sessionID":"native-1","questions":[{"question":"Continue?"}]}`),
	}

	response, err := current.promptWithRoute(context.Background(), promptCoverageParams(current.id), "nonce")
	require.ErrorContains(t, err, "question callback arrived outside its originating turn")
	require.Empty(t, response.StopReason)
	require.Equal(t, 1, client.questionRejectCount())
	require.Empty(t, connection.elicitations)
}

func TestRunPromptTurnEveryCancellationFenceFailureReturn(t *testing.T) {
	t.Run("cancelled reconciliation", func(t *testing.T) {
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
		connection := newRecordingAgentClient()
		connection.elicitErr = errors.New("client stopped")
		agent.setAgentClient(connection)
		client := newFakeOpenCodeClient()
		client.closeErr = errors.New("containment failed")
		client.pendingQuestions = []opencode.QuestionRequest{{ID: "question", SessionID: "native-1"}}
		current := testSession(agent, client)
		turnCtx := current.beginTurn(context.Background(), "nonce")
		current.cancelled = true
		_, err := current.runPromptTurn(context.Background(), turnCtx, promptCoverageParams(current.id), func(context.Context) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{}, nil
		}, opencode.NativeCommand{}, false)
		require.ErrorContains(t, err, "opencode_cancellation_fence_failed")
	})

	t.Run("ordinary reconciliation", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.permissionsErr = errors.New("permissions failed")
		client.closeErr = errors.New("containment failed")
		current := testSession(NewAgent(), client)
		turnCtx := current.beginTurn(context.Background(), "nonce")
		_, err := current.runPromptTurn(context.Background(), turnCtx, promptCoverageParams(current.id), func(context.Context) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{}, nil
		}, opencode.NativeCommand{}, false)
		require.ErrorContains(t, err, "permissions failed")
		require.ErrorContains(t, err, "opencode_cancellation_fence_failed")
	})

	t.Run("stream error", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.closeErr = errors.New("containment failed")
		client.errs <- errors.New("stream failed")
		current := testSession(NewAgent(), client)
		turnCtx := current.beginTurn(context.Background(), "nonce")
		_, err := current.runPromptTurn(context.Background(), turnCtx, promptCoverageParams(current.id), func(ctx context.Context) (opencode.NativeMessage, error) {
			<-ctx.Done()

			return opencode.NativeMessage{}, ctx.Err()
		}, opencode.NativeCommand{}, false)
		require.ErrorContains(t, err, "opencode_cancellation_fence_failed")
	})

	t.Run("cancelled native result", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.closeErr = errors.New("containment failed")
		current := testSession(NewAgent(), client)
		turnCtx := current.beginTurn(context.Background(), "nonce")
		current.cancelled = true
		_, err := current.runPromptTurn(context.Background(), turnCtx, promptCoverageParams(current.id), func(context.Context) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{}, errors.New("native failed")
		}, opencode.NativeCommand{}, false)
		require.ErrorContains(t, err, "opencode_cancellation_fence_failed")
	})

	t.Run("timeout", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.closeErr = errors.New("containment failed")
		current := testSession(NewAgent(WithTurnTimeout(time.Millisecond)), client)
		turnCtx := current.beginTurn(context.Background(), "nonce")
		release := make(chan struct{})
		_, err := current.runPromptTurn(context.Background(), turnCtx, promptCoverageParams(current.id), func(context.Context) (opencode.NativeMessage, error) {
			<-release

			return opencode.NativeMessage{}, nil
		}, opencode.NativeCommand{}, false)
		close(release)
		require.ErrorContains(t, err, "opencode_cancellation_fence_failed")
	})

	t.Run("turn context", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.closeErr = errors.New("containment failed")
		current := testSession(NewAgent(), client)
		ctx, cancel := context.WithCancel(context.Background())
		turnCtx := current.beginTurn(ctx, "nonce")
		cancel()
		release := make(chan struct{})
		_, err := current.runPromptTurn(ctx, turnCtx, promptCoverageParams(current.id), func(context.Context) (opencode.NativeMessage, error) {
			<-release

			return opencode.NativeMessage{}, nil
		}, opencode.NativeCommand{}, false)
		close(release)
		require.ErrorContains(t, err, "opencode_cancellation_fence_failed")
	})
}

func TestPartTextDeltaEmptyNativeDeltaBranch(t *testing.T) {
	current := testSession(NewAgent(), newFakeOpenCodeClient())
	text, commit := current.partTextDelta(opencode.NativePart{ID: "part"}, "")
	require.Empty(t, text)
	require.Nil(t, commit)
}
