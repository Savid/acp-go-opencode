package opencodeacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type proofFailureRuntimeClient struct {
	*fakeOpenCodeClient
	entered chan struct{}
	resume  chan struct{}
	err     error
	calls   atomic.Int32
}

type panickingRuntimeExitClient struct {
	*fakeOpenCodeClient
}

type panickingRuntimeShutdownClient struct {
	*fakeOpenCodeClient
}

func (*panickingRuntimeExitClient) RuntimeExited() <-chan struct{} {
	panic("runtime exit channel panic")
}

func (*panickingRuntimeShutdownClient) Shutdown(context.Context) error {
	panic("runtime shutdown panic")
}

func TestImplicitRuntimeHomeIsDirectChildOfScratch(t *testing.T) {
	scratch := t.TempDir()
	agent := NewAgent(WithScratchDir(scratch))

	require.Equal(t, filepath.Join(scratch, defaultAgentName), agent.homeRoot())
}

func (client *proofFailureRuntimeClient) Shutdown(context.Context) error {
	if client.calls.Add(1) == 1 {
		close(client.entered)
	}
	<-client.resume

	return client.err
}

func TestRuntimeRetirementMemoizesExactGenerationResult(t *testing.T) {
	containmentErr := errors.Join(errors.New("containment failed"), opencode.ErrProcessContainmentIncomplete)
	client := &proofFailureRuntimeClient{
		fakeOpenCodeClient: newFakeOpenCodeClient(),
		entered:            make(chan struct{}),
		resume:             make(chan struct{}),
		err:                containmentErr,
	}
	agent := NewAgent(WithHome(t.TempDir()))
	agent.runtime = client
	agent.runtimeGeneration = 1

	results := make(chan error, 2)
	go func() { results <- agent.retireSharedRuntime(1, "cancel") }()
	go func() { results <- agent.retireSharedRuntime(1, "timeout") }()
	<-client.entered
	close(client.resume)
	first := <-results
	second := <-results
	require.True(t, first == second, "concurrent callers must receive one exact memoized result")
	require.ErrorIs(t, first, containmentErr)
	require.EqualValues(t, 1, client.calls.Load())

	secondClient := newFakeOpenCodeClient()
	agent.mu.Lock()
	agent.runtime = secondClient
	agent.runtimeGeneration = 2
	agent.runtimeFatalErr = nil
	agent.mu.Unlock()
	require.NoError(t, agent.retireSharedRuntime(2, "next generation"))
	require.True(t, first == agent.retireSharedRuntime(1, "late first-generation waiter"))

	missing := NewAgent()
	require.ErrorIs(t, missing.retireSharedRuntime(9, "missing"), opencode.ErrProcessContainmentIncomplete)
	missing.runtimeFatalErr = containmentErr
	require.ErrorIs(t, missing.retireSharedRuntime(9, "fatal"), containmentErr)

	nilTarget := NewAgent(WithHome(t.TempDir()))
	nilTarget.runtime = newFakeOpenCodeClient()
	nilTarget.runtimeGeneration = 1
	require.NoError(t, nilTarget.retireSharedRuntime(1, "nil target", nil))
}

func TestRuntimeRetirementContainsDetachPanic(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := &session{
		runtimeGeneration: 1,
		directoryRelease: func() {
			panic("detach release panic")
		},
	}
	agent := NewAgent(WithHome(t.TempDir()))
	agent.runtime = client
	agent.runtimeGeneration = 1
	agent.sessions["panic"] = current

	var err error
	require.NotPanics(t, func() {
		err = agent.retireSharedRuntime(1, "runtime exited")
	})
	require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, agent.runtimeFatalErr, opencode.ErrProcessContainmentIncomplete)
	require.Zero(t, current.runtimeGeneration)
	require.Equal(t, "runtime exited", current.runtimeLostCause)
}

func TestRuntimeExitWatcherPublishesBoundaryPanics(t *testing.T) {
	shutdownBase := newFakeOpenCodeClient()
	releaseBase := newFakeOpenCodeClient()
	tests := []struct {
		name      string
		client    opencode.Client
		release   func()
		exitCh    chan struct{}
		panicText string
	}{
		{
			name: "shutdown",
			client: &panickingRuntimeShutdownClient{
				fakeOpenCodeClient: shutdownBase,
			},
			exitCh:    shutdownBase.runtimeExited,
			panicText: "runtime shutdown panic",
		},
		{
			name:   "resource release",
			client: releaseBase,
			release: func() {
				panic("runtime release panic")
			},
			exitCh:    releaseBase.runtimeExited,
			panicText: "runtime release panic",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := NewAgent(WithHome(t.TempDir()))
			agent.runtime = test.client
			agent.runtimeGeneration = 1
			agent.runtimeNativeRelease = test.release

			go agent.watchSharedRuntime(context.Background(), test.client, 1)
			close(test.exitCh)

			require.Eventually(t, func() bool {
				agent.mu.Lock()
				retirement := agent.runtimeRetirements[1]
				agent.mu.Unlock()
				if retirement == nil {
					return false
				}

				select {
				case <-retirement.done:
					return true
				default:
					return false
				}
			}, time.Second, time.Millisecond)

			agent.mu.Lock()
			retirement := agent.runtimeRetirements[1]
			fatalErr := agent.runtimeFatalErr
			agent.mu.Unlock()
			require.ErrorIs(t, retirement.err, opencode.ErrProcessContainmentIncomplete)
			require.ErrorContains(t, retirement.err, test.panicText)
			require.ErrorIs(t, fatalErr, opencode.ErrProcessContainmentIncomplete)
			require.True(t, retirement.err == agent.retireSharedRuntime(1, "late waiter"))
		})
	}
}

