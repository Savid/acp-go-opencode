package opencodeacp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

type runtimeRetirement struct {
	generation uint64
	done       chan struct{}
	runtime    opencode.Client
	retryMu    sync.Mutex
	err        error
}

var (
	runtimeEvalSymlinks = filepath.EvalSymlinks
	runtimeAbs          = filepath.Abs
	runtimeJSONMarshal  = json.Marshal
	runtimeStartServer  = opencode.StartServer
	runtimeRemoveAll    = os.RemoveAll
)

func fatalRuntimeCleanup(err error) bool {
	return errors.Is(err, ErrHostAuthorityUnavailable) ||
		errors.Is(err, ErrContainmentIncomplete) ||
		errors.Is(err, opencode.ErrRuntimeScratchCleanup)
}

func (a *Agent) sharedRuntimeBinding(
	ctx context.Context,
) (opencode.Client, uint64, error) {
	for {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()

			return nil, 0, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
		}

		if a.runtimeFatalErr != nil {
			err := a.runtimeFatalErr
			a.mu.Unlock()

			return nil, 0, err
		}

		if sequencing := a.runtimeSequencing; sequencing != nil {
			a.mu.Unlock()

			if err := a.retryRuntimeCleanup(ctx, sequencing); err != nil {
				return nil, 0, err
			}

			continue
		}

		if a.runtime != nil {
			runtime := a.runtime
			generation := a.runtimeGeneration

			a.mu.Unlock()

			exited := runtime.RuntimeExited()
			if exited != nil {
				select {
				case <-exited:
					a.handleSharedRuntimeExit(runtime, generation)

					continue
				default:
				}
			}

			return runtime, generation, nil
		}

		if waiting := a.runtimeStarting; waiting != nil {
			a.mu.Unlock()

			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
		}

		var generation uint64

		starting := make(chan struct{})
		a.runtimeStarting = starting
		a.mu.Unlock()

		runtime, err := a.startSharedRuntime(context.WithoutCancel(ctx))

		a.mu.Lock()
		if err == nil && !a.closed {
			a.runtimeGeneration++
			generation = a.runtimeGeneration
			a.runtime = runtime
			a.runtimeStartErr = nil
			a.runtimeStarting = nil

			close(starting)
			a.mu.Unlock()

			// The watcher belongs to the published runtime, not the request that
			// happened to start it.
			go a.watchSharedRuntime(context.WithoutCancel(ctx), runtime, generation)

			return runtime, generation, nil
		}

		if err == nil {
			err = acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
		}
		a.mu.Unlock()

		var shutdownErr error

		if runtime != nil {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), closeTimeout)
			shutdownErr = runtime.Shutdown(shutdownCtx)

			shutdownCancel()
		}

		startErr := errors.Join(err, shutdownErr)

		a.mu.Lock()

		a.runtimeStartErr = startErr
		if fatalRuntimeCleanup(startErr) {
			a.runtimeFatalErr = startErr
		}

		if a.runtimeStarting == starting {
			a.runtimeStarting = nil
		}

		close(starting)
		a.mu.Unlock()

		return nil, 0, startErr
	}
}

func (a *Agent) watchSharedRuntime(ctx context.Context, runtime opencode.Client, generation uint64) {
	defer recoverAgentGoroutine(ctx, a.log, "shared runtime watcher")

	exited := runtime.RuntimeExited()
	if exited == nil {
		return
	}

	<-exited
	a.handleSharedRuntimeExit(runtime, generation)
}

func (a *Agent) handleSharedRuntimeExit(runtime opencode.Client, generation uint64) {
	a.mu.Lock()
	current := !a.closed && a.runtime == runtime && a.runtimeGeneration == generation
	a.mu.Unlock()

	if !current {
		return
	}

	cause := errValueSharedRuntimeExited
	if reporter, ok := runtime.(interface{ RuntimeRevoked() bool }); ok && reporter.RuntimeRevoked() {
		cause = "shared OpenCode runtime revoked"
	}

	if err := a.retireSharedRuntime(generation, cause); err != nil && a.log != nil {
		a.log.ErrorContext(context.Background(), "clean up exited shared OpenCode runtime")
	}
}

