//go:build integration

package integration

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/wire"
	opencodeacp "github.com/savid/acp-go-opencode"
	"github.com/stretchr/testify/require"
)

// TestNativePersistence exercises native creation, import, and delete without
// model calls.
func TestNativePersistence(t *testing.T) {
	if os.Getenv("ACP_GO_OPENCODE_RUN_INTEGRATION") != "1" {
		t.Skip("set ACP_GO_OPENCODE_RUN_INTEGRATION=1")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not installed on PATH")
	}
	store := acpcore.NewInMemorySessionStore()
	a := opencodeacp.NewAgent(opencodeacp.WithHome(t.TempDir()), opencodeacp.WithSessionStore(store))
	t.Cleanup(func() { _ = a.Close() })
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	cwd := t.TempDir()
	// A carrier larger than a pipe buffer must survive the native database
	// command's output and a restore into an empty home.
	marker := strings.Repeat("history-payload-", 8192)
	options := opencodeacp.NewOpenCodeOptions(opencodeacp.WithOpenCodeEnv(map[string]string{"HISTORY_PROBE": marker}))
	session, err := a.NewSession(t.Context(), wire.NewSessionRequest(cwd, opencodeacp.WithSessionOpenCodeOptions(options)))
	require.NoError(t, err)
	require.NotEmpty(t, session.SessionId)
	rows, err := store.Load(t.Context(), string(session.SessionId))
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	var mirrored bytes.Buffer
	for _, row := range rows[""] {
		mirrored.Write(row)
	}
	require.Contains(t, mirrored.String(), marker)
	require.NoError(t, a.Close())
	b := opencodeacp.NewAgent(opencodeacp.WithHome(t.TempDir()), opencodeacp.WithSessionStore(store))
	t.Cleanup(func() { _ = b.Close() })
	_, err = b.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	_, err = b.LoadSession(t.Context(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	_, err = b.UnstableDeleteSession(t.Context(), wire.DeleteSessionRequest(session.SessionId))
	require.NoError(t, err)
	list, err := b.ListSessions(t.Context(), wire.ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
	_, err = b.LoadSession(t.Context(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.Error(t, err)
	require.NoError(t, b.Close())
}
func TestNativeContinuation(t *testing.T) {
	if os.Getenv("ACP_GO_OPENCODE_RUN_LIVE_TOKENS") != "1" {
		t.Skip("live token gate")
	}
	home, cwd := nativeHome(t), t.TempDir()
	store := acpcore.NewInMemorySessionStore()
	opts := []opencodeacp.Option{opencodeacp.WithHome(home), opencodeacp.WithSessionStore(store)}
	if model := os.Getenv("ACP_GO_OPENCODE_MODEL"); model != "" {
		opts = append(opts, opencodeacp.WithDefaultModel(model))
	}
	h := newHarness(t, opts...)
	h.initialize(withLifecycle())
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)
	response, err := h.prompt(session.SessionId, "Remember the project slug apricot-orbit. Reply with exactly apricot-orbit and nothing else. Do not use tools.", promptMeta(1))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.Contains(t, agentText(h.rec.snapshot()), "apricot-orbit")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	h.stop()
	command := exec.CommandContext(h.ctx(), "opencode", "run", "--dir", cwd, "--format", "json", "--session", nativeSessionID(t, session.Meta), "Remember the release label cobalt-lantern. Reply with the project slug and release label, and nothing else. Do not use tools.")
	command.Args = append(command.Args, "--model", os.Getenv("ACP_GO_OPENCODE_MODEL"))
	command.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Dir = cwd
	command.Env = nativeEnvironment(home)
	data, err := command.Output()
	require.NoError(t, err, "native resume: %s", stderr.String())
	require.Contains(t, string(data), "apricot-orbit")
	require.Contains(t, string(data), "cobalt-lantern")
	restored := newHarness(t, opts...)
	restored.initialize(withLifecycle())
	_, err = restored.conn.LoadSession(restored.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.Contains(t, agentText(restored.rec.snapshot()), "apricot-orbit")
	require.Contains(t, agentText(restored.rec.snapshot()), "cobalt-lantern")
	_, err = restored.prompt(session.SessionId, "What project slug and release label did we choose? Do not use tools.", promptMeta(2))
	require.NoError(t, err)
	require.Contains(t, agentText(restored.rec.snapshot()), "apricot-orbit")
	require.Contains(t, agentText(restored.rec.snapshot()), "cobalt-lantern")
	_, err = restored.conn.CloseSession(restored.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	restored.stop()
	imported := newHarness(t, opencodeacp.WithHome(nativeHome(t)), opencodeacp.WithSessionStore(store))
	imported.initialize(withLifecycle())
	_, err = imported.conn.LoadSession(imported.ctx(), wire.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	require.Contains(t, agentText(imported.rec.snapshot()), "cobalt-lantern")
	_, err = imported.prompt(session.SessionId, "What project slug and release label did we choose? Do not use tools.", promptMeta(3))
	require.NoError(t, err)
	require.Contains(t, agentText(imported.rec.snapshot()), "apricot-orbit")
	require.Contains(t, agentText(imported.rec.snapshot()), "cobalt-lantern")
}

func nativeEnvironment(home string) []string {
	env := os.Environ()
	for key, subdir := range map[string]string{"XDG_DATA_HOME": "data", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state"} {
		env = append(env, key+"="+filepath.Join(home, subdir))
	}

	return env
}
func nativeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	data := filepath.Join(home, "data", "opencode")
	require.NoError(t, os.MkdirAll(data, 0o700))
	original, err := os.UserHomeDir()
	require.NoError(t, err)
	auth, err := os.ReadFile(filepath.Join(original, ".local", "share", "opencode", "auth.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(data, "auth.json"), auth, 0o600))

	return home
}
func TestNativeCallbacksPathAndCancellation(t *testing.T) {
	if os.Getenv("ACP_GO_OPENCODE_RUN_LIVE_TOKENS") != "1" {
		t.Skip("live token gate")
	}
	home, cwd := nativeHome(t), t.TempDir()
	directories := []string{t.TempDir(), t.TempDir()}
	for index, dir := range directories {
		script := "#!/bin/sh\nprintf '%s\\n' 'marker-" + strconv.Itoa(index) + "'\nprintf 'ACP_PATH=%s\\n' \"$PATH\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "acpgogo-native-probe"), []byte(script), 0700))
	}
	h := newHarness(t, opencodeacp.WithHome(home), opencodeacp.WithDefaultModel(os.Getenv("ACP_GO_OPENCODE_MODEL")))
	var questions atomic.Int32
	h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
		questions.Add(1)

		return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"0": "cobalt"}}}, nil
	}
	h.initialize(withLifecycle(), withFormElicitation())
	session, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd, opencodeacp.WithSessionRawEvents(true), opencodeacp.WithSessionOpenCodeOptions(opencodeacp.NewOpenCodeOptions(opencodeacp.WithOpenCodeExtraPathDirs(directories[0])))))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "remove-me.txt"), []byte("test-only"), 0600))
	response, err := h.prompt(session.SessionId, "Use the bash tool to run exactly: rm remove-me.txt. Do not use any other tool. Reply DONE when finished.", promptMeta(1))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.NoFileExists(t, filepath.Join(cwd, "remove-me.txt"))
	h.rec.mu.Lock()
	permissions := len(h.rec.permissions)
	raw := len(h.rec.raw)
	h.rec.mu.Unlock()

	require.Positive(t, permissions)
	require.Positive(t, raw)
	_, err = h.prompt(session.SessionId, "Use the question tool to ask one question: Which color? Offer cobalt and silver. After receiving my answer, reply with that color. Do not use any other tool.", promptMeta(2))
	require.NoError(t, err)
	require.EqualValues(t, 1, questions.Load())
	for index, dir := range directories {
		if index > 0 {
			_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
			require.NoError(t, err)
			_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(session.SessionId, cwd, opencodeacp.WithSessionRawEvents(true), opencodeacp.WithSessionOpenCodeOptions(opencodeacp.NewOpenCodeOptions(opencodeacp.WithOpenCodeExtraPathDirs(dir)))))
			require.NoError(t, err)
		}
		before := len(h.rec.snapshot())
		_, err = h.prompt(session.SessionId, "Use the bash tool to run the exact command acpgogo-native-probe. Do not set PATH, use an absolute command path, or run other commands. Reply DONE after it runs.", promptMeta(index+3))
		require.NoError(t, err)
		output := toolText(h.rec.snapshot()[before:])
		require.Contains(t, output, "marker-"+strconv.Itoa(index))
		require.Contains(t, output, "ACP_PATH="+dir+string(os.PathListSeparator))
		if index > 0 {
			require.NotContains(t, output, directories[0])
		}
	}
	done := make(chan acp.PromptResponse, 1)
	failed := make(chan error, 1)
	go func() {
		response, promptErr := h.prompt(session.SessionId, "Use the bash tool to run exactly: printf started > sleep-started; sleep 60. Do not use a timeout or run it in the background. Reply DONE when it finishes.", promptMeta(5))
		done <- response
		failed <- promptErr
	}()
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(cwd, "sleep-started"))

		return statErr == nil
	}, 45*time.Second, 25*time.Millisecond)
	require.NoError(t, h.conn.Cancel(h.ctx(), acp.CancelNotification{SessionId: session.SessionId}))
	require.NoError(t, <-failed)
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
}

func toolText(updates []acp.SessionNotification) string {
	var text strings.Builder
	for _, update := range updates {
		if tool := update.Update.ToolCallUpdate; tool != nil {
			for _, item := range tool.Content {
				if item.Content != nil && item.Content.Content.Text != nil {
					text.WriteString(item.Content.Content.Text.Text)
				}
			}
		}
	}

	return text.String()
}

func nativeSessionID(t *testing.T, meta map[string]any) string {
	t.Helper()
	binding, ok := meta["opencode"].(map[string]any)
	require.True(t, ok)
	id, ok := binding["nativeSessionId"].(string)
	require.True(t, ok)
	require.NotEmpty(t, id)

	return id
}
