package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestQuestionToolElicitationAcceptDeclineAndNoCapability(t *testing.T) {
	ctx := context.Background()

	t.Run("accept replies to native question", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.questionReplied = make(chan struct{}, 1)
		conn := newRecordingAgentClient()
		conn.elicitation = acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Action: "accept", Content: map[string]any{
				"question_1": "Yes",
				"question_2": []any{"Red", "Blue"},
			}},
		}
		agent := negotiatedAgent(t)
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)
		session.markPublishedToolCall("call-1")
		turnCtx := session.beginTurn(ctx, "question-accept")
		defer session.finishTurn()

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
		if err := session.routeNativeQuestion(turnCtx, req); err != nil {
			t.Fatalf("routeNativeQuestion: %v", err)
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
		requireSignal(t, client.questionReplied)
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
		client.questionRejected = make(chan struct{}, 1)
		conn := newRecordingAgentClient()
		conn.elicitation = acp.NewUnstableCreateElicitationResponseDecline()
		agent := negotiatedAgent(t)
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)
		turnCtx := session.beginTurn(ctx, "question-decline")
		defer session.finishTurn()
		if err := session.routeNativeQuestion(turnCtx, opencode.QuestionRequest{ID: "q", SessionID: "native-1"}); err != nil {
			t.Fatalf("routeNativeQuestion: %v", err)
		}
		requireSignal(t, client.questionRejected)
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %d, want 1", client.questionRejectCount())
		}
	})

	t.Run("missing capability rejects without ACP request", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.questionRejected = make(chan struct{}, 1)
		conn := newRecordingAgentClient()
		agent := negotiatedAgent(t)
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)
		if err := session.routeNativeQuestion(ctx, opencode.QuestionRequest{ID: "q", SessionID: "native-1"}); err != nil {
			t.Fatalf("routeNativeQuestion: %v", err)
		}
		if len(conn.elicitations) != 0 {
			t.Fatalf("elicitation sent without capability: %#v", conn.elicitations)
		}
		requireSignal(t, client.questionRejected)
		if client.questionRejectCount() != 1 {
			t.Fatalf("question rejects = %d, want 1", client.questionRejectCount())
		}
	})
}

func TestPermissionRoutingReplies(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.permissionReplied = make(chan struct{}, 2)
	conn := newRecordingAgentClient()
	agent := negotiatedAgent(t)
	agent.setAgentClient(conn)
	session := testSession(t, agent, client)
	session.markPublishedToolCall("tool-current")

	require.NoError(t, session.routeNativePermission(ctx, opencode.PermissionRequest{
		ID:        "perm-1",
		SessionID: "native-1",
		Action:    "edit",
		Resources: []string{"file.txt"},
		Metadata:  map[string]any{"path": "file.txt"},
		Tool:      opencode.PermissionTool{CallID: "tool-current"},
	}))
	requireSignal(t, client.permissionReplied)

	reply := client.permissionReply(0)
	if reply.reply != "once" || reply.sessionID != "native-1" || reply.requestID != "perm-1" {
		t.Fatalf("permission reply = %#v", reply)
	}

	if len(conn.permissions) != 1 || conn.permissions[0].ToolCall.Title == nil || *conn.permissions[0].ToolCall.Title != "edit" {
		t.Fatalf("permission request = %#v", conn.permissions)
	}

	conn.permission = acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	require.NoError(t, session.routeNativePermission(ctx, opencode.PermissionRequest{
		ID: "perm-2", SessionID: "native-1", Tool: opencode.PermissionTool{CallID: "tool-current"},
	}))
	requireSignal(t, client.permissionReplied)
	require.Equal(t, "reject", client.permissionReply(1).reply)
}

func TestPermissionAndQuestionCallbacksFollowExactToolStartOnACPWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	agent, _, wireClient, peer := newWireCoverageConnection(t)
	_, err := peer.Initialize(ctx, acp.InitializeRequest{Meta: lifecycleOffer(), ClientCapabilities: acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
	}})
	require.NoError(t, err)

	native := newFakeOpenCodeClient()
	native.permissionReplied = make(chan struct{}, 1)
	native.questionRejected = make(chan struct{}, 2)
	session := testSession(t, agent, native)
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
		require.NoError(t, session.applyNativeEvent(turnCtx, opencode.Event{
			Type: opencode.EventMessagePartCreated, Properties: properties,
		}))
	}

	emitToolStart("permission-tool")
	require.NoError(t, session.applyNativeEvent(turnCtx, opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: json.RawMessage(
			`{"id":"permission-1","sessionID":"native-1","action":"edit","tool":{"callID":"permission-tool"}}`,
		),
	}))

	emitToolStart("question-tool")
	require.NoError(t, session.applyNativeEvent(turnCtx, opencode.Event{
		Type: opencode.EventQuestionV2Asked,
		Properties: json.RawMessage(
			`{"id":"question-1","sessionID":"native-1","tool":{"callID":"question-tool"},"questions":[{"question":"Proceed?"}]}`,
		),
	}))

	requireSignal(t, native.permissionReplied)
	requireSignal(t, native.questionRejected)
	wireClient.mu.Lock()
	actionOrder := make([]string, 0, 4)
	for _, entry := range wireClient.order {
		if strings.HasPrefix(entry, "tool_call:") || strings.HasPrefix(entry, "permission:") ||
			strings.HasPrefix(entry, "elicitation:") {
			actionOrder = append(actionOrder, entry)
		}
	}
	require.Equal(t, []string{
		"tool_call:permission-tool",
		"permission:permission-tool",
		"tool_call:question-tool",
		"elicitation:question-tool",
	}, actionOrder)
	wireClient.mu.Unlock()

	err = session.applyNativeEvent(turnCtx, opencode.Event{
		Type: opencode.EventQuestionV2Asked,
		Properties: json.RawMessage(
			`{"id":"question-stale","sessionID":"native-1","tool":{"callID":"not-published"},"questions":[{"question":"Proceed?"}]}`,
		),
	})
	require.ErrorContains(t, err, "does not target a tool call this session published")
	requireSignal(t, native.questionRejected)
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
	session := testSession(t, agent, client)
	session.rawMessages = rawMessageConfig{enabled: true}

	todoProps := json.RawMessage(`{"sessionID":"native-1","todos":[{"content":"Ship it","status":"in_progress","priority":"high"}]}`)
	if err := session.applyNativeEvent(ctx, eventFromJSON(t, `{"type":"todo.updated","properties":`+string(todoProps)+`}`)); err != nil {
		t.Fatalf("todo event: %v", err)
	}
	textProps := json.RawMessage(`{"id":"part-1","sessionID":"native-1","messageID":"message-1","type":"text","text":"hello"}`)
	session.routeNativeEvent(ctx, opencode.Event{Type: "message.part.created", Properties: textProps, Raw: json.RawMessage(`{"type":"message.part.created"}`)})
	if err := session.applyNativeEvent(ctx, opencode.Event{Type: "message.part.created", Properties: textProps}); err != nil {
		t.Fatalf("duplicate text event: %v", err)
	}
	reasoningProps := json.RawMessage(`{"part":{"id":"part-2","sessionID":"native-1","messageID":"message-1","type":"reasoning","text":"thinking"}}`)
	if err := session.applyNativeEvent(ctx, opencode.Event{Type: "message.part.updated", Properties: reasoningProps}); err != nil {
		t.Fatalf("reasoning event: %v", err)
	}
	toolProps := json.RawMessage(`{"id":"part-3","sessionID":"native-1","messageID":"message-1","type":"tool","tool":"bash","callID":"call-1","state":{"status":"completed","title":"Run"}}`)
	if err := session.applyNativeEvent(ctx, opencode.Event{Type: "message.part.created", Properties: toolProps}); err != nil {
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
	flushRawEvents(t, session)

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
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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
	if !reflect.DeepEqual(update.RawOutput, map[string]any{"error": "native tool failed"}) {
		t.Fatalf("failed tool raw output = %#v", update.RawOutput)
	}

	replaySession := testSession(t, NewAgent(), newFakeOpenCodeClient())
	replayed := committedPartUpdates(replaySession, "assistant", part, "")
	if len(replayed) != 1 || replayed[0].ToolCall == nil {
		t.Fatalf("replayed failed tool = %#v, want completed start", replayed)
	}
	if replayed[0].ToolCall.Status != acp.ToolCallStatusFailed {
		t.Fatalf("replayed failed status = %q", replayed[0].ToolCall.Status)
	}
	if !reflect.DeepEqual(replayed[0].ToolCall.RawOutput, map[string]any{"error": "native tool failed"}) {
		t.Fatalf("replayed failed raw output = %#v", replayed[0].ToolCall.RawOutput)
	}
}

func TestUpdateReconciliationDoesNotCommitBeforeFailClosedDelivery(t *testing.T) {
	ctx := context.Background()
	conn := newRecordingAgentClient()
	conn.updateErr = errors.New("delivery failed")
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(t, agent, newFakeOpenCodeClient())

	textPart := opencode.NativePart{
		ID:        "text-retry",
		MessageID: "message-retry",
		Type:      partTypeText,
		Text:      "delivered once",
	}
	if err := session.emitPartUpdates(ctx, "assistant", textPart, "", false); err == nil {
		t.Fatal("text delivery unexpectedly succeeded")
	}
	if _, ok := session.emittedPartText[textPart.ID]; ok {
		t.Fatal("failed text delivery committed reconciliation state")
	}
	client, ok := session.currentClient().(*fakeOpenCodeClient)
	require.True(t, ok)
	requireSignal(t, client.closeSignal)
	agent.mu.Lock()
	require.Nil(t, agent.runtime, "authoritative delivery failure did not contain the incarnation")
	agent.mu.Unlock()
}

