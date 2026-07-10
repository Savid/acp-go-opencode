package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

func TestTurnFenceHelperBranches(t *testing.T) {
	session := testSession(NewAgent(), newFakeOpenCodeClient())
	if !session.claimPermissionRequest("") || !session.claimQuestionRequest("") {
		t.Fatal("empty request ids should not be fenced")
	}
	session.processedPermission = nil
	session.processedQuestion = nil
	if !session.claimPermissionRequest("perm") || !session.claimQuestionRequest("question") {
		t.Fatal("nil processed request maps were not initialized")
	}
	session.markActiveMessageID("")
	session.activeMessageIDs = nil
	session.markActiveMessageID("message-1")
	session.failedMessageIDs = nil
	session.failedStreamEpochs = nil
	session.markStreamFailed(9)
	if !session.shouldSuppressEvent(opencode.Event{StreamEpoch: 9}) {
		t.Fatal("failed stream epoch was not suppressed")
	}
	if !session.shouldSuppressEvent(opencode.Event{
		Properties: json.RawMessage(`{"sessionID":"native-1","messageID":"message-1","type":"text","text":"late"}`),
	}) {
		t.Fatal("failed message id was not suppressed")
	}
	if session.shouldSuppressEvent(opencode.Event{
		Properties: json.RawMessage(`{"sessionID":"native-1","messageID":"message-2","type":"text","text":"ok"}`),
	}) {
		t.Fatal("unfailed message id was suppressed")
	}
	if err := session.handleEvent(context.Background(), opencode.Event{
		StreamEpoch: 9,
		Properties:  json.RawMessage(`{"sessionID":"native-1","messageID":"message-1","type":"text","text":"late"}`),
	}); err != nil {
		t.Fatalf("suppressed handleEvent: %v", err)
	}
}

func TestSessionContextWindow(t *testing.T) {
	ctx := context.Background()

	t.Run("caches lookups per model", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		session := testSession(NewAgent(), client)
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 1000 {
			t.Fatalf("first lookup = %d, want 1000", got)
		}
		client.providersErr = errors.New("boom")
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 1000 {
			t.Fatalf("cached lookup = %d, want 1000 (must not re-fetch)", got)
		}
	})

	t.Run("provider error reports unknown", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.providersErr = errors.New("boom")
		session := testSession(NewAgent(), client)
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 0 {
			t.Fatalf("provider error lookup = %d, want 0", got)
		}
	})

	t.Run("nil client reports unknown", func(t *testing.T) {
		session := testSession(NewAgent(), newFakeOpenCodeClient())
		session.client = nil
		if got := session.contextWindow(ctx, "openai", "gpt-test"); got != 0 {
			t.Fatalf("nil client lookup = %d, want 0", got)
		}
	})
}

func TestModelValueSplitAndJoin(t *testing.T) {
	if splitProvider, splitModel := splitModelValue("model-only", "p", "m"); splitProvider != "p" || splitModel != "model-only" {
		t.Fatalf("split fallback = %q %q", splitProvider, splitModel)
	}
	if joinModelValue("", "m") != "m" || joinModelValue("p", "") != "p" {
		t.Fatal("joinModelValue fallback mismatch")
	}
}
