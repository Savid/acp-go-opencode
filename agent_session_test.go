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

	_, err = agent.Prompt(ctx, TextPromptRequest(created.SessionId, "failed-refresh", "do not run"))
	require.ErrorContains(t, err, "refresh OpenCode MCP catalog")

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
	client.hangsAfterDispatch(started)
	client.abortFunc = func(id string) error {
		client.publishSessionIdle(id)

		return nil
	}

	done := make(chan acp.PromptResponse, 1)

	go func() {
		response, promptErr := current.Prompt(ctx, TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
		require.NoError(t, promptErr)
		done <- response
	}()

	<-started
	require.NoError(t, agent.Cancel(ctx, CancelRequest(current.id, internalSeamTurnNonce)))
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)

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
	agent.setAgentClient(newRecordingAgentClient())
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
	require.NoError(t, agent.Cancel(ctx, CancelRequest(current.id, internalSeamTurnNonce)))
	require.ErrorContains(t, <-done, "sync history unavailable")
	require.NotNil(t, agent.runtime, "a commit failure retired the shared runtime")

	retained, err := store.Load(ctx, SessionKey{SessionID: string(current.id), Subpath: SessionStoreMainSubpath})
	require.NoError(t, err)
	require.Equal(t, []SessionStoreEntry{entry}, retained)
}
func TestRuntimeResourceHooksAcquireRejectAndReleaseExactlyOnce(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-1")
	client.agents = []opencode.NativeAgent{{Name: "build"}}
	var nativeAcquire, scratchAcquire, nativeRelease, scratchRelease atomic.Int64
	hooks := RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			nativeAcquire.Add(1)

			return func() { nativeRelease.Add(1) }, nil
		},
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			scratchAcquire.Add(1)

			return func() { scratchRelease.Add(1) }, nil
		},
	}
	agent := NewAgent(WithScratchDir(t.TempDir()), WithRuntimeResourceHooks(hooks), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) { return client, nil }
	})
	_, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("openai/gpt-test")))))
	require.NoError(t, err)
	require.EqualValues(t, 1, nativeAcquire.Load())
	require.EqualValues(t, 1, scratchAcquire.Load())
	require.NoError(t, agent.Close())
	require.EqualValues(t, 1, nativeRelease.Load())
	require.EqualValues(t, 1, scratchRelease.Load())

	rejected := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) { return nil, errors.New("pool full") },
	}))
	_, err = rejected.sharedRuntime(ctx)
	require.ErrorContains(t, err, "pool full")
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

	var factoryCalls, nativeReleases, scratchReleases atomic.Int64
	agent := NewAgent(WithScratchDir(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleases.Add(1) }, nil
		},
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleases.Add(1) }, nil
		},
	}), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			if factoryCalls.Add(1) == 1 {
				return first, nil
			}

			return second, nil
		}
	})

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	close(first.runtimeExited)
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		retained := agent.runtime == nil && len(agent.sessions) == 1
		agent.mu.Unlock()

		return retained && nativeReleases.Load() == 1 && scratchReleases.Load() == 1
	}, time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, nativeReleases.Load())
	require.EqualValues(t, 1, scratchReleases.Load())

	response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "recovery-turn", "continue after restart"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	require.EqualValues(t, 2, factoryCalls.Load())
	require.Len(t, agent.sessions, 1)
	require.NoError(t, agent.sessions[created.SessionId].runtimeFailure())
	require.NoError(t, agent.Close())
	require.EqualValues(t, 2, nativeReleases.Load())
	require.EqualValues(t, 2, scratchReleases.Load())
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
		events: make(chan opencode.Event), errs: make(chan error),
	}, nil
}

type fanoutScope struct {
	opencode.Client
	root      *fakeOpenCodeClient
	directory string
	nativeID  string
	events    chan opencode.Event
	errs      chan error
}

func (s *fanoutScope) Close(context.Context) error { return nil }

func (s *fanoutScope) CreateSessionWithPolicy(context.Context, string, []opencode.PermissionRule) (opencode.NativeSession, error) {
	native := testNativeSession(s.nativeID)
	native.Directory = s.directory
	s.root.ensureSyncAggregate(s.nativeID)

	return native, nil
}

