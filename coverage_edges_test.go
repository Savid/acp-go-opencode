package opencodeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type cancelAfterFirstErrContext struct {
	context.Context //nolint:containedctx // Test context stages the gate's post-admission cancellation recheck.
	calls           atomic.Int32
	done            <-chan struct{}
}

func (c *cancelAfterFirstErrContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterFirstErrContext) Done() <-chan struct{}       { return c.done }
func (c *cancelAfterFirstErrContext) Err() error {
	if c.calls.Add(1) > 1 {
		return context.Canceled
	}

	return nil
}

func TestRuntimeOptionAndScratchEdges(t *testing.T) {
	require.Error(t, validateRuntimeOptions(Options{Env: map[string]string{"home": "/reserved"}}))
	require.Error(t, validateRuntimeOptions(Options{Home: "relative"}))
	require.Error(t, validateRuntimeOptions(Options{hostAuthorityConfigured: true}))
	require.Error(t, validateDurableHomePath("/tmp/control\npath"))
	require.True(t, reservedOpenCodeEnvKey(privateAdapterEnvPrefix+"TOKEN"))
	require.True(t, adapterPrivateEnvKey(privateAdapterEnvPrefix+"TOKEN"))

	homeAgent := &Agent{options: Options{Home: "/durable/home"}}
	root, generated, err := homeAgent.newRuntimeRoot()
	require.NoError(t, err)
	require.Equal(t, "/durable/home", root)
	require.False(t, generated)

	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	broken := &Agent{options: Options{ScratchDir: filepath.Join(file, "child")}}
	_, _, err = broken.newRuntimeRoot()
	require.ErrorContains(t, err, "create scratch parent")
}