func TestUpdateReconciliationEdgeBranches(t *testing.T) {
	ctx := context.Background()
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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

	session.stopPump()
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
	updates, commit, err := session.partUpdates(context.Background(), role, part, nativeDelta, false)
	if err != nil {
		panic(err)
	}

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
	session := testSession(t, agent, client)

	// The native stream declares the user message (wrapped payload) before its
	// part events; the prompt echo must not stream back as an agent chunk.
	if err := session.applyNativeEvent(ctx, opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: json.RawMessage(`{"info":{"id":"user-1","sessionID":"native-1","role":"user"}}`),
	}); err != nil {
		t.Fatalf("message.updated event: %v", err)
	}
	if err := session.applyNativeEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pu-1","sessionID":"native-1","messageID":"user-1","type":"text","text":"the prompt"}}`),
	}); err != nil {
		t.Fatalf("user part event: %v", err)
	}
	if conn.updateCount() != 0 {
		t.Fatalf("user prompt echoed: %#v", conn.updates)
	}

	// Bare (unwrapped) message.updated payloads are also recognized.
	if err := session.applyNativeEvent(ctx, opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: json.RawMessage(`{"id":"user-2","sessionID":"native-1","role":"user"}`),
	}); err != nil {
		t.Fatalf("bare message.updated event: %v", err)
	}
	if err := session.applyNativeEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pu-2","sessionID":"native-1","messageID":"user-2","type":"text","text":"another prompt"}}`),
	}); err != nil {
		t.Fatalf("second user part event: %v", err)
	}
	if conn.updateCount() != 0 {
		t.Fatalf("bare-role user prompt echoed: %#v", conn.updates)
	}

	// Assistant parts (role declared or unknown) still stream.
	if err := session.applyNativeEvent(ctx, opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: json.RawMessage(`{"info":{"id":"asst-1","sessionID":"native-1","role":"assistant"}}`),
	}); err != nil {
		t.Fatalf("assistant message.updated event: %v", err)
	}
	if err := session.applyNativeEvent(ctx, opencode.Event{
		Type:       "message.part.updated",
		Properties: json.RawMessage(`{"part":{"id":"pa-1","sessionID":"native-1","messageID":"asst-1","type":"text","text":"reply"}}`),
	}); err != nil {
		t.Fatalf("assistant part event: %v", err)
	}
	if err := session.applyNativeEvent(ctx, opencode.Event{
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
		if err := session.applyNativeEvent(ctx, opencode.Event{
			Type:       opencode.EventMessageUpdated,
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

// TestStreamTerminalFencesTheExactGenerationAndOpenTurn proves a native gap is
// terminal for the generation that produced it. The open turn reports the loss,
// the shared producer is removed from admission, and a late delivery carrying
// that exact old binding cannot reach the host.
func TestStreamTerminalFencesTheExactGenerationAndOpenTurn(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	current.agent.mu.Lock()
	current.agent.runtime = client
	current.agent.runtimeGeneration = 1
	current.agent.mu.Unlock()
	current.mu.Lock()
	current.runtimeGeneration = 1
	current.mu.Unlock()

	started := make(chan struct{})
	client.hangsAfterDispatch(started)

	done := make(chan error, 1)

	go func() {
		_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
		done <- err
	}()

	<-started
	client.publishEvent(opencode.Event{
		Type:       opencode.EventMessagePartCreated,
		Properties: json.RawMessage(`{"id":"stream-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
	})
	requireEventually(t, func() bool { return connection.updateCount() > 1 }, "the streamed part was not emitted")

	client.publishStreamTerminal(errors.New("stream failed"))
	assertTurnFailed(t, <-done, causeTransport, "")
	requireEventually(t, func() bool {
		current.agent.mu.Lock()
		defer current.agent.mu.Unlock()

		return current.agent.runtime == nil
	}, "the failed generation remained admissible")

	before := connection.updateCount()
	require.NoError(t, current.routeNativeEventForIncarnation(context.Background(), testIncarnation(current), opencode.Event{
		Type:       opencode.EventMessagePartCreated,
		Properties: json.RawMessage(`{"id":"late-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"late"}`),
	}))
	require.Equal(t, before, connection.updateCount(), "a late event from the failed epoch was emitted")
}

// TestIdleStreamTerminalRejectsLaterAdmission proves a gap between prompts is
// still terminal. With no durable generation to restore, the next prompt is
// refused instead of borrowing a replacement route on the broken incarnation.
func TestIdleStreamTerminalRejectsLaterAdmission(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	current.agent.mu.Lock()
	current.agent.runtime = client
	current.agent.runtimeGeneration = 1
	current.agent.mu.Unlock()
	current.mu.Lock()
	current.runtimeGeneration = 1
	current.mu.Unlock()

	client.publishStreamTerminal(errors.New("idle stream closed"))
	requireEventually(t, func() bool { return current.runtimeFailure() != nil }, "the idle stream gap was not latched")

	_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
	assertTurnFailed(t, err, causeTransport, "")
	require.NotContains(t, err.Error(), "idle stream closed")
}

// TestActionRequestFailureReportsFailedAndAnswersOpenCode proves an error path
// terminalizes: a host request that fails reports the action failed, answers
// OpenCode so the harness is not left blocked, and releases the foreground.
func TestActionRequestFailureReportsFailedAndAnswersOpenCode(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	connection.mu.Lock()
	connection.permErr = errors.New("permission failed")
	connection.mu.Unlock()

	current.markPublishedToolCall("tool-current")
	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "perm", "sessionID": current.idmap.NativeSessionID, "action": "edit",
			"tool": map[string]any{"callID": "tool-current"},
		}),
	})

	requireEventually(t, func() bool { return client.permissionReplyCount() == 1 }, "OpenCode was left blocked")
	require.Equal(t, permissionReplyReject, client.permissionReply(0).reply)

	requireEventually(t, func() bool {
		actions := connection.lifecycleEventsOfType(t, "action_update")
		if len(actions) != 2 {
			return false
		}

		terminal, _ := actions[1]["action"].(map[string]any)

		return terminal["state"] == "failed"
	}, "the failed action never terminalized")

	requireEventually(t, func() bool {
		transitions := connection.lifecycleEventsOfType(t, "state_update")

		return transitions[len(transitions)-1]["state"] == "running"
	}, "the foreground stayed blocked")

	requireLifecycleReduces(t, connection)
}

// TestNativeReplyFailureReportsAFailedAction proves the native acknowledgement is
// load-bearing: an action whose reply OpenCode refused is reported failed rather
// than accepted.
func TestNativeReplyFailureReportsAFailedAction(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	client.replyErr = errors.New("native reply refused")
	current.markPublishedToolCall("tool-current")

	client.publishEvent(opencode.Event{
		Type: opencode.EventPermissionV2Asked,
		Properties: mustJSONValue(map[string]any{
			"id": "perm", "sessionID": current.idmap.NativeSessionID, "action": "edit",
			"tool": map[string]any{"callID": "tool-current"},
		}),
	})

	requireEventually(t, func() bool {
		actions := connection.lifecycleEventsOfType(t, "action_update")
		if len(actions) != 2 {
			return false
		}

		terminal, _ := actions[1]["action"].(map[string]any)

		return terminal["state"] == "failed"
	}, "a refused native reply was reported as an answer")

	requireLifecycleReduces(t, connection)
}

// TestQuestionWithoutFormSupportIsRejectedNatively proves a host with no form
// elicitation capability declines the question and OpenCode is answered.
func TestQuestionWithoutFormSupportIsRejectedNatively(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.questionRejected = make(chan struct{}, 1)
	session := testSession(t, negotiatedAgent(t), client)

	require.NoError(t, session.routeNativeQuestion(context.Background(), opencode.QuestionRequest{
		ID: "question", SessionID: "native-1",
	}))
	requireSignal(t, client.questionRejected)
	require.Equal(t, 1, client.questionRejectCount())
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
	}, nil)
	if err != nil {
		t.Fatalf("promptToOpenCodeParts: %v", err)
	}
	if len(parts) != 6 || parts[0]["text"] != "hello" || parts[1]["text"] != "file:///tmp/a" ||
		parts[2]["text"] != "embedded" || parts[3]["text"] != "file:///tmp/blob" ||
		parts[4]["type"] != "file" || parts[4]["mime"] != "image/png" || parts[4]["url"] != "data:image/png;base64,AA==" ||
		parts[5]["type"] != "file" || parts[5]["mime"] != "image/png" || parts[5]["url"] != "data:image/png;base64,AA==" {
		t.Fatalf("parts = %#v", parts)
	}
	if _, err = promptToOpenCodeParts(nil, nil); err == nil {
		t.Fatal("empty prompt accepted")
	}
	if _, err = promptToOpenCodeParts([]acp.ContentBlock{{Audio: &acp.ContentBlockAudio{Type: "audio", Data: "AA==", MimeType: "audio/wav"}}}, nil); err == nil {
		t.Fatal("audio prompt accepted")
	}
	if _, err = promptToOpenCodeParts([]acp.ContentBlock{{Resource: &acp.ContentBlockResource{
		Type: "resource", Resource: acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{}},
	}}}, nil); err == nil {
		t.Fatal("empty embedded text resource accepted")
	}
	// No image block URI contributes anything to the native part, whatever it
	// spells: the part is built from the media type and the bytes alone.
	for _, uri := range []string{"%", "https://example.com", "file:///tmp/shot.png"} {
		parts, err = promptToOpenCodeParts([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png", Uri: &uri}}}, nil)
		if err != nil || !reflect.DeepEqual(parts[0], map[string]any{"type": "file", "mime": "image/png", "url": "data:image/png;base64,AA=="}) {
			t.Fatalf("image parts for uri %q = %#v err=%v", uri, parts, err)
		}
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
	session := testSession(t, agent, client)
	require.NoError(t, session.establish(context.Background()))
	t.Cleanup(session.stopPump)
	png := fixtureImageBase64(t, "valid.png")
	jpeg := fixtureImageBase64(t, "valid.jpg")
	imageURI := "file:///tmp/screenshot.jpg"
	client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		want := []map[string]any{
			{"type": "text", "text": "look"},
			{"type": "file", "mime": "image/png", "url": "data:image/png;base64," + png},
			{"type": "file", "mime": "image/jpeg", "url": "data:image/jpeg;base64," + jpeg},
		}
		if !reflect.DeepEqual(req.Parts, want) {
			t.Fatalf("native parts = %#v, want %#v", req.Parts, want)
		}

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
	}

	// The second image carries a URI alongside authoritative data. The
	// submitted pixels come from the data, and the URI contributes nothing:
	// the two images build the same shape of native part.
	_, err := session.Prompt(context.Background(), acp.PromptRequest{
		SessionId: session.id,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("look"),
			{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: "image/png"}},
			{Image: &acp.ContentBlockImage{Type: "image", Data: jpeg, MimeType: "image/jpeg", Uri: &imageURI}},
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
	session := testSession(t, agent, client)

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

func TestSlashCommandRefreshAdvertisesInitialEmptyCatalogOnce(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(t, agent, client)

	require.NoError(t, session.refreshCommands(ctx))
	require.Len(t, conn.updates, 1)
	update := conn.updates[0].Update.AvailableCommandsUpdate
	require.NotNil(t, update)
	require.NotNil(t, update.AvailableCommands)
	require.Empty(t, update.AvailableCommands)

	require.NoError(t, session.refreshCommands(ctx))
	require.Len(t, conn.updates, 1)
}

func TestSlashCommandRefreshEmptyClearAndFailureKeepsCache(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := NewAgent()
	agent.setAgentClient(conn)
	session := testSession(t, agent, client)

	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if conn.updateCount() != 1 {
		t.Fatalf("initial empty update missing: %#v", conn.updates)
	}

	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("non-empty refresh: %v", err)
	}
	if conn.updateCount() != 2 {
		t.Fatalf("updates after non-empty = %#v", conn.updates)
	}
	if err := session.refreshCommands(ctx); err != nil {
		t.Fatalf("unchanged refresh: %v", err)
	}
	if conn.updateCount() != 2 {
		t.Fatalf("unchanged refresh emitted: %#v", conn.updates)
	}

	client.commandsErr = errors.New("commands failed")
	if err := session.refreshCommands(ctx); err == nil {
		t.Fatal("refresh failure returned nil")
	}
	if conn.updateCount() != 2 {
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
	if conn.updateCount() != 3 {
		t.Fatalf("clear update missing: %#v", conn.updates)
	}
	clearUpdate := conn.updates[2].Update.AvailableCommandsUpdate
	if clearUpdate == nil || len(clearUpdate.AvailableCommands) != 0 {
		t.Fatalf("clear update = %#v", clearUpdate)
	}
	wire, err := json.Marshal(conn.updates[2])
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
		session := testSession(t, agent, client)
		messageID := "msg-user"
		client.dispatchCommand = func(_ context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
			if id != "native-1" {
				t.Fatalf("native id = %q", id)
			}
			if req.MessageID == "" || req.MessageID == messageID || req.Agent != "build" || req.Model != "openai/gpt-test" ||
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
				session := testSession(t, agent, client)
				client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
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
		session := testSession(t, agent, client)
		client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
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
		session := testSession(t, agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("initial refresh: %v", err)
		}
		if conn.updateCount() != 1 {
			t.Fatalf("initial updates = %#v", conn.updates)
		}
		client.commands = nil
		client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			t.Fatal("removed command fell back to plain message")

			return opencode.NativeMessage{}, nil
		}
		client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
			t.Fatal("removed command was sent to native command endpoint")

			return opencode.NativeMessage{}, nil
		}

		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/stale now")}})
		if err == nil || !strings.Contains(err.Error(), "opencode_command_removed") || strings.Contains(err.Error(), "stale") {
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
		session := testSession(t, NewAgent(), client)
		client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			t.Fatal("shadowing command fell back to plain message")

			return opencode.NativeMessage{}, nil
		}
		client.dispatchCommand = func(_ context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
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

func TestNativePromptIdentityIsWrapperMintedPerRequest(t *testing.T) {
	current, client, _ := lifecycleSession(t)
	callerMessageID := "caller-reusable-message-id"
	requests := make(chan string, 2)
	client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
		requests <- req.MessageID

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{
			ID: "assistant-" + req.MessageID, SessionID: id, Role: "assistant", Finish: "stop",
		}}, nil
	}

	for _, nonce := range []string{"request-one", "request-two"} {
		request := correlatedPrompt(current.id, nonce, "hello")
		request.MessageId = &callerMessageID
		response, err := current.Prompt(context.Background(), request)
		require.NoError(t, err)
		require.NotNil(t, response.UserMessageId)
		require.Equal(t, callerMessageID, *response.UserMessageId)
	}

	first := <-requests
	second := <-requests
	require.NotEmpty(t, first)
	require.NotEmpty(t, second)
	require.NotEqual(t, callerMessageID, first)
	require.NotEqual(t, callerMessageID, second)
	require.NotEqual(t, first, second, "native acceptance identity was reused across requests")
}

func TestPromptSlashCommandMixedContent(t *testing.T) {
	ctx := context.Background()
	t.Run("matched command sends file parts", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		session := testSession(t, NewAgent(), client)
		png := fixtureImageBase64(t, "valid.png")
		resourceMime := "text/plain"
		blobMime := "application/octet-stream"
		client.dispatchCommand = func(_ context.Context, id string, req opencode.CommandRequest) (opencode.NativeMessage, error) {
			want := []map[string]any{
				{"type": "file", "mime": "image/png", "url": "data:image/png;base64," + png},
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
				{Image: &acp.ContentBlockImage{Type: "image", Data: png, MimeType: "image/png"}},
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
		session := testSession(t, NewAgent(), client)
		client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
			t.Fatal("DispatchCommand called for unconvertible block")

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
		for _, block := range []acp.ContentBlock{
			acp.TextBlock("extra text"),
			{Resource: &acp.ContentBlockResource{Type: "resource"}},
			{},
		} {
			_, err := commandPromptParts([]acp.ContentBlock{block}, nil)
			if err == nil {
				t.Fatalf("unconvertible command block accepted: %#v", block)
			}
			requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueUnsupported, jsonFieldField: "prompt"})
		}
		if _, err := blobResourceOpenCodePart(&acp.BlobResourceContents{}, ""); err == nil {
			t.Fatal("empty blob resource accepted")
		}
		named := resourceLinkOpenCodePart(&acp.ContentBlockResourceLink{Name: "named.txt", Uri: "file:///tmp/ignored"})
		if named["mime"] != "application/octet-stream" || named["filename"] != "named.txt" {
			t.Fatalf("named resource link part = %#v", named)
		}
	})

	t.Run("a uri the parser rejects contributes no filename", func(t *testing.T) {
		// The uri is host-supplied and never validated before this point, so the
		// name derivation has to answer for one net/url refuses outright. The
		// blob still carries the bytes, so the part is built and sent — it just
		// names nothing.
		blobMime := "application/octet-stream"
		part, err := blobResourceOpenCodePart(&acp.BlobResourceContents{
			Uri:      "file:///tmp/re\x7fjected.bin",
			MimeType: &blobMime,
		}, "AA==")
		if err != nil {
			t.Fatalf("blob resource with an unparsable uri: %v", err)
		}
		if _, named := part["filename"]; named {
			t.Fatalf("part = %#v, want no filename", part)
		}
		if part["url"] != "data:application/octet-stream;base64,AA==" {
			t.Fatalf("part = %#v, want the blob inlined", part)
		}
	})

	t.Run("unmatched slash keeps supported mixed content as plain message", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		session := testSession(t, NewAgent(), client)
		client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if len(req.Parts) != 2 || req.Parts[0]["text"] != "/missing" || req.Parts[1]["type"] != "file" {
				t.Fatalf("plain mixed parts = %#v", req.Parts)
			}

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt: []acp.ContentBlock{
				acp.TextBlock("/missing"),
				{Image: &acp.ContentBlockImage{Type: "image", Data: fixtureImageBase64(t, "valid.png"), MimeType: "image/png"}},
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
			client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
				t.Fatal("stale command retried as plain prompt")

				return opencode.NativeMessage{}, nil
			}
			client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
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
			session := testSession(t, agent, client)

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
	client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
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
	session := testSession(t, NewAgent(), client)

	_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("/stale now")}})
	var reqErr *acp.RequestError
	if !errors.As(err, &reqErr) {
		t.Fatalf("expected *acp.RequestError, got %T: %v", err, err)
	}
	data, ok := reqErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data = %#v", reqErr.Data)
	}
	if data["structuredOutputRequested"] != false || data["statusCode"] != 429 {
		t.Fatalf("assistant error data = %#v", data)
	}
	require.NotContains(t, data, jsonFieldProviderCode)
}

