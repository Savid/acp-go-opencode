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

func TestAgentSessionDefaultsToOrdinaryExecution(t *testing.T) {
	const (
		canary        = "ACP_GO_OPENCODE_TEST_ACTUAL_AMBIENT"
		privateCanary = privateAdapterEnvPrefix + "SPOOF"
	)

	t.Setenv(canary, "captured")
	t.Setenv(privateCanary, "leaked")

	var launched opencode.StartOptions

	client := newFakeOpenCodeClient()
	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession("ordinary-native-session"), nil
	}

	agent := NewAgent(WithScratchDir(t.TempDir()))
	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		launched = options

		return client, nil
	}
	t.Cleanup(func() { require.NoError(t, agent.Close()) })
	t.Setenv(canary, "mutated")

	_, err := agent.NewSession(t.Context(), acp.NewSessionRequest{Cwd: t.TempDir()})
	require.NoError(t, err)
	require.Nil(t, launched.StartProcess)
	require.Nil(t, launched.PrepareTree)
	require.Nil(t, launched.ReclaimTree)
	require.NotNil(t, launched.NativeEnvironment)
	environment := launched.NativeEnvironment()
	require.Equal(t, "captured", environment[canary])
	require.NotContains(t, environment, privateCanary)

	environment[canary] = "caller mutation"
	require.Equal(t, "captured", agent.options.implicitEnvironment[canary])
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

	root, generated, err := agent.newRuntimeRoot()
	require.NoError(t, err)
	require.True(t, generated)
	require.Equal(t, scratch, filepath.Dir(root))
	require.True(t, filepath.Base(root) != defaultAgentName)
	require.NoError(t, os.RemoveAll(root))
}

func (client *proofFailureRuntimeClient) Shutdown(context.Context) error {
	if client.calls.Add(1) == 1 {
		close(client.entered)
	}
	<-client.resume

	return client.err
}

func TestRuntimeRetirementMemoizesExactGenerationResult(t *testing.T) {
	containmentErr := errors.Join(errors.New("containment failed"), ErrContainmentIncomplete)
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
	require.ErrorIs(t, missing.retireSharedRuntime(9, "missing"), ErrContainmentIncomplete)
	missing.runtimeFatalErr = containmentErr
	require.ErrorIs(t, missing.retireSharedRuntime(9, "fatal"), containmentErr)

	sessionless := NewAgent(WithHome(t.TempDir()))
	sessionless.runtime = newFakeOpenCodeClient()
	sessionless.runtimeGeneration = 1
	require.NoError(t, sessionless.retireSharedRuntime(1, "no sessions"))
}

func TestRuntimeRetirementContainsDetachPanic(t *testing.T) {
	client := newFakeOpenCodeClient()
	current := &session{
		client:            client,
		runtimeGeneration: 1,
		incarnation: &nativeIncarnationBinding{
			client: client, generation: 1, registry: newActionRegistry(),
		},
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
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.ErrorIs(t, agent.runtimeFatalErr, ErrContainmentIncomplete)
	require.Zero(t, current.runtimeGeneration)
	require.Equal(t, "runtime exited", current.runtimeLostCause)
}

func TestRuntimeRetirementIncompleteProofFencesConfiguredHomeUntilRetry(t *testing.T) {
	home := t.TempDir()
	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(ErrContainmentIncomplete, opencode.ErrProcessContainmentIncomplete)
	agent := NewAgent(WithHome(home))
	agent.runtime = client
	agent.runtimeGeneration = 1

	err := agent.retireSharedRuntime(1, "runtime containment incomplete")
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.NotNil(t, agent.runtimeSequencing)
	require.Nil(t, agent.runtimeFatalErr)

	var starts atomic.Int32
	replacement := newFakeOpenCodeClient()
	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		starts.Add(1)
		require.Equal(t, home, options.Root)

		return replacement, nil
	}

	_, _, err = agent.sharedRuntimeBinding(t.Context())
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Zero(t, starts.Load(), "replacement rematerialized configured Home before containment proof")

	client.closeErr = nil
	got, generation, err := agent.sharedRuntimeBinding(t.Context())
	require.NoError(t, err)
	require.Same(t, replacement, got)
	require.EqualValues(t, 2, generation)
	require.EqualValues(t, 1, starts.Load())
}