func TestAgentCloseMemoizesRetirementAndWaitsForOneAlreadyInProgress(t *testing.T) {
	containmentErr := errors.Join(errors.New("containment failed"), opencode.ErrProcessContainmentIncomplete)
	client := &proofFailureRuntimeClient{
		fakeOpenCodeClient: newFakeOpenCodeClient(),
		entered:            make(chan struct{}),
		resume:             make(chan struct{}),
		err:                containmentErr,
	}
	agent := NewAgent(WithHome(t.TempDir()))
	agent.runtime = client
	agent.runtimeGeneration = 1

	results := make(chan error, 2)
	go func() { results <- agent.Close() }()
	<-client.entered
	go func() { results <- agent.Close() }()
	close(client.resume)
	first := <-results
	second := <-results
	require.True(t, first == second)
	require.ErrorIs(t, first, containmentErr)
	require.EqualValues(t, 1, client.calls.Load())

	waiting := NewAgent()
	retirement := &runtimeRetirement{generation: 1, done: make(chan struct{}), err: containmentErr}
	waiting.runtimeGeneration = 1
	waiting.runtimeStarting = retirement.done
	waiting.runtimeRetirements[1] = retirement
	done := make(chan error, 1)
	go func() { done <- waiting.Close() }()
	close(retirement.done)
	require.ErrorIs(t, <-done, containmentErr)

	completed := NewAgent()
	completed.runtimeFatalErr = containmentErr
	require.ErrorIs(t, completed.Close(), containmentErr)
	require.ErrorIs(t, completed.Close(), containmentErr)
}

func TestAgentCloseWaitsForConstructionCleanupAndReturnsContainmentFailure(t *testing.T) {
	containmentErr := errors.Join(errors.New("containment failed"), opencode.ErrProcessContainmentIncomplete)
	client := &proofFailureRuntimeClient{
		fakeOpenCodeClient: newFakeOpenCodeClient(),
		entered:            make(chan struct{}),
		resume:             make(chan struct{}),
		err:                containmentErr,
	}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	agent := NewAgent(WithHome(t.TempDir()))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		close(factoryEntered)
		<-releaseFactory

		return client, nil
	}

	startResult := make(chan error, 1)
	go func() {
		_, err := agent.sharedRuntime(context.Background())
		startResult <- err
	}()
	<-factoryEntered

	closeResult := make(chan error, 1)
	go func() { closeResult <- agent.Close() }()
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.closed
	}, time.Second, time.Millisecond)
	close(releaseFactory)
	<-client.entered
	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before construction cleanup completed: %v", err)
	default:
	}
	close(client.resume)

	require.ErrorIs(t, <-startResult, containmentErr)
	require.ErrorIs(t, <-closeResult, containmentErr)
	require.EqualValues(t, 1, client.calls.Load())
}

func TestSharedRuntimeRemainingCoordinationBranches(t *testing.T) {
	closed := NewAgent()
	closed.closed = true
	_, err := closed.sharedRuntime(context.Background())
	require.Error(t, err)

	waiting := NewAgent()
	waiting.runtimeStarting = make(chan struct{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = waiting.sharedRuntime(cancelled)
	require.ErrorIs(t, err, context.Canceled)

	client := newFakeOpenCodeClient()
	ready := NewAgent()
	ready.runtimeStarting = make(chan struct{})
	go func() {
		ready.mu.Lock()
		ready.runtime = client
		close(ready.runtimeStarting)
		ready.runtimeStarting = nil
		ready.mu.Unlock()
	}()
	got, err := ready.sharedRuntime(context.Background())
	require.NoError(t, err)
	require.Same(t, client, got)

	var nativeReleased, scratchReleased atomic.Bool
	duringStart := NewAgent(WithScratchDir(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleased.Store(true) }, nil
		},
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleased.Store(true) }, nil
		},
	}))
	duringStart.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		duringStart.mu.Lock()
		duringStart.closed = true
		duringStart.mu.Unlock()

		return client, nil
	}
	_, err = duringStart.sharedRuntime(context.Background())
	require.Error(t, err)
	require.True(t, client.closed)
	require.True(t, nativeReleased.Load())
	require.True(t, scratchReleased.Load())
}