func TestPromptSlashCommandExclusiveTurn(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	agent := NewAgent()
	session := testSession(t, agent, client)
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
			session := testSession(t, agent, client)

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
		session := testSession(t, agent, client)
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		messageID := "user-message"
		client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
			if id != "native-1" || req.MessageID == "" || req.MessageID == messageID || len(req.Parts) != 1 {
				t.Fatalf("DispatchMessage id=%q req=%#v", id, req)
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
		client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{}, errors.New("send failed")
		}
		session := testSession(t, NewAgent(), client)
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
			t.Fatal("send error prompt succeeded")
		}
	})

	t.Run("snapshot error after final message", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		agent := NewAgent(WithSessionStore(&errorSessionStore{err: errors.New("snapshot failed")}))
		session := testSession(t, agent, client)
		client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil ||
			!strings.Contains(err.Error(), "snapshot failed") {
			t.Fatalf("snapshot error = %v", err)
		}
	})

	t.Run("prompt validation and turn backpressure", func(t *testing.T) {
		session := testSession(t, NewAgent(), newFakeOpenCodeClient())
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

	t.Run("turn context cancellation", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.omitPromptEvidence = true
		started := make(chan struct{})
		client.dispatchMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			close(started)
			<-ctx.Done()

			return opencode.NativeMessage{}, ctx.Err()
		}
		session := testSession(t, NewAgent(), client)
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
		session := testSession(t, agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("refreshCommands: %v", err)
		}
		client.dispatchMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
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
		session := testSession(t, agent, client)
		if err := session.refreshCommands(ctx); err != nil {
			t.Fatalf("refreshCommands: %v", err)
		}
		client.dispatchMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
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
		session := testSession(t, agent, client)
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
	s := testSession(t, agent, client)
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
		session := testSession(t, agent, client)
		dispatched := make(chan struct{})
		client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			close(dispatched)

			return opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- err
		}()
		<-dispatched
		client.publishEvent(opencode.Event{Type: "server.connected"})
		client.publishEvent(opencode.Event{
			Type:       "message.part.created",
			Properties: json.RawMessage(`{"id":"event-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
		})
		deadline := time.After(time.Second)
		for conn.updateCount() == 0 {
			select {
			case <-deadline:
				t.Fatalf("stream update was not emitted: %#v", conn.updates)
			default:
				time.Sleep(time.Millisecond)
			}
		}
		client.publishTurnCompletion("native-1", "assistant")
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
		session := testSession(t, agent, client)
		dispatched := make(chan struct{})
		client.dispatchMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			close(dispatched)

			return opencode.NativeMessage{}, nil
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- err
		}()
		<-dispatched
		client.publishEvent(opencode.Event{
			Type:       "message.part.created",
			Properties: json.RawMessage(`{"id":"event-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
		})
		assertTurnFailed(t, <-done, causeTransport, "")
		requireSignal(t, client.closeSignal)
	})

	t.Run("final message update error returns", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.updateErr = errors.New("final update failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)
		client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
			return opencode.NativeMessage{
				Info:  opencode.NativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"},
				Parts: []opencode.NativePart{{ID: "final", SessionID: id, MessageID: "assistant", Type: "text", Text: "done", Raw: json.RawMessage(`{"id":"final"}`)}},
			}, nil
		}
		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		require.ErrorContains(t, err, "authoritative session update delivery failed")
		require.NotContains(t, err.Error(), "final update failed")
	})
}

