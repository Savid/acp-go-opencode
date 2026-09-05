package opencodeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestAgentOwnsOneSharedRuntimeForManyDirectories(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	var created atomic.Int64
	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession(fmt.Sprintf("native-%d", created.Add(1))), nil
	}
	client.getSession = testNativeSession("native-1")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	var factoryCalls atomic.Int64
	agent := NewAgent(
		WithHome(t.TempDir()),
		func(options *Options) {
			options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
				factoryCalls.Add(1)

				return client, nil
			}
		},
	)
	agent.setAgentClient(newRecordingAgentClient())

	first, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(
		NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test"), WithOpenCodePermission("deny")),
	)))
	require.NoError(t, err)
	second, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(
		NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test"), WithOpenCodePermission("allow")),
	)))
	require.NoError(t, err)
	require.NotEqual(t, first.SessionId, second.SessionId)
	require.EqualValues(t, 1, factoryCalls.Load())
	require.Len(t, agent.sessions, 2)

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	require.Len(t, agent.sessions, 1)
	require.NotNil(t, agent.runtime, "session close must not close the shared runtime")
	require.NoError(t, agent.Close())
}

func TestDirectoryMCPPrincipalFailsClosed(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession(fmt.Sprintf("native-%d", len(client.syncEvents)+1)), nil
	}
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) { return client, nil }
	})

	mcp := HTTPMCPServer("gateway", "http://127.0.0.1:9/mcp", map[string]string{"Authorization": "Bearer one"})
	_, err := agent.NewSession(ctx, NewSessionRequest(cwd, WithSessionMCPServers(mcp),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test")))))
	require.NoError(t, err)
	_, err = agent.NewSession(ctx, NewSessionRequest(cwd, WithSessionMCPServers(mcp),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test")))))
	require.ErrorContains(t, err, "directory_mcp_principal")

	other := HTTPMCPServer("gateway", "http://127.0.0.1:9/mcp", map[string]string{"Authorization": "Bearer two"})
	_, err = agent.NewSession(ctx, NewSessionRequest(cwd, WithSessionMCPServers(other),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test")))))
	require.ErrorContains(t, err, "mcp_principal_conflict")
}

func TestDirectoryWithoutExplicitMCPStillHasOneSessionPrincipal(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-one")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) { return client, nil }
	})

	_, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = agent.NewSession(ctx, NewSessionRequest(cwd))
	require.ErrorContains(t, err, "directory_mcp_principal")
}

func TestLifecycleMCPRefreshesImmediatelyBeforeFirstNativePrompt(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-refresh")
	client.agents = []opencode.NativeAgent{{Name: "build"}}

	var (
		orderMu sync.Mutex
		order   []string
	)

	client.refreshMCPFunc = func(_ context.Context, servers []opencode.MCPServerConfig) error {
		require.Equal(t, []opencode.MCPServerConfig{{
			Name:    "wagie",
			URL:     "http://127.0.0.1:9/mcp",
			Headers: map[string]string{"Authorization": "Bearer session-grant"},
		}}, servers)

		orderMu.Lock()
		order = append(order, "refresh")
		orderMu.Unlock()

		return nil
	}
	client.dispatchMessage = func(_ context.Context, id string, _ opencode.MessageRequest) (opencode.NativeMessage, error) {
		orderMu.Lock()
		order = append(order, "prompt")
		orderMu.Unlock()

		return opencode.NativeMessage{Info: opencode.NativeMessageInfo{
			ID: "assistant-refresh", SessionID: id, Role: "assistant", Finish: "stop",
		}}, nil
	}

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(cwd,
		WithSessionMCPServers(HTTPMCPServer(
			"wagie",
			"http://127.0.0.1:9/mcp",
			map[string]string{"Authorization": "Bearer session-grant"},
		)),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test"))),
	))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)
	require.Empty(t, order, "lifecycle must not freeze the pre-arm catalog as prompt-ready")

	_, err = agent.Prompt(ctx, TextPromptRequest(created.SessionId, "refresh-turn", "use the armed tool"))
	require.NoError(t, err)
	require.Equal(t, []string{"refresh", "prompt"}, order)

	_, err = agent.Prompt(ctx, TextPromptRequest(created.SessionId, "second-turn", "continue"))
	require.NoError(t, err)
	require.Equal(t, []string{"refresh", "prompt", "prompt"}, order,
		"one lifecycle must refresh exactly once")

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Empty(t, agent.directories, "successful native MCP teardown must release the directory principal")
}

func TestLifecycleMCPRefreshFailureBlocksPromptAndRetainsPrincipal(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-refresh-failure")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	client.refreshMCPErr = errors.Join(opencode.ErrMCPDisconnectUnproven, errors.New("delete failed"))
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		t.Fatal("native prompt ran with a stale MCP catalog")

		return opencode.NativeMessage{}, nil
	}

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())
	mcp := HTTPMCPServer("wagie", "http://127.0.0.1:9/mcp", nil)

	created, err := agent.NewSession(ctx, NewSessionRequest(cwd,
		WithSessionMCPServers(mcp),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test"))),
	))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)

	_, err = agent.Prompt(ctx, TextPromptRequest(created.SessionId, "failed-refresh", "do not run"))
	assertTurnFailed(t, err, causeTransport, "")

	_, err = agent.NewSession(ctx, NewSessionRequest(cwd,
		WithSessionMCPServers(mcp),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test"))),
	))
	require.ErrorContains(t, err, "directory_mcp_principal",
		"failed refresh must retain the directory principal until native teardown is proven")

	client.closeErr = nil
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.Empty(t, agent.directories)
}

func TestCloseSessionRetainsPrincipalUntilNativeScopeCloseSucceeds(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	current := testSession(t, agent, client)
	releases := 0
	current.directoryRelease = func() { releases++ }
	agent.sessions[current.id] = current
	client.closeErr = errors.New("MCP disconnect unproven")

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorContains(t, err, "MCP disconnect unproven")
	require.Same(t, current, agent.sessions[current.id])
	require.Zero(t, releases)

	client.closeErr = nil
	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.NoError(t, err)
	require.NotContains(t, agent.sessions, current.id)
	require.Equal(t, 1, releases)
}

// TestCancellationPublishesInterruptedPrefixWithoutRetiringTheRuntime proves
// routine cancellation is session-scoped: the addressed native session is
// interrupted, its interrupted prefix is committed durably before the turn
// settles, and the shared runtime keeps running.
func TestCancellationPublishesInterruptedPrefixWithoutRetiringTheRuntime(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newFakeOpenCodeClient()
	agent := negotiatedAgent(t, WithHome(t.TempDir()), WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	require.NoError(t, current.establish(ctx))

	committed := validSyncSnapshot(string(current.id), current.idmap.NativeSessionID, current.cwd)
	committedAssistant := terminalMessageEvent(
		current.idmap.NativeSessionID, 1, "assistant-before-interrupt", "assistant", "stop", nil,
	)
	committedAssistant.ID = "event-assistant-before-interrupt"
	committed.Events[current.idmap.NativeSessionID] = append(
		committed.Events[current.idmap.NativeSessionID], committedAssistant,
	)
	interruptedUser := terminalMessageEvent(
		current.idmap.NativeSessionID, 2, "user-interrupted", "user", "", nil,
	)
	interruptedUser.ID = "event-user-interrupted"
	interruptedAssistant := terminalMessageEvent(
		current.idmap.NativeSessionID, 3, "assistant-interrupted", "assistant", "", nil,
	)
	interruptedAssistant.ID = "event-assistant-interrupted"
	client.syncEvents = append(
		append([]opencode.SyncEvent(nil), committed.Events[current.idmap.NativeSessionID]...),
		interruptedUser,
		interruptedAssistant,
	)
	entry, err := json.Marshal(committed)
	require.NoError(t, err)
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(current.id)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(current.id), Subpath: SessionStoreMainSubpath},
		Entries: []SessionStoreEntry{entry},
	}}))

	// The native session reports idle when the interrupt lands, which is the
	// acknowledgement the cancelled turn settles on.
	started := make(chan struct{})
	accepted := make(chan struct{}, 1)
	connection.mu.Lock()
	connection.updateHook = func(notification acp.SessionNotification) {
		envelope, _ := notification.Meta[lifecycle.MetaKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "prompt_accepted" {
			signalTestHook(accepted)
		}
	}
	connection.mu.Unlock()
	client.hangsAfterDispatch(started)
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}

	done := make(chan promptResult, 1)

	go func() {
		response, promptErr := current.Prompt(ctx, TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		done <- promptResult{response: response, err: promptErr}
	}()

	<-started
	<-accepted
	require.NoError(t, agent.Cancel(ctx, CancelRequest(current.id, internalSeamTurnNonce)))
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)

	require.Equal(t, []string{current.idmap.NativeSessionID}, client.abortedSessions())
	require.NotNil(t, agent.runtime, "routine cancellation retired the shared runtime")
	require.NoError(t, current.ensureNotPoisoned())

	captured, err := store.Load(ctx, SessionKey{SessionID: string(current.id), Subpath: SessionStoreMainSubpath})
	require.NoError(t, err)
	require.Len(t, captured, 1)
	require.NotEqual(t, SessionStoreEntry(entry), captured[0])

	var interrupted stateSnapshot
	require.NoError(t, json.Unmarshal(captured[0], &interrupted))
	require.Equal(t, []opencode.SyncEvent{
		client.syncEvents[0], client.syncEvents[1], interruptedUser, interruptedAssistant,
	}, interrupted.Events[current.idmap.NativeSessionID])

	// The terminal idle is the last lifecycle event of the cancelled turn, and it
	// is emitted after the commit above.
	requireLifecycleOutcome(t, connection, lifecycle.OutcomeCancelled)
}

