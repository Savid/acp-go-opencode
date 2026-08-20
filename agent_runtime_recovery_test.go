package opencodeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/lifecycle"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type recoveryClient struct {
	*fakeOpenCodeClient
	scopeFunc     func(context.Context, opencode.ScopeOptions) (opencode.Client, error)
	providersFunc func(context.Context) (opencode.ProvidersResponse, error)
	getFunc       func(context.Context, string) (opencode.NativeSession, error)
}

func (c *recoveryClient) Scope(ctx context.Context, options opencode.ScopeOptions) (opencode.Client, error) {
	if c.scopeFunc != nil {
		return c.scopeFunc(ctx, options)
	}

	return c, nil
}

func (c *recoveryClient) ConfigProviders(ctx context.Context) (opencode.ProvidersResponse, error) {
	if c.providersFunc != nil {
		return c.providersFunc(ctx)
	}

	return c.fakeOpenCodeClient.ConfigProviders(ctx)
}

func (c *recoveryClient) GetSession(ctx context.Context, id string) (opencode.NativeSession, error) {
	if c.getFunc != nil {
		return c.getFunc(ctx, id)
	}

	return c.fakeOpenCodeClient.GetSession(ctx, id)
}

type cancelOnLoadStore struct {
	SessionStore
	cancel context.CancelFunc
}

func (s cancelOnLoadStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	entries, err := s.SessionStore.Load(ctx, key)
	s.cancel()

	return entries, err
}

func recoveryFixture(t *testing.T, store SessionStore, factories ...opencode.Client) (*Agent, *session) {
	t.Helper()

	if store == nil {
		store = NewInMemorySessionStore()
	}

	snapshot := validSyncSnapshot("session-1", "native-1", t.TempDir())
	info, err := json.Marshal(map[string]any{"id": "native-1", syncFieldDirectory: snapshot.Session.Cwd})
	require.NoError(t, err)
	snapshot.Events["native-1"][0].Data["info"] = info
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NoError(t, store.Replace(context.Background(), SessionKey{SessionID: "session-1"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session-1"}, Entries: []SessionStoreEntry{encoded},
	}}))

	var calls atomic.Int64
	agent := NewAgent(WithHome(t.TempDir()), WithSessionStore(store), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			index := int(calls.Add(1)) - 1
			if index >= len(factories) {
				return nil, fmt.Errorf("unexpected recovery factory call %d", index+1)
			}

			return factories[index], nil
		}
	})
	old := newFakeOpenCodeClient()
	native := testNativeSession("native-1")
	current := newSession(agent, "session-1", snapshot.Session.Cwd, nil, native, old, sessionMeta{}, idmapRecord{
		SessionID: "session-1", NativeSessionID: "native-1", Format: SessionStoreFormat,
	})
	current.runtimeLostCause = "runtime exited"
	agent.sessions[current.id] = current

	return agent, current
}

func readyRecoveryClient() *recoveryClient {
	fake := newFakeOpenCodeClient()
	fake.getSession = testNativeSession("native-1")
	fake.agents = []opencode.NativeAgent{{Name: "build"}}

	return &recoveryClient{fakeOpenCodeClient: fake}
}