func TestReplayAndEventEdgeBranches(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	conn := newRecordingAgentClient()
	agent := negotiatedAgent(t)
	agent.setAgentClient(conn)
	session := testSession(t, agent, client)
	replayStart := conn.updateCount()

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
	if conn.updateCount() != replayStart+1 || conn.updates[replayStart].Update.UserMessageChunk == nil {
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
	nilConnSession := testSession(t, NewAgent(), newFakeOpenCodeClient())
	if err := nilConnSession.emitUpdate(ctx, acp.UpdatePlan(acp.PlanEntry{Content: "no client"})); err != nil {
		t.Fatalf("emitUpdate without conn: %v", err)
	}

	noConnClient := newFakeOpenCodeClient()
	noConnSession := testSession(t, negotiatedAgent(t), noConnClient)
	noConnGeneration := noConnSession.runtimeGeneration
	noConnSession.agent.setAgentClient(nil)
	noConnClient.permissionReplied = make(chan struct{}, 1)
	if err := noConnSession.routeNativePermission(ctx, opencode.PermissionRequest{}); err != nil {
		t.Fatalf("empty permission: %v", err)
	}

	if err := noConnSession.routeNativePermission(ctx, opencode.PermissionRequest{ID: "p", SessionID: "native-1"}); err == nil {
		t.Fatal("nil connection admitted an undeliverable lifecycle action")
	}
	requireSignal(t, noConnClient.permissionReplied)
	require.Equal(t, "p", noConnClient.permissionReply(0).requestID)
	require.Equal(t, 1, noConnClient.permissionReplyCount())
	noConnSession.agent.mu.Lock()
	retirement := noConnSession.agent.runtimeRetirements[noConnGeneration]
	noConnSession.agent.mu.Unlock()
	require.NotNil(t, retirement)
	<-retirement.done
	require.True(t, noConnClient.isClosed())

	if err := session.routeNativeQuestion(ctx, opencode.QuestionRequest{}); err != nil {
		t.Fatalf("empty question: %v", err)
	}
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	malformedSession := testSession(t, NewAgent(), newFakeOpenCodeClient())
	if err := malformedSession.applyNativeEvent(ctx, opencode.Event{Type: "permission.v2.asked", Properties: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed permission event succeeded")
	}
	conn.permission = acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("once")}
	client.permissionReplied = make(chan struct{}, 1)
	client.questionReplied = make(chan struct{}, 1)
	client.questionRejected = make(chan struct{}, 1)
	acceptOpenTestTurn(t, session)
	session.markPublishedToolCall("c1")
	defer session.finishTurn()
	actionCtx := withNativeIncarnationBinding(withTurnRoute(ctx, "nonce-1"), testIncarnation(session))
	if err := session.applyNativeEvent(actionCtx, opencode.Event{
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
	requireSignal(t, client.permissionReplied)
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
	if err := session.applyNativeEvent(actionCtx, opencode.Event{
		Type:       "question.v2.asked",
		Properties: json.RawMessage(`{"id":"q-v2","sessionID":"native-1","questions":[{"question":"Continue?"}]}`),
	}); err != nil {
		t.Fatalf("question.v2.asked event: %v", err)
	}
	requireSignal(t, client.questionReplied)
	questionReply := client.questionReply(client.questionReplyCount() - 1)
	if questionReply.route != opencode.QuestionRouteAPI || questionReply.requestID != "q-v2" {
		t.Fatalf("question.v2 reply = %#v", questionReply)
	}
	assertEventEdgeAndHelperBranches(t, ctx, session)
	conn.elicitErr = errors.New("elicitation failed")
	require.NoError(t, session.routeNativeQuestion(actionCtx, opencode.QuestionRequest{ID: "q-error", SessionID: "native-1"}))
	requireSignal(t, client.questionRejected)
}

func assertEventEdgeAndHelperBranches(t *testing.T, ctx context.Context, session *session) {
	t.Helper()
	if err := session.applyNativeEvent(ctx, opencode.Event{Type: "todo.updated", Properties: json.RawMessage(`{"sessionID":"other","todos":[{"content":"x"}]}`)}); err != nil {
		t.Fatalf("foreign todo event: %v", err)
	}
	if err := session.applyNativeEvent(ctx, opencode.Event{Type: "question.asked", Properties: json.RawMessage(`{"request":{"id":"q","sessionID":"other"}}`)}); err != nil {
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
	rawSession := testSession(t, NewAgent(), newFakeOpenCodeClient())
	rawSession.rawMessages = rawMessageConfig{enabled: true}
	if err := rawSession.emitRawOpenCodeEvent(ctx, opencode.Event{Raw: json.RawMessage(`{"type":"x"}`)}); err != nil {
		t.Fatalf("raw event without conn: %v", err)
	}
	if usageFromTokens(opencode.NativeTokens{}) != nil {
		t.Fatal("empty tokens produced usage")
	}
	var emptyResource acp.EmbeddedResourceResource
	if _, err := embeddedResourceOpenCodePart(0, emptyResource, nil); err == nil {
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
		client.omitPromptEvidence = true
		session := testSession(t, NewAgent(), client)
		client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			session.lifecycleMu.Lock()
			session.cancelled = true
			session.lifecycleMu.Unlock()

			return opencode.NativeMessage{}, errors.New("cancelled send")
		}
		resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err == nil || !strings.Contains(err.Error(), "opencode_prompt_cancelled_before_dispatch") {
			t.Fatalf("cancelled send resp=%#v err=%v", resp, err)
		}
	})

	t.Run("successful result marked cancelled", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(t, NewAgent(), client)
		client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
			session.lifecycleMu.Lock()
			session.cancelled = true
			session.lifecycleMu.Unlock()

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
		session := testSession(t, agent, client)
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
		session = testSession(t, agent, client)
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
		session := testSession(t, agent, client)
		part := opencode.NativePart{ID: "dup", SessionID: "native-1", MessageID: "assistant", Type: "text", Text: "hello"}
		if err := session.emitMessage(ctx, opencode.NativeMessage{Info: opencode.NativeMessageInfo{ID: "assistant", Role: "assistant"}, Parts: []opencode.NativePart{part, part}}, false); err != nil {
			t.Fatalf("duplicate emitMessage: %v", err)
		}
		// Raw delivery is observational and does not poison typed/lifecycle routing.
		conn.notifyErr = errors.New("notify failed")
		session.rawMessages = rawMessageConfig{enabled: true}
		session.routeNativeEvent(ctx, opencode.Event{Type: "unknown", Properties: json.RawMessage(`{"sessionID":"native-1"}`), Raw: json.RawMessage(`{"type":"unknown"}`)})
		require.NoError(t, session.delivery.flushRaw(ctx))
		require.NoError(t, session.lifecycleFailure())
		conn.notifyErr = nil
		require.NoError(t, session.emitRawOpenCodeEvent(ctx, normalRawEvent("after-failure")))
		require.NoError(t, session.delivery.flushRaw(ctx))
		events := rawEventNotifications(conn, session.id)
		require.Equal(t, int64(1), events[len(events)-1][jsonFieldSequence])
	})

	t.Run("same-session action errors", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(t, agent, client)
		conn.permErr = errors.New("permission failed")
		if err := session.applyNativeEvent(ctx, opencode.Event{
			Type:       "permission.v2.asked",
			Properties: json.RawMessage(`{"id":"p","sessionID":"native-1"}`),
		}); err == nil {
			t.Fatal("permission event ignored client error")
		}
		conn.permErr = nil

		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		conn.elicitErr = errors.New("elicitation failed")
		if err := session.applyNativeEvent(ctx, opencode.Event{
			Type:       "question.asked",
			Properties: json.RawMessage(`{"id":"q","sessionID":"native-1"}`),
		}); err == nil {
			t.Fatal("question event ignored client error")
		}
		if req, ok := eventQuestion(json.RawMessage(`{"id":"direct","sessionID":"native-1"}`)); !ok || req.ID != "direct" {
			t.Fatalf("direct eventQuestion = %#v ok=%v", req, ok)
		}
	})
}