func TestWatchSharedRuntimeRemainingBranches(t *testing.T) {
	require.NotPanics(t, func() {
		NewAgent().watchSharedRuntime(context.Background(), &panickingRuntimeExitClient{fakeOpenCodeClient: newFakeOpenCodeClient()}, 0)
	})

	nilExit := newFakeOpenCodeClient()
	nilExit.runtimeExited = nil
	NewAgent().watchSharedRuntime(context.Background(), nilExit, 0)

	stale := newFakeOpenCodeClient()
	close(stale.runtimeExited)
	agent := NewAgent()
	agent.runtime = newFakeOpenCodeClient()
	agent.watchSharedRuntime(context.Background(), stale, 0)

	closedClient := newFakeOpenCodeClient()
	close(closedClient.runtimeExited)
	agent.runtime = closedClient
	agent.closed = true
	agent.watchSharedRuntime(context.Background(), closedClient, 0)
}

func TestStartSharedRuntimeRemainingFailureAndDefaultBranches(t *testing.T) {
	var observedProcess RuntimeProcessKind
	var observedDelta int64
	var observedSnapshot int
	var observedLifecycle RuntimeResourceKind
	var observedStage RuntimeStartupStage
	observedClient := newFakeOpenCodeClient()
	observed := NewAgent(WithHome(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		ObserveProcess: func(_ context.Context, kind RuntimeProcessKind, delta int64) {
			observedProcess = kind
			observedDelta = delta
		},
		ObserveProcessSnapshot: func(_ context.Context, kind RuntimeProcessKind, count int) {
			require.Equal(t, RuntimeProcessProviderDescendant, kind)
			observedSnapshot = count
		},
		ObserveStartupStage: func(_ context.Context, lifecycle RuntimeResourceKind, stage RuntimeStartupStage, _ time.Duration, _ error) {
			observedLifecycle = lifecycle
			observedStage = stage
		},
	}))
	observed.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		options.ObserveProcess(ctx, string(RuntimeProcessHomeLockSupervisor), 2)
		options.ObserveProcessSnapshot(ctx, string(RuntimeProcessProviderDescendant), 3)
		options.ObserveStartupStage(ctx, string(RuntimeResourceRuntime), string(RuntimeStartupReadiness), time.Second, nil)

		return observedClient, nil
	}
	runtime, nativeRelease, scratchRelease, err := observed.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.NoError(t, err)
	require.Same(t, observedClient, runtime)
	require.NotNil(t, nativeRelease)
	require.Nil(t, scratchRelease)
	require.Equal(t, RuntimeProcessHomeLockSupervisor, observedProcess)
	require.EqualValues(t, 2, observedDelta)
	require.Equal(t, 3, observedSnapshot)
	require.Equal(t, RuntimeResourceRuntime, observedLifecycle)
	require.Equal(t, RuntimeStartupReadiness, observedStage)
	nativeRelease()

	scratchFailure := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return nil, errors.New("scratch denied")
		},
	}))
	runtime, nativeRelease, scratchRelease, err = scratchFailure.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.ErrorContains(t, err, "scratch denied")
	require.Nil(t, runtime)
	require.Nil(t, nativeRelease)
	require.Nil(t, scratchRelease)

	var scratchReleased atomic.Bool
	nativeFailure := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleased.Store(true) }, nil
		},
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return nil, errors.New("native denied")
		},
	}))
	runtime, nativeRelease, scratchRelease, err = nativeFailure.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.ErrorContains(t, err, "native denied")
	require.Nil(t, runtime)
	require.Nil(t, nativeRelease)
	require.Nil(t, scratchRelease)
	require.True(t, scratchReleased.Load())

	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	var nativeReleased atomic.Bool
	xdgFailure := NewAgent(WithHome(filepath.Join(notDirectory, "child")), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleased.Store(true) }, nil
		},
	}))
	runtime, nativeRelease, scratchRelease, err = xdgFailure.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.Error(t, err)
	require.Nil(t, runtime)
	require.Nil(t, nativeRelease)
	require.Nil(t, scratchRelease)
	require.True(t, nativeReleased.Load())

	originalStart := runtimeStartServer
	runtimeStartServer = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		return nil, errors.New("default factory failed")
	}
	t.Cleanup(func() { runtimeStartServer = originalStart })
	defaultFactory := NewAgent(WithHome(t.TempDir()))
	defaultFactory.options.clientFactory = nil
	runtime, nativeRelease, scratchRelease, err = defaultFactory.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.ErrorContains(t, err, "default factory failed")
	require.Nil(t, runtime)
	require.Nil(t, nativeRelease)
	require.Nil(t, scratchRelease)
}