// TestCancellationCaptureFailureFencesInsteadOfSettling proves a cancelled turn
// whose native-safe prefix cannot be committed reports a settlement failure
// instead of an idle the store cannot back, keeps the prior checkpoint, and still
// leaves the shared runtime alone.
func TestCancellationCaptureFailureFencesInsteadOfSettling(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	client := newFakeOpenCodeClient()
	agent := negotiatedAgent(t, WithHome(t.TempDir()), WithSessionStore(store))
	connection := newRecordingAgentClient()
	agent.setAgentClient(connection)
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	require.NoError(t, current.establish(ctx))

	committed := validSyncSnapshot(string(current.id), current.idmap.NativeSessionID, current.cwd)
	entry, err := json.Marshal(committed)
	require.NoError(t, err)
	require.NoError(t, store.Replace(ctx, SessionKey{SessionID: string(current.id)}, []SessionStoreReplacement{{
		Key:     SessionKey{SessionID: string(current.id), Subpath: SessionStoreMainSubpath},
		Entries: []SessionStoreEntry{entry},
	}}))
	client.syncHistoryErr = errors.New("sync history unavailable")

	started := make(chan struct{})
	accepted := make(chan struct{}, 1)
	connection.mu.Lock()
	connection.updateHook = func(notification acp.SessionNotification) {
		envelope, _ := notification.Meta[lifecycle.MetaKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "prompt_accepted" {
			signalTestHook(accepted)
		}
	}
	connection.mu.Unlock()
	client.hangsAfterDispatch(started)
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}

	done := make(chan error, 1)

	go func() {
		_, promptErr := current.Prompt(ctx, TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		done <- promptErr
	}()

	<-started
	<-accepted
	require.NoError(t, agent.Cancel(ctx, CancelRequest(current.id, internalSeamTurnNonce)))
	promptErr := <-done
	require.ErrorContains(t, promptErr, "sync history unavailable")
	require.NotNil(t, agent.runtime, "a commit failure retired the shared runtime")

	retained, err := store.Load(ctx, SessionKey{SessionID: string(current.id), Subpath: SessionStoreMainSubpath})
	require.NoError(t, err)
	require.Equal(t, []SessionStoreEntry{entry}, retained)
}

func TestUnexpectedSharedRuntimeExitRecoversLoadedSessionBeforeNextPrompt(t *testing.T) {
	ctx := context.Background()
	first := newFakeOpenCodeClient()
	first.createSession = testNativeSession("native-first")
	first.agents = []opencode.NativeAgent{{Name: "build"}}
	second := newFakeOpenCodeClient()
	second.xdg = first.xdg
	second.getSession = testNativeSession("native-first")
	second.agents = []opencode.NativeAgent{{Name: "build"}}

	var factoryCalls atomic.Int64
	agent := NewAgent(WithScratchDir(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			if factoryCalls.Add(1) == 1 {
				return first, nil
			}

			return second, nil
		}
	})

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)
	close(first.runtimeExited)
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		retained := agent.runtime == nil && len(agent.sessions) == 1
		agent.mu.Unlock()

		return retained
	}, time.Second, 10*time.Millisecond)

	response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "recovery-turn", "continue after restart"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.EqualValues(t, 2, factoryCalls.Load())
	require.Len(t, agent.sessions, 1)
	require.NoError(t, agent.sessions[created.SessionId].runtimeFailure())
	require.NoError(t, agent.Close())
}

func TestRecoverySkipsCrashedReplacementGenerationBeforePrompt(t *testing.T) {
	ctx := context.Background()
	first := newFakeOpenCodeClient()
	first.createSession = testNativeSession("native-first")
	first.agents = []opencode.NativeAgent{{Name: "build"}}

	second := newFakeOpenCodeClient()
	second.xdg = first.xdg
	second.getSession = testNativeSession("native-first")
	second.agents = []opencode.NativeAgent{{Name: "build"}}
	close(second.runtimeExited)

	third := newFakeOpenCodeClient()
	third.xdg = first.xdg
	third.getSession = testNativeSession("native-first")
	third.agents = []opencode.NativeAgent{{Name: "build"}}

	runtimes := []opencode.Client{first, second, third}
	var factoryCalls atomic.Int64
	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			index := int(factoryCalls.Add(1)) - 1

			return runtimes[index], nil
		}
	})

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)
	close(first.runtimeExited)
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.runtime == nil
	}, time.Second, 10*time.Millisecond)

	response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "fenced-recovery", "continue"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.EqualValues(t, 3, factoryCalls.Load())
	require.EqualValues(t, 3, agent.sessions[created.SessionId].runtimeGeneration)
	require.NoError(t, agent.Close())
}

func TestRuntimeCrashFailsInflightTurnThenRecoversBeforeFollowingPrompt(t *testing.T) {
	ctx := context.Background()
	first := newFakeOpenCodeClient()
	first.createSession = testNativeSession("native-first")
	first.agents = []opencode.NativeAgent{{Name: "build"}}
	started := make(chan struct{})
	first.hangsAfterDispatch(started)

	second := newFakeOpenCodeClient()
	second.xdg = first.xdg
	second.getSession = testNativeSession("native-first")
	second.agents = []opencode.NativeAgent{{Name: "build"}}

	var factoryCalls atomic.Int64
	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			if factoryCalls.Add(1) == 1 {
				return first, nil
			}

			return second, nil
		}
	})
	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)

	turnResult := make(chan error, 1)
	go func() {
		_, promptErr := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "crashed-turn", "block"))
		turnResult <- promptErr
	}()
	<-started
	close(first.runtimeExited)
	assertTurnFailed(t, <-turnResult, causeTransport, "shared OpenCode runtime exited")

	response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "following-turn", "continue"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.EqualValues(t, 2, factoryCalls.Load())
	require.NoError(t, agent.Close())
}

type fanoutRuntime struct {
	*fakeOpenCodeClient
	next atomic.Int64
}

func (r *fanoutRuntime) Scope(_ context.Context, options opencode.ScopeOptions) (opencode.Client, error) {
	id := fmt.Sprintf("native-%d", r.next.Add(1))

	return &fanoutScope{
		Client: r.fakeOpenCodeClient, root: r.fakeOpenCodeClient,
		directory: options.Directory, nativeID: id,
		eventStream: make(chan opencode.EventStreamItem),
	}, nil
}

type fanoutScope struct {
	opencode.Client
	root        *fakeOpenCodeClient
	directory   string
	nativeID    string
	eventStream chan opencode.EventStreamItem
}

func (s *fanoutScope) Close(context.Context) error { return nil }

func (s *fanoutScope) CreateSessionWithPolicy(context.Context, string, []opencode.PermissionRule) (opencode.NativeSession, error) {
	native := testNativeSession(s.nativeID)
	native.Directory = s.directory
	s.root.ensureSyncAggregate(s.nativeID)

	return native, nil
}

func (s *fanoutScope) DispatchMessage(_ context.Context, id string, request opencode.MessageRequest) error {
	if id != s.nativeID {
		return fmt.Errorf("scope %q received native session %q", s.nativeID, id)
	}

	text, _ := request.Parts[0][partTypeText].(string)
	if err := os.WriteFile(filepath.Join(s.directory, "native-cwd-proof.txt"), []byte(text), 0o600); err != nil {
		return err
	}

	assistantID := "assistant-" + id
	s.root.mu.Lock()
	s.root.messages = append(s.root.messages, opencode.NativeMessage{Info: opencode.NativeMessageInfo{
		ID: assistantID, SessionID: id, Role: "assistant", Finish: "stop",
	}})
	s.root.mu.Unlock()

	userEvent := opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": request.MessageID, "sessionID": id, "role": roleUser,
		}}),
	}
	s.eventStream <- opencode.EventStreamItem{Event: &userEvent}
	messageEvent := opencode.Event{
		Type: opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{
			"id": assistantID, "sessionID": id, "role": "assistant",
			"parentID": request.MessageID, "finish": "stop",
		}}),
	}
	s.eventStream <- opencode.EventStreamItem{Event: &messageEvent}
	idleEvent := opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": id}),
	}
	s.eventStream <- opencode.EventStreamItem{Event: &idleEvent}

	return nil
}

func (s *fanoutScope) EventStream() <-chan opencode.EventStreamItem { return s.eventStream }
func (s *fanoutScope) XDGDirs() opencode.XDGDirs                    { return s.root.XDGDirs() }
func (s *fanoutScope) RuntimeExited() <-chan struct{}               { return s.root.RuntimeExited() }
func (s *fanoutScope) Shutdown(ctx context.Context) error           { return s.root.Shutdown(ctx) }

func (s *fanoutScope) Messages(_ context.Context, id string) ([]opencode.NativeMessage, error) {
	s.root.mu.Lock()
	defer s.root.mu.Unlock()

	messages := make([]opencode.NativeMessage, 0, len(s.root.messages))
	for _, message := range s.root.messages {
		if message.Info.SessionID == id {
			messages = append(messages, message)
		}
	}

	return messages, s.root.messagesErr
}

func TestSharedRuntimeEightSessionRaceNativeCWDIsolation(t *testing.T) {
	const sessionCount = 8

	ctx := context.Background()
	runtime := &fanoutRuntime{fakeOpenCodeClient: newFakeOpenCodeClient()}
	runtime.agents = []opencode.NativeAgent{{Name: "build"}}
	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return runtime, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	type sessionCase struct {
		id     acp.SessionId
		cwd    string
		marker string
	}
	cases := make([]sessionCase, sessionCount)

	var createGroup sync.WaitGroup
	createErrors := make(chan error, sessionCount)
	for index := range cases {
		cases[index].cwd = t.TempDir()
		cases[index].marker = fmt.Sprintf("cwd-marker-%d", index)
		createGroup.Add(1)

		go func() {
			defer createGroup.Done()

			created, err := agent.NewSession(ctx, NewSessionRequest(cases[index].cwd))
			if err == nil {
				cases[index].id = created.SessionId
			}
			createErrors <- err
		}()
	}
	createGroup.Wait()
	close(createErrors)
	for err := range createErrors {
		require.NoError(t, err)
	}
	require.Len(t, agent.sessions, sessionCount)
	require.EqualValues(t, 1, agent.runtimeGeneration)
	for index := range cases {
		establishCreatedSession(t, agent, cases[index].id)
	}

	var promptGroup sync.WaitGroup
	promptErrors := make(chan error, sessionCount)
	for index := range cases {
		promptGroup.Add(1)

		go func() {
			defer promptGroup.Done()

			response, err := agent.Prompt(ctx, TextPromptRequest(cases[index].id, fmt.Sprintf("turn-%d", index), cases[index].marker))
			if err == nil && response.StopReason != acp.StopReasonEndTurn {
				err = fmt.Errorf("stop reason = %q", response.StopReason)
			}
			promptErrors <- err
		}()
	}
	promptGroup.Wait()
	close(promptErrors)
	for err := range promptErrors {
		require.NoError(t, err)
	}

	for index := range cases {
		data, err := os.ReadFile(filepath.Join(cases[index].cwd, "native-cwd-proof.txt"))
		require.NoError(t, err)
		require.Equal(t, cases[index].marker, string(data))
	}

	require.NoError(t, agent.Close())
}