func TestRecoveryAndLifecycleAdmissionEdges(t *testing.T) {
	done := make(chan struct{})
	ctx := &cancelAfterFirstErrContext{Context: t.Context(), done: done}
	var gate sessionRecoveryGate
	require.ErrorIs(t, gate.lock(ctx), context.Canceled)
	require.NoError(t, gate.lock(t.Context()), "cancelled post-admission check must return the permit")
	gate.unlock()

	agent := NewAgent()
	id := acp.SessionId("session")
	flight := &sessionLifecycleFlight{done: make(chan struct{})}
	fence := make(chan struct{})
	close(fence)
	agent.lifecycleFence = fence
	agent.lifecycleFlights = make(map[acp.SessionId]*sessionLifecycleFlight)
	agent.lifecycleFlights[id] = flight
	_, err := agent.acquireSessionLifecycle(t.Context(), id)
	require.Error(t, err)
	agent.releaseSessionLifecycle(id, &sessionLifecycleFlight{})

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = agent.LoadSession(cancelled, acp.LoadSessionRequest{SessionId: id})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAgentStoreAndActiveLoadMatchEdges(t *testing.T) {
	agent := NewAgent()
	id := acp.SessionId("deleted")
	agent.deleted[id] = struct{}{}
	require.Error(t, agent.storeStartedSession(&session{id: id}))

	active := &session{
		cwd: "/cwd", providerID: "provider", modelID: "model", mode: "build", permission: "ask",
		outputSchema: map[string]any{"type": "object"}, carrier: sessionCarrier{},
	}
	snapshot := active.snapshot()
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{Model: "other/model"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{Mode: "plan"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{PermissionSet: true, Permission: "allow"}, sessionCarrier{}))
	require.False(t, activeLoadRequestMatches(snapshot, active, "/cwd", nil, nil, nil,
		sessionMeta{OutputSchema: map[string]any{"type": "array"}}, sessionCarrier{}))
}

type edgeAuthority struct {
	environment func() map[string]string
	prepare     func(context.Context, string) error
	reclaim     func(context.Context, string) error
	start       func(context.Context, NativeRequest) (NativeProcess, error)
}

func (a edgeAuthority) NativeEnvironment() map[string]string { return a.environment() }
func (a edgeAuthority) PrepareNativeTree(ctx context.Context, path string) error {
	if a.prepare == nil {
		return nil
	}

	return a.prepare(ctx, path)
}
func (a edgeAuthority) ReclaimNativeTree(ctx context.Context, path string) error {
	if a.reclaim == nil {
		return nil
	}

	return a.reclaim(ctx, path)
}
func (a edgeAuthority) StartNative(ctx context.Context, request NativeRequest) (NativeProcess, error) {
	return a.start(ctx, request)
}

type edgeNativeProcess struct {
	stdin  func() io.WriteCloser
	stdout func() io.ReadCloser
	stderr func() io.ReadCloser
	wait   func(context.Context) (NativeResult, error)
	revoke func(context.Context) error
}

func (p edgeNativeProcess) Stdin() io.WriteCloser { return p.stdin() }
func (p edgeNativeProcess) Stdout() io.ReadCloser { return p.stdout() }
func (p edgeNativeProcess) Stderr() io.ReadCloser { return p.stderr() }
func (p edgeNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	return p.wait(ctx)
}
func (p edgeNativeProcess) Revoke(ctx context.Context) error { return p.revoke(ctx) }

func usableEdgeNativeProcess() edgeNativeProcess {
	return edgeNativeProcess{
		stdin:  func() io.WriteCloser { return nopWriteCloser{Writer: io.Discard} },
		stdout: func() io.ReadCloser { return io.NopCloser(&emptyReader{}) },
		stderr: func() io.ReadCloser { return io.NopCloser(&emptyReader{}) },
		wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
		revoke: func(context.Context) error { return nil },
	}
}

func TestNativeAuthorityDefensiveEdges(t *testing.T) {
	_, err := hostAuthorityEnvironment(edgeAuthority{environment: func() map[string]string { panic("environment") }})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	_, err = hostAuthorityEnvironment(edgeAuthority{environment: func() map[string]string { return nil }})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	_, err = hostAuthorityEnvironment(edgeAuthority{environment: func() map[string]string {
		return map[string]string{"BAD=KEY": "value"}
	}})
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	startPanic := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start:       func(context.Context, NativeRequest) (NativeProcess, error) { panic("start") },
	})
	_, err = startPanic(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	startErr := errors.New("start refused")
	refusing := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return nil, startErr
		},
	})
	_, err = refusing(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, startErr)

	var typedNil *edgeNativeProcess
	typedNilStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return typedNil, nil
		},
	})
	_, err = typedNilStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	stdioPanicProcess := usableEdgeNativeProcess()
	stdioPanicProcess.stdin = func() io.WriteCloser { panic("stdin") }
	stdioPanicStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return stdioPanicProcess, nil
		},
	})
	_, err = stdioPanicStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)

	missingStdioProcess := usableEdgeNativeProcess()
	missingStdioProcess.stderr = func() io.ReadCloser { return nil }
	missingStdioStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return missingStdioProcess, nil
		},
	})
	_, err = missingStdioStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.ErrorContains(t, err, "unusable host stdio")

	waitPanicProcess := usableEdgeNativeProcess()
	waitPanicProcess.wait = func(context.Context) (NativeResult, error) { panic("wait") }
	waitPanicStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return waitPanicProcess, nil
		},
	})
	handle, err := waitPanicStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.NoError(t, err)
	_, err = handle.Await(t.Context())
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, err, ErrContainmentIncomplete)

	stopPanicProcess := usableEdgeNativeProcess()
	stopPanicProcess.revoke = func(context.Context) error { panic("revoke") }
	stopPanicStarter := authorityProcessStarter(edgeAuthority{
		environment: func() map[string]string { return map[string]string{} },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			return stopPanicProcess, nil
		},
	})
	handle, err = stopPanicStarter(t.Context(), "opencode", nil, nil, t.TempDir())
	require.NoError(t, err)
	require.ErrorIs(t, handle.Stop(t.Context()), ErrHostAuthorityUnavailable)

	settleRevokePanic := usableEdgeNativeProcess()
	settleRevokePanic.revoke = func(context.Context) error { panic("revoke") }
	require.ErrorIs(t, settleUnusableNativeProcess(settleRevokePanic), ErrHostAuthorityUnavailable)
	settleWaitPanic := usableEdgeNativeProcess()
	settleWaitPanic.wait = func(context.Context) (NativeResult, error) { panic("wait") }
	require.ErrorIs(t, settleUnusableNativeProcess(settleWaitPanic), ErrHostAuthorityUnavailable)
	require.True(t, nativeProcessNil(nil))
	require.False(t, nativeProcessNil(usableEdgeNativeProcess()))
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

	readOnlyScratch := t.TempDir()
	require.NoError(t, os.Chmod(readOnlyScratch, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(readOnlyScratch, 0o700)) })
	_, _, err = (&Agent{options: Options{ScratchDir: readOnlyScratch}}).newRuntimeRoot()
	require.ErrorContains(t, err, "create OpenCode runtime root")

	generatedRoot, generated, err := (&Agent{options: Options{ScratchDir: t.TempDir()}}).newRuntimeRoot()
	require.NoError(t, err)
	require.True(t, generated)
	info, err := os.Stat(generatedRoot)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())

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

