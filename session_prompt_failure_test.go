package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

// providerNativeError builds a native assistant provider error with the given
// detail, HTTP status, and provider code.
func providerNativeError(detail string, status int, code string) *opencode.NativeError {
	e := &opencode.NativeError{Name: "APIError"}
	e.Data.Message = detail
	e.Data.StatusCode = status
	e.Data.ResponseBody = `{"error":{"code":"` + code + `"}}`

	return e
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
	if err := agent.Cancel(ctx, acp.CancelNotification{SessionId: session.id}); err != nil {
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
}

// T6 — with WithTurnTimeout set, a hanging native turn fails with cause
// "timeout" (not cancelled) and the native turn is aborted.
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