func TestForkPermissionMustInherit(t *testing.T) {
	client := newFakeOpenCodeClient()
	agent := NewAgent()
	parent := testSession(t, agent, client)
	parent.cwd = t.TempDir()
	parent.permission = openCodePermissionDeny
	agent.sessions[parent.id] = parent
	_, err := agent.forkSession(context.Background(), ForkSessionRequest(parent.id, parent.cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodePermission("allow")))))
	require.ErrorContains(t, err, "child_permission_must_inherit")
}
func TestAgentLifecycleNewLoadResumeListCloseDelete(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-lifecycle")
	client.getSession = testNativeSession("native-lifecycle")
	client.messages = []opencode.NativeMessage{{
		Info:  opencode.NativeMessageInfo{ID: "assistant", SessionID: "native-lifecycle", Role: "assistant"},
		Parts: []opencode.NativePart{{ID: "part", SessionID: "native-lifecycle", MessageID: "assistant", Type: "text", Text: "history"}},
	}}
	client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review"}}

	agent := NewAgent()
	agent.runtime = client
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(cwd,
		WithSessionMCPServers(HTTPMCPServer("remote", "https://mcp.test", map[string]string{"Authorization": "secret"})),
		WithSessionOpenCodeOptions(OpenCodeOptions{
			Model: "openai/gpt-test", Mode: "build", Permission: "allow",
			Env:           map[string]string{"WAGIE_API_TOKEN": "bearer-one", "EMPTY": ""},
			ExtraPathDirs: []string{absTestPath("original", "bin")},
		}),
	))
	require.NoError(t, err)
	require.NotEmpty(t, created.SessionId)
	require.Equal(t, []string{absTestPath("original", "bin")}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-one", "EMPTY": ""}, client.scopes()[0].Env)

	listed, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	loaded, err := agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	require.NotNil(t, loaded.Meta)
	require.Equal(t, []string{absTestPath("original", "bin")}, client.scopes()[1].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-one", "EMPTY": ""}, client.scopes()[1].Env)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	resumed, err := agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeExtraPathDirs(absTestPath("replacement", "bin")),
			WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "rotated"}),
		)),
	))
	require.NoError(t, err)
	require.NotNil(t, resumed.Meta)
	require.Equal(t, []string{absTestPath("replacement", "bin")}, client.scopes()[2].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "rotated"}, client.scopes()[2].Env)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	loaded, err = agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeExtraPathDirs(),
			WithOpenCodeEnv(map[string]string{}),
		)),
	))
	require.NoError(t, err)
	require.NotNil(t, loaded.Meta)
	require.Empty(t, client.scopes()[3].ExtraPathDirs)
	require.Empty(t, client.scopes()[3].Env)

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
	require.NoError(t, err)
	_, err = agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd))
	require.Error(t, err)

	listed, err = agent.ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
	require.NoError(t, agent.Close())
}

func TestActiveLoadResumeReuseAnUnchangedCarrierWithoutConsumingCapacity(t *testing.T) {
	for _, method := range []struct {
		name string
		call func(*Agent, acp.SessionId, string, ...SessionRequestOption) error
	}{
		{
			name: "load with omitted carrier",
			call: func(agent *Agent, id acp.SessionId, cwd string, options ...SessionRequestOption) error {
				_, err := agent.LoadSession(t.Context(), LoadSessionRequest(id, cwd, options...))

				return err
			},
		},
		{
			name: "resume with identical carrier",
			call: func(agent *Agent, id acp.SessionId, cwd string, options ...SessionRequestOption) error {
				_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, cwd, options...))

				return err
			},
		},
	} {
		t.Run(method.name, func(t *testing.T) {
			cwd := t.TempDir()
			client := newFakeOpenCodeClient()
			agent := NewAgent(
				WithSessionStore(NewInMemorySessionStore()),
				WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
			)
			current := testSession(t, agent, client)
			current.cwd = cwd
			current.carrier = newSessionCarrier(map[string]string{"SESSION_COLOR": "same"}, []string{absTestPath("same", "bin")})
			require.NoError(t, current.snapshotToStore(t.Context()))

			var options []SessionRequestOption
			if method.name == "resume with identical carrier" {
				options = append(options, WithSessionOpenCodeOptions(NewOpenCodeOptions(
					WithOpenCodeEnv(map[string]string{"SESSION_COLOR": "same"}),
					WithOpenCodeExtraPathDirs(absTestPath("same", "bin")),
				)))
			}

			require.NoError(t, method.call(agent, current.id, cwd, options...))

			agent.mu.Lock()
			mapped := agent.sessions[current.id]
			activeCount := len(agent.sessions)
			agent.mu.Unlock()

			require.Same(t, current, mapped)
			require.Equal(t, 1, activeCount)
			require.False(t, client.isClosed(), "reuse contained the binding it was meant to retain")
			require.Empty(t, client.scopes(), "reuse allocated a second native directory scope")
		})
	}
}

func TestActiveResumeHardCutsChangedCarrierBeforeSuccessorAdmission(t *testing.T) {
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	predecessorClient := newFakeOpenCodeClient()
	agent := NewAgent(
		WithSessionStore(store),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
	)
	predecessor := testSession(t, agent, predecessorClient)
	predecessor.cwd = cwd
	predecessor.carrier = newSessionCarrier(map[string]string{"SESSION_COLOR": "old"}, []string{absTestPath("old", "bin")})
	require.NoError(t, predecessor.snapshotToStore(t.Context()))

	successorClient := newFakeOpenCodeClient()
	successorClient.getSession = testNativeSession(predecessor.idmap.NativeSessionID)
	successorClient.scopeFunc = func(opencode.ScopeOptions) error {
		require.True(t, predecessorClient.isClosed(), "successor overlapped its predecessor")

		return nil
	}
	agent.mu.Lock()
	agent.runtime = successorClient
	agent.mu.Unlock()

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(predecessor.id, cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeEnv(map[string]string{"SESSION_COLOR": "new"}),
			WithOpenCodeExtraPathDirs(absTestPath("new", "bin")),
		)),
	))
	require.NoError(t, err)
	require.True(t, predecessorClient.isClosed())

	agent.mu.Lock()
	successor := agent.sessions[predecessor.id]
	activeCount := len(agent.sessions)
	agent.mu.Unlock()

	require.NotNil(t, successor)
	require.NotSame(t, predecessor, successor)
	require.Equal(t, 1, activeCount, "replacement leaked an active-session slot")
	require.True(t, successor.snapshot().carrier.equal(newSessionCarrier(
		map[string]string{"SESSION_COLOR": "new"}, []string{absTestPath("new", "bin")},
	)))
	require.Len(t, successorClient.scopes(), 1)
}

func TestActiveResumeRebindsAnUnchangedCarrierAfterRuntimeLoss(t *testing.T) {
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	predecessorClient := newFakeOpenCodeClient()
	agent := NewAgent(
		WithSessionStore(store),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
	)
	current := testSession(t, agent, predecessorClient)
	current.cwd = cwd
	current.carrier = newSessionCarrier(map[string]string{"SESSION_COLOR": "same"}, []string{absTestPath("same", "bin")})
	require.NoError(t, current.snapshotToStore(t.Context()))

	current.detachRuntime(current.runtimeGeneration, errValueSharedRuntimeExited)

	successorClient := newFakeOpenCodeClient()
	successorClient.getSession = testNativeSession(current.idmap.NativeSessionID)
	agent.mu.Lock()
	agent.runtime = successorClient
	agent.runtimeGeneration++
	agent.mu.Unlock()

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(current.id, cwd))
	require.NoError(t, err)

	agent.mu.Lock()
	mapped := agent.sessions[current.id]
	activeCount := len(agent.sessions)
	agent.mu.Unlock()

	require.Same(t, current, mapped)
	require.Equal(t, 1, activeCount)
	require.Len(t, successorClient.scopes(), 1)
	require.True(t, current.snapshot().carrier.equal(newSessionCarrier(
		map[string]string{"SESSION_COLOR": "same"}, []string{absTestPath("same", "bin")},
	)))
}

func TestActiveResumePublishesNoSuccessorWhenHardCutContainmentFails(t *testing.T) {
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	predecessorClient := newFakeOpenCodeClient()
	agent := NewAgent(
		WithSessionStore(store),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
	)
	predecessor := testSession(t, agent, predecessorClient)
	predecessor.cwd = cwd
	predecessor.carrier = newSessionCarrier(map[string]string{"SESSION_COLOR": "old"}, []string{absTestPath("old", "bin")})
	require.NoError(t, predecessor.snapshotToStore(t.Context()))
	predecessorClient.closeErr = errors.Join(errors.New("scope still live"), ErrContainmentIncomplete)

	successorClient := newFakeOpenCodeClient()
	agent.mu.Lock()
	agent.runtime = successorClient
	agent.mu.Unlock()

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(predecessor.id, cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeEnv(map[string]string{"SESSION_COLOR": "new"}),
		)),
	))
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	agent.mu.Lock()
	mapped := agent.sessions[predecessor.id]
	activeCount := len(agent.sessions)
	agent.mu.Unlock()

	require.Same(t, predecessor, mapped)
	require.Equal(t, 1, activeCount)
	require.Empty(t, successorClient.scopes(), "failed containment admitted a successor")
}

func TestActiveResumeBoundsContendedPredecessorCloseAdmission(t *testing.T) {
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	predecessorClient := newFakeOpenCodeClient()
	agent := NewAgent(
		WithSessionStore(store),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}),
	)
	agent.sessionReplacementTimeout = 25 * time.Millisecond

	predecessor := testSession(t, agent, predecessorClient)
	predecessor.cwd = cwd
	predecessor.carrier = newSessionCarrier(map[string]string{"SESSION_COLOR": "old"}, []string{absTestPath("old", "bin")})
	require.NoError(t, predecessor.snapshotToStore(t.Context()))
	require.NoError(t, predecessor.recoveryMu.lock(context.Background()))

	gateHeld := true
	defer func() {
		if gateHeld {
			predecessor.recoveryMu.unlock()
		}
	}()

	successorClient := newFakeOpenCodeClient()
	successorClient.getSession = testNativeSession(predecessor.idmap.NativeSessionID)
	agent.mu.Lock()
	agent.runtime = successorClient
	agent.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(predecessor.id, cwd,
			WithSessionOpenCodeOptions(NewOpenCodeOptions(
				WithOpenCodeEnv(map[string]string{"SESSION_COLOR": "new"}),
			)),
		))
		result <- err
	}()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("active replacement held the lifecycle gate past its close-admission deadline")
	}

	agent.lifecycleAdmissionMu.Lock()
	require.Empty(t, agent.lifecycleFlights, "timed-out close admission retained the lifecycle flight")
	agent.lifecycleAdmissionMu.Unlock()

	agent.mu.Lock()
	mapped := agent.sessions[predecessor.id]
	activeCount := len(agent.sessions)
	agent.mu.Unlock()

	predecessor.mu.Lock()
	predecessorClosed := predecessor.closed
	predecessor.mu.Unlock()

	require.Same(t, predecessor, mapped)
	require.Equal(t, 1, activeCount)
	require.False(t, predecessorClosed, "timed-out admission partially closed the predecessor")
	require.Empty(t, successorClient.scopes(), "timed-out admission published a successor")

	predecessor.recoveryMu.unlock()
	gateHeld = false
	agent.sessionReplacementTimeout = settlementTimeout

	_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(predecessor.id, cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeEnv(map[string]string{"SESSION_COLOR": "new"}),
		)),
	))
	require.NoError(t, err, "released close admission was not retryable")
	require.Len(t, successorClient.scopes(), 1)
}