func TestHostAuthorityLossFencesEverySharedRuntimeSession(t *testing.T) {
	client := newFakeOpenCodeClient()
	client.closeErr = errors.Join(errors.New("authority connection closed"), ErrHostAuthorityUnavailable)
	agent := NewAgent(WithHome(t.TempDir()))
	agent.runtime = client
	agent.runtimeGeneration = 1

	var cancelled atomic.Int32
	for _, id := range []acp.SessionId{"first", "second"} {
		current := &session{
			agent: agent, id: id, client: client, runtimeGeneration: 1,
			cancel: func() { cancelled.Add(1) },
			incarnation: &nativeIncarnationBinding{
				client: client, generation: 1, registry: newActionRegistry(),
			},
		}
		agent.sessions[id] = current
	}

	err := agent.retireSharedRuntime(1, "host authority unavailable")
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, agent.runtimeFatalErr, ErrHostAuthorityUnavailable)
	require.EqualValues(t, 2, cancelled.Load())
	for _, current := range agent.sessions {
		require.Zero(t, current.runtimeGeneration)
		require.Equal(t, "host authority unavailable", current.runtimeLostCause)
		require.Nil(t, current.incarnation)
	}
}

func TestRuntimeExitWatcherPublishesBoundaryPanics(t *testing.T) {
	shutdownBase := newFakeOpenCodeClient()
	tests := []struct {
		name      string
		client    opencode.Client
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := NewAgent(WithHome(t.TempDir()))
			agent.runtime = test.client
			agent.runtimeGeneration = 1
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
			require.ErrorIs(t, retirement.err, ErrContainmentIncomplete)
			require.ErrorContains(t, retirement.err, "panicked")
			require.NotContains(t, retirement.err.Error(), test.panicText)
			require.ErrorIs(t, fatalErr, ErrContainmentIncomplete)
			require.True(t, retirement.err == agent.retireSharedRuntime(1, "late waiter"))
		})
	}
}

func TestAgentCloseMemoizesRetirementAndWaitsForOneAlreadyInProgress(t *testing.T) {
	containmentErr := errors.Join(errors.New("containment failed"), ErrContainmentIncomplete)
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
	containmentErr := errors.Join(errors.New("containment failed"), ErrContainmentIncomplete)
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
		_, _, err := agent.sharedRuntimeBinding(context.Background())
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
	_, _, err := closed.sharedRuntimeBinding(context.Background())
	require.Error(t, err)

	waiting := NewAgent()
	waiting.runtimeStarting = make(chan struct{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = waiting.sharedRuntimeBinding(cancelled)
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
	got, _, err := ready.sharedRuntimeBinding(context.Background())
	require.NoError(t, err)
	require.Same(t, client, got)

	duringStart := NewAgent(WithScratchDir(t.TempDir()))
	duringStart.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		duringStart.mu.Lock()
		duringStart.closed = true
		duringStart.mu.Unlock()

		return client, nil
	}
	_, _, err = duringStart.sharedRuntimeBinding(context.Background())
	require.Error(t, err)
	require.True(t, client.closed)
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
	home := t.TempDir()
	client := newFakeOpenCodeClient()
	var handed opencode.StartOptions
	agent := NewAgent(WithHome(home))
	agent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		handed = options

		return client, nil
	}

	runtime, err := agent.startSharedRuntime(context.Background())
	require.NoError(t, err)
	require.Same(t, client, runtime)
	require.Equal(t, home, handed.Root)
	require.Equal(t, opencode.ControlRootForXDG(home), handed.ControlRoot)
	require.False(t, handed.RemoveRoot)
	require.Nil(t, handed.StartProcess)
	require.Nil(t, handed.PrepareTree)
	require.Nil(t, handed.ReclaimTree)
	require.Equal(t, agent.pluginSeedDir(context.Background()), handed.PluginSeedDir)
	require.NotEmpty(t, handed.PluginSeedDir, "the runtime is handed the resolved plugin seed cache")

	originalStart := runtimeStartServer
	runtimeStartServer = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		return nil, errors.New("default factory failed")
	}
	t.Cleanup(func() { runtimeStartServer = originalStart })
	defaultFactory := NewAgent(WithHome(t.TempDir()))
	defaultFactory.options.clientFactory = nil
	runtime, err = defaultFactory.startSharedRuntime(context.Background())
	require.ErrorContains(t, err, "default factory failed")
	require.Nil(t, runtime)

	generated := NewAgent(WithScratchDir(t.TempDir()))
	var generatedRoot string
	generated.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		generatedRoot = options.Root

		return nil, errors.New("launch failed")
	}
	runtime, err = generated.startSharedRuntime(context.Background())
	require.ErrorContains(t, err, "launch failed")
	require.Nil(t, runtime)
	require.NotEmpty(t, generatedRoot)
	require.NoDirExists(t, generatedRoot)
	require.NoDirExists(t, opencode.ControlRootForXDG(generatedRoot))
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

	scoped, release, generation, err := agent.newOpenCodeClient(context.Background(), "session", cwd, []opencode.MCPServerConfig{{Name: "tools"}}, sessionCarrier{})
	require.ErrorIs(t, err, opencode.ErrMCPDisconnectUnproven)
	require.Nil(t, scoped)
	require.Nil(t, release)
	require.Zero(t, generation)
	require.Len(t, agent.directories, 1, "unproven native cleanup must retain directory ownership")
	scoped, release, generation, err = agent.newOpenCodeClient(context.Background(), "session", cwd, []opencode.MCPServerConfig{{Name: "tools"}}, sessionCarrier{})
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
	current := testSession(t, agent, client)
	current.directoryRelease = func() { releases++ }
	err = agent.closeFailedSession(current)
	require.ErrorContains(t, err, "disconnect failed")
	require.Zero(t, releases)
	require.ErrorIs(t, agent.runtimeFatalErr, opencode.ErrMCPDisconnectUnproven)

	client.closeErr = nil
	require.NoError(t, agent.Close(), "logical scope cleanup failure must not become native containment failure")
}