func TestDirectoryBindingRemainingOSHashAndReleaseBranches(t *testing.T) {
	originalEval := runtimeEvalSymlinks
	originalAbs := runtimeAbs
	originalMarshal := runtimeJSONMarshal
	t.Cleanup(func() {
		runtimeEvalSymlinks = originalEval
		runtimeAbs = originalAbs
		runtimeJSONMarshal = originalMarshal
	})

	agent := NewAgent()
	runtimeEvalSymlinks = func(string) (string, error) { return "relative", nil }
	runtimeAbs = func(string) (string, error) { return "", errors.New("abs failed") }
	_, err := agent.bindDirectory("session", "cwd", nil)
	require.ErrorContains(t, err, "abs failed")

	runtimeEvalSymlinks = originalEval
	runtimeAbs = originalAbs
	runtimeJSONMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal failed") }
	_, err = agent.directoryMCPFingerprint([]opencode.MCPServerConfig{{Name: "same"}, {Name: "same"}})
	require.ErrorContains(t, err, "marshal failed")
	_, err = agent.bindDirectory("session", t.TempDir(), []opencode.MCPServerConfig{{Name: "same"}})
	require.ErrorContains(t, err, "marshal failed")

	runtimeJSONMarshal = originalMarshal
	_, err = agent.directoryMCPFingerprint([]opencode.MCPServerConfig{{Name: "same", URL: "one"}, {Name: "same", URL: "two"}})
	require.NoError(t, err)
	_, err = agent.directoryMCPFingerprint([]opencode.MCPServerConfig{{Name: "a"}, {Name: "b"}})
	require.NoError(t, err)

	cwd := t.TempDir()
	release, err := agent.bindDirectory("one", cwd, nil)
	require.NoError(t, err)
	agent.mu.Lock()
	agent.directories[cwd] = directoryBinding{SessionID: "two"}
	agent.mu.Unlock()
	release()
	release()
}

func TestScopeCleanupFailureRetainsDirectoryPrincipal(t *testing.T) {
	cwd := t.TempDir()
	client := newFakeOpenCodeClient()
	client.scopeErr = errors.Join(opencode.ErrMCPDisconnectUnproven, errors.New("delete failed"))
	agent := NewAgent()
	agent.runtime = client

	scoped, release, generation, err := agent.newOpenCodeClient(context.Background(), "session", cwd, []opencode.MCPServerConfig{{Name: "tools"}}, runtimeEnvironment{})
	require.ErrorIs(t, err, opencode.ErrMCPDisconnectUnproven)
	require.Nil(t, scoped)
	require.Nil(t, release)
	require.Zero(t, generation)
	require.Len(t, agent.directories, 1, "unproven native cleanup must retain directory ownership")
	scoped, release, generation, err = agent.newOpenCodeClient(context.Background(), "session", cwd, []opencode.MCPServerConfig{{Name: "tools"}}, runtimeEnvironment{})
	require.ErrorIs(t, err, opencode.ErrMCPDisconnectUnproven)
	require.Nil(t, scoped)
	require.Nil(t, release)
	require.Zero(t, generation)
	require.Len(t, agent.directories, 1, "quarantined runtime must not replace the retained principal")
	require.NoError(t, agent.Close())
}

func TestDirectoryScopeCloseFailureQuarantinesWithoutRelease(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.closeErr = errors.New("disconnect failed")
	agent := NewAgent()
	agent.runtime = client
	releases := 0

	err := agent.closeDirectoryScope(client, func() { releases++ }, 0)
	require.ErrorContains(t, err, "disconnect failed")
	require.Zero(t, releases)
	require.ErrorIs(t, agent.runtimeFatalErr, opencode.ErrMCPDisconnectUnproven)

	agent.runtimeFatalErr = nil
	current := testSession(agent, client)
	current.directoryRelease = func() { releases++ }
	err = agent.closeFailedSession(current)
	require.ErrorContains(t, err, "disconnect failed")
	require.Zero(t, releases)
	require.ErrorIs(t, agent.runtimeFatalErr, opencode.ErrMCPDisconnectUnproven)

	client.closeErr = nil
	require.NoError(t, agent.Close())
}

func TestDirectoryBindingIncarnationSkipsZeroAfterWrap(t *testing.T) {
	agent := NewAgent()
	agent.directoryIncarnation = ^directoryBindingIncarnation(0)

	require.Equal(t, directoryBindingIncarnation(1), agent.nextDirectoryBindingIncarnationLocked())
}