func TestSessionLifecycleFlightsIsolateIDsFenceCloseAndCleanUp(t *testing.T) {
	cwd := t.TempDir()
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	for logicalID, nativeID := range map[string]string{"s1": "native-1", "s2": "native-2"} {
		snapshot := validSyncSnapshot(logicalID, nativeID, cwd)
		encoded, err := json.Marshal(snapshot)
		require.NoError(t, err)
		require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: logicalID}, []SessionStoreReplacement{{
			Key: SessionKey{SessionID: logicalID}, Entries: []SessionStoreEntry{encoded},
		}}))
	}

	client := newFakeOpenCodeClient()
	client.getSessionFunc = func(_ context.Context, id string) (opencode.NativeSession, error) {
		return testNativeSession(id), nil
	}
	agent := NewAgent(WithSessionStore(store))
	agent.runtime = client

	reached := make(chan struct{})
	releaseLoad := make(chan struct{})
	callbackDone := make(chan error, 1)
	var once sync.Once
	store.onLoad = func(key SessionKey) ([]SessionStoreEntry, error) {
		if key.SessionID != "s1" {
			return store.InMemorySessionStore.Load(t.Context(), key)
		}

		var callbackErr error
		once.Do(func() {
			_, callbackErr = agent.UnstableDeleteSession(t.Context(), DeleteSessionRequest("callback-session"))
			callbackDone <- callbackErr
			close(reached)
			<-releaseLoad
		})
		if callbackErr != nil {
			return nil, callbackErr
		}

		return store.InMemorySessionStore.Load(t.Context(), key)
	}

	s1Result := make(chan error, 1)
	go func() {
		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest("s1", cwd))
		s1Result <- err
	}()

	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("s1 did not reach the blocked store load")
	}
	require.NoError(t, <-callbackDone, "store callback could not run an independent lifecycle operation")

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	canceledResult := make(chan error, 1)
	go func() {
		_, err := agent.ResumeSession(canceled, ResumeSessionRequest("s2", cwd))
		canceledResult <- err
	}()
	select {
	case err := <-canceledResult:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled s2 lifecycle call waited behind s1")
	}

	s2Result := make(chan error, 1)
	go func() {
		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest("s2", cwd))
		s2Result <- err
	}()
	select {
	case err := <-s2Result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("s2 lifecycle call waited behind s1")
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- agent.Close() }()
	select {
	case err := <-closeResult:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Agent.Close waited behind the blocked s1 lifecycle flight")
	}

	close(releaseLoad)
	require.Error(t, <-s1Result)

	agent.lifecycleAdmissionMu.Lock()
	require.Empty(t, agent.lifecycleFlights)
	require.True(t, agent.lifecycleFenced)
	agent.lifecycleAdmissionMu.Unlock()
}

func TestSessionLifecycleFlightSerializesSameIDPublication(t *testing.T) {
	cwd := t.TempDir()
	snapshot := validSyncSnapshot("session", "native", cwd)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session"}, Entries: []SessionStoreEntry{encoded},
	}}))

	reached := make(chan struct{})
	releaseLoad := make(chan struct{})
	var once sync.Once
	store.onLoad = func(key SessionKey) ([]SessionStoreEntry, error) {
		once.Do(func() {
			close(reached)
			<-releaseLoad
		})

		return store.InMemorySessionStore.Load(t.Context(), key)
	}

	client := newFakeOpenCodeClient()
	client.getSession = testNativeSession("native")
	agent := NewAgent(WithSessionStore(store))
	agent.runtime = client

	results := make(chan error, 2)
	go func() {
		_, loadErr := agent.LoadSession(t.Context(), LoadSessionRequest("session", cwd))
		results <- loadErr
	}()
	go func() {
		_, resumeErr := agent.ResumeSession(t.Context(), ResumeSessionRequest("session", cwd))
		results <- resumeErr
	}()

	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("first same-id resume did not reach the store")
	}
	close(releaseLoad)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Len(t, client.scopes(), 1, "same-id resumes published more than one native binding")

	agent.mu.Lock()
	require.Len(t, agent.sessions, 1)
	agent.mu.Unlock()
	agent.lifecycleAdmissionMu.Lock()
	require.Empty(t, agent.lifecycleFlights)
	agent.lifecycleAdmissionMu.Unlock()

	callbackResult := make(chan error, 1)
	store.onReplace = func(SessionKey) error {
		_, callbackErr := agent.ResumeSession(t.Context(), ResumeSessionRequest("independent", cwd))
		callbackResult <- callbackErr

		return nil
	}
	require.NoError(t, agent.Close())
	select {
	case callbackErr := <-callbackResult:
		require.ErrorContains(t, callbackErr, errValueAgentClosed)
	case <-time.After(time.Second):
		t.Fatal("Agent.Close store callback could not reenter an independent lifecycle operation")
	}
}

func TestAgentCloseFromLifecycleStoreCallbackDoesNotDeadlock(t *testing.T) {
	cwd := t.TempDir()
	snapshot := validSyncSnapshot("session", "native", cwd)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session"}, Entries: []SessionStoreEntry{encoded},
	}}))
	client := newFakeOpenCodeClient()
	client.getSession = testNativeSession("native")
	agent := NewAgent(WithSessionStore(store))
	agent.runtime = client

	store.onLoad = func(key SessionKey) ([]SessionStoreEntry, error) {
		if closeErr := agent.Close(); closeErr != nil {
			return nil, closeErr
		}

		return store.InMemorySessionStore.Load(t.Context(), key)
	}

	result := make(chan error, 1)
	go func() {
		_, resumeErr := agent.ResumeSession(t.Context(), ResumeSessionRequest("session", cwd))
		result <- resumeErr
	}()
	select {
	case resumeErr := <-result:
		require.Error(t, resumeErr)
	case <-time.After(2 * time.Second):
		t.Fatal("Agent.Close deadlocked inside the lifecycle store callback")
	}
}

func TestAgentLifecycleValidationAndStorageFailures(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	agent.runtime = client

	_, err := agent.NewSession(ctx, acp.NewSessionRequest{Cwd: "relative"})
	require.Error(t, err)
	_, err = agent.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "bad", Url: "https://bad"}}}})
	require.Error(t, err)
	_, err = agent.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, Meta: map[string]any{opencodeMetaKey: "bad"}})
	require.Error(t, err)

	_, err = agent.LoadSession(ctx, acp.LoadSessionRequest{})
	require.Error(t, err)
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: "missing", Cwd: cwd})
	require.Error(t, err)

	badStoreAgent := NewAgent(WithSessionStore(&errorSessionStore{err: errors.New("store failed")}))
	badStoreAgent.runtime = client
	_, err = badStoreAgent.LoadSession(ctx, LoadSessionRequest("missing", cwd))
	require.ErrorContains(t, err, "store failed")
	_, err = badStoreAgent.ListSessions(ctx, ListSessionsRequest())
	require.ErrorContains(t, err, "store failed")
	_, err = badStoreAgent.UnstableDeleteSession(ctx, DeleteSessionRequest("missing"))
	require.ErrorContains(t, err, "store failed")

	_, err = agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd("relative")))
	require.Error(t, err)
	badCursor := "not-base64!"
	_, err = agent.ListSessions(ctx, acp.ListSessionsRequest{Cursor: &badCursor})
	require.Error(t, err)

	_, err = agent.UnstableDeleteSession(ctx, acp.UnstableDeleteSessionRequest{})
	require.Error(t, err)

	require.NoError(t, agent.Close())
	_, err = agent.NewSession(ctx, NewSessionRequest(cwd))
	require.Error(t, err)
	_, err = agent.ListSessions(ctx, ListSessionsRequest())
	require.Error(t, err)
	_, err = agent.HandleExtensionMethod(ctx, "_unknown", nil)
	require.Error(t, err)
}

func TestAgentHelperFailureAndCapacityBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(WithSessionStoreLoadTimeout(time.Nanosecond))
	storeCtx, cancel := agent.sessionStoreContext(ctx)
	defer cancel()
	select {
	case <-storeCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("session store context did not expire")
	}

	defaultAgent := NewAgent()
	defaultAgent.options.SessionStoreLoadTimeout = 0
	_, defaultCancel := defaultAgent.sessionStoreContext(ctx)
	defaultCancel()
	require.NotNil(t, defaultAgent.sessionStore())

	release, err := agent.acquireClientCall(ctx)
	require.NoError(t, err)
	for i := 1; i < cap(agent.clientCalls); i++ {
		agent.clientCalls <- struct{}{}
	}
	_, err = agent.acquireClientCall(ctx)
	require.Error(t, err)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	_, err = agent.acquireClientCall(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	release()
	for len(agent.clientCalls) > 0 {
		<-agent.clientCalls
	}

	require.Equal(t, acp.PositionEncodingKindUtf8, selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf16, acp.PositionEncodingKindUtf8}))
	require.Equal(t, acp.PositionEncodingKindUtf16, selectPositionEncoding([]acp.PositionEncodingKind{acp.PositionEncodingKindUtf16}))

	closed := NewAgent()
	require.NoError(t, closed.Close())
	require.Error(t, closed.ensureOpen())
	_, err = closed.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{`))
	require.Error(t, err)
}

func TestAgentConstructionInitializationAndStoreBranches(t *testing.T) {
	oldRead := agentRandRead
	agentRandRead = func([]byte) (int, error) { return 0, errors.New("fingerprint entropy failed") }
	failedEntropy := NewAgent()
	agentRandRead = oldRead
	t.Cleanup(func() { agentRandRead = oldRead })
	_, err := failedEntropy.Initialize(context.Background(), acp.InitializeRequest{})
	// The construction verdict is one closed token. The joined prose stays on
	// the wrapped Go error for the operator's log and never reaches the wire.
	require.ErrorContains(t, err, "fingerprint entropy failed")
	data := requireInternalErrorData(t, err)
	require.Equal(t, errValueInvalidOptions, data[jsonFieldError])
	require.Len(t, data, 1)

	for name, option := range map[string]Option{
		"health":       WithOpenCodeHealthCheckTimeout(0),
		"turn timeout": WithTurnTimeout(-time.Second),
		"limits":       WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: -1}),
		"image limits": WithImageLimits(ImageLimits{MaxOutputBytesPerToolCall: -1}),
	} {
		t.Run(name, func(t *testing.T) {
			refused := NewAgent(option)

			// Both entry points must reach the same verdict: an embedded host can
			// open a session and prompt without ever handshaking.
			_, err := refused.Initialize(context.Background(), acp.InitializeRequest{})
			require.Error(t, err)
			require.NotEmpty(t, requireInternalErrorData(t, err)[jsonFieldError])

			ensureErr := refused.ensureOpen()
			require.Error(t, ensureErr)
			require.Equal(t, requireInternalErrorData(t, err), requireInternalErrorData(t, ensureErr))
		})
	}

	agent := NewAgent()
	agent.options.SessionStore = nil
	require.NotNil(t, agent.sessionStore())
	current := testSession(t, agent, newFakeOpenCodeClient())
	agent.runtime = nil
	require.Error(t, agent.storeStartedSession(current), "missing runtime must reject publication")
	agent.runtime = newFakeOpenCodeClient()
	require.NoError(t, agent.storeStartedSession(current))
	require.False(t, agent.removeSessionIf(current.id, &session{}))
	require.True(t, agent.removeSessionIf(current.id, current))
	require.False(t, agent.removeSessionIf(current.id, current))
	agent.closed = true
	require.Error(t, agent.storeStartedSession(current))
}

func TestHandleExtensionAndLocalConnectionHelperBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	_, err := agent.HandleExtensionMethod(ctx, "_unknown", nil)
	require.Error(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{`))
	require.Error(t, err)
	_, err = agent.HandleExtensionMethod(ctx, ForkSessionMethod, json.RawMessage(`{}`))
	require.Error(t, err)

	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	_, reqErr := conn.handle(ctx, "unknown", nil)
	require.NotNil(t, reqErr)
	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionList, json.RawMessage(`{`))
	require.NotNil(t, reqErr)
	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionList, json.RawMessage(`{"cwd":"relative"}`))
	require.NotNil(t, reqErr)

	require.Nil(t, requestError(ctx, nil))
	require.Equal(t, -32603, requestError(ctx, errors.New("boom")).Code)
	reqError := acp.NewInvalidParams(nil)
	require.Same(t, reqError, requestError(ctx, reqError))

	withdrawn, cancel := context.WithCancelCause(ctx)
	cancel(context.Canceled)

	require.Equal(t, -32800, requestError(withdrawn, context.Canceled).Code)

	_, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{}, elicitationScope{})
	require.ErrorContains(t, err, "include form or url")

	gate := newConnectionInputGate(strings.NewReader("{}\npartial"))
	gate.open()
	all, err := io.ReadAll(gate)
	require.NoError(t, err)
	require.Equal(t, "{}\npartial", string(all))
}

func TestLifecycleMCPPaginationAndRequestBuilderHelpers(t *testing.T) {
	stdio := &acp.McpServerStdio{Name: "stdio", Command: "tool", Args: []string{"serve"}, Env: []acp.EnvVariable{{Name: "TOKEN", Value: "secret"}}}
	unstable := []acp.UnstableMcpServer{
		{Http: &acp.UnstableMcpServerHttp{Name: "http", Url: "https://mcp.test", Headers: []acp.HttpHeader{{Name: "X", Value: "Y"}}}},
		{Stdio: stdio},
	}
	configs := nativeMCPServerConfigsFromUnstable(unstable)
	require.Len(t, configs, 2)
	require.Equal(t, []string{"tool", "serve"}, configs[1].Command)
	require.Equal(t, "secret", configs[1].Env["TOKEN"])
	require.Nil(t, nativeMCPServerConfigsFromUnstable(nil))
	require.Nil(t, httpHeaderMap(nil))
	require.Empty(t, nativeStdioMCPServerConfig(&acp.McpServerStdio{Name: "empty"}).Env)
	require.ElementsMatch(t, []string{"Y", "secret"}, mcpSecretNeedles(configs))

	require.NoError(t, validateUnstableMCPServers(unstable))
	require.Error(t, validateUnstableMCPServers([]acp.UnstableMcpServer{{Sse: &acp.UnstableMcpServerSse{Name: "sse"}}}))
	require.Error(t, validateUnstableMCPServers([]acp.UnstableMcpServer{{Acp: &acp.UnstableMcpServerAcpInline{Name: "acp"}}}))
	requireInvalidParamsData(t, validateUnstableMCPServers([]acp.UnstableMcpServer{{}}), map[string]any{
		jsonFieldError: errValueNoTransport,
		jsonFieldField: "mcpServers[0]",
	})
	require.Error(t, validateUnstableMCPServers([]acp.UnstableMcpServer{{Http: &acp.UnstableMcpServerHttp{}}}))
	require.Error(t, validateUnstableMCPServers([]acp.UnstableMcpServer{
		{Http: &acp.UnstableMcpServerHttp{Name: "same"}}, {Stdio: &acp.McpServerStdio{Name: "same"}},
	}))

	infos := make([]acp.SessionInfo, listSessionsPageSize+1)
	for index := range infos {
		infos[index].SessionId = acp.SessionId(fmt.Sprintf("session-%d", index))
	}
	page, next, err := paginateSessionInfos(infos, nil)
	require.NoError(t, err)
	require.Len(t, page, listSessionsPageSize)
	require.NotNil(t, next)
	page, next, err = paginateSessionInfos(infos, next)
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Nil(t, next)

	negative := base64.RawURLEncoding.EncodeToString([]byte("-1"))
	_, err = decodeListCursor(&negative)
	require.Error(t, err)
	nonnumeric := base64.RawURLEncoding.EncodeToString([]byte("x"))
	_, err = decodeListCursor(&nonnumeric)
	require.Error(t, err)
	past := encodeListCursor(len(infos) + 1)
	_, _, err = paginateSessionInfos(infos, &past)
	require.Error(t, err)

	require.Equal(t, acp.SessionId("delete"), DeleteSessionRequest("delete").SessionId)
	list := ListSessionsRequest(WithListSessionsCwd(absTestPath("repo")))
	require.Equal(t, absTestPath("repo"), *list.Cwd)
}

func TestDirectoryBindingFingerprintAndResourceBranches(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(realDir, link))
	agent := NewAgent()
	servers := []opencode.MCPServerConfig{{Name: "b", URL: "https://b"}, {Name: "a", URL: "https://a"}}
	fingerprint, err := agent.directoryMCPFingerprint(servers)
	require.NoError(t, err)
	require.NotEmpty(t, fingerprint)
	require.Empty(t, mustDirectoryFingerprint(t, agent, nil))

	release, err := agent.bindDirectory("one", "", link, servers)
	require.NoError(t, err)
	_, err = agent.bindDirectory("two", "", realDir, servers)
	require.Error(t, err)
	_, err = agent.bindDirectory("one", "", realDir, []opencode.MCPServerConfig{{Name: "other", URL: "https://other"}})
	require.Error(t, err)
	release()
	_, err = agent.bindDirectory("one", "", filepath.Join(root, "missing"), nil)
	require.ErrorContains(t, err, "canonicalize cwd")
}

func mustDirectoryFingerprint(t *testing.T, agent *Agent, configs []opencode.MCPServerConfig) string {
	t.Helper()
	value, err := agent.directoryMCPFingerprint(configs)
	require.NoError(t, err)

	return value
}

func TestNewSessionRemainingFailureStages(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()

	oldReader := sessionIDRandReader
	sessionIDRandReader = errorReader{err: errors.New("id entropy failed")}
	_, err := NewAgent().NewSession(ctx, NewSessionRequest(cwd))
	require.ErrorContains(t, err, "id entropy failed")
	sessionIDRandReader = oldReader
	t.Cleanup(func() { sessionIDRandReader = oldReader })

	factoryFailure := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return nil, errors.New("runtime start failed")
		}
	})
	_, err = factoryFailure.NewSession(ctx, NewSessionRequest(cwd))
	require.ErrorContains(t, err, "runtime start failed")

	for name, configure := range map[string]func(*fakeOpenCodeClient, *Agent){
		"scope":  func(client *fakeOpenCodeClient, _ *Agent) { client.scopeErr = errors.New("scope failed") },
		"create": func(client *fakeOpenCodeClient, _ *Agent) { client.createErr = errors.New("create failed") },
		"snapshot history": func(client *fakeOpenCodeClient, _ *Agent) {
			client.syncHistoryErr = errors.New("history failed")
		},
		"store": func(_ *fakeOpenCodeClient, agent *Agent) {
			agent.options.SessionStore = &errorSessionStore{err: errors.New("replace failed")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			client.createSession = testNativeSession("native")
			agent := NewAgent()
			agent.runtime = client
			configure(client, agent)
			_, testErr := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(OpenCodeOptions{Model: "openai/gpt-test"})))
			require.Error(t, testErr)
		})
	}

	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native")
	limited := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}))
	limited.runtime = client
	limited.sessions["existing"] = testSession(t, limited, client)
	_, err = limited.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.ErrorContains(t, err, "backpressure")
}

func TestLoadResumeRemainingFailureStages(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	snapshot := validSyncSnapshot("session", "native", cwd)
	snapshot.Session.Model = stateSnapshotModel{ProviderID: "openai", ModelID: "gpt-test"}
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	newStoredAgent := func(client *fakeOpenCodeClient) *Agent {
		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
			Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{encoded},
		}}))
		agent := NewAgent(WithSessionStore(store))
		agent.runtime = client

		return agent
	}

	for name, configure := range map[string]func(*fakeOpenCodeClient){
		"scope":       func(client *fakeOpenCodeClient) { client.scopeErr = errors.New("scope failed") },
		"history":     func(client *fakeOpenCodeClient) { client.syncHistoryErr = errors.New("history failed") },
		"replay":      func(client *fakeOpenCodeClient) { client.syncReplayErr = errors.New("replay failed") },
		"get session": func(client *fakeOpenCodeClient) { client.getErr = errors.New("get failed") },
	} {
		t.Run(name, func(t *testing.T) {
			client := newFakeOpenCodeClient()
			client.getSession = testNativeSession("native")
			configure(client)
			agent := newStoredAgent(client)
			_, testErr := agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
			require.Error(t, testErr)
		})
	}

	agent := newStoredAgent(newFakeOpenCodeClient())
	agent.deleted["session"] = struct{}{}
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
	require.Error(t, err)
	_, err = agent.ResumeSession(ctx, acp.ResumeSessionRequest{SessionId: "session", Cwd: cwd, McpServers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "bad"}}}})
	require.Error(t, err)
}