func TestDirectoryBindingIncarnationSkipsZeroAfterWrap(t *testing.T) {
	agent := NewAgent()
	agent.directoryIncarnation = ^directoryBindingIncarnation(0)

	require.Equal(t, directoryBindingIncarnation(1), agent.nextDirectoryBindingIncarnationLocked())
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

type shutdownHookClient struct {
	*fakeOpenCodeClient
	shutdown func(context.Context) error
}

func (c *shutdownHookClient) Shutdown(ctx context.Context) error { return c.shutdown(ctx) }

type revokedRuntimeClient struct{ *fakeOpenCodeClient }

func (*revokedRuntimeClient) RuntimeRevoked() bool { return true }

func TestSharedRuntimeCoordinationEdges(t *testing.T) {
	internalContainment := errors.Join(errors.New("wait incomplete"), opencode.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, classifyRuntimeContainment(internalContainment), ErrContainmentIncomplete)
	alreadyClassified := errors.Join(internalContainment, ErrContainmentIncomplete)
	require.True(t, classifyRuntimeContainment(alreadyClassified) == alreadyClassified)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, classifyRuntimeShutdown(cancelled, context.Canceled), opencode.ErrProcessContainmentIncomplete)
	require.False(t, retryableRuntimeCleanup(errors.Join(internalContainment, ErrHostAuthorityUnavailable)))

	revoked := &revokedRuntimeClient{fakeOpenCodeClient: newFakeOpenCodeClient()}
	agent := NewAgent()
	agent.runtime = revoked
	agent.runtimeGeneration = 1
	agent.handleSharedRuntimeExit(revoked, 1)

	retirement := &runtimeRetirement{runtime: newFakeOpenCodeClient()}
	require.NoError(t, agent.retryRuntimeCleanup(t.Context(), retirement))

	changedAgent := NewAgent()
	changedRetirement := &runtimeRetirement{}
	changedRetirement.runtime = &shutdownHookClient{
		fakeOpenCodeClient: newFakeOpenCodeClient(),
		shutdown: func(context.Context) error {
			changedAgent.mu.Lock()
			changedAgent.runtimeSequencing = nil
			changedAgent.mu.Unlock()

			return errors.New("shutdown refused")
		},
	}
	changedAgent.runtimeSequencing = changedRetirement
	require.ErrorContains(t, changedAgent.retryRuntimeCleanup(t.Context(), changedRetirement), "shutdown refused")

	fatalAgent := NewAgent()
	fatalClient := newFakeOpenCodeClient()
	fatalClient.closeErr = ErrContainmentIncomplete
	fatalRetirement := &runtimeRetirement{runtime: fatalClient}
	fatalAgent.runtimeSequencing = fatalRetirement
	require.ErrorIs(t, fatalAgent.retryRuntimeCleanup(t.Context(), fatalRetirement), ErrContainmentIncomplete)
	require.ErrorIs(t, fatalAgent.runtimeFatalErr, ErrContainmentIncomplete)
	require.Nil(t, fatalAgent.runtimeSequencing)

	closeAgent := NewAgent()
	closeAgent.runtimeSequencing = &runtimeRetirement{runtime: newFakeOpenCodeClient()}
	require.NoError(t, closeAgent.Close())

	retained := NewAgent()
	cleanup := func() error { return nil }
	retained.retainNativeTree("tree", false, cleanup)
	retained.retainNativeTree("tree", true, nil)
	require.True(t, retained.retiredNativeTrees["tree"].reclaimed)
	require.NotNil(t, retained.retiredNativeTrees["tree"].cleanup)
	zeroRetained := &Agent{}
	zeroRetained.retainNativeTree("tree", false, nil)
	require.Contains(t, zeroRetained.retiredNativeTrees, "tree")

	panicAuthority := edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		reclaim:     func(context.Context, string) error { panic("reclaim") },
	}
	require.ErrorIs(t, reclaimManagedNativeTree(t.Context(), panicAuthority, "tree"), ErrHostAuthorityUnavailable)

	cleanupAgent := NewAgent()
	cleanupAgent.retiredNativeTrees["tree"] = retiredNativeTree{
		reclaimed: true,
		cleanup:   func() error { return errors.New("cleanup refused") },
	}
	require.ErrorIs(t, cleanupAgent.retryRetiredNativeTrees(t.Context()), opencode.ErrRuntimeScratchCleanup)
}