func (s *fanoutScope) SendMessage(_ context.Context, id string, request opencode.MessageRequest) (opencode.NativeMessage, error) {
	if id != s.nativeID {
		return opencode.NativeMessage{}, fmt.Errorf("scope %q received native session %q", s.nativeID, id)
	}

	text, _ := request.Parts[0][partTypeText].(string)
	if err := os.WriteFile(filepath.Join(s.directory, "native-cwd-proof.txt"), []byte(text), 0o600); err != nil {
		return opencode.NativeMessage{}, err
	}

	return opencode.NativeMessage{Info: opencode.NativeMessageInfo{
		ID: "assistant-" + id, SessionID: id, Role: "assistant", Finish: "stop",
	}}, nil
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

	s.events <- opencode.Event{
		Type:       opencode.EventMessageUpdated,
		Properties: mustJSONValue(map[string]any{"info": map[string]any{"id": assistantID, "sessionID": id, "role": "assistant"}}),
	}
	s.events <- opencode.Event{
		Type:       opencode.EventSessionIdle,
		Properties: mustJSONValue(map[string]any{"sessionID": id}),
	}

	return nil
}

func (s *fanoutScope) Events() <-chan opencode.Event      { return s.events }
func (s *fanoutScope) EventErrors() <-chan error          { return s.errs }
func (s *fanoutScope) XDGDirs() opencode.XDGDirs          { return s.root.XDGDirs() }
func (s *fanoutScope) RuntimeExited() <-chan struct{}     { return s.root.RuntimeExited() }
func (s *fanoutScope) Shutdown(ctx context.Context) error { return s.root.Shutdown(ctx) }

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
	parent.permission = openCodePermissionDeny
	agent.sessions[parent.id] = parent
	_, err := agent.forkSession(context.Background(), ForkSessionRequest(parent.id, t.TempDir(),
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
			ExtraPathDirs: []string{"/original/bin"},
		}),
	))
	require.NoError(t, err)
	require.NotEmpty(t, created.SessionId)
	require.Equal(t, []string{"/original/bin"}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-one", "EMPTY": ""}, client.scopes()[0].Env)

	listed, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	loaded, err := agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	require.NotNil(t, loaded.Meta)
	// A cold load keeps the durable directories and starts from no environment:
	// an operation bearer is never frozen into the store, so the loading request
	// owns it.
	require.Equal(t, []string{"/original/bin"}, client.scopes()[1].ExtraPathDirs)
	require.Empty(t, client.scopes()[1].Env)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	resumed, err := agent.ResumeSession(ctx, ResumeSessionRequest(created.SessionId, cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeExtraPathDirs("/replacement/bin"),
			WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "rotated"}),
		)),
	))
	require.NoError(t, err)
	require.NotNil(t, resumed.Meta)
	require.Equal(t, []string{"/replacement/bin"}, client.scopes()[2].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "rotated"}, client.scopes()[2].Env)

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(created.SessionId))
	require.NoError(t, err)
	_, err = agent.LoadSession(ctx, LoadSessionRequest(created.SessionId, cwd))
	require.Error(t, err)

	listed, err = agent.ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
	require.NoError(t, agent.Close())
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
	require.ErrorContains(t, err, "fingerprint entropy failed")
	require.Contains(t, requireInternalErrorData(t, err)[jsonFieldError], "fingerprint entropy failed")

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
	list := ListSessionsRequest(WithListSessionsCwd("/repo"))
	require.Equal(t, "/repo", *list.Cwd)
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

	release, err := agent.bindDirectory("one", link, servers)
	require.NoError(t, err)
	_, err = agent.bindDirectory("two", realDir, servers)
	require.Error(t, err)
	_, err = agent.bindDirectory("one", realDir, []opencode.MCPServerConfig{{Name: "other", URL: "https://other"}})
	require.Error(t, err)
	release()
	_, err = agent.bindDirectory("one", filepath.Join(root, "missing"), nil)
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
		"model":  func(client *fakeOpenCodeClient, _ *Agent) { client.providersErr = errors.New("providers failed") },
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
		"model":       func(client *fakeOpenCodeClient) { client.providersErr = errors.New("providers failed") },
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
		agent.sessions[parent.id] = parent

		return agent, client, parent
	}

	agent, _, parent := newForkAgent()
	response, err := agent.forkSession(ctx, ForkSessionRequest(parent.id, t.TempDir()))
	require.NoError(t, err)
	require.NotEmpty(t, response.SessionId)

	for name, configure := range map[string]func(*Agent, *fakeOpenCodeClient){
		"native fork": func(_ *Agent, client *fakeOpenCodeClient) { client.forkErr = errors.New("fork failed") },
		"scope":       func(_ *Agent, client *fakeOpenCodeClient) { client.scopeErr = errors.New("scope failed") },
		"model":       func(_ *Agent, client *fakeOpenCodeClient) { client.providersErr = errors.New("providers failed") },
		"get":         func(_ *Agent, client *fakeOpenCodeClient) { client.getErr = errors.New("get failed") },
		"snapshot": func(_ *Agent, client *fakeOpenCodeClient) {
			client.syncHistoryErr = errors.New("snapshot failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			testAgent, client, testParent := newForkAgent()
			configure(testAgent, client)
			_, testErr := testAgent.forkSession(ctx, ForkSessionRequest(testParent.id, t.TempDir()))
			require.Error(t, testErr)
		})
	}

	agent, _, parent = newForkAgent()
	oldReader := sessionIDRandReader
	sessionIDRandReader = errorReader{err: errors.New("entropy failed")}
	_, err = agent.forkSession(ctx, ForkSessionRequest(parent.id, t.TempDir()))
	require.ErrorContains(t, err, "entropy failed")
	sessionIDRandReader = oldReader

	_, err = agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: parent.id, Cwd: "relative"})
	require.Error(t, err)
	_, err = agent.forkSession(ctx, acp.UnstableForkSessionRequest{SessionId: parent.id, Cwd: t.TempDir(), Meta: map[string]any{opencodeMetaKey: "bad"}})
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
		parent.carrier = newSessionCarrier(map[string]string{"WAGIE_API_TOKEN": "parent-token"}, []string{"/parent/bin"})
		agent.sessions[parent.id] = parent

		return agent, client, parent
	}

	agent, client, parent := newForkAgent()
	_, err := agent.forkSession(t.Context(), ForkSessionRequest(parent.id, t.TempDir()))
	require.NoError(t, err)
	require.Equal(t, []string{"/parent/bin"}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "parent-token"}, client.scopes()[0].Env)

	agent, client, parent = newForkAgent()
	_, err = agent.forkSession(t.Context(), ForkSessionRequest(parent.id, t.TempDir(),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeExtraPathDirs("/child/bin"))),
	))
	require.NoError(t, err)
	require.Equal(t, []string{"/child/bin"}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "parent-token"}, client.scopes()[0].Env,
		"replacing one half of the carrier must leave the other half alone")

	// A child that names an environment replaces the parent's outright, and an
	// empty map is a replacement rather than an omission.
	agent, client, parent = newForkAgent()
	_, err = agent.forkSession(t.Context(), ForkSessionRequest(parent.id, t.TempDir(),
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{}))),
	))
	require.NoError(t, err)
	require.Equal(t, []string{"/parent/bin"}, client.scopes()[0].ExtraPathDirs)
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
	snapshot := validSyncSnapshot("session", "native", "/source")
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

	client = newFakeOpenCodeClient()
	client.commandsErr = errors.New("commands failed")
	agent = NewAgent()
	session := testSession(t, agent, client)
	agent.sessions[session.id] = session
	require.NoError(t, agent.establishSession(ctx, session))

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
	_, err = agent.ResumeSession(ctx, ResumeSessionRequest("session", cwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeModel("missing/model")))))
	require.Error(t, err)

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
	agent.sessions[parent.id] = parent

	_, err := agent.forkSession(context.Background(), acp.UnstableForkSessionRequest{
		SessionId:  parent.id,
		Cwd:        t.TempDir(),
		McpServers: []acp.UnstableMcpServer{{Sse: &acp.UnstableMcpServerSse{Name: "bad"}}},
	})
	require.Error(t, err)

	_, err = agent.forkSession(context.Background(), ForkSessionRequest(parent.id, t.TempDir()))
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
		WithEnv(map[string]string{"PATH": "/static/bin"}),
		func(options *Options) {
			options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
				started = options

				return client, nil
			}
		},
	)
	agent.setAgentClient(newRecordingAgentClient())

	_, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeExtraPathDirs("/session/bin"),
		WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "session-token"}),
	))))
	require.NoError(t, err)
	require.Equal(t, []string{"/session/bin"}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "session-token"}, client.scopes()[0].Env)

	// The shared process is started under the operator's environment only. A
	// session value there would be the same value for every session of the
	// Agent and would outlive the session that asked for it.
	require.Equal(t, "/static/bin", started.Env["PATH"])
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

	_, err := agent.NewSession(ctx, session("/one", "/shared", "/one"))
	require.NoError(t, err)

	_, err = agent.NewSession(ctx, session("/two", "/shared"))
	require.NoError(t, err)
	require.EqualValues(t, 1, factoryCalls.Load())
	require.Equal(t, []string{"/one", "/shared", "/one"}, client.scopes()[0].ExtraPathDirs)
	require.Equal(t, []string{"/two", "/shared"}, client.scopes()[1].ExtraPathDirs)
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
		WithOpenCodeExtraPathDirs("/session/bin"),
	))))
	require.NoError(t, err)
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
	require.Equal(t, []string{"/session/bin"}, second.scopes()[0].ExtraPathDirs)
	require.NoError(t, agent.Close())
}