func TestRuntimeResourceCleanupProofAndDeletionGates(t *testing.T) {
	originalRemoveAll := runtimeRemoveAll
	t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

	t.Run("ordinary post-proof error releases both permits after deletion", func(t *testing.T) {
		root := t.TempDir()
		agent := NewAgent(WithScratchDir(root))
		require.NoError(t, os.MkdirAll(agent.homeRoot(), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(agent.homeRoot(), "runtime-state"), []byte("state"), 0o600))

		var nativeReleased, scratchReleased atomic.Bool
		ordinary := errors.New("ordinary post-proof shutdown error")
		err := agent.cleanupRuntimeResources(
			ordinary,
			func() { nativeReleased.Store(true) },
			func() { scratchReleased.Store(true) },
		)
		require.ErrorIs(t, err, ordinary)
		require.True(t, nativeReleased.Load())
		require.True(t, scratchReleased.Load())
		_, statErr := os.Stat(agent.homeRoot())
		require.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("unproven tree retains both permits and scratch", func(t *testing.T) {
		root := t.TempDir()
		agent := NewAgent(WithScratchDir(root))
		require.NoError(t, os.MkdirAll(agent.homeRoot(), 0o700))

		var nativeReleased, scratchReleased atomic.Bool
		unproven := errors.Join(errors.New("shutdown failed"), opencode.ErrProcessContainmentIncomplete)
		err := agent.cleanupRuntimeResources(
			unproven,
			func() { nativeReleased.Store(true) },
			func() { scratchReleased.Store(true) },
		)
		require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
		require.False(t, nativeReleased.Load())
		require.False(t, scratchReleased.Load())
		_, statErr := os.Stat(agent.homeRoot())
		require.NoError(t, statErr)
	})

	t.Run("XDG delete failure releases native but retains XDG reservation", func(t *testing.T) {
		root := t.TempDir()
		agent := NewAgent(WithScratchDir(root))
		removeErr := errors.New("remove failed")
		runtimeRemoveAll = func(string) error { return removeErr }
		t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

		var nativeReleased, scratchReleased atomic.Bool
		err := agent.cleanupRuntimeResources(
			nil,
			func() { nativeReleased.Store(true) },
			func() { scratchReleased.Store(true) },
		)
		require.ErrorIs(t, err, removeErr)
		require.ErrorIs(t, err, errRuntimeScratchCleanup)
		require.True(t, nativeReleased.Load())
		require.False(t, scratchReleased.Load())
	})

	t.Run("explicit home is preserved", func(t *testing.T) {
		home := t.TempDir()
		agent := NewAgent(WithHome(home))
		var nativeReleased, scratchReleased atomic.Bool
		require.NoError(t, agent.cleanupRuntimeResources(
			nil,
			func() { nativeReleased.Store(true) },
			func() { scratchReleased.Store(true) },
		))
		require.True(t, nativeReleased.Load())
		require.True(t, scratchReleased.Load())
		_, statErr := os.Stat(home)
		require.NoError(t, statErr)
	})

	t.Run("generation delete failure independently releases XDG after deletion", func(t *testing.T) {
		agent := NewAgent(WithScratchDir(t.TempDir()))
		require.NoError(t, os.MkdirAll(agent.homeRoot(), 0o700))
		var nativeReleased, xdgReleased atomic.Bool
		cleanupFailure := errors.Join(errors.New("generation removal failed"), opencode.ErrRuntimeScratchCleanup)
		err := agent.cleanupRuntimeResources(
			cleanupFailure,
			func() { nativeReleased.Store(true) },
			func() { xdgReleased.Store(true) },
		)
		require.ErrorIs(t, err, opencode.ErrRuntimeScratchCleanup)
		require.True(t, nativeReleased.Load())
		require.True(t, xdgReleased.Load())
		_, statErr := os.Stat(agent.homeRoot())
		require.ErrorIs(t, statErr, os.ErrNotExist)
		require.True(t, fatalRuntimeCleanup(err))
	})

	t.Run("dual generation and XDG delete failures retain both scratch reservations", func(t *testing.T) {
		agent := NewAgent(WithScratchDir(t.TempDir()))
		xdgRemoveErr := errors.New("XDG removal failed")
		runtimeRemoveAll = func(string) error { return xdgRemoveErr }
		t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

		var nativeReleased, xdgReleased atomic.Bool
		generationRemoveErr := errors.New("generation removal failed")
		err := agent.cleanupRuntimeResources(
			errors.Join(opencode.ErrRuntimeScratchCleanup, generationRemoveErr),
			func() { nativeReleased.Store(true) },
			func() { xdgReleased.Store(true) },
		)
		require.ErrorIs(t, err, opencode.ErrRuntimeScratchCleanup)
		require.ErrorIs(t, err, generationRemoveErr)
		require.ErrorIs(t, err, errRuntimeScratchCleanup)
		require.ErrorIs(t, err, xdgRemoveErr)
		require.True(t, nativeReleased.Load())
		require.False(t, xdgReleased.Load())
		require.True(t, fatalRuntimeCleanup(err))
	})
}

func TestDarwinBestEffortScratchReservationCardinality(t *testing.T) {
	client := newFakeOpenCodeClient()

	t.Run("adapter XDG and containment generation each hold one live reservation", func(t *testing.T) {
		var acquired, live, released atomic.Int64
		agent := NewAgent(WithScratchDir(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				require.Equal(t, RuntimeResourceRuntime, kind)
				acquired.Add(1)
				live.Add(1)

				return func() {
					released.Add(1)
					live.Add(-1)
				}, nil
			},
		}))
		agent.containmentMode = RuntimeContainmentBestEffort

		var generationRelease func()
		agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
			require.EqualValues(t, 1, acquired.Load(), "XDG must be the only reservation before generation creation")
			require.EqualValues(t, 1, live.Load())
			require.DirExists(t, agent.homeRoot())
			require.NotNil(t, options.ReserveContainmentScratch)

			var err error
			generationRelease, err = options.ReserveContainmentScratch(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 2, acquired.Load())
			require.EqualValues(t, 2, live.Load(), "both roots must have independent live reservations")

			return client, nil
		}

		runtime, nativeRelease, xdgRelease, err := agent.startSharedRuntime(context.Background(), runtimeEnvironment{})
		require.NoError(t, err)
		require.Same(t, client, runtime)
		require.NotNil(t, xdgRelease)
		require.EqualValues(t, 2, live.Load())

		generationRelease()
		require.EqualValues(t, 1, live.Load())
		require.NoError(t, agent.cleanupRuntimeResources(nil, nativeRelease, xdgRelease))
		require.EqualValues(t, 0, live.Load())
		require.EqualValues(t, 2, released.Load())
	})

	t.Run("explicit home reserves only containment generation", func(t *testing.T) {
		var acquired, live, released atomic.Int64
		home := t.TempDir()
		agent := NewAgent(WithHome(home), WithRuntimeResourceHooks(RuntimeResourceHooks{
			ReserveScratchRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
				require.Equal(t, RuntimeResourceRuntime, kind)
				acquired.Add(1)
				live.Add(1)

				return func() {
					released.Add(1)
					live.Add(-1)
				}, nil
			},
		}))
		agent.containmentMode = RuntimeContainmentBestEffort

		var generationRelease func()
		agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
			require.Zero(t, acquired.Load(), "explicit Home must not reserve adapter XDG scratch")
			require.True(t, options.DarwinBestEffort)

			var err error
			generationRelease, err = options.ReserveContainmentScratch(ctx)
			require.NoError(t, err)
			require.EqualValues(t, 1, acquired.Load())
			require.EqualValues(t, 1, live.Load())

			return client, nil
		}

		runtime, nativeRelease, xdgRelease, err := agent.startSharedRuntime(context.Background(), runtimeEnvironment{})
		require.NoError(t, err)
		require.Same(t, client, runtime)
		require.Nil(t, xdgRelease)
		require.EqualValues(t, 1, live.Load())

		generationRelease()
		require.NoError(t, agent.cleanupRuntimeResources(nil, nativeRelease, xdgRelease))
		require.EqualValues(t, 0, live.Load())
		require.EqualValues(t, 1, acquired.Load())
		require.EqualValues(t, 1, released.Load())
		_, statErr := os.Stat(home)
		require.NoError(t, statErr)
	})
}