func TestForkSessionSuccessAndFailureStages(t *testing.T) {
	ctx := context.Background()
	newForkAgent := func() (*Agent, *fakeOpenCodeClient, *session) {
		client := newFakeOpenCodeClient()
		client.forkSession = testNativeSession("native-child")
		client.getSession = testNativeSession("native-child")
		client.ensureSyncAggregate("native-child")
		agent := NewAgent()
		agent.runtime = client
		parent := testSession(t, agent, client)
		// A fork inherits its parent's workspace, so the parent's cwd has to be
		// a directory the runtime can canonicalize.
		parent.cwd = t.TempDir()
		agent.sessions[parent.id] = parent

		return agent, client, parent
	}

	agent, _, parent := newForkAgent()
	response, err := agent.forkSession(ctx, ForkSessionRequest(parent.id, parent.cwd))
	require.NoError(t, err)
	require.NotEmpty(t, response.SessionId)

	for name, configure := range map[string]func(*Agent, *fakeOpenCodeClient){
		"native fork": func(_ *Agent, client *fakeOpenCodeClient) { client.forkErr = errors.New("fork failed") },
		"scope":       func(_ *Agent, client *fakeOpenCodeClient) { client.scopeErr = errors.New("scope failed") },
		"get":         func(_ *Agent, client *fakeOpenCodeClient) { client.getErr = errors.New("get failed") },
		"snapshot": func(_ *Agent, client *fakeOpenCodeClient) {
			client.syncHistoryErr = errors.New("snapshot failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			testAgent, client, testParent := newForkAgent()
			configure(testAgent, client)
			_, testErr := testAgent.forkSession(ctx, ForkSessionRequest(testParent.id, testParent.cwd))
			require.Error(t, testErr)
		})
	}

	agent, _, parent = newForkAgent()
	oldReader := sessionIDRandReader
	sessionIDRandReader = errorReader{err: errors.New("entropy failed")}
	_, err = agent.forkSession(ctx, ForkSessionRequest(parent.id, parent.cwd))
	require.ErrorContains(t, err, "entropy failed")
	sessionIDRandReader = oldReader

	_, err = agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: parent.id, Cwd: "relative"})
	require.Error(t, err)
	_, err = agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: parent.id, Cwd: parent.cwd, Meta: map[string]any{opencodeMetaKey: "bad"}})
	require.Error(t, err)
	_, err = agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: "missing", Cwd: t.TempDir()})
	require.Error(t, err)
}

func TestForkCarrierInheritsUnlessExplicitlyReplaced(t *testing.T) {
	newForkAgent := func() (*Agent, *fakeOpenCodeClient, *session) {
		client := newFakeOpenCodeClient()
		client.forkSession = testNativeSession("native-child")
		client.getSession = testNativeSession("native-child")
		client.ensureSyncAggregate("native-child")
		agent := NewAgent()
		agent.runtime = client
		parent := testSession(t, agent, client)
		parent.cwd = t.TempDir()
		parent.carrier = newSessionCarrier(map[string]string{"WAGIE_API_TOKEN": "parent-token"}, []string{absTestPath("parent", "bin")})
		agent.sessions[parent.id] = parent

		return agent, client, parent
	}

	agent, client, parent := newForkAgent()
	_, err := agent.forkSession(t.Context(), ForkSessionRequest(parent.id, parent.cwd))
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("parent", "bin")}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "parent-token"}, client.scopes()[0].Env)

	agent, client, parent = newForkAgent()
	_, err = agent.forkSession(t.Context(), ForkSessionRequest(parent.id, parent.cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeExtraPathDirs(absTestPath("child", "bin")))),
	))
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("child", "bin")}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "parent-token"}, client.scopes()[0].Env,
		"replacing one half of the carrier must leave the other half alone")

	// A child that names an environment replaces the parent's outright, and an
	// empty map is a replacement rather than an omission.
	agent, client, parent = newForkAgent()
	_, err = agent.forkSession(t.Context(), ForkSessionRequest(parent.id, parent.cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{}))),
	))
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("parent", "bin")}, client.scopes()[0].ExtraPathDirs)
	require.Empty(t, client.scopes()[0].Env)
}

type summaryCoverageStore struct {
	*InMemorySessionStore
	summaries []SessionSummary
}

func (store *summaryCoverageStore) ListSessions(context.Context) ([]SessionSummary, error) {
	return append([]SessionSummary(nil), store.summaries...), nil
}

func TestLifecycleRemainingReplayRefreshValidationAndPublicationBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	snapshot := validSyncSnapshot("session", "native", absTestPath("source"))
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	storedAgent := func(client *fakeOpenCodeClient) *Agent {
		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
			Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{encoded},
		}}))
		agent := NewAgent(WithSessionStore(store))
		agent.runtime = client

		return agent
	}

	client := newFakeOpenCodeClient()
	client.getSession = testNativeSession("native")
	client.messagesErr = errors.New("messages failed")
	agent := storedAgent(client)
	_, err = agent.LoadSession(ctx, LoadSessionRequest("session", cwd))
	require.ErrorContains(t, err, "messages failed")

	closed := storedAgent(newFakeOpenCodeClient())
	closed.closed = true
	_, err = closed.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
	require.Error(t, err)

	for _, request := range []acp.ResumeSessionRequest{
		{SessionId: "session", Cwd: "relative"},
		{SessionId: "session", Cwd: cwd, McpServers: []acp.McpServer{{Sse: &acp.McpServerSseInline{Name: "bad"}}}},
		{SessionId: "session", Cwd: cwd, Meta: map[string]any{opencodeMetaKey: "bad"}},
	} {
		_, err = storedAgent(newFakeOpenCodeClient()).ResumeSession(ctx, request)
		require.Error(t, err)
	}

	client = newFakeOpenCodeClient()
	client.getSession = testNativeSession("native")
	agent = storedAgent(client)
	agent.options.ConcurrencyLimits.MaxActiveSessions = 1
	agent.sessions["occupied"] = testSession(t, agent, client)
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
	require.ErrorContains(t, err, "backpressure")
}

func TestListSessionsRemainingFilteringSortingAndCloseBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	otherCwd := t.TempDir()
	store := &summaryCoverageStore{InMemorySessionStore: NewInMemorySessionStore(), summaries: []SessionSummary{
		{SessionID: "seen", Cwd: cwd, Title: "seen", UpdatedAtUnixMilli: 10},
		{SessionID: "deleted", Cwd: cwd, Title: "deleted", UpdatedAtUnixMilli: 10},
		{SessionID: "other-cwd", Cwd: otherCwd, Title: "other", UpdatedAtUnixMilli: 20},
		{SessionID: "stored-z", Cwd: cwd, Title: "z", UpdatedAtUnixMilli: 30},
		{SessionID: "stored-a", Cwd: cwd, Title: "a", UpdatedAtUnixMilli: 30},
	}}
	agent := NewAgent(WithSessionStore(store))
	client := newFakeOpenCodeClient()
	active := testSession(t, agent, client)
	active.id = "seen"
	active.cwd = cwd
	filtered := testSession(t, agent, client)
	filtered.id = "filtered-active"
	filtered.cwd = otherCwd
	agent.sessions[active.id] = active
	agent.sessions[filtered.id] = filtered
	agent.deleted["deleted"] = struct{}{}

	response, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Equal(t, []acp.SessionId{"seen", "stored-a", "stored-z"}, []acp.SessionId{
		response.Sessions[0].SessionId, response.Sessions[1].SessionId, response.Sessions[2].SessionId,
	})

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: "missing"})
	require.Error(t, err)
}

func TestForkAndMCPMappingRemainingValidationCapacityAndUnionBranches(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.forkSession = testNativeSession("native-child")
	client.getSession = testNativeSession("native-child")
	client.ensureSyncAggregate("native-child")
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 1}))
	agent.runtime = client
	parent := testSession(t, agent, client)
	parent.cwd = t.TempDir()
	agent.sessions[parent.id] = parent

	_, err := agent.forkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  parent.id,
		Cwd:        t.TempDir(),
		McpServers: []acp.UnstableMcpServer{{Sse: &acp.UnstableMcpServerSse{Name: "bad"}}},
	})
	require.Error(t, err)

	_, err = agent.forkSession(context.Background(), ForkSessionRequest(parent.id, parent.cwd))
	require.ErrorContains(t, err, "backpressure")

	configs := nativeMCPServerConfigs([]acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "stdio", Command: "tool"}},
		{},
	})
	require.Len(t, configs, 1)
}

func TestSessionCarrierReachesTheAddressedNativeScopeOnly(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-env")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	var started opencode.StartOptions

	agent := NewAgent(
		WithHome(t.TempDir()),
		WithEnv(map[string]string{"PATH": absTestPath("static", "bin")}),
		func(options *Options) {
			options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
				started = options

				return client, nil
			}
		},
	)
	agent.setAgentClient(newRecordingAgentClient())

	_, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeExtraPathDirs(absTestPath("session", "bin")),
		WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "session-token"}),
	))))
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("session", "bin")}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "session-token"}, client.scopes()[0].Env)

	// The shared process is started under the operator's environment only. A
	// session value there would be the same value for every session of the
	// Agent and would outlive the session that asked for it.
	require.Equal(t, absTestPath("static", "bin"), started.Env["PATH"])
	require.NotContains(t, started.Env, "WAGIE_API_TOKEN")

	require.NoError(t, agent.Close())
}

func TestSessionExtraPathDirsFailBeforeNativeCreation(t *testing.T) {
	launches := 0
	agent := NewAgent(
		WithHome(t.TempDir()),
		func(options *Options) {
			options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
				launches++

				return newFakeOpenCodeClient(), nil
			}
		},
	)

	_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeExtraPathDirs("relative/bin"),
	))))
	require.Error(t, err)
	require.Zero(t, launches)
}

