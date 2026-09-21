package opencodeacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	stdimage "image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-core/wire"
)

func promptRaster(t *testing.T) string {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 1, 1))))

	return base64.StdEncoding.EncodeToString(b.Bytes())
}
func TestImageInputOrder(t *testing.T) {
	s := &session{agent: NewAgent()}
	blocks := []acp.ContentBlock{acp.TextBlock("before"), acp.ImageBlock(promptRaster(t), "image/png"), acp.TextBlock("after")}
	mapped, err := s.mapPrompt(t.Context(), blocks)
	require.NoError(t, err)
	_, request := s.promptRequest(mapped, "msg_1")
	data, err := json.Marshal(request)
	require.NoError(t, err)
	var native struct {
		Parts []map[string]any `json:"parts"`
	}
	require.NoError(t, json.Unmarshal(data, &native))
	kinds := make([]string, 0, len(native.Parts))
	for _, part := range native.Parts {
		kind, ok := part["type"].(string)
		require.True(t, ok)
		kinds = append(kinds, kind)
	}
	require.Equal(t, []string{"text", "file", "text"}, kinds)
}

// A turn is the session's before the prompt has anything to dispatch, so a
// session/cancel that lands while opencode is still being relaunched ends it
// there: the prompt answers cancelled, opencode never receives the turn, and
// the lifecycle stream carries nothing for it.
func TestPromptCancelledWhileRelaunching(t *testing.T) {
	t.Parallel()

	held := filepath.Join(t.TempDir(), "relaunch-held")
	h := newHarness(t, WithEnv(map[string]string{fakeOpenCodeEnv: "1", fakeOpenCodeEnvResumeHold: held}))
	h.initialize(withLifecycle())
	session := h.newSession()

	// The server dies mid-turn, so the next prompt starts a replacement and
	// looks the session up on it; the replacement never answers that lookup.
	_, err := h.prompt(session.SessionId, "CRASH", promptMeta(1))
	require.Equal(t, "process_exit", requestErrorData(t, err)["cause"])

	before := eventTypes(lifecycleEvents(h.rec.snapshot()))

	type result struct {
		resp acp.PromptResponse
		err  error
	}

	done := make(chan result, 1)

	go func() {
		resp, promptErr := h.prompt(session.SessionId, "HELLO", promptMeta(2))
		done <- result{resp, promptErr}
	}()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(held)

		return statErr == nil
	}, testTimeout, time.Millisecond)

	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))

	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, acp.StopReasonCancelled, got.resp.StopReason)
	require.Equal(t, before, eventTypes(lifecycleEvents(h.rec.snapshot())),
		"a prompt opencode never received opens no incarnation and publishes no acceptance")
}

// A $/cancel_request ends only the addressed handler's context: the turn it
// was driving stays the session's, completes successfully once, and the
// session keeps serving prompts.
func TestCancelRequestSettlesTheOriginalRequestOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	permissionCtx, releasePermission := context.WithCancel(t.Context())
	defer releasePermission()
	entered := make(chan struct{}, 1)
	answer := h.rec.answer
	h.rec.answer = func(request acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		entered <- struct{}{}
		<-permissionCtx.Done()

		return answer(request)
	}
	h.initialize(withLifecycle())
	session := h.newSession()

	request := wire.TextPromptRequest(session.SessionId, "PERMISSION")
	request.Meta = promptMeta(1)
	failed := make(chan error, 1)

	go func() {
		response, err := h.conn.Prompt(h.ctx(), request)
		if err == nil && response.StopReason != acp.StopReasonEndTurn {
			err = errors.New("request cancellation ended the native turn")
		}
		failed <- err
	}()

	select {
	case <-entered:
	case <-h.ctx().Done():
		t.Fatal("native permission request did not arrive")
	}
	require.NoError(t, h.input.cancelPrompt())

	_, busyErr := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.Equal(t, "backpressure", requestErrorData(t, busyErr)["error"])
	releasePermission()
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		return slices.Contains(eventTypes(lifecycleEvents(updates)), "state_update:idle")
	})

	idles := 0

	for _, update := range h.rec.snapshot() {
		envelope, _ := update.Meta[wire.LifecycleKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "state_update" && event["state"] == "idle" {
			idles++
			require.Equal(t, "success", event["outcome"])
			require.Equal(t, string(acp.StopReasonEndTurn), event["stopReason"])
		}
	}

	require.Equal(t, 1, idles, "the turn the cancelled request started settles exactly once")
	require.NoError(t, <-failed)

	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(3))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
}

// Handler cancellation can precede native dispatch; only session cancellation owns the turn.
func TestPromptOwnsCancellationBeforeDispatch(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(newRecorder(), nil)
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response, err := a.Prompt(ctx, wire.TextPromptRequest(created.SessionId, "HELLO"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}