func TestNativeOwnedDurableRuntimeHomeIsNeverMaterializedByTheAdapter(t *testing.T) {
	home := testNativeOwnedHome(t)
	client := newFakeOpenCodeClient()
	agent := NewAgent(
		WithProcessIsolation(ProcessIsolation{
			UID:                 uint32(os.Geteuid()),
			GID:                 uint32(os.Getegid()),
			StandaloneOwnerID:   "runtime-owner",
			StandaloneStateRoot: home,
		}),
	)
	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		require.True(t, options.NativeOwnedXDG)
		require.Equal(t, opencode.RuntimeXDGDirs(home), options.ExistingXDG)
		entries, err := os.ReadDir(home)
		require.NoError(t, err)
		require.Empty(t, entries)

		return client, nil
	}

	runtime, nativeRelease, scratchRelease, err := agent.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.NoError(t, err)
	require.Same(t, client, runtime)
	require.NotNil(t, nativeRelease)
	require.Nil(t, scratchRelease)
	nativeRelease()
}

func TestNativeOwnedDurableRuntimeHomeRejectsSeedFilesBeforeLaunch(t *testing.T) {
	home := testNativeOwnedHome(t)
	agent := NewAgent(
		WithProcessIsolation(ProcessIsolation{
			UID:                 uint32(os.Geteuid()),
			GID:                 uint32(os.Getegid()),
			StandaloneOwnerID:   "runtime-owner",
			StandaloneStateRoot: home,
		}),
		WithSeedFiles(map[string]string{"provider.json": `{}`}),
	)
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		t.Fatal("native runtime started")

		return nil, errors.New("unreachable native runtime start")
	}

	client, nativeRelease, scratchRelease, err := agent.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.ErrorContains(t, err, "seed file \"provider.json\" is unsupported")
	require.Nil(t, client)
	require.Nil(t, nativeRelease)
	require.Nil(t, scratchRelease)
}