func TestLifecycleAndDeliveryCancellationEdges(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	current := &session{}
	require.ErrorIs(t, current.ensureRuntime(cancelled), context.Canceled)
	require.ErrorIs(t, current.refreshLifecycleMCP(cancelled), context.Canceled)
	require.Panics(t, func() { new(sessionRecoveryGate).unlock() })

	done := make(chan struct{})
	close(done)
	ctx := &cancelAfterFirstErrContext{Context: t.Context(), done: done}
	agent := NewAgent()
	id := acp.SessionId("busy")
	agent.lifecycleFlights = make(map[acp.SessionId]*sessionLifecycleFlight)
	agent.lifecycleFlights[id] = &sessionLifecycleFlight{done: make(chan struct{})}
	_, err := agent.acquireSessionLifecycle(ctx, id)
	require.ErrorIs(t, err, context.Canceled)

	fenced := NewAgent()
	fenced.lifecycleFenced = true
	_, err = fenced.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: id})
	require.Error(t, err)
	_, err = fenced.UnstableDeleteSession(t.Context(), acp.UnstableDeleteSessionRequest{SessionId: id})
	require.Error(t, err)

	stoppedBeforeSend := newSessionDelivery(nil, "raw-before")
	stoppedBeforeSend.started = true
	stoppedBeforeSend.raw = make(chan rawDelivery)
	close(stoppedBeforeSend.rawDone)
	require.ErrorContains(t, stoppedBeforeSend.enqueueRaw(t.Context(), map[string]any{}), "stopped")

	stoppedAfterSend := newSessionDelivery(nil, "raw-after")
	stoppedAfterSend.started = true
	received := make(chan struct{})
	go func() {
		<-stoppedAfterSend.raw
		close(stoppedAfterSend.rawDone)
		close(received)
	}()
	require.ErrorContains(t, stoppedAfterSend.enqueueRaw(t.Context(), map[string]any{}), "stopped")
	<-received
}

func TestStateSnapshotDecoderReachableEdges(t *testing.T) {
	snapshot := validSyncSnapshot("session", "native", "/source")
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	var original map[string]any
	require.NoError(t, json.Unmarshal(encoded, &original))
	encode := func(t *testing.T, mutate func(map[string]any)) []byte {
		t.Helper()
		candidate := cloneJSONMap(t, original)
		mutate(candidate)
		raw, marshalErr := json.Marshal(candidate)
		require.NoError(t, marshalErr)

		return raw
	}

	for name, raw := range map[string][]byte{
		"typed unmarshal": encode(t, func(value map[string]any) {
			value[snapshotFieldCapturedAtUnixMilli] = "not-an-integer"
		}),
		"environment object": encode(t, func(value map[string]any) {
			session, ok := value[snapshotFieldSession].(map[string]any)
			require.True(t, ok)
			session[snapshotFieldEnv] = []any{}
		}),
		"graph array": encode(t, func(value map[string]any) {
			value[snapshotFieldGraph] = map[string]any{}
		}),
		"events object": encode(t, func(value map[string]any) {
			value[jsonFieldEvents] = []any{}
		}),
		"aggregate array": encode(t, func(value map[string]any) {
			events, ok := value[jsonFieldEvents].(map[string]any)
			require.True(t, ok)
			events["native"] = map[string]any{}
		}),
		"event data object": encode(t, func(value map[string]any) {
			events, ok := value[jsonFieldEvents].(map[string]any)
			require.True(t, ok)
			aggregate, ok := events["native"].([]any)
			require.True(t, ok)
			event, ok := aggregate[0].(map[string]any)
			require.True(t, ok)
			event[jsonFieldData] = []any{}
		}),
	} {
		t.Run(name, func(t *testing.T) {
			_, decodeErr := decodeStateSnapshot(raw)
			require.Error(t, decodeErr)
		})
	}

	require.Error(t, rejectDuplicateJSONFields([]byte(`[] {`)))
	require.Error(t, rejectDuplicateJSONFields([]byte(`{"x":1,"x":2}`)))
	require.Error(t, rejectDuplicateJSONFields([]byte(`{"x":1]`)))
	require.Error(t, rejectDuplicateJSONFields([]byte(`[1}`)))
	require.NoError(t, rejectDuplicateJSONFields([]byte(`{"x":[{"y":1}]}`)))
	require.Error(t, scanUniqueJSONValue(json.NewDecoder(bytes.NewReader(nil)), "empty"))
	require.Error(t, scanUniqueJSONValue(json.NewDecoder(bytes.NewReader([]byte(`[1`))), "array"))
	_, err = exactJSONObject([]byte(`{`), "invalid", nil, nil)
	require.Error(t, err)
	_, err = exactJSONObject([]byte(`null`), "null", nil, nil)
	require.Error(t, err)
}