func TestRuntimeGenerationAndRecoveryFailureBranches(t *testing.T) {
	t.Run("store publication generation mismatch", func(t *testing.T) {
		agent := NewAgent()
		agent.runtime = newFakeOpenCodeClient()
		agent.runtimeGeneration = 2
		current := testSession(t, agent, newFakeOpenCodeClient())
		current.runtimeGeneration = 1
		require.ErrorContains(t, agent.storeStartedSession(current), "runtime generation changed")
	})

	t.Run("shared runtime retires already exited generation", func(t *testing.T) {
		exited := newFakeOpenCodeClient()
		close(exited.runtimeExited)
		replacement := newFakeOpenCodeClient()
		agent := NewAgent(func(options *Options) {
			options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
				return replacement, nil
			}
		})
		agent.runtime = exited
		agent.runtimeGeneration = 1
		got, generation, err := agent.sharedRuntimeBinding(context.Background())
		require.NoError(t, err)
		require.Same(t, replacement, got)
		require.EqualValues(t, 2, generation)
		require.NoError(t, agent.Close())
	})

	t.Run("generation predicate shapes", func(t *testing.T) {
		agent := NewAgent()
		require.False(t, agent.runtimeGenerationIsCurrent(1))

		nilExit := newFakeOpenCodeClient()
		nilExit.runtimeExited = nil
		agent.runtime = nilExit
		agent.runtimeGeneration = 1
		require.True(t, agent.runtimeGenerationIsCurrent(1))

		exited := newFakeOpenCodeClient()
		close(exited.runtimeExited)
		agent.runtime = exited
		require.False(t, agent.runtimeGenerationIsCurrent(1))

		agent = NewAgent()
		agent.runtime = exited
		agent.runtimeGeneration = 1
		current := testSession(t, agent, newFakeOpenCodeClient())
		installed, closed := current.installRecoveredRuntime(exited, func() {}, current.idmap, 1)
		require.False(t, installed)
		require.False(t, closed)

		agent.runtime = newFakeOpenCodeClient()
		agent.runtimeGeneration = 2
		installed, closed = current.installRecoveredRuntime(exited, func() {}, current.idmap, 1)
		require.False(t, installed)
		require.False(t, closed)
	})

	t.Run("scope crash observes caller cancellation", func(t *testing.T) {
		candidate := readyRecoveryClient()
		candidate.scopeFunc = func(context.Context, opencode.ScopeOptions) (opencode.Client, error) {
			close(candidate.runtimeExited)

			return nil, errors.New("scope crashed")
		}
		agent := NewAgent()
		agent.runtime = candidate
		agent.runtimeGeneration = 1
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _, err := agent.newOpenCodeClient(ctx, "session", t.TempDir(), nil, sessionCarrier{})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("closed session", func(t *testing.T) {
		agent, current := recoveryFixture(t, nil, readyRecoveryClient())
		current.closed = true
		require.ErrorContains(t, current.ensureRuntime(context.Background()), errValueSessionUnknown)
		require.NoError(t, agent.Close())
	})

	t.Run("prompt fences watcher detach window", func(t *testing.T) {
		candidate := readyRecoveryClient()
		agent, current := recoveryFixture(t, nil, candidate)
		current.runtimeLostCause = ""
		current.runtimeGeneration = 1
		require.NoError(t, current.ensureRuntime(context.Background()))
		require.Empty(t, current.runtimeLostCause)
		require.EqualValues(t, 1, current.runtimeGeneration)
		require.NoError(t, agent.Close())
	})

	t.Run("store error", func(t *testing.T) {
		store := &errorSessionStore{err: errors.New("store failed")}
		agent := NewAgent(WithSessionStore(store))
		current := testSession(t, agent, newFakeOpenCodeClient())
		current.runtimeLostCause = "runtime exited"
		require.ErrorContains(t, current.ensureRuntime(context.Background()), "store failed")
	})

	t.Run("missing committed generation reaches prompt as the loss", func(t *testing.T) {
		agent := NewAgent()
		current := testSession(t, agent, newFakeOpenCodeClient())
		current.runtimeLostCause = "runtime exited"
		_, err := current.promptWithRoute(context.Background(), TextPromptRequest(current.id, "turn", "hello"), "turn", lifecycle.Submission{})
		assertTurnFailed(t, err, causeTransport, "runtime exited")
	})

	t.Run("cancelled after successful store load", func(t *testing.T) {
		store := NewInMemorySessionStore()
		ctx, cancel := context.WithCancel(context.Background())
		wrapped := cancelOnLoadStore{SessionStore: store, cancel: cancel}
		agent, current := recoveryFixture(t, wrapped, readyRecoveryClient())
		require.ErrorIs(t, current.ensureRuntime(ctx), context.Canceled)
		require.NoError(t, agent.Close())
	})

	t.Run("cancelled between recovery attempts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		crashed := readyRecoveryClient()
		crashed.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
			close(crashed.runtimeExited)
			cancel()

			return nil, errors.New("generation exited")
		}
		agent, current := recoveryFixture(t, nil, crashed)
		require.ErrorIs(t, current.ensureRuntime(ctx), context.Canceled)
		require.NoError(t, agent.Close())
	})

	t.Run("runtime construction error", func(t *testing.T) {
		agent, current := recoveryFixture(t, nil)
		require.ErrorContains(t, current.ensureRuntime(context.Background()), "unexpected recovery factory call")
		require.NoError(t, agent.Close())
	})

	t.Run("restore error on live generation", func(t *testing.T) {
		candidate := readyRecoveryClient()
		candidate.syncHistoryErr = errors.New("history failed")
		agent, current := recoveryFixture(t, nil, candidate)
		require.ErrorContains(t, current.ensureRuntime(context.Background()), "history failed")
		require.NoError(t, agent.Close())
	})

	t.Run("restore discards exited generation", func(t *testing.T) {
		crashed := readyRecoveryClient()
		crashed.syncHistoryFunc = func(context.Context, map[string]int64) ([]opencode.SyncEvent, error) {
			close(crashed.runtimeExited)

			return nil, errors.New("generation exited")
		}
		replacement := readyRecoveryClient()
		agent, current := recoveryFixture(t, nil, crashed, replacement)
		require.NoError(t, current.ensureRuntime(context.Background()))
		require.EqualValues(t, 2, current.runtimeGeneration)
		require.NoError(t, agent.Close())
	})

	t.Run("restored native id drift", func(t *testing.T) {
		candidate := readyRecoveryClient()
		candidate.getSession = testNativeSession("other-native")
		agent, current := recoveryFixture(t, nil, candidate)
		require.ErrorContains(t, current.ensureRuntime(context.Background()), "native session id drift")
		require.NoError(t, agent.Close())
	})

	t.Run("session closes during recovery", func(t *testing.T) {
		candidate := readyRecoveryClient()
		agent, current := recoveryFixture(t, nil, candidate)
		candidate.getFunc = func(context.Context, string) (opencode.NativeSession, error) {
			current.mu.Lock()
			current.closed = true
			current.mu.Unlock()

			return testNativeSession("native-1"), nil
		}
		require.ErrorContains(t, current.ensureRuntime(context.Background()), errValueSessionUnknown)
		require.NoError(t, agent.Close())
	})
}