func TestConcurrentSessionsCarryDistinctOrderedPathDirs(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()

	var created atomic.Int64

	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession(fmt.Sprintf("native-%d", created.Add(1))), nil
	}
	client.agents = []opencode.NativeAgent{{Name: "build"}}

	var factoryCalls atomic.Int64

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			factoryCalls.Add(1)

			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	session := func(dirs ...string) acp.NewSessionRequest {
		return NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeExtraPathDirs(dirs...),
		)))
	}

	_, err := agent.NewSession(ctx, session(absTestPath("one"), absTestPath("shared"), absTestPath("one")))
	require.NoError(t, err)

	_, err = agent.NewSession(ctx, session(absTestPath("two"), absTestPath("shared")))
	require.NoError(t, err)
	require.EqualValues(t, 1, factoryCalls.Load())
	require.Equal(t, []string{absTestPath("one"), absTestPath("shared"), absTestPath("one")}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, []string{absTestPath("two"), absTestPath("shared")}, client.scopes()[1].ExtraPathDirs)
	require.NoError(t, agent.Close())
}

func TestRecoveredSessionKeepsItsExtraPathDirs(t *testing.T) {
	ctx := context.Background()
	first := newFakeOpenCodeClient()
	first.createSession = testNativeSession("native-first")
	first.agents = []opencode.NativeAgent{{Name: "build"}}
	second := newFakeOpenCodeClient()
	second.xdg = first.xdg
	second.getSession = testNativeSession("native-first")
	second.agents = []opencode.NativeAgent{{Name: "build"}}

	var (
		factoryCalls atomic.Int64
		startedMu    sync.Mutex
		started      []opencode.StartOptions
	)

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(_ context.Context, start opencode.StartOptions) (opencode.Client, error) {
			startedMu.Lock()
			started = append(started, start)
			startedMu.Unlock()

			if factoryCalls.Add(1) == 1 {
				return first, nil
			}

			return second, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeExtraPathDirs(absTestPath("session", "bin")),
	))))
	require.NoError(t, err)
	establishCreatedSession(t, agent, created.SessionId)
	close(first.runtimeExited)
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.runtime == nil
	}, time.Second, 10*time.Millisecond)

	response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "recovery-turn", "continue after restart"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	startedMu.Lock()
	require.Len(t, started, 2)
	startedMu.Unlock()
	require.Equal(t, []string{absTestPath("session", "bin")}, second.scopes()[0].ExtraPathDirs)
	require.NoError(t, agent.Close())
}

// TestEstablishingHandlersPublishNothingBeforeTheyReturn proves the opening
// lifecycle snapshot and the initial command catalog leave none of the four
// establishing handlers. Both are owed to the host only after the establishing
// response has been written to the transport, so a handler that has returned has
// published neither.
func TestEstablishingHandlersPublishNothingBeforeTheyReturn(t *testing.T) {
	ctx := context.Background()

	requireNothingPublished := func(t *testing.T, connection *recordingAgentClient) {
		t.Helper()

		require.Empty(t, connection.lifecycleEnvelopes(t), "the opening snapshot left the establishing handler")
		require.Empty(t, connection.availableCommandUpdates(), "the command catalog left the establishing handler")
	}

	t.Run("new", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.createSession = testNativeSession("native")
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review changes"}}
		agent := negotiatedAgent(t)
		connection := newRecordingAgentClient()
		agent.setAgentClient(connection)
		agent.runtime = client

		_, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
		require.NoError(t, err)
		requireNothingPublished(t, connection)
	})

	stored := func(t *testing.T, cwd string) (*Agent, *recordingAgentClient) {
		t.Helper()

		snapshot := validSyncSnapshot("session", "native", cwd)
		snapshot.Session.Model = stateSnapshotModel{ProviderID: "openai", ModelID: "gpt-test"}
		encoded, err := json.Marshal(snapshot)
		require.NoError(t, err)

		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(ctx, SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
			Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{encoded},
		}}))

		client := newFakeOpenCodeClient()
		client.getSession = testNativeSession("native")
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review changes"}}
		agent := negotiatedAgent(t, WithSessionStore(store))
		connection := newRecordingAgentClient()
		agent.setAgentClient(connection)
		agent.runtime = client

		return agent, connection
	}

	t.Run("load", func(t *testing.T) {
		cwd := t.TempDir()
		agent, connection := stored(t, cwd)
		_, err := agent.LoadSession(ctx, LoadSessionRequest("session", cwd))
		require.NoError(t, err)
		requireNothingPublished(t, connection)
	})

	t.Run("resume", func(t *testing.T) {
		cwd := t.TempDir()
		agent, connection := stored(t, cwd)
		_, err := agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd))
		require.NoError(t, err)
		requireNothingPublished(t, connection)
	})

	t.Run("fork", func(t *testing.T) {
		client := newFakeOpenCodeClient()
		client.forkSession = testNativeSession("native-child")
		client.getSession = testNativeSession("native-child")
		client.commands = []opencode.NativeCommand{{Name: "review", Description: "Review changes"}}
		client.ensureSyncAggregate("native-child")

		agent := negotiatedAgent(t)
		connection := newRecordingAgentClient()
		agent.setAgentClient(connection)
		agent.runtime = client
		parent := testSession(t, agent, client)
		parent.cwd = t.TempDir()
		agent.sessions[parent.id] = parent

		// The parent was established by the test helper; only what the fork
		// itself publishes is at stake.
		connection.mu.Lock()
		connection.updates = nil
		connection.mu.Unlock()

		_, err := agent.forkSession(ctx, ForkSessionRequest(parent.id, parent.cwd))
		require.NoError(t, err)
		requireNothingPublished(t, connection)
	})
}

// TestCloseSessionRefusesWithoutADurableSnapshot proves close is store-backed:
// a session whose snapshot cannot be committed stays addressable for a retry
// rather than being released with state no reload could restore. The commit is
// the last rung of the ladder, so a store that is offline never costs the
// containment proof that runs ahead of it.
func TestCloseSessionRefusesWithoutADurableSnapshot(t *testing.T) {
	client := newFakeOpenCodeClient()
	agent := NewAgent(WithSessionStore(&errorSessionStore{err: errors.New("store offline")}))
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: current.id})
	require.ErrorContains(t, err, "store offline")
	require.True(t, client.isClosed(), "an uncommittable snapshot skipped the containment boundary")
	require.Contains(t, agent.sessions, current.id, "the session was released without a durable snapshot")
}

// TestDeleteHidesTheSessionEvenWhenTeardownFails proves the delete order and
// what hiding an id does and does not mean. The durable tombstone is written
// first and the id is hidden with it, so a teardown that fails afterwards is
// reported to the caller without leaving a session that delete already answered
// for still listable, loadable, or resumable.
//
// Hidden is a wire fact, not a bookkeeping fact. The failed teardown left a live
// native scope, so this agent keeps internal ownership of it: dropping the handle
// would leave a scope nothing in this process could reach again. The retained
// handle is what a later delete retries the cleanup through, and only a teardown
// that proved containment releases it.
func TestDeleteHidesTheSessionEvenWhenTeardownFails(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(errors.New("scope refused to close"), ErrContainmentIncomplete)
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	require.NoError(t, current.snapshotToStore(ctx))

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(current.id))
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.True(t, agent.isDeleted(current.id), "a failed teardown left the deleted id addressable")
	require.Contains(t, agent.sessions, current.id, "the failed teardown abandoned the native scope it left running")

	listed, err := agent.ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions, "a deleted session was still listable")

	stored, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, stored, "the tombstone was cleared by the failed teardown")

	_, err = agent.LoadSession(ctx, LoadSessionRequest(current.id, t.TempDir()))
	requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})

	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(current.id, t.TempDir()))
	requireInvalidParamsData(t, err, map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})

	// The scope the failed teardown left behind is reclaimed by the next delete,
	// which runs the same containment again rather than answering for a session
	// it never contained.
	client.closeErr = nil
	attempts := client.containmentAttempts()

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(current.id))
	require.NoError(t, err)
	require.Greater(t, client.containmentAttempts(), attempts, "the retried delete never reached the scope again")
	require.NotContains(t, agent.sessions, current.id, "the proven teardown kept the handle")

	stored, err = store.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, stored, "the retried delete resurrected the tombstoned row")
}

// TestAgentCloseSweepsTheScopeAFailedDeleteLeftBehind proves the other half of
// that retained ownership: an agent shutting down closes the native scope of a
// session whose delete could not contain it, exactly as it closes every other
// session's. A handle dropped at the tombstone would have left that scope running
// past the process that owned it.
func TestAgentCloseSweepsTheScopeAFailedDeleteLeftBehind(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(errors.New("scope refused to close"), ErrContainmentIncomplete)
	agent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(current.id))
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	client.closeErr = nil
	attempts := client.containmentAttempts()

	require.NoError(t, agent.Close())
	require.Greater(t, client.containmentAttempts(), attempts,
		"the shutdown swept every scope but the one the failed delete left behind")
}

// TestACommitRacingASucceededDeleteRecreatesNothing proves the write barrier a
// late settlement meets: a turn that was still in flight when delete tombstoned
// the id commits nothing afterwards, because a replacement unlists the tombstone
// for every key it writes.
func TestACommitRacingASucceededDeleteRecreatesNothing(t *testing.T) {
	ctx := context.Background()
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	current := testSession(t, agent, newFakeOpenCodeClient())
	agent.sessions[current.id] = current
	require.NoError(t, current.snapshotToStore(ctx))

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(current.id))
	require.NoError(t, err)

	require.NoError(t, current.commitForegroundPrefix(ctx))

	stored, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, stored, "a settlement after the delete recreated the row")
}

// TestDeleteLeavesNoWriteThatRecreatesTheRow proves the tombstone survives the
// teardown it precedes: the settlement that follows a successful delete writes
// nothing, because a replacement unlists the tombstone for every key it writes
// and would resurrect a session already reported gone.
func TestDeleteLeavesNoWriteThatRecreatesTheRow(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := NewAgent(WithSessionStore(store))
	current := testSession(t, agent, client)
	agent.sessions[current.id] = current
	require.NoError(t, current.snapshotToStore(ctx))

	openTestCycle(current, true)

	var wroteAfterTombstone bool

	store.onDelete = func(key SessionKey) error {
		store.onReplace = func(SessionKey) error {
			wroteAfterTombstone = true

			return nil
		}

		return store.InMemorySessionStore.Delete(ctx, key)
	}

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(current.id))
	require.NoError(t, err)
	require.False(t, wroteAfterTombstone, "a write after the tombstone recreated the deleted row")

	stored, err := store.ListSessions(ctx)
	require.NoError(t, err)
	require.Empty(t, stored, "the deleted session is listable again")

	entries, err := store.Load(ctx, SessionKey{SessionID: string(current.id)})
	require.NoError(t, err)
	require.Empty(t, entries, "the deleted session's row was recreated")
}