func TestNativeOwnedDurableRuntimeHomeRejectsWrongOwnerBeforeLaunch(t *testing.T) {
	home := testNativeOwnedHome(t)
	agent := NewAgent(
		WithHome(home),
		WithProcessIsolation(ProcessIsolation{UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid()) + 1}),
	)
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		t.Fatal("native runtime started")

		return nil, errors.New("unreachable native runtime start")
	}

	client, nativeRelease, scratchRelease, err := agent.startSharedRuntime(context.Background(), runtimeEnvironment{})
	require.ErrorContains(t, err, nativeOwnedHomeRefusal)
	require.Nil(t, client)
	require.Nil(t, nativeRelease)
	require.Nil(t, scratchRelease)
}

func TestRuntimeExitWatcherLatchesUnprovenTree(t *testing.T) {
	base := newFakeOpenCodeClient()
	client := &proofFailureRuntimeClient{
		fakeOpenCodeClient: base,
		entered:            make(chan struct{}),
		resume:             make(chan struct{}),
		err:                errors.Join(errors.New("containment proof missing"), opencode.ErrProcessContainmentIncomplete),
	}
	var nativeReleased, scratchReleased atomic.Bool
	var replacementStarts atomic.Int32
	agent := NewAgent(WithScratchDir(t.TempDir()))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		replacementStarts.Add(1)

		return newFakeOpenCodeClient(), nil
	}
	agent.runtime = client
	agent.runtimeGeneration = 1
	agent.runtimeNativeRelease = func() { nativeReleased.Store(true) }
	agent.runtimeXDGScratchRelease = func() { scratchReleased.Store(true) }
	require.NoError(t, os.MkdirAll(agent.homeRoot(), 0o700))

	go agent.watchSharedRuntime(context.Background(), client, 1)
	close(base.runtimeExited)
	<-client.entered

	agent.mu.Lock()
	cleanupDone := agent.runtimeStarting
	agent.mu.Unlock()
	require.NotNil(t, cleanupDone)
	close(client.resume)
	<-cleanupDone

	require.False(t, nativeReleased.Load())
	require.False(t, scratchReleased.Load())
	_, err := agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
	require.EqualValues(t, 0, replacementStarts.Load())
}

func TestRuntimeExitWatcherLatchesScratchCleanupFailure(t *testing.T) {
	originalRemoveAll := runtimeRemoveAll
	removeErr := errors.New("remove failed")
	runtimeRemoveAll = func(string) error { return removeErr }
	t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

	base := newFakeOpenCodeClient()
	client := &proofFailureRuntimeClient{
		fakeOpenCodeClient: base,
		entered:            make(chan struct{}),
		resume:             make(chan struct{}),
	}
	var nativeReleased, scratchReleased atomic.Bool
	var replacementStarts atomic.Int32
	agent := NewAgent(WithScratchDir(t.TempDir()))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		replacementStarts.Add(1)

		return newFakeOpenCodeClient(), nil
	}
	agent.runtime = client
	agent.runtimeGeneration = 1
	agent.runtimeNativeRelease = func() { nativeReleased.Store(true) }
	agent.runtimeXDGScratchRelease = func() { scratchReleased.Store(true) }
	require.NoError(t, os.MkdirAll(agent.homeRoot(), 0o700))

	go agent.watchSharedRuntime(context.Background(), client, 1)
	close(base.runtimeExited)
	<-client.entered

	agent.mu.Lock()
	cleanupDone := agent.runtimeStarting
	agent.mu.Unlock()
	require.NotNil(t, cleanupDone)
	close(client.resume)
	<-cleanupDone

	require.True(t, nativeReleased.Load())
	require.False(t, scratchReleased.Load())
	_, err := agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, removeErr)
	require.ErrorIs(t, err, errRuntimeScratchCleanup)
	require.EqualValues(t, 0, replacementStarts.Load())
}

func TestRuntimeStartLatchesUnprovenTree(t *testing.T) {
	var starts atomic.Int32
	var nativeReleased, scratchReleased atomic.Bool
	agent := NewAgent(WithScratchDir(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleased.Store(true) }, nil
		},
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleased.Store(true) }, nil
		},
	}))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		starts.Add(1)

		return nil, errors.Join(errors.New("start containment failed"), opencode.ErrProcessContainmentIncomplete)
	}

	_, err := agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
	_, err = agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
	require.EqualValues(t, 1, starts.Load())
	require.False(t, nativeReleased.Load())
	require.False(t, scratchReleased.Load())
}