// TestLostRuntimeFailsAPromptBeforeItIsAccepted proves a session whose runtime
// generation was lost fails its prompt with the transport cause and never
// dispatches: the loss is the answer, and no turn is accepted against a runtime
// this session no longer holds.
func TestLostRuntimeFailsAPromptBeforeItIsAccepted(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := testSession(t, NewAgent(), client)

	dispatched := false
	client.dispatchMessage = func(context.Context, string, opencode.MessageRequest) (opencode.NativeMessage, error) {
		dispatched = true

		return opencode.NativeMessage{}, nil
	}

	current.mu.Lock()
	current.runtimeLostCause = "runtime exited"
	current.mu.Unlock()

	_, err := current.Prompt(context.Background(), TextPromptRequest(current.id, internalSeamTurnNonce, "hello"))
	assertTurnFailed(t, err, causeTransport, "runtime exited")
	require.False(t, dispatched, "a lost runtime still received a frame")
}

func TestStaleDirectoryReleaseCannotDeleteRecoveredBinding(t *testing.T) {
	agent := NewAgent()
	cwd := t.TempDir()
	canonical, err := runtimeEvalSymlinks(cwd)
	require.NoError(t, err)
	canonical, err = runtimeAbs(canonical)
	require.NoError(t, err)

	staleRelease, err := agent.bindDirectory("session-1", cwd, nil)
	require.NoError(t, err)

	// Runtime retirement clears the old generation's principal map before its
	// session detach callbacks finish. Recovery may therefore rebound the same
	// logical session and directory before the stale release executes.
	agent.mu.Lock()
	agent.directories = make(map[string]directoryBinding)
	agent.mu.Unlock()

	recoveredRelease, err := agent.bindDirectory("session-1", cwd, nil)
	require.NoError(t, err)

	releaseStale := make(chan struct{})
	staleReleased := make(chan struct{})
	go func() {
		<-releaseStale
		staleRelease()
		close(staleReleased)
	}()

	close(releaseStale)
	<-staleReleased

	agent.mu.Lock()
	recovered, ok := agent.directories[canonical]
	agent.mu.Unlock()
	require.True(t, ok)
	require.Equal(t, acp.SessionId("session-1"), recovered.SessionID)
	require.NotZero(t, recovered.Incarnation)

	recoveredRelease()
	agent.mu.Lock()
	_, ok = agent.directories[canonical]
	agent.mu.Unlock()
	require.False(t, ok)
}

var _ acp.Agent = (*Agent)(nil)

func TestArtifactLoadRejectsCorruptRecordOnRecoveryPaths(t *testing.T) {
	corrupt := func(t *testing.T, agent *Agent) {
		t.Helper()

		key := SessionKey{SessionID: "session-1", Subpath: imageArtifactSubpath("corrupt")}
		require.NoError(t, agent.sessionStore().Append(context.Background(), key, []SessionStoreEntry{json.RawMessage(`{`)}))
	}

	t.Run("load or resume", func(t *testing.T) {
		agent, current := recoveryFixture(t, nil, readyRecoveryClient())
		corrupt(t, agent)
		_, err := agent.loadOrResumeSession(context.Background(), "session-1", current.cwd, nil, nil, nil)
		require.Error(t, err)
		require.NoError(t, agent.Close())
	})

	t.Run("ensure runtime", func(t *testing.T) {
		agent, current := recoveryFixture(t, nil, readyRecoveryClient())
		corrupt(t, agent)
		require.Error(t, current.ensureRuntime(context.Background()))
		require.NoError(t, agent.Close())
	})
}