func TestStateStoreValidationReachableEdges(t *testing.T) {
	node := stateSnapshotNode{SessionID: "session", NativeSessionID: "native", SourceCwd: "/source"}
	partEvent := opencode.SyncEvent{
		ID: "part", AggregateID: "native", Type: syncTypeMessagePartUpdated,
		Data: map[string]json.RawMessage{
			syncFieldSessionID: json.RawMessage(`"native"`),
			syncFieldPart:      json.RawMessage(`{}`),
			jsonFieldTime:      json.RawMessage(`{`),
		},
	}
	require.Error(t, validateSyncEvent(partEvent, node))
	partEvent.Data[jsonFieldTime] = json.RawMessage(`"one"`)
	require.Error(t, validateSyncEvent(partEvent, node))
	partEvent.Data[jsonFieldTime] = json.RawMessage(`1e10000`)
	require.Error(t, validateSyncEvent(partEvent, node))

	invalid := validSyncSnapshot("session", "native", "/source")
	invalid.Format = "unsupported"
	entry, err := json.Marshal(invalid)
	require.NoError(t, err)
	store := NewInMemorySessionStore()
	require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
		Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{entry},
	}}))
	_, _, found, err := hydrateStateFromStore(t.Context(), store, "session")
	require.Error(t, err)
	require.False(t, found)

	badMarshal := validSyncSnapshot("session", "native", "/source")
	badMarshal.Events["native"][0].Data[syncFieldInfo] = json.RawMessage(`{`)
	require.Error(t, scanStateSnapshot(badMarshal, nil))

	terminal := terminalTestSnapshot(t,
		terminalMessageEvent("native", 1, "assistant", "assistant", "stop", int64Pointer(100)),
	)
	terminal = mutateTerminalTestSnapshot(t, terminal, func(value *stateSnapshot) {
		value.Events["native"][1].Data[syncFieldInfo] = json.RawMessage(
			`{"id":[],"sessionID":"native","role":"assistant","finish":"stop"}`,
		)
	})
	_, err = InspectSessionStoreTerminalState("session", []SessionStoreEntry{terminal})
	require.ErrorContains(t, err, "decode OpenCode message event")
}

func TestActiveReplacementAndArtifactLoadEdges(t *testing.T) {
	t.Run("zero replacement timeout takes the default", func(t *testing.T) {
		cwd := t.TempDir()
		store := NewInMemorySessionStore()
		client := newFakeOpenCodeClient()
		agent := NewAgent(WithSessionStore(store))
		agent.sessionReplacementTimeout = 0
		current := testSession(t, agent, client)
		current.cwd = cwd
		current.carrier = newSessionCarrier(map[string]string{"COLOR": "old"}, nil)
		require.NoError(t, current.snapshotToStore(t.Context()))
		client.closeErr = errors.Join(errors.New("still live"), ErrContainmentIncomplete)

		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(current.id, cwd,
			WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{"COLOR": "new"}))),
		))
		require.ErrorIs(t, err, ErrContainmentIncomplete)
	})

	t.Run("replacement detects a changed active mapping", func(t *testing.T) {
		cwd := t.TempDir()
		store := NewInMemorySessionStore()
		client := newFakeOpenCodeClient()
		agent := NewAgent(WithSessionStore(store))
		current := testSession(t, agent, client)
		current.cwd = cwd
		current.carrier = newSessionCarrier(map[string]string{"COLOR": "old"}, nil)
		require.NoError(t, current.snapshotToStore(t.Context()))
		replacement := &session{id: current.id}
		client.closeHook = func() {
			agent.mu.Lock()
			agent.sessions[current.id] = replacement
			agent.mu.Unlock()
		}

		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(current.id, cwd,
			WithSessionOpenCodeOptions(NewOpenCodeOptions(WithOpenCodeEnv(map[string]string{"COLOR": "new"}))),
		))
		require.ErrorContains(t, err, "changed during replacement")
	})

	t.Run("missing image artifact blocks hydration", func(t *testing.T) {
		cwd := t.TempDir()
		snapshot := validSyncSnapshot("session", "native", cwd)
		snapshot.Events["native"] = append(snapshot.Events["native"], opencode.SyncEvent{
			ID: "part", AggregateID: "native", Sequence: 1, Type: syncTypeMessagePartUpdated,
			Data: map[string]json.RawMessage{
				syncFieldSessionID: json.RawMessage(`"native"`),
				syncFieldPart: json.RawMessage(
					`{"url":"` + imageArtifactReferenceScheme + `missing"}`,
				),
				jsonFieldTime: json.RawMessage(`1`),
			},
		})
		entry, err := json.Marshal(snapshot)
		require.NoError(t, err)
		store := NewInMemorySessionStore()
		require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: "session"}, []SessionStoreReplacement{{
			Key: SessionKey{SessionID: "session", Subpath: SessionStoreMainSubpath}, Entries: []SessionStoreEntry{entry},
		}}))
		agent := NewAgent(WithSessionStore(store))
		_, err = agent.ResumeSession(t.Context(), ResumeSessionRequest("session", cwd))
		require.ErrorContains(t, err, outputReasonStorageFailed)
	})
}