// retireSharedRuntime is the sole exact-generation retirement fence.
// Every caller for one generation observes the same shutdown/proof result, and
// no replacement runtime can start until that result has been published.
func (a *Agent) retireSharedRuntime(generation uint64, cause string) error {
	return a.retireSharedRuntimeStarted(generation, cause, nil)
}

// containSharedRuntimeGeneration starts the exact-generation retirement and
// returns once admission has been fenced. Cleanup continues asynchronously so a
// session pump can leave its own goroutine before retirement waits for that pump
// to stop.
func (a *Agent) containSharedRuntimeGeneration(generation uint64, cause string) {
	started := make(chan struct{})

	go func() {
		err := a.retireSharedRuntimeStarted(generation, cause, started)
		if err != nil && a.log != nil {
			a.log.ErrorContext(context.Background(), "contain failed OpenCode runtime generation",
				slog.Uint64("generation", generation))
		}
	}()

	<-started
}

func (a *Agent) retireSharedRuntimeStarted(generation uint64, cause string, started chan<- struct{}) error {
	var signalOnce sync.Once

	signalStarted := func() {
		if started != nil {
			signalOnce.Do(func() { close(started) })
		}
	}
	defer signalStarted()

	a.mu.Lock()
	if retirement := a.runtimeRetirements[generation]; retirement != nil {
		done := retirement.done
		a.mu.Unlock()
		signalStarted()
		<-done

		return retirement.err
	}

	if a.runtime == nil || a.runtimeGeneration != generation {
		err := a.runtimeFatalErr
		a.mu.Unlock()
		signalStarted()

		if err != nil {
			return err
		}

		return errors.Join(
			ErrContainmentIncomplete,
			fmt.Errorf("OpenCode runtime generation %d has no containment result", generation),
		)
	}

	runtime := a.runtime
	sessions := make([]*session, 0, len(a.sessions))

	for _, current := range a.sessions {
		sessions = append(sessions, current)
	}

	retirement := &runtimeRetirement{generation: generation, done: make(chan struct{}), runtime: runtime}
	a.runtimeRetirements[generation] = retirement
	a.directories = make(map[string]directoryBinding)
	a.runtime = nil
	a.runtimeStarting = retirement.done
	a.mu.Unlock()
	signalStarted()

	cleanupErr := a.settleSharedRuntimeRetirement(
		runtime,
		generation,
		cause,
		sessions,
	)

	a.mu.Lock()
	retirement.err = cleanupErr

	if errors.Is(cleanupErr, ErrNativeTreeBusy) {
		a.runtimeSequencing = retirement
	}

	if fatalRuntimeCleanup(cleanupErr) {
		a.runtimeFatalErr = cleanupErr
	}

	if a.runtimeStarting == retirement.done {
		a.runtimeStarting = nil
	}

	close(retirement.done)
	a.mu.Unlock()

	return cleanupErr
}