func TestSharedRuntimeConstructionEdges(t *testing.T) {
	badEnvironment := NewAgent()
	badEnvironment.options.hostAuthorityConfigured = true
	badEnvironment.options.HostAuthority = edgeAuthority{
		environment: func() map[string]string { return nil },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return nil, errors.New("unexpected start")
		},
	}
	_, err := badEnvironment.startSharedRuntime(t.Context())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	badRoot := NewAgent(WithScratchDir(filepath.Join(file, "child")))
	_, err = badRoot.startSharedRuntime(t.Context())
	require.ErrorContains(t, err, "create scratch parent")

	mkdirTempFailure := errors.New("mkdir temp refused")
	originalMkdirTemp := runtimeMkdirTemp
	t.Cleanup(func() { runtimeMkdirTemp = originalMkdirTemp })
	runtimeMkdirTemp = func(string, string) (string, error) { return "", mkdirTempFailure }
	_, _, err = (&Agent{options: Options{ScratchDir: t.TempDir()}}).newRuntimeRoot()
	require.ErrorContains(t, err, "create OpenCode runtime root")
	require.ErrorIs(t, err, mkdirTempFailure)
	runtimeMkdirTemp = originalMkdirTemp

	generatedRoot, generated, err := (&Agent{options: Options{ScratchDir: t.TempDir()}}).newRuntimeRoot()
	require.NoError(t, err)
	require.True(t, generated)
	requireOwnerOnlyMode(t, generatedRoot, 0o700)

	authority := edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		prepare:     func(context.Context, string) error { panic("prepare") },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return nil, errors.New("unexpected start")
		},
	}
	prepareAgent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	var startOptions opencode.StartOptions
	prepareAgent.options.clientFactory = func(_ context.Context, options opencode.StartOptions) (opencode.Client, error) {
		startOptions = options

		return newFakeOpenCodeClient(), nil
	}
	runtime, err := prepareAgent.startSharedRuntime(t.Context())
	require.NoError(t, err)
	require.NotNil(t, runtime)
	require.ErrorIs(t, startOptions.PrepareTree(t.Context(), t.TempDir()), ErrHostAuthorityUnavailable)
}