func TestLoadRacingDeleteSerializesTheSameLogicalSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-race")
	client.getSession = testNativeSession("native-race")

	store := &hookSessionStore{InMemorySessionStore: NewInMemorySessionStore()}
	agent := NewAgent(WithSessionStore(store))
	agent.runtime = client
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	reached := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	store.onLoad = func(key SessionKey) ([]SessionStoreEntry, error) {
		entries, loadErr := store.InMemorySessionStore.Load(ctx, key)

		once.Do(func() {
			close(reached)
			<-release
		})

		return entries, loadErr
	}

	var (
		wg      sync.WaitGroup
		loadErr error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		_, loadErr = agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd))
	}()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the load never reached the store read")
	}

	deleteResult := make(chan error, 1)
	go func() {
		_, delErr := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
		deleteResult <- delErr
	}()
	select {
	case err := <-deleteResult:
		t.Fatalf("same-id delete overtook the active load: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	wg.Wait()
	require.NoError(t, <-deleteResult)

	store.onLoad = nil

	require.NoError(t, loadErr)
	require.True(t, agent.isDeleted(created.SessionId),
		"the serialized delete did not retain its marker")

	agent.mu.Lock()
	_, mapped := agent.sessions[created.SessionId]
	agent.mu.Unlock()
	require.False(t, mapped, "the losing replacement was installed anyway")

	listed, listErr := agent.ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, listErr)
	require.Empty(t, listed.Sessions, "the deleted session is listable again")

	rows, rowErr := store.ListSessions(ctx)
	require.NoError(t, rowErr)
	require.Empty(t, rows, "the deleted session's durable row came back")

	_, reloadErr := agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd))
	requireInvalidParamsData(t, reloadErr, map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
}

// TestRollbackStartedSessionKeepsTheRefusalTheRequestOwes proves the answer to a
// refused installation is the refusal itself. A load that lost its race with a
// delete owes the host the uniform unknown-session invalid params, and wrapping
// that in a join would turn it into an internal error; only a teardown that
// itself failed has anything to add.
func TestRollbackStartedSessionKeepsTheRefusalTheRequestOwes(t *testing.T) {
	t.Parallel()

	refusal := acp.NewInvalidParams(map[string]any{
		jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID,
	})

	t.Run("clean teardown", func(t *testing.T) {
		t.Parallel()

		agent := NewAgent()
		current := testSession(t, agent, newFakeOpenCodeClient())

		requireInvalidParamsData(t, agent.rollbackStartedSession(current, refusal),
			map[string]any{jsonFieldError: errValueSessionUnknown, jsonFieldField: jsonFieldSessionID})
	})

	t.Run("teardown that failed too", func(t *testing.T) {
		t.Parallel()

		client := newFakeOpenCodeClient()
		client.closeErr = errors.New("disconnect failed")
		agent := NewAgent()
		current := testSession(t, agent, client)

		err := agent.rollbackStartedSession(current, refusal)
		require.ErrorIs(t, err, refusal)
		require.ErrorContains(t, err, "disconnect failed")
	})
}
func TestAgentStoreAndActiveLoadMatchEdges(t *testing.T) {
	agent := NewAgent()
	id := acp.SessionId("deleted")
	agent.deleted[id] = struct{}{}
	require.Error(t, agent.storeStartedSession(&session{id: id}))

	active := &session{
		cwd: absTestPath("cwd"), providerID: "provider", modelID: "model", mode: "build", permission: "ask",
		outputSchema: map[string]any{"type": "object"}, carrier: sessionCarrier{},
	}
	snapshot := active.snapshot()
	require.False(t, activeLoadRequestMatches(snapshot, active, absTestPath("cwd"), nil, nil, nil,
		sessionMeta{Model: "other/model"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, absTestPath("cwd"), nil, nil, nil,
		sessionMeta{Mode: "plan"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, absTestPath("cwd"), nil, nil, nil,
		sessionMeta{PermissionSet: true, Permission: "allow"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, absTestPath("cwd"), nil, nil, nil,
		sessionMeta{OutputSchema: map[string]any{"type": "array"}}, sessionCarrier{}))
}

// A stored snapshot this adapter will not replay is the one internal failure a
// host can act on, so `session/load` and `session/resume` classify it instead of
// reducing it to the unclassified handler token. A restore error that already
// carries its own wire classification keeps it, and neither shape ever carries
// native or store prose.
func TestRestoreFailureIsClassifiedAndCarriesNoProse(t *testing.T) {
	const storeSecret = "sourceCwd /home/operator/private escapes target"

	agent := NewAgent()

	t.Run("already classified errors pass through", func(t *testing.T) {
		refusal := unsupportedField("_meta.opencode.options.model")

		got := agent.classifyRestoreFailure(t.Context(), "session-1", fmt.Errorf("wrapped: %w", refusal))

		var reqErr *acp.RequestError
		require.ErrorAs(t, got, &reqErr)
		require.Equal(t, -32602, reqErr.Code)
		require.Equal(t, map[string]any{
			jsonFieldError: errValueUnsupported,
			jsonFieldField: "_meta.opencode.options.model",
		}, reqErr.Data)
	})

	t.Run("a store that failed its own I/O passes through", func(t *testing.T) {
		storeErr := errors.New("store failed")

		got := agent.classifyRestoreFailure(t.Context(), "session-1", storeErr)

		require.Same(t, storeErr, got)
	})

	t.Run("an unrestorable snapshot becomes the closed restore token", func(t *testing.T) {
		got := agent.classifyRestoreFailure(t.Context(), "session-1", unrestorableSnapshot(errors.New(storeSecret)))

		var reqErr *acp.RequestError
		require.ErrorAs(t, got, &reqErr)
		require.Equal(t, -32603, reqErr.Code)
		require.Equal(t, map[string]any{jsonFieldError: errValueRestoreFailed}, reqErr.Data)

		encoded, err := json.Marshal(reqErr)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), storeSecret)
		require.NotContains(t, string(encoded), errValueInternalFailure)
	})

	t.Run("an agent with no logger still classifies", func(t *testing.T) {
		got := (&Agent{}).classifyRestoreFailure(t.Context(), "session-1", unrestorableSnapshot(errors.New(storeSecret)))

		var reqErr *acp.RequestError
		require.ErrorAs(t, got, &reqErr)
		require.Equal(t, map[string]any{jsonFieldError: errValueRestoreFailed}, reqErr.Data)
	})
}

// forkLineageAgent builds an agent whose fake runtime answers a parent and a
// forked native session, each with its own sync aggregate. OpenCode's native
// fork copies the branched history into the fork's own aggregate, so the fake
// models the same thing: two independent aggregates, and a fork whose native
// directory is the parent's.
func forkLineageAgent(t *testing.T) (*Agent, *fakeOpenCodeClient) {
	t.Helper()

	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-parent")
	client.forkSession = testNativeSession("native-fork")
	client.getSessionFunc = func(_ context.Context, id string) (opencode.NativeSession, error) {
		return testNativeSession(id), nil
	}
	client.ensureSyncAggregate("native-parent")
	client.ensureSyncAggregate("native-fork")

	agent := NewAgent()
	agent.runtime = client
	agent.setAgentClient(newRecordingAgentClient())

	t.Cleanup(func() { _ = agent.Close() })

	return agent, client
}

// TestForkInheritsTheParentWorkspace pins the one cwd rule fork can honour.
// OpenCode's `POST /session/{id}/fork` keeps the source session's directory and
// ignores the `directory` query parameter, so a fork that named another
// workspace would report a cwd its native session does not have. The refusal is
// the family-uniform invalid-params shape rather than a token of this adapter's
// own.
func TestForkInheritsTheParentWorkspace(t *testing.T) {
	ctx := context.Background()
	agent, _ := forkLineageAgent(t)
	cwd := t.TempDir()

	parent, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)

	_, err = agent.forkSession(ctx, ForkSessionRequest(parent.SessionId, t.TempDir()))
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: jsonFieldCwd,
	})

	// A relative cwd takes the same verdict on the same field: one uniform
	// answer for every cwd this surface refuses.
	_, err = agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: parent.SessionId, Cwd: "relative"})
	requireInvalidParamsData(t, err, map[string]any{
		jsonFieldError: errValueUnsupported,
		jsonFieldField: jsonFieldCwd,
	})

	// The parent's own spelling is accepted, and so is a differently spelled
	// path naming the same directory; either way the fork records the parent's.
	fork, err := agent.forkSession(ctx, ForkSessionRequest(parent.SessionId, cwd+string(filepath.Separator)))
	require.NoError(t, err)

	agent.mu.Lock()
	forked := agent.sessions[fork.SessionId]
	agent.mu.Unlock()

	require.NotNil(t, forked)
	require.Equal(t, cwd, forked.snapshot().cwd)
	require.Equal(t, string(parent.SessionId), forked.snapshot().idmap.ParentSessionID)
}

// TestForkSharesTheParentDirectoryPrincipal proves a fork is admitted alongside
// its parent in one canonical directory. It has to be: the native fork cannot
// leave the parent's workspace, so refusing the second holder would make fork
// unusable while its parent is loaded. Sharing is limited to one fork lineage —
// an unrelated session still owns the directory exclusively.
func TestForkSharesTheParentDirectoryPrincipal(t *testing.T) {
	ctx := context.Background()
	agent, _ := forkLineageAgent(t)
	cwd := t.TempDir()

	parent, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)

	fork, err := agent.forkSession(ctx, ForkSessionRequest(parent.SessionId, cwd))
	require.NoError(t, err)

	// An unrelated session naming the same directory is still refused.
	_, err = agent.bindDirectory("unrelated", "", cwd, nil)
	require.ErrorContains(t, err, errValueBackpressure)

	// Either direction of the lineage may join, whichever holder is present.
	release, err := agent.bindDirectory("later-fork", parent.SessionId, cwd, nil)
	require.NoError(t, err)
	release()

	// A closed parent does not orphan the lineage: the fork's durable parent
	// identity still names the root the parent resumes into.
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: parent.SessionId})
	require.NoError(t, err)

	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(parent.SessionId, cwd))
	require.NoError(t, err)

	// The principal survives while a co-holder remains and is gone once the
	// last holder leaves.
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: fork.SessionId})
	require.NoError(t, err)
	require.NotEmpty(t, agent.directoryHoldersForTest(t, cwd))

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: parent.SessionId})
	require.NoError(t, err)
	require.Empty(t, agent.directoryHoldersForTest(t, cwd))
}
