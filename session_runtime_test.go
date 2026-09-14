package opencodeacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/stretchr/testify/require"
)

func TestSharedRuntimeAndFreshHomeRestore(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	a := NewAgent(testOptions(t, WithSessionStore(store))...)
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	first, err := a.NewSession(t.Context(), NewSessionRequest(cwd))
	require.NoError(t, err)
	second, err := a.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	one, err := a.session(t.Context(), first.SessionId)
	require.NoError(t, err)
	two, err := a.session(t.Context(), second.SessionId)
	require.NoError(t, err)
	require.Same(t, one.runtime.server, two.runtime.server)
	response, err := a.Prompt(t.Context(), TextPromptRequest(first.SessionId, "remember"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	_, err = a.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	_, err = a.Prompt(t.Context(), TextPromptRequest(second.SessionId, "peer"))
	require.NoError(t, err)
	require.NoError(t, a.Close())
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(first.SessionId, cwd))
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "hello remember")
	_, err = h.prompt(first.SessionId, "restored", nil)
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "hello restored")
}
func TestNativeCallbacksHaveToolIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize(withLifecycle(), withFormElicitation())
	h.rec.elicit = func(request acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
		require.NotNil(t, request.Form)
		require.Contains(t, request.Form.RequestedSchema.Properties, "0")

		return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"0": "blue"}}}, nil
	}
	session := h.newSession(WithSessionRawEvents(true))
	for i, text := range []string{"PERMISSION", "QUESTION"} {
		_, err := h.prompt(session.SessionId, text, promptMeta(i))
		require.NoError(t, err)
	}
	h.rec.mu.Lock()
	permissions := append([]acp.RequestPermissionRequest(nil), h.rec.permissions...)
	raw := len(h.rec.raw)
	h.rec.mu.Unlock()
	require.Len(t, permissions, 1)
	require.Positive(t, raw)
	callID := permissions[0].ToolCall.ToolCallId
	found := false
	for _, notification := range h.rec.snapshot() {
		if call := notification.Update.ToolCall; call != nil && call.ToolCallId == callID {
			found = true
		}
	}
	require.True(t, found, "permission references an emitted native tool")
	events := lifecycleEvents(h.rec.snapshot())
	count := 0
	for _, event := range events {
		if event["type"] == "action_update" {
			count++
		}
	}
	require.Equal(t, 4, count)
}
func TestEnvironmentAndStructuredOptionsSurviveRestore(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	cwd := t.TempDir()
	dir := filepath.Join(t.TempDir(), "bin")
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	options := NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{"PATH": "/usr/bin", "CUSTOM": "one"}), WithOpenCodeExtraPathDirs(dir), WithOpenCodeMode("plan"), WithOpenCodePermission("allow"), WithOpenCodeOutputSchema(map[string]any{"type": "object"}), WithOpenCodeEffort("high"))
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd, WithSessionOpenCodeOptions(options)))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "ENV", nil)
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "CUSTOM")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, "plan", record.Mode)
	require.Equal(t, "high", record.Effort)
	require.Equal(t, "allow", record.Permission)
	require.Equal(t, options.Env, record.Env)
	require.Equal(t, []string{dir}, record.ExtraPathDirs)
	require.NotNil(t, record.OutputSchema)
}
func TestRuntimeDeathRebindsPeers(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t, WithLogger(slog.Default()))...)
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	one, err := a.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	two, err := a.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	old := a.runtime
	_, err = a.Prompt(t.Context(), TextPromptRequest(one.SessionId, "CRASH"))
	require.Error(t, err)
	require.Equal(t, "process_exit", requestErrorData(t, err)["cause"])
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = a.Prompt(ctx, TextPromptRequest(two.SessionId, "peer survived"))
	require.NoError(t, err)
	require.NotSame(t, old, a.runtime)
	_, err = a.Prompt(ctx, TextPromptRequest(one.SessionId, "first survived"))
	require.NoError(t, err)
}

func TestImageReplaySurvivesNativeFileRemoval(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	h.initialize()
	cwd := t.TempDir()
	session, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd, WithSessionRawEvents(true)))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "IMAGE", nil)
	require.NoError(t, err)
	var original string
	for _, notification := range h.rec.snapshot() {
		if chunk := notification.Update.AgentMessageChunk; chunk != nil && chunk.Content.Image != nil {
			original = chunk.Content.Image.Data
		}
	}
	require.NotEmpty(t, original)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(cwd, "output.png")))
	restored := newHarness(t, WithSessionStore(store))
	restored.initialize()
	_, err = restored.conn.LoadSession(restored.ctx(), LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	var replayed string
	for _, notification := range restored.rec.snapshot() {
		if chunk := notification.Update.AgentMessageChunk; chunk != nil && chunk.Content.Image != nil {
			replayed = chunk.Content.Image.Data
		}
	}
	require.Equal(t, original, replayed)
}