func TestLifecycleMCPRefreshRemainingRuntimeBranches(t *testing.T) {
	t.Run("missing client", func(t *testing.T) {
		session := testSession(t, NewAgent(), newFakeOpenCodeClient())
		session.mcpRefreshPending = true
		session.client = nil

		err := session.refreshLifecycleMCP(context.Background())
		assertTurnFailed(t, err, causeTransport, "")
		require.True(t, session.mcpRefreshPending)
	})

	t.Run("runtime changes during refresh", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(t, NewAgent(), client)
		session.mcpRefreshPending = true
		session.mcpServers = []opencode.MCPServerConfig{{Name: "wagie", URL: "https://mcp.test"}}
		client.refreshMCPFunc = func(context.Context, []opencode.MCPServerConfig) error {
			session.mu.Lock()
			session.runtimeLostCause = "runtime exited"
			session.mu.Unlock()

			return nil
		}

		err := session.refreshLifecycleMCP(context.Background())
		assertTurnFailed(t, err, causeTransport, "")
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
		session := testSession(t, agent, client)
		session.outputSchema = map[string]any{
			"type":       "object",
			"properties": map[string]any{"answer": map[string]any{"type": "string"}},
		}
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
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
		session := testSession(t, agent, client)
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		client.dispatchMessage = func(_ context.Context, id string, req opencode.MessageRequest) (opencode.NativeMessage, error) {
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
		session := testSession(t, agent, client)
		session.outputSchema = map[string]any{"type": "object"}
		agent.mu.Lock()
		agent.sessions[session.id] = session
		agent.mu.Unlock()
		client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
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
			session := testSession(t, NewAgent(), client)
			if tt.withSchema {
				session.outputSchema = map[string]any{"type": "object"}
			}
			client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
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
			require.NotContains(t, data, jsonFieldProviderCode)
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
	part, err := embeddedResourceOpenCodePart(0, resource, nil)
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
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	session.stopPump()
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
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		panic("native turn exploded")
	}
	session := testSession(t, NewAgent(), client)

	resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	if err == nil {
		t.Fatalf("panicking native turn returned no error: %#v", resp)
	}
	assertTurnFailed(t, err, causeTransport, "")
}

func TestFailureFramesNeverCarryNativePayloads(t *testing.T) {
	data := turnFailedData(causeTransport, "SECRET_SENTINEL", 503, "SECRET_SENTINEL")
	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "SECRET_SENTINEL")
	handlerFrame, err := json.Marshal(requestError(context.Background(), errors.New("SECRET_SENTINEL")))
	require.NoError(t, err)
	require.NotContains(t, string(handlerFrame), "SECRET_SENTINEL")

	part := opencode.NativePart{Tool: "handler", State: json.RawMessage(`{"status":"error","error":"SECRET_SENTINEL"}`)}
	state := nativeToolPartState(part, "tool-1")
	encoded, err = json.Marshal(state.output)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "SECRET_SENTINEL")
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
			client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
				msg := opencode.NativeMessage{Info: opencode.NativeMessageInfo{
					ID: "assistant-1", SessionID: id, Role: "assistant", Finish: "error",
					Error: providerNativeError(tt.detail, tt.status, tt.code),
				}}

				return opencode.NativeMessage{}, opencode.AssistantMessageError(msg)
			}
			session := testSession(t, NewAgent(), client)

			resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
			if resp.StopReason != "" {
				t.Fatalf("StopReason = %q, want empty (no PromptResponse)", resp.StopReason)
			}
			data := assertTurnFailed(t, err, causeProvider, tt.detail)
			if data["statusCode"] != tt.status {
				t.Fatalf("statusCode = %#v, want %d", data["statusCode"], tt.status)
			}
			require.NotContains(t, data, jsonFieldProviderCode)
		})
	}
}

// T3 — a transport failure from the native POST surfaces the uniform
// turn-failure error (cause "transport") with the real cause text, and leaves
// the session addressable and retriable (not poisoned, not unknown-session).
func TestPromptTransportErrorIsRetriable(t *testing.T) {
	ctx := context.Background()

	client := newFakeOpenCodeClient()
	client.omitPromptEvidence = true
	client.dispatchMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{}, io.ErrUnexpectedEOF
	}
	session := testSession(t, NewAgent(), client)

	_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	assertTurnFailed(t, err, causeTransport, io.ErrUnexpectedEOF.Error())

	if poisonErr := session.ensureNotPoisoned(); poisonErr != nil {
		t.Fatalf("session poisoned after transport failure: %v", poisonErr)
	}

	// The next turn re-drives successfully: the session stayed addressable.
	client.omitPromptEvidence = false
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
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
	client.dispatchMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}
	session := testSession(t, NewAgent(), client)

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
	client.publishStreamTerminal(errors.New("connection reset by peer"))
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

func TestPromptSessionErrorTerminatesTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	dispatched := make(chan opencode.MessageRequest, 1)
	client.dispatchMessage = func(_ context.Context, _ string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched <- request

		return opencode.NativeMessage{}, nil
	}
	session := testSession(t, NewAgent(), client)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
		done <- err
	}()

	var request opencode.MessageRequest
	select {
	case request = <-dispatched:
	case <-ctx.Done():
		t.Fatal("Prompt did not start")
	}
	client.publishEvent(opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSON(t, map[string]any{"info": map[string]any{
			"id": "assistant-error", "sessionID": session.idmap.NativeSessionID,
			"role": roleAssistant, "parentID": request.MessageID,
		}}),
	})

	client.publishEvent(opencode.Event{
		Type: opencode.EventSessionError,
		Properties: mustJSON(t, opencode.SessionError{
			SessionID: "foreign-session",
			Error:     providerNativeError("foreign failure", 400, "foreign"),
		}),
	})
	client.publishEvent(opencode.Event{
		Type: opencode.EventSessionError,
		Properties: mustJSON(t, opencode.SessionError{
			SessionID: session.idmap.NativeSessionID,
			Error:     providerNativeError("model rejected", 400, "invalid_model"),
		}),
	})
	client.publishSessionIdle(session.idmap.NativeSessionID)

	select {
	case err := <-done:
		data := assertTurnFailed(t, err, causeProvider, "model rejected")
		if data[jsonFieldStatusCode] != 400 {
			t.Fatalf("session error data = %#v", data)
		}
		require.NotContains(t, data, jsonFieldProviderCode)
	case <-ctx.Done():
		t.Fatal("Prompt did not return after session.error")
	}

	client.mu.Lock()
	closed := client.closed
	client.mu.Unlock()
	if closed {
		t.Fatal("session.error retired the shared runtime")
	}
}