func (a *Agent) retryRuntimeCleanup(ctx context.Context, retirement *runtimeRetirement) error {
	retirement.retryMu.Lock()
	defer retirement.retryMu.Unlock()

	a.mu.Lock()
	current := a.runtimeSequencing == retirement
	a.mu.Unlock()

	if !current {
		return nil
	}

	err := retirement.runtime.Shutdown(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.runtimeSequencing != retirement {
		return err
	}

	retirement.err = err
	if err == nil {
		a.runtimeSequencing = nil

		return nil
	}

	if fatalRuntimeCleanup(err) {
		a.runtimeFatalErr = err
		a.runtimeSequencing = nil
	}

	return err
}

func (a *Agent) settleSharedRuntimeRetirement(
	runtime opencode.Client,
	generation uint64,
	cause string,
	sessions []*session,
) (cleanupErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cleanupErr = errors.Join(
				ErrContainmentIncomplete,
				fmt.Errorf("retire OpenCode runtime generation %d panicked", generation),
			)
		}
	}()

	var (
		detachGroup sync.WaitGroup
		detachErrMu sync.Mutex
		detachErr   error
	)

	detachGroup.Add(len(sessions))

	for _, current := range sessions {
		go func() {
			defer detachGroup.Done()
			defer func() {
				recovered := recover()
				handleAgentGoroutinePanic(context.Background(), a.log, "shared runtime detach", nil, recovered)

				if recovered == nil {
					return
				}

				detachErrMu.Lock()
				detachErr = errors.Join(
					detachErr,
					ErrContainmentIncomplete,
					fmt.Errorf("detach OpenCode session from runtime generation %d panicked", generation),
				)
				detachErrMu.Unlock()
			}()

			current.detachRuntime(generation, cause)
		}()
	}

	detachGroup.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), settlementTimeout)
	defer cancel()

	shutdownErr := errors.Join(runtime.Shutdown(ctx), detachErr)

	return shutdownErr
}

func (a *Agent) runtimeGenerationIsCurrent(generation uint64) bool {
	a.mu.Lock()
	current := !a.closed && a.runtime != nil && a.runtimeGeneration == generation
	runtime := a.runtime
	a.mu.Unlock()

	if !current {
		return false
	}

	exited := runtime.RuntimeExited()
	if exited == nil {
		return true
	}

	select {
	case <-exited:
		a.handleSharedRuntimeExit(runtime, generation)

		return false
	default:
		return true
	}
}

func (a *Agent) quarantineRuntimeConfiguration(generation uint64, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.closed && a.runtime != nil && a.runtimeGeneration == generation && a.runtimeFatalErr == nil {
		a.runtimeFatalErr = errors.Join(opencode.ErrMCPDisconnectUnproven, err)
	}
}

func (a *Agent) closeDirectoryScope(client opencode.Client, releaseDirectory func(), generation uint64) error {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
	closeErr := client.Close(closeCtx)

	closeCancel()

	current := a.runtimeGenerationIsCurrent(generation)
	if closeErr == nil || !current {
		releaseDirectory()

		return closeErr
	}

	a.quarantineRuntimeConfiguration(generation, closeErr)

	return closeErr
}

func (a *Agent) closeFailedSession(session *session) error {
	closeErr := session.Close(context.Background())
	if closeErr != nil {
		a.quarantineRuntimeConfiguration(session.runtimeGeneration, closeErr)
	}

	return closeErr
}