func TestRuntimeStartLatchesScratchCleanupFailure(t *testing.T) {
	originalRemoveAll := runtimeRemoveAll
	removeErr := errors.New("remove failed")
	runtimeRemoveAll = func(string) error { return removeErr }
	t.Cleanup(func() { runtimeRemoveAll = originalRemoveAll })

	var starts atomic.Int32
	var nativeReleased, scratchReleased atomic.Bool
	agent := NewAgent(WithScratchDir(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleased.Store(true) }, nil
		},
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleased.Store(true) }, nil
		},
	}))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		starts.Add(1)

		return nil, errors.New("start failed after scratch creation")
	}

	_, err := agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, removeErr)
	require.ErrorIs(t, err, errRuntimeScratchCleanup)
	_, err = agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, errRuntimeScratchCleanup)
	require.EqualValues(t, 1, starts.Load())
	require.True(t, nativeReleased.Load())
	require.False(t, scratchReleased.Load())
}

func TestRuntimeStartLatchesGenerationScratchCleanupFailure(t *testing.T) {
	var starts, scratchAcquired atomic.Int32
	var nativeReleased, generationReleased atomic.Bool
	agent := NewAgent(WithHome(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleased.Store(true) }, nil
		},
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			scratchAcquired.Add(1)

			return func() { generationReleased.Store(true) }, nil
		},
	}))
	agent.containmentMode = RuntimeContainmentBestEffort
	generationRemoveErr := errors.New("generation removal failed")
	agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		starts.Add(1)
		_, err := options.ReserveContainmentScratch(ctx)
		require.NoError(t, err)

		return nil, errors.Join(opencode.ErrRuntimeScratchCleanup, generationRemoveErr)
	}

	_, err := agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, opencode.ErrRuntimeScratchCleanup)
	require.ErrorIs(t, err, generationRemoveErr)
	_, err = agent.sharedRuntime(context.Background())
	require.ErrorIs(t, err, opencode.ErrRuntimeScratchCleanup)
	require.EqualValues(t, 1, starts.Load())
	require.EqualValues(t, 1, scratchAcquired.Load())
	require.True(t, nativeReleased.Load())
	require.False(t, generationReleased.Load())
}

// TestReadySharedRuntimeSessionReleaseGate times each logical session from the
// NewSession call through its complete response, after an untimed warm-up has
// made the multiplexed runtime ready. Directory preparation is outside the
// interval, no Prompt call occurs, and the in-memory native fixture performs no
// provider or network work. Five samples make nearest-rank p95 the slowest.
func TestReadySharedRuntimeSessionReleaseGate(t *testing.T) {
	const repetitions = 5

	client := newFakeOpenCodeClient()
	var launches atomic.Int32
	var nativeSessions atomic.Int32
	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession("native-release-gate-" + strconv.Itoa(int(nativeSessions.Add(1)))), nil
	}
	agent := NewAgent(WithHome(t.TempDir()))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		launches.Add(1)

		return client, nil
	}
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)

	cwds := make([]string, repetitions)
	for index := range cwds {
		cwds[index] = t.TempDir()
	}

	durations := make([]time.Duration, 0, repetitions)
	for _, cwd := range cwds {
		started := time.Now()
		_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: cwd})
		durations = append(durations, time.Since(started))
		require.NoError(t, err)
	}

	require.EqualValues(t, 1, launches.Load(), "ready logical sessions must reuse the shared native runtime")
	slices.Sort(durations)
	p95 := durations[len(durations)-1]
	t.Logf("ready-session deterministic adapter gate: repetitions=%d p95=%s", repetitions, p95)
	require.Less(t, p95, 500*time.Millisecond)
}

// A rebind for a changed environment inherits the containment fence: when
// retiring the running generation cannot be proven, the request that asked for
// the new environment fails with that proof failure rather than starting a
// second native process beside it.
func TestRuntimeEnvironmentRebindFailsOnUnprovenRetirement(t *testing.T) {
	agent := NewAgent(WithHome(t.TempDir()))
	agent.runtime = &panickingRuntimeShutdownClient{fakeOpenCodeClient: newFakeOpenCodeClient()}
	agent.runtimeGeneration = 1

	_, _, err := agent.sharedRuntimeBinding(context.Background(), runtimeEnvironment{
		Env: map[string]string{"HOST_API_TOKEN": "secret"},
	})
	require.ErrorIs(t, err, opencode.ErrProcessContainmentIncomplete)
	require.Nil(t, agent.runtime)
}