func TestPromptCancellationWinsSessionErrorEvent(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}
	dispatched := make(chan opencode.MessageRequest, 1)
	client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-cancel", "sessionID": id, "role": roleAssistant,
				"parentID": request.MessageID,
			}}),
		})
		dispatched <- request

		return opencode.NativeMessage{}, nil
	}
	agent := NewAgent()
	session := testSession(t, agent, client)
	assistantObserved := make(chan struct{})
	errorObserved := make(chan struct{})
	releaseError := make(chan struct{})
	session.mu.Lock()
	session.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
		if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "assistant-cancel" {
			signalTestHook(assistantObserved)
		}
		if event.Type == opencode.EventSessionError {
			signalTestHook(errorObserved)
			<-releaseError
		}
	}
	session.mu.Unlock()
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()
	done := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, err := session.Prompt(context.Background(), acp.PromptRequest{
			SessionId: session.id,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
		done <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: err}
	}()
	<-dispatched
	requireSignal(t, assistantObserved)
	client.publishEvent(opencode.Event{
		Type: opencode.EventSessionError,
		Properties: mustJSON(t, opencode.SessionError{
			SessionID: session.idmap.NativeSessionID,
			Error:     providerNativeError("cancelled provider error", 400, "cancelled"),
		}),
	})
	requireSignal(t, errorObserved)
	if err := agent.Cancel(context.Background(), CancelRequest(session.id, internalSeamTurnNonce)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(releaseError)
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)
}

func TestSessionErrorEventRejectsMalformedProperties(t *testing.T) {
	session := testSession(t, NewAgent(), newFakeOpenCodeClient())
	err := session.applyNativeEvent(context.Background(), opencode.Event{Type: opencode.EventSessionError, Properties: json.RawMessage(`{`)})
	require.Error(t, err)
}

// T5 — a native dispatch failure observed while the host cancelled reports the
// cancellation rather than the native error: the cancel guard runs before
// failure mapping, and with no accepted turn there is no cancelled
// PromptResponse to return.
func TestPromptCancelSuppressesNativeError(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.omitPromptEvidence = true
	started := make(chan struct{})
	client.dispatchMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-ctx.Done()

		return opencode.NativeMessage{}, errors.New("provider error raised during cancel")
	}
	agent := NewAgent()
	session := testSession(t, agent, client)
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
	if err := agent.Cancel(ctx, CancelRequest(session.id, internalSeamTurnNonce)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case got := <-done:
		if got.err == nil || !strings.Contains(got.err.Error(), "opencode_prompt_cancelled_before_dispatch") {
			t.Fatalf("cancelled dispatch error = %v", got.err)
		}
		if got.resp.StopReason != "" {
			t.Fatalf("StopReason = %q, want empty (no PromptResponse)", got.resp.StopReason)
		}
	case <-ctx.Done():
		t.Fatal("Prompt did not return after cancel")
	}
	if client.closed {
		t.Fatal("routine cancellation retired the shared runtime")
	}
	if client.abortCount() != 1 {
		t.Fatalf("abort count = %d, want 1", client.abortCount())
	}
}

// T6 — with WithTurnTimeout set, a hanging native turn fails with cause
// "timeout" (not cancelled) after the exact shared runtime is retired.
func TestPromptTurnTimeoutFailsWithTimeoutCause(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.omitPromptEvidence = true
	client.dispatchMessage = func(ctx context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		<-ctx.Done()

		return opencode.NativeMessage{}, ctx.Err()
	}
	agent := NewAgent(WithTurnTimeout(20 * time.Millisecond))
	session := testSession(t, agent, client)

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
	if client.closed {
		t.Fatal("a turn timeout retired the shared runtime")
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
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}
	started := make(chan struct{})
	releaseDispatch := make(chan struct{})
	client.dispatchMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		close(started)
		<-releaseDispatch

		return opencode.NativeMessage{}, nil
	}
	agent := NewAgent(WithTurnTimeout(15 * time.Millisecond))
	session := testSession(t, agent, client)
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
	session.lifecycleMu.Lock()
	session.cancelled = true
	session.lifecycleMu.Unlock()
	close(releaseDispatch)

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
	client.dispatchMessage = func(_ context.Context, _ string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		return opencode.NativeMessage{}, fmt.Errorf(
			"opencode message POST: %w; message re-fetch failed: %v",
			io.ErrUnexpectedEOF,
			errors.New("opencode GET /session/native-1/message returned 502 Bad Gateway: gateway is down"),
		)
	}
	session := testSession(t, NewAgent(), client)

	_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	data := assertTurnFailed(t, err, causeTransport, "unexpected EOF")

	require.NotContains(t, fmt.Sprint(data), "unexpected EOF")
}
func TestPromptCancelAndLoadRejectRemainingRouteAndMCPBranches(t *testing.T) {
	agent := NewAgent()
	_, err := agent.Prompt(context.Background(), acp.PromptRequest{})
	require.Error(t, err)

	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	require.Error(t, agent.Cancel(context.Background(), CancelRequest(current.id, "stale")))

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId: "session", Cwd: t.TempDir(),
		McpServers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "bad"}}},
	})
	require.Error(t, err)
}

func TestPartTextDeltaEmptyNativeDeltaBranch(t *testing.T) {
	current := testSession(t, NewAgent(), newFakeOpenCodeClient())
	text, commit := current.partTextDelta(opencode.NativePart{ID: "part"}, "")
	require.Empty(t, text)
	require.Nil(t, commit)
}

func TestFilePartUpdates(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")

	t.Run("user image data url becomes user chunk", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f1", MessageID: "m1", Type: partTypeFile, Mime: mimePNG, URL: dataURL(mimePNG, png)}
		require.NoError(t, session.emitPartUpdates(ctx, roleUser, part, "", true))
		require.NoError(t, session.emitPartUpdates(ctx, roleUser, part, "", true))
		require.Len(t, conn.updates, 1)
		require.NotNil(t, conn.updates[0].Update.UserMessageChunk)
	})

	t.Run("user remote url becomes resource link", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f2", MessageID: "m1", Type: partTypeFile, Mime: mimePNG, URL: "https://x/g.png", Filename: "g.png"}
		require.NoError(t, session.emitPartUpdates(ctx, roleUser, part, "", true))
		require.Len(t, conn.updates, 1)
		require.NotNil(t, conn.updates[0].Update.UserMessageChunk.Content.ResourceLink)
	})

	t.Run("user non-image data url is skipped", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f3", MessageID: "m1", Type: partTypeFile, URL: "data:text/plain;base64,QUJD"}
		require.NoError(t, session.emitPartUpdates(ctx, roleUser, part, "", true))
		require.Empty(t, conn.updates)
	})

	t.Run("user image data url with bad base64 is skipped", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f3b", MessageID: "m1", Type: partTypeFile, Mime: mimePNG, URL: "data:image/png;base64,!!!!"}
		require.NoError(t, session.emitPartUpdates(ctx, roleUser, part, "", true))
		require.Empty(t, conn.updates)
	})

	t.Run("user local file is skipped", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f4", MessageID: "m1", Type: partTypeFile, URL: "file:///tmp/note.txt"}
		require.NoError(t, session.emitPartUpdates(ctx, roleUser, part, "", true))
		require.Empty(t, conn.updates)
	})

	t.Run("assistant image data url becomes agent chunk", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f5", MessageID: "m1", Type: partTypeFile, Mime: mimePNG, URL: dataURL(mimePNG, png)}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))
		require.Len(t, conn.updates, 1)
		require.NotNil(t, conn.updates[0].Update.AgentMessageChunk.Content.Image)
	})

	t.Run("assistant non-image is skipped", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f6", MessageID: "m1", Type: partTypeFile, Mime: "text/plain", URL: "data:text/plain;base64,QUJD"}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))
		require.Empty(t, conn.updates)
	})

	// An assistant image the adapter will not ship has no tool call to
	// attribute to, so its guidance takes the image's place and the turn
	// keeps its context instead of ending.
	t.Run("assistant refusal becomes agent guidance and keeps the turn", func(t *testing.T) {
		session, conn := newImageSession(t)
		part := opencode.NativePart{ID: "f7", MessageID: "m1", Type: partTypeFile, Mime: mimePNG, URL: "data:image/png;base64,!!!!"}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))
		require.Len(t, conn.updates, 1)
		require.Equal(t, imageGuidanceInvalidBase64, conn.updates[0].Update.AgentMessageChunk.Content.Text.Text)

		next := opencode.NativePart{ID: "f8", MessageID: "m1", Type: partTypeText, Text: "still here"}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", next, "", false))
		require.Len(t, conn.updates, 2)
	})

	// The artifact store breaking is the adapter's own durability failing, so
	// it still ends the turn.
	t.Run("assistant storage failure is turn fatal", func(t *testing.T) {
		session, _ := newImageSession(t)
		original := imageJSONMarshal
		imageJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal boom") }

		t.Cleanup(func() { imageJSONMarshal = original })

		part := opencode.NativePart{ID: "f9", MessageID: "m1", Type: partTypeFile, Mime: mimePNG, URL: dataURL(mimePNG, png)}
		err := session.emitPartUpdates(ctx, "assistant", part, "", false)
		data := assertTurnFailed(t, err, causeTransport, "")
		require.Equal(t, outputReasonStorageFailed, data[jsonFieldReason])
	})
}