func (a *Agent) startSharedRuntime(ctx context.Context) (opencode.Client, error) {
	var managedEnvironment map[string]string

	if a.options.hostAuthorityConfigured {
		var err error

		managedEnvironment, err = hostAuthorityEnvironment(a.options.HostAuthority)
		if err != nil {
			return nil, err
		}
	}

	root, generated, err := a.newRuntimeRoot()
	if err != nil {
		return nil, err
	}

	startOptions := opencode.StartOptions{
		Root: root, ControlRoot: opencode.ControlRootForXDG(root), RemoveRoot: generated,
		ScratchParent: a.scratchParent(), ExecutablePath: a.options.ExecutablePath,
		Env: a.observe.InjectTraceEnv(ctx, cloneStringMap(a.options.Env)),
		NativeEnvironment: func() map[string]string {
			return cloneStringMap(a.options.implicitEnvironment)
		},
		Pure: a.options.Pure, QuestionTool: a.options.QuestionTool,
		LogLevel: a.options.LogLevel, MinVersion: minNativeVersion,
		HealthTimeout: a.options.HealthCheckTimeout, Logger: a.log,
		SeedFiles: a.options.SeedFiles,
	}

	if a.options.hostAuthorityConfigured {
		authority := a.options.HostAuthority
		startOptions.NativeEnvironment = func() map[string]string {
			return cloneStringMap(managedEnvironment)
		}
		startOptions.PrepareTree = func(prepareCtx context.Context, path string) (err error) {
			defer func() {
				if recover() != nil {
					err = opencode.MarkAuthorityUnavailable(ErrHostAuthorityUnavailable)
				}
			}()

			if err := authority.PrepareNativeTree(prepareCtx, path); err != nil {
				return err
			}

			return nil
		}
		startOptions.ReclaimTree = func(reclaimCtx context.Context, path string) (err error) {
			defer func() {
				if recover() != nil {
					err = errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete)
				}
			}()

			if err := authority.ReclaimNativeTree(reclaimCtx, path); err != nil {
				if errors.Is(err, ErrNativeTreeBusy) {
					return opencode.MarkTreeReclaimPending(err)
				}

				return errors.Join(ErrContainmentIncomplete, err)
			}

			return nil
		}
		startOptions.StartProcess = authorityProcessStarter(authority)
	}

	factory := a.options.clientFactory
	if factory == nil {
		factory = runtimeStartServer
	}

	a.observe.RecordOpenCodeProcessStart(ctx)

	runtime, err := factory(ctx, startOptions)
	if err != nil {
		if generated && !fatalRuntimeCleanup(err) && !opencode.NativeCleanupRetained(err) {
			err = errors.Join(err, runtimeRemoveAll(root), runtimeRemoveAll(opencode.ControlRootForXDG(root)))
		}

		return nil, startupFailure(err)
	}

	return runtime, nil
}

func (a *Agent) bindDirectory(id acp.SessionId, cwd string, servers []opencode.MCPServerConfig) (func(), error) {
	canonical, err := runtimeEvalSymlinks(cwd)
	if err != nil {
		return nil, fmt.Errorf("canonicalize cwd: %w", err)
	}

	canonical, err = runtimeAbs(canonical)
	if err != nil {
		return nil, fmt.Errorf("canonicalize cwd: %w", err)
	}

	fingerprint, err := a.directoryMCPFingerprint(servers)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.runtimeFatalErr != nil {
		return nil, a.runtimeFatalErr
	}

	if existing, ok := a.directories[canonical]; ok {
		if existing.MCPFingerprint != fingerprint {
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: "mcp_principal_conflict", jsonFieldCwd: canonical})
		}

		if existing.SessionID != id {
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueBackpressure, jsonFieldLimit: "directory_mcp_principal"})
		}
	}

	incarnation := a.nextDirectoryBindingIncarnationLocked()
	a.directories[canonical] = directoryBinding{
		SessionID:      id,
		MCPFingerprint: fingerprint,
		Incarnation:    incarnation,
	}

	return func() {
		a.mu.Lock()
		if current, ok := a.directories[canonical]; ok &&
			current.SessionID == id && current.Incarnation == incarnation {
			delete(a.directories, canonical)
		}
		a.mu.Unlock()
	}, nil
}

func (a *Agent) directoryMCPFingerprint(servers []opencode.MCPServerConfig) (string, error) {
	if len(servers) == 0 {
		return "", nil
	}

	canonical := append([]opencode.MCPServerConfig(nil), servers...)

	slices.SortFunc(canonical, func(left, right opencode.MCPServerConfig) int {
		if left.Name < right.Name {
			return -1
		}

		if left.Name > right.Name {
			return 1
		}

		return 0
	})

	data, err := runtimeJSONMarshal(canonical)
	if err != nil {
		return "", fmt.Errorf("canonicalize MCP configuration: %w", err)
	}

	mac := hmac.New(sha256.New, a.fingerprintKey[:])
	_, _ = mac.Write(data)

	return hex.EncodeToString(mac.Sum(nil)), nil
}
