package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
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

		req := questionRequest{
			ID:        "question-1",
			SessionID: "native-1",
			Tool:      questionTool{MessageID: "message-1", CallID: "call-1"},
			Questions: []questionInfo{
				{
					Question: "Proceed?",
					Header:   "Decision",
					Options: []questionOption{
						{Label: "Yes", Description: "Continue"},
						{Label: "No"},
					},
				},
				{
					Question: "Colors?",
					Header:   "Palette",
					Multiple: true,
					Options: []questionOption{
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
		if err := session.handleQuestion(ctx, questionRequest{ID: "q", SessionID: "native-1"}); err != nil {
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
		if err := session.handleQuestion(ctx, questionRequest{ID: "q", SessionID: "native-1"}); err != nil {
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

func TestQuestionToolReconcileAndCancelRejectsPending(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.pendingQuestions = []questionRequest{
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
	session.questions["q2"] = questionRequest{ID: "q2", SessionID: "native-1"}
	session.pending["p1"] = permissionRequest{ID: "p1", SessionID: "native-1"}
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

	if err := session.handlePermission(ctx, permissionRequest{
		ID:        "perm-1",
		SessionID: "native-1",
		Action:    "edit",
		Resources: []string{"file.txt"},
		Metadata:  map[string]any{"path": "file.txt"},
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
	if err := session.handlePermission(ctx, permissionRequest{ID: "perm-2", SessionID: "native-1"}); err != nil {
		t.Fatalf("handlePermission cancelled: %v", err)
	}
	if got := client.permissionReply(1).reply; got != "reject" {
		t.Fatalf("cancelled reply = %q, want reject", got)
	}

	client.pendingPermissions = []permissionRequest{
		{ID: "foreign", SessionID: "other"},
		{ID: "perm-3", SessionID: "native-1"},
	}
	if err := session.reconcilePermissions(ctx); err != nil {
		t.Fatalf("reconcilePermissions: %v", err)
	}
	if got := client.permissionReply(2).requestID; got != "perm-3" {
		t.Fatalf("reconciled request id = %q", got)
	}
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
	if err := session.handleEvent(ctx, openCodeEvent{Type: "message.part.created", Properties: textProps, Raw: json.RawMessage(`{"type":"message.part.created"}`)}); err != nil {
		t.Fatalf("text event: %v", err)
	}
	if err := session.handleEvent(ctx, openCodeEvent{Type: "message.part.created", Properties: textProps}); err != nil {
		t.Fatalf("duplicate text event: %v", err)
	}
	reasoningProps := json.RawMessage(`{"part":{"id":"part-2","sessionID":"native-1","messageID":"message-1","type":"reasoning","text":"thinking"}}`)
	if err := session.handleEvent(ctx, openCodeEvent{Type: "message.part.updated", Properties: reasoningProps}); err != nil {
		t.Fatalf("reasoning event: %v", err)
	}
	toolProps := json.RawMessage(`{"id":"part-3","sessionID":"native-1","messageID":"message-1","type":"tool","tool":"bash","callID":"call-1","state":{"status":"completed","title":"Run"}}`)
	if err := session.handleEvent(ctx, openCodeEvent{Type: "message.part.created", Properties: toolProps}); err != nil {
		t.Fatalf("tool event: %v", err)
	}
	if err := session.emitMessage(ctx, nativeMessage{
		Info: nativeMessageInfo{ID: "message-1", SessionID: "native-1", Role: "assistant", Tokens: nativeTokens{Total: 9}},
		Parts: []nativePart{{
			SessionID: "native-1",
			MessageID: "message-1",
			Type:      "step-finish",
			Tokens:    nativeTokens{Input: 2, Output: 3, Reasoning: 1},
		}},
	}, false); err != nil {
		t.Fatalf("emitMessage: %v", err)
	}

	if conn.updateCount() != 6 {
		t.Fatalf("updates = %d, want 6: %#v", conn.updateCount(), conn.updates)
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
	if conn.updates[4].Update.UsageUpdate == nil || conn.updates[5].Update.UsageUpdate == nil {
		t.Fatalf("usage updates missing: %#v", conn.updates)
	}
	if len(conn.extensions) == 0 || conn.extensions[0].method != RawEventMethod {
		t.Fatalf("raw events = %#v", conn.extensions)
	}
}

func TestPromptSSEDisconnectAbortsNativeTurn(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.sendMessage = func(ctx context.Context, _ string, _ openCodeMessageRequest) (nativeMessage, error) {
		<-ctx.Done()
		return nativeMessage{}, ctx.Err()
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
		_, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		done <- err
	}()
	client.errs <- errors.New("stream closed")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "opencode_sse_disconnect") {
			t.Fatalf("Prompt error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Prompt did not return")
	}
	if client.abortCount() != 1 {
		t.Fatalf("abort count = %d, want 1", client.abortCount())
	}
}

func TestPromptHelpersAndAnswerMapping(t *testing.T) {
	parts, err := promptToOpenCodeParts([]acp.ContentBlock{
		acp.TextBlock("hello"),
		{ResourceLink: &acp.ContentBlockResourceLink{Name: "a", Type: "resource_link", Uri: "file:///tmp/a"}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: "embedded", Uri: "file:///tmp/b"},
		}}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "AA==", Uri: "file:///tmp/blob"},
		}}},
		{Image: &acp.ContentBlockImage{Type: "image", Data: "AA==", MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("promptToOpenCodeParts: %v", err)
	}
	if len(parts) != 5 || parts[0]["text"] != "hello" || parts[1]["text"] != "file:///tmp/a" ||
		parts[2]["text"] != "embedded" || parts[3]["text"] != "file:///tmp/blob" {
		t.Fatalf("parts = %#v", parts)
	}
	if _, err := promptToOpenCodeParts(nil); err == nil {
		t.Fatal("empty prompt accepted")
	}
	if _, err := promptToOpenCodeParts([]acp.ContentBlock{{Audio: &acp.ContentBlockAudio{Type: "audio", Data: "AA==", MimeType: "audio/wav"}}}); err == nil {
		t.Fatal("audio prompt accepted")
	}
	req, ids := questionElicitationRequest(questionRequest{ID: "q", SessionID: "s"})
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
		client.sendMessage = func(_ context.Context, id string, req openCodeMessageRequest) (nativeMessage, error) {
			if id != "native-1" || req.MessageID != messageID || len(req.Parts) != 1 {
				t.Fatalf("SendMessage id=%q req=%#v", id, req)
			}
			msg := nativeMessage{Info: nativeMessageInfo{
				ID:        "assistant-1",
				SessionID: id,
				Role:      "assistant",
				Finish:    "length",
				Tokens:    nativeTokens{Total: 3, Input: 1, Output: 2},
			}}
			msg.Parts = []nativePart{{SessionID: id, MessageID: "assistant-1", Type: "text", Text: "hi"}}
			return msg, nil
		}
		resp, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: session.id, MessageId: &messageID, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
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
		client.sendMessage = func(context.Context, string, openCodeMessageRequest) (nativeMessage, error) {
			return nativeMessage{}, errors.New("send failed")
		}
		session := testSession(NewAgent(), client)
		if _, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}}); err == nil {
			t.Fatal("send error prompt succeeded")
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
		client.sendMessage = func(ctx context.Context, _ string, _ openCodeMessageRequest) (nativeMessage, error) {
			close(started)
			<-ctx.Done()
			return nativeMessage{}, ctx.Err()
		}
		session := testSession(NewAgent(), client)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan acp.PromptResponse, 1)
		go func() {
			resp, _ := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
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
		if _, err := agent.Prompt(ctx, acp.PromptRequest{SessionId: "missing"}); err == nil {
			t.Fatal("unknown agent prompt succeeded")
		}
		if err := agent.Cancel(ctx, acp.CancelNotification{SessionId: "missing"}); err == nil {
			t.Fatal("unknown agent cancel succeeded")
		}
	})
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
		client.sendMessage = func(_ context.Context, id string, _ openCodeMessageRequest) (nativeMessage, error) {
			close(started)
			<-release
			return nativeMessage{Info: nativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"}}, nil
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- err
		}()
		<-started
		client.events <- openCodeEvent{Type: "server.connected"}
		client.events <- openCodeEvent{
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
		client.sendMessage = func(ctx context.Context, _ string, _ openCodeMessageRequest) (nativeMessage, error) {
			close(started)
			<-ctx.Done()
			return nativeMessage{}, ctx.Err()
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
			done <- err
		}()
		<-started
		client.events <- openCodeEvent{
			Type:       "message.part.created",
			Properties: json.RawMessage(`{"id":"event-part","sessionID":"native-1","messageID":"assistant","type":"text","text":"stream"}`),
		}
		if err := <-done; err == nil || !strings.Contains(err.Error(), "update failed") {
			t.Fatalf("event error = %v", err)
		}
	})

	t.Run("final message update error returns", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		conn.updateErr = errors.New("final update failed")
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		client.sendMessage = func(_ context.Context, id string, _ openCodeMessageRequest) (nativeMessage, error) {
			return nativeMessage{
				Info:  nativeMessageInfo{ID: "assistant", SessionID: id, Role: "assistant", Finish: "stop"},
				Parts: []nativePart{{ID: "final", SessionID: id, MessageID: "assistant", Type: "text", Text: "done", Raw: json.RawMessage(`{"id":"final"}`)}},
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

	client.messages = []nativeMessage{{
		Info: nativeMessageInfo{ID: "user-1", SessionID: "native-1", Role: "user"},
		Parts: []nativePart{{
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
	if err := session.emitMessage(ctx, nativeMessage{Info: nativeMessageInfo{Role: "user"}}, false); err != nil {
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
	noConnSession.pending = nil
	if err := noConnSession.handlePermission(ctx, permissionRequest{ID: "p", SessionID: "native-1"}); err != nil {
		t.Fatalf("nil conn permission: %v", err)
	}
	if got := noConnClient.permissionReply(0).message; got != "client unavailable" {
		t.Fatalf("nil conn permission reply = %q", got)
	}
	session.questions = nil
	if err := session.handleQuestion(ctx, questionRequest{}); err != nil {
		t.Fatalf("empty question: %v", err)
	}
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	conn.elicitErr = errors.New("elicitation failed")
	if err := session.handleQuestion(ctx, questionRequest{ID: "q-error", SessionID: "native-1"}); err == nil {
		t.Fatal("elicitation error was ignored")
	}
	conn.elicitErr = nil
	if err := session.handleEvent(ctx, openCodeEvent{Type: "permission.v2.asked", Properties: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed permission event succeeded")
	}
	if err := session.handleEvent(ctx, openCodeEvent{Type: "todo.updated", Properties: json.RawMessage(`{"sessionID":"other","todos":[{"content":"x"}]}`)}); err != nil {
		t.Fatalf("foreign todo event: %v", err)
	}
	if err := session.handleEvent(ctx, openCodeEvent{Type: "question.asked", Properties: json.RawMessage(`{"request":{"id":"q","sessionID":"other"}}`)}); err != nil {
		t.Fatalf("foreign question event: %v", err)
	}
	if part, ok := eventPart(json.RawMessage(`{"part":{"type":"text","text":"x"}}`)); !ok || part.Text != "x" {
		t.Fatalf("wrapped eventPart = %#v ok=%v", part, ok)
	}
	if _, ok := eventPart(json.RawMessage(`{`)); ok {
		t.Fatal("malformed eventPart succeeded")
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"question":{"id":"q1","sessionID":"s"}}`),
		json.RawMessage(`{"data":{"id":"q2","sessionID":"s"}}`),
		json.RawMessage(`{`),
	} {
		eventQuestion(raw)
	}
	if err := session.emitPlan(ctx, []nativeTodo{{Content: ""}}); err != nil {
		t.Fatalf("empty plan: %v", err)
	}
	rawSession := testSession(NewAgent(), newFakeOpenCodeClient())
	rawSession.rawMessages = rawMessageConfig{enabled: true}
	if err := rawSession.emitRawOpenCodeEvent(ctx, openCodeEvent{Raw: json.RawMessage(`{"type":"x"}`)}); err != nil {
		t.Fatalf("raw event without conn: %v", err)
	}
	if usageFromTokens(nativeTokens{}) != nil {
		t.Fatal("empty tokens produced usage")
	}
	var emptyResource acp.EmbeddedResourceResource
	if got := embeddedResourceText(emptyResource); got != "" {
		t.Fatalf("empty embeddedResourceText = %q", got)
	}
	if updates := partUpdates("assistant", nativePart{Type: "text"}); updates != nil {
		t.Fatalf("empty text updates = %#v", updates)
	}
	if updates := partUpdates("assistant", nativePart{Type: "reasoning"}); updates != nil {
		t.Fatalf("empty reasoning updates = %#v", updates)
	}
}

func TestPromptRemainingErrorBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("send error after cancelled state", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		client.sendMessage = func(context.Context, string, openCodeMessageRequest) (nativeMessage, error) {
			session.mu.Lock()
			session.cancelled = true
			session.mu.Unlock()
			return nativeMessage{}, errors.New("cancelled send")
		}
		resp, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.TextBlock("hello")}})
		if err != nil || resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("cancelled send resp=%#v err=%v", resp, err)
		}
	})

	t.Run("successful result marked cancelled", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		client.sendMessage = func(context.Context, string, openCodeMessageRequest) (nativeMessage, error) {
			session.mu.Lock()
			session.cancelled = true
			session.mu.Unlock()
			return nativeMessage{Info: nativeMessageInfo{ID: "assistant", SessionID: "native-1", Role: "assistant", Finish: "stop"}}, nil
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
		client.messages = []nativeMessage{{
			Info: nativeMessageInfo{ID: "user", SessionID: "native-1", Role: "user"},
			Parts: []nativePart{{
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
		if err := session.emitMessage(ctx, nativeMessage{
			Info: nativeMessageInfo{ID: "assistant", SessionID: "native-1", Role: "assistant"},
			Parts: []nativePart{{
				ID:        "usage",
				SessionID: "native-1",
				MessageID: "assistant",
				Type:      "step-finish",
				Tokens:    nativeTokens{Total: 1},
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
		part := nativePart{ID: "dup", SessionID: "native-1", MessageID: "assistant", Type: "text", Text: "hello"}
		if err := session.emitMessage(ctx, nativeMessage{Info: nativeMessageInfo{ID: "assistant", Role: "assistant"}, Parts: []nativePart{part, part}}, false); err != nil {
			t.Fatalf("duplicate emitMessage: %v", err)
		}
		conn.notifyErr = errors.New("notify failed")
		session.rawMessages = rawMessageConfig{enabled: true}
		if err := session.handleEvent(ctx, openCodeEvent{Type: "unknown", Raw: json.RawMessage(`{"type":"unknown"}`)}); err == nil {
			t.Fatal("handleEvent ignored raw notify error")
		}
	})

	t.Run("same-session events and reconcile errors", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		conn := newRecordingAgentClient()
		agent := NewAgent()
		agent.setAgentClient(conn)
		session := testSession(agent, client)
		conn.permErr = errors.New("permission failed")
		if err := session.handleEvent(ctx, openCodeEvent{
			Type:       "permission.v2.asked",
			Properties: json.RawMessage(`{"id":"p","sessionID":"native-1"}`),
		}); err == nil {
			t.Fatal("permission event ignored client error")
		}
		client.pendingPermissions = []permissionRequest{{ID: "p2", SessionID: "native-1"}}
		if err := session.reconcilePermissions(ctx); err == nil {
			t.Fatal("reconcilePermissions ignored handle error")
		}
		conn.permErr = nil

		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		conn.elicitErr = errors.New("elicitation failed")
		if err := session.handleEvent(ctx, openCodeEvent{
			Type:       "question.asked",
			Properties: json.RawMessage(`{"id":"q","sessionID":"native-1"}`),
		}); err == nil {
			t.Fatal("question event ignored client error")
		}
		client.pendingQuestions = []questionRequest{{ID: "q2", SessionID: "native-1"}}
		if err := session.reconcileQuestions(ctx); err == nil {
			t.Fatal("reconcileQuestions ignored handle error")
		}
		if req, ok := eventQuestion(json.RawMessage(`{"id":"direct","sessionID":"native-1"}`)); !ok || req.ID != "direct" {
			t.Fatalf("direct eventQuestion = %#v ok=%v", req, ok)
		}
	})
}

func eventFromJSON(t *testing.T, raw string) openCodeEvent {
	t.Helper()
	var event openCodeEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	return event
}