func TestToolPartUpdatesWithAttachments(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")

	t.Run("completed tool carries image content", func(t *testing.T) {
		session, conn := newImageSession(t)
		state, err := json.Marshal(map[string]any{
			"status":      "completed",
			"title":       "read",
			"attachments": []map[string]any{{"id": "a", "type": "file", "mime": mimePNG, "url": dataURL(mimePNG, png)}},
		})
		require.NoError(t, err)
		part := opencode.NativePart{CallID: "call-1", Type: partTypeTool, Tool: "read", State: state}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))
		require.Len(t, conn.updates, 1)
		require.NotNil(t, conn.updates[0].Update.ToolCall)
		require.Len(t, conn.updates[0].Update.ToolCall.Content, 1)
	})

	t.Run("refused attachment fails the tool call and keeps the turn", func(t *testing.T) {
		session, conn := newImageSession(t)
		state, err := json.Marshal(map[string]any{
			"status":      "completed",
			"title":       "read",
			"attachments": []map[string]any{{"id": "a", "type": "file", "mime": mimePNG}},
		})
		require.NoError(t, err)
		part := opencode.NativePart{CallID: "call-2", Type: partTypeTool, Tool: "read", State: state}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))
		require.Len(t, conn.updates, 1)

		call := conn.updates[0].Update.ToolCall
		require.Equal(t, acp.ToolCallStatusFailed, call.Status)
		require.Len(t, call.Content, 1)
		require.Equal(t, imageGuidanceMissingFile, call.Content[0].Content.Content.Text.Text)

		// The turn keeps making progress after the refusal.
		next := opencode.NativePart{ID: "t2", MessageID: "m1", Type: partTypeText, Text: "still here"}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", next, "", false))
		require.Len(t, conn.updates, 2)
	})

	// A tool call already published advances to failed and carries the same
	// guidance on the update rather than on a start.
	t.Run("refused attachment on a published tool call carries guidance", func(t *testing.T) {
		session, conn := newImageSession(t)
		pending := opencode.NativePart{
			CallID: "call-3", Type: partTypeTool, Tool: "read",
			State: json.RawMessage(`{"status":"pending","title":"read"}`),
		}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", pending, "", false))

		state, err := json.Marshal(map[string]any{
			"status":      "completed",
			"title":       "read",
			"attachments": []map[string]any{{"id": "a", "type": "file", "mime": mimePNG}},
		})
		require.NoError(t, err)

		part := opencode.NativePart{CallID: "call-3", Type: partTypeTool, Tool: "read", State: state}
		require.NoError(t, session.emitPartUpdates(ctx, "assistant", part, "", false))

		update := conn.updates[len(conn.updates)-1].Update.ToolCallUpdate
		require.Equal(t, acp.ToolCallStatusFailed, *update.Status)
		require.Len(t, update.Content, 1)
		require.Equal(t, imageGuidanceMissingFile, update.Content[0].Content.Content.Text.Text)
	})
}

func TestToolPartUpdateSnapshotAdvances(t *testing.T) {
	ctx := context.Background()
	png := fixtureImage(t, "valid.png")
	session, conn := newImageSession(t)

	pending := opencode.NativePart{CallID: "call-1", Type: partTypeTool, Tool: "read", State: json.RawMessage(`{"status":"pending","title":"read"}`)}
	require.NoError(t, session.emitPartUpdates(ctx, "assistant", pending, "", false))

	state, err := json.Marshal(map[string]any{
		"status":      "completed",
		"title":       "read",
		"attachments": []map[string]any{{"id": "a", "type": "file", "mime": mimePNG, "url": dataURL(mimePNG, png)}},
	})
	require.NoError(t, err)
	completed := opencode.NativePart{CallID: "call-1", Type: partTypeTool, Tool: "read", State: state}
	require.NoError(t, session.emitPartUpdates(ctx, "assistant", completed, "", false))

	last := conn.updates[len(conn.updates)-1].Update
	require.NotNil(t, last.ToolCallUpdate)
	require.Len(t, last.ToolCallUpdate.Content, 1)
	require.Len(t, session.emittedToolContent["call-1"], 1)
}

func TestFailedToolAttributionSeenBranches(t *testing.T) {
	session, _ := newImageSession(t)
	id := acp.ToolCallId("call-1")
	part := opencode.NativePart{CallID: "call-1", Tool: "read"}
	current := nativeToolState{status: acp.ToolCallStatusCompleted, title: "read"}
	mapErr := errors.New("boom")

	t.Run("seen and can advance", func(t *testing.T) {
		previous := emittedToolState{status: acp.ToolCallStatusInProgress}
		updates, commit, err := session.failedToolAttribution(id, part, current, previous, true, mapErr)
		require.ErrorIs(t, err, mapErr)
		require.Len(t, updates, 1)
		require.NotNil(t, commit)
		commit()
	})

	t.Run("seen and cannot advance", func(t *testing.T) {
		previous := emittedToolState{status: acp.ToolCallStatusCompleted}
		updates, _, err := session.failedToolAttribution(id, part, current, previous, true, mapErr)
		require.ErrorIs(t, err, mapErr)
		require.Nil(t, updates)
	})
}

func TestPromptRejectsInvalidImage(t *testing.T) {
	ctx := context.Background()
	badImage := acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", MimeType: mimePNG}}

	t.Run("message path", func(t *testing.T) {
		session := testSession(t, NewAgent(), newFakeOpenCodeClient())
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt:    []acp.ContentBlock{acp.TextBlock("look"), badImage},
		})
		requireInvalidParamsData(t, err, map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMissingData, jsonFieldIndex: 0,
		})
	})

	t.Run("slash command path", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
		session := testSession(t, NewAgent(), client)
		_, err := session.Prompt(ctx, acp.PromptRequest{
			SessionId: session.id,
			Prompt:    []acp.ContentBlock{acp.TextBlock("/review"), badImage},
		})
		requireInvalidParamsData(t, err, map[string]any{
			jsonFieldField: fieldPromptImage, jsonFieldError: imageErrorMissingData, jsonFieldIndex: 0,
		})
	})
}

func TestSanitizeRawEventValue(t *testing.T) {
	png := fixtureImage(t, "valid.png")
	value := map[string]any{
		"url":  "https://cdn.example.com/a.png?token=secret#frag",
		"data": dataURL(mimePNG, png),
		"nested": []any{
			map[string]any{"uri": "https://x/y?sig=abc"},
			dataURL(mimePNG, png),
		},
		"plain": "not touched",
	}

	sanitizeRawEventValue(value)

	require.Equal(t, "https://cdn.example.com/a.png", value["url"])
	require.Contains(t, value["data"], "[redacted")
	require.Equal(t, "not touched", value["plain"])

	nested, ok := value["nested"].([]any)
	require.True(t, ok)
	first, ok := nested[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "https://x/y", first["uri"])
	second, ok := nested[1].(string)
	require.True(t, ok)
	require.Contains(t, second, "[redacted")
}

func TestCommandPromptPartsPropagatesBlobError(t *testing.T) {
	_, err := commandPromptParts([]acp.ContentBlock{{Resource: &acp.ContentBlockResource{
		Type: "resource", Resource: acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{}},
	}}}, nil)
	require.Error(t, err)
}

// TestPromptRefusesAnUnreadableSubmissionCorrelation proves the public prompt
// path refuses an unreadable lifecycle envelope.
func TestPromptRefusesAnUnreadableSubmissionCorrelation(t *testing.T) {
	current, _, connection := lifecycleSession(t)

	request := TextPromptRequest(current.id, internalSeamTurnNonce, "hello")
	request.Meta[lifecycle.MetaKey] = "not an envelope"

	_, err := current.Prompt(context.Background(), request)
	require.Error(t, err)
	require.Equal(t, []string{"lifecycle_snapshot"}, connection.lifecycleEvents(t))
}

// TestCommandDispatchWithoutEvidenceFailsOnTheTurnDeadline proves the dispatch
// boundary is bounded too: a completion-reporting route that acknowledges nothing
// and publishes nothing fails on the turn deadline instead of holding the prompt
// past it.
func TestCommandDispatchWithoutEvidenceFailsOnTheTurnDeadline(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}

	hold := make(chan struct{})
	defer close(hold)

	client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
		<-hold

		return opencode.NativeMessage{}, nil
	}

	current := testSession(t, NewAgent(WithTurnTimeout(20*time.Millisecond)), client)

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "/review"))
	assertTurnFailed(t, err, causeTimeout, "deadline")
	require.Equal(t, 1, client.abortCount(), "the timed-out dispatch left native work running")
	require.Nil(t, current.currentCycle(), "a frame that acknowledged nothing opened a turn")
}

// TestCommandDispatchWithdrawnByCancelIsReportedAsTheCancellation proves a host
// cancel during the dispatch boundary answers the prompt as a cancellation: the
// frame was never accepted, so no turn and no submission exist to report.
func TestCommandDispatchWithdrawnByCancelIsReportedAsTheCancellation(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}

	started := make(chan struct{})
	hold := make(chan struct{})

	defer close(hold)

	client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
		close(started)
		<-hold

		return opencode.NativeMessage{}, nil
	}

	agent := NewAgent()
	current := testSession(t, agent, client)
	agent.mu.Lock()
	agent.sessions[current.id] = current
	agent.mu.Unlock()

	done := make(chan error, 1)

	go func() {
		_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "/review"))
		done <- err
	}()

	<-started
	require.NoError(t, agent.Cancel(context.Background(), CancelRequest(current.id, internalSeamTurnNonce)))
	require.ErrorContains(t, <-done, "opencode_prompt_cancelled_before_dispatch")
}

// TestCommandDispatchPanicAnswersThePrompt proves a crashed native command route
// answers its caller: the panic becomes the dispatch failure it is instead of
// leaving the prompt waiting on a goroutine that died.
func TestCommandDispatchPanicAnswersThePrompt(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}
	client.dispatchCommand = func(context.Context, string, opencode.CommandRequest) (opencode.NativeMessage, error) {
		panic("native command exploded")
	}

	current := testSession(t, NewAgent(), client)

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "/review"))
	assertTurnFailed(t, err, causeTransport, "panicked")
}

// TestDispatchFailureAfterTheBindingIsLostReportsTheLostRuntime proves a route
// that failed because its runtime binding died reports the loss rather than the
// route's own description of it.
func TestDispatchFailureAfterTheBindingIsLostReportsTheLostRuntime(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)

	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		// The runtime-exit watcher records the loss while the frame is in flight;
		// the turn's own cancel handle was published before it, so the route
		// simply fails against a binding that is already gone.
		current.mu.Lock()
		current.runtimeLostCause = "shared OpenCode runtime exited"
		current.mu.Unlock()

		return opencode.NativeMessage{}, errors.New("write on a closed runtime")
	}

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	assertTurnFailed(t, err, causeTransport, "shared OpenCode runtime exited")
}

// TestRefusedMessageFrameIsReportedAsRejected proves a plain prompt frame the
// native dispatcher refused with a bad request is the caller's answer: invalid
// params naming the refusal, never a turn failure.
func TestRefusedMessageFrameIsReportedAsRejected(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.refusesDispatch(&opencode.HTTPError{
		Method: "POST", Path: "/session/native-1/message", Status: "400 Bad Request",
		StatusCode: http.StatusBadRequest, Body: "unsupported part",
	})

	current := testSession(t, NewAgent(), client)

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32602, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "opencode_prompt_rejected", data[jsonFieldError])
}

// TestCommandRunFailureAfterAcceptanceFailsTheAcceptedTurn proves the
// completion-reporting route's two boundaries are distinct: the first native
// event admits the frame, and the route's later error is the accepted turn's
// outcome rather than a dispatch refusal.
func TestCommandRunFailureAfterAcceptanceFailsTheAcceptedTurn(t *testing.T) {
	current, client, connection := lifecycleSession(t)
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review", Source: "command"}}

	hold := make(chan struct{})
	evidenceSent := make(chan struct{})
	client.dispatchCommand = func(_ context.Context, id string, request opencode.CommandRequest) (opencode.NativeMessage, error) {
		client.publishEvent(opencode.Event{
			Type:       opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{"id": request.MessageID, "sessionID": id, "role": roleUser}}),
		})
		close(evidenceSent)
		<-hold

		return opencode.NativeMessage{}, errors.New("command run failed")
	}

	done := make(chan error, 1)

	go func() {
		_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "/review"))
		done <- err
	}()

	<-evidenceSent
	current.lifecycleMu.Lock()
	dispatchEvidence := current.cycle.dispatchEvidence
	current.lifecycleMu.Unlock()
	<-dispatchEvidence

	close(hold)
	assertTurnFailed(t, <-done, causeTransport, "command run failed")
	requireLifecycleOutcome(t, connection, lifecycle.OutcomeFailed)
	requireLifecycleReduces(t, connection)
}

// TestAcceptedTurnOutlivingItsCallerReadsTheEvidenceItAlreadyHas proves a turn
// context that dies for a reason this host did not ask for is not a cancellation:
// the turn reports the terminal evidence its cycle already holds.
func TestAcceptedTurnOutlivingItsCallerReadsTheEvidenceItAlreadyHas(t *testing.T) {
	client := newFakeOpenCodeClient()
	requestID := make(chan string, 1)
	assistantObserved := make(chan struct{})
	client.dispatchMessage = func(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
		client.stageAssistantMessage(id, "assistant-1")
		client.publishEvent(opencode.Event{
			Type: opencode.EventMessageUpdated,
			Properties: mustJSONValue(map[string]any{"info": map[string]any{
				"id": "assistant-1", "sessionID": id, "role": roleAssistant,
				"parentID": request.MessageID,
			}}),
		})
		requestID <- request.MessageID

		return opencode.NativeMessage{}, nil
	}

	current := testSession(t, NewAgent(), client)
	current.mu.Lock()
	current.pump.afterObserve = func(event opencode.Event, _ *nativeEventObservation) {
		if info, ok := eventMessageInfo(event.Properties); ok && info.ID == "assistant-1" {
			signalTestHook(assistantObserved)
		}
	}
	current.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	type promptResult struct {
		response acp.PromptResponse
		err      error
	}
	done := make(chan promptResult, 1)

	go func() {
		response, err := current.Prompt(ctx, TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		done <- promptResult{response: response, err: err}
	}()

	messageID := <-requestID
	requireSignal(t, assistantObserved)
	current.lifecycleMu.Lock()
	dispatchEvidence := current.cycle.dispatchEvidence
	current.lifecycleMu.Unlock()
	<-dispatchEvidence
	cancel()
	client.publishEvent(opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": "assistant-1", "sessionID": current.idmap.NativeSessionID,
			"role": roleAssistant, "parentID": messageID, "finish": "stop",
		}}),
	})
	client.publishSessionIdle(current.idmap.NativeSessionID)

	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonEndTurn, result.response.StopReason)
	require.False(t, current.wasCancelled(), "a released caller context was reported as a cancellation")
}

// TestAcceptedTurnThatOutlivesItsDeadlineFailsAsATimeout proves the configured
// deadline settles an accepted turn the same way every other terminal does: the
// native work is interrupted, the harness acknowledges it stopped, and the turn
// fails with cause timeout rather than reporting a cancellation nobody asked for.
func TestAcceptedTurnThatOutlivesItsDeadlineFailsAsATimeout(t *testing.T) {
	client := newFakeOpenCodeClient()
	started := make(chan struct{})
	client.hangsAfterDispatch(started)
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}

	current := testSession(t, NewAgent(WithTurnTimeout(20*time.Millisecond)), client)

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	assertTurnFailed(t, err, causeTimeout, "deadline")
	require.Equal(t, 1, client.abortCount())
}

// TestTimedOutTurnWhoseInterruptIsRefusedReportsAnUnprovenSettlement proves the
// interrupt acknowledgement is the settlement boundary: a harness that refused
// the interrupt leaves the turn's end unproven, and the prompt says so rather
// than reporting a deadline it cannot back.
func TestTimedOutTurnWhoseInterruptIsRefusedReportsAnUnprovenSettlement(t *testing.T) {
	client := newFakeOpenCodeClient()
	started := make(chan struct{})
	client.hangsAfterDispatch(started)
	client.abortFunc = func(string) error { return errors.New("harness refused the interrupt") }

	current := testSession(t, NewAgent(WithTurnTimeout(20*time.Millisecond)), client)

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	require.ErrorContains(t, err, "opencode_turn_settlement_unproven")
	require.NotContains(t, err.Error(), "harness refused the interrupt")
}

// TestCancelledTurnThatCannotReportItsEndSurfacesTheDeliveryFailure proves the
// ending transition is load-bearing: a cancellation whose one ending envelope
// could not be delivered answers with that failure instead of a clean cancelled
// response the stream never carried.
func TestCancelledTurnThatCannotReportItsEndSurfacesTheDeliveryFailure(t *testing.T) {
	current, client, connection := lifecycleSession(t)

	started := make(chan struct{})
	client.hangsAfterDispatch(started)
	client.abortFunc = func(id string) error {
		connection.mu.Lock()
		connection.updateErr = errors.New("wire down")
		connection.mu.Unlock()
		client.publishSessionIdle(id)

		return nil
	}

	done := make(chan error, 1)

	go func() {
		_, err := current.Prompt(context.Background(), correlatedPrompt(current.id, internalSeamTurnNonce, "hello"))
		done <- err
	}()

	<-started
	current.lifecycleMu.Lock()
	dispatchEvidence := current.cycle.dispatchEvidence
	current.lifecycleMu.Unlock()
	<-dispatchEvidence
	require.NoError(t, current.agent.Cancel(context.Background(), CancelRequest(current.id, internalSeamTurnNonce)))
	turnErr := <-done
	assertTurnFailed(t, turnErr, causeTransport, "")
	require.NotContains(t, turnErr.Error(), "wire down")

	require.ErrorContains(t, current.lifecycleFailure(), "lifecycle delivery failed",
		"an undeliverable ending transition left the stream unlatched")
}

// TestTurnWithNoReadableTranscriptFails proves the settling read is not optional:
// a transcript this session cannot read, and a transcript holding no assistant
// message for the turn, both fail the turn rather than answering end_turn with
// nothing behind it.
func TestTurnWithNoReadableTranscriptFails(t *testing.T) {
	t.Run("transcript read fails", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.messagesErr = errors.New("transcript unavailable")
		current := testSession(t, NewAgent(), client)

		_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		assertTurnFailed(t, err, causeTransport, "read OpenCode turn messages")
	})

	t.Run("transcript holds no assistant message", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.messages = []opencode.NativeMessage{{Info: opencode.NativeMessageInfo{
			ID: "user-1", SessionID: "native-1", Role: roleUser,
		}}}
		current := testSession(t, NewAgent(), client)

		_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		data := requireInternalErrorData(t, err)
		require.Equal(t, "opencode_turn_assistant_identity_missing", data[jsonFieldError])
		require.Equal(t, causeProvider, data[jsonFieldCause])
	})
}
