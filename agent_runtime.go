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
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

type runtimeRetirement struct {
	generation uint64
	done       chan struct{}
	err        error
}

var (
	runtimeEvalSymlinks      = filepath.EvalSymlinks
	runtimeAbs               = filepath.Abs
	runtimeJSONMarshal       = json.Marshal
	runtimeStartServer       = opencode.StartServer
	runtimeRemoveAll         = os.RemoveAll
	errRuntimeScratchCleanup = errors.New("adapter-created OpenCode runtime scratch cleanup failed")
)

func fatalRuntimeCleanup(err error) bool {
	return errors.Is(err, opencode.ErrProcessContainmentIncomplete) ||
		errors.Is(err, opencode.ErrRuntimeScratchCleanup) ||
		errors.Is(err, errRuntimeScratchCleanup)
}

func (a *Agent) sharedRuntime(ctx context.Context) (opencode.Client, error) {
	runtime, _, err := a.sharedRuntimeBinding(ctx)

	return runtime, err
}

func (a *Agent) sharedRuntimeBinding(ctx context.Context) (opencode.Client, uint64, error) {
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

		runtime, nativeRelease, xdgScratchRelease, err := a.startSharedRuntime(context.WithoutCancel(ctx))

		a.mu.Lock()
		if err == nil && !a.closed {
			a.runtimeGeneration++
			generation = a.runtimeGeneration
			a.runtime = runtime
			a.runtimeNativeRelease = nativeRelease
			a.runtimeXDGScratchRelease = xdgScratchRelease
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

		cleanupErr := shutdownErr
		if runtime != nil || nativeRelease != nil || xdgScratchRelease != nil {
			cleanupErr = a.cleanupRuntimeResources(shutdownErr, nativeRelease, xdgScratchRelease)
		}

		startErr := errors.Join(err, cleanupErr)

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

	if err := a.retireSharedRuntime(generation, "shared OpenCode runtime exited"); err != nil && a.log != nil {
		a.log.ErrorContext(context.Background(), "clean up exited shared OpenCode runtime", slog.Any("error", err))
	}
}

// retireSharedRuntime is the sole exact-generation process-containment fence.
// Every caller for one generation observes the same shutdown/proof result, and
// no replacement runtime can start until that result has been published.
func (a *Agent) retireSharedRuntime(generation uint64, cause string, targets ...*session) error {
	a.mu.Lock()
	if retirement := a.runtimeRetirements[generation]; retirement != nil {
		done := retirement.done
		a.mu.Unlock()
		<-done

		return retirement.err
	}

	if a.runtime == nil || a.runtimeGeneration != generation {
		err := a.runtimeFatalErr
		a.mu.Unlock()

		if err != nil {
			return err
		}

		return errors.Join(
			opencode.ErrProcessContainmentIncomplete,
			fmt.Errorf("OpenCode runtime generation %d has no containment result", generation),
		)
	}

	runtime := a.runtime
	sessions := make([]*session, 0, len(a.sessions))

	seenSessions := make(map[*session]struct{}, len(a.sessions)+len(targets))
	for _, current := range a.sessions {
		sessions = append(sessions, current)
		seenSessions[current] = struct{}{}
	}

	for _, target := range targets {
		if target == nil {
			continue
		}

		if _, exists := seenSessions[target]; exists {
			continue
		}

		sessions = append(sessions, target)
		seenSessions[target] = struct{}{}
	}

	retirement := &runtimeRetirement{generation: generation, done: make(chan struct{})}
	a.runtimeRetirements[generation] = retirement
	a.directories = make(map[string]directoryBinding)
	a.runtime = nil
	a.runtimeStarting = retirement.done
	nativeRelease := a.runtimeNativeRelease
	a.runtimeNativeRelease = nil
	xdgScratchRelease := a.runtimeXDGScratchRelease
	a.runtimeXDGScratchRelease = nil
	a.mu.Unlock()

	cleanupErr := a.settleSharedRuntimeRetirement(
		runtime,
		generation,
		cause,
		sessions,
		nativeRelease,
		xdgScratchRelease,
	)

	a.mu.Lock()
	retirement.err = cleanupErr

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

func (a *Agent) settleSharedRuntimeRetirement(
	runtime opencode.Client,
	generation uint64,
	cause string,
	sessions []*session,
	nativeRelease func(),
	xdgScratchRelease func(),
) (cleanupErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cleanupErr = errors.Join(
				opencode.ErrProcessContainmentIncomplete,
				fmt.Errorf("retire OpenCode runtime generation %d: %v", generation, recovered),
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
					opencode.ErrProcessContainmentIncomplete,
					fmt.Errorf("detach OpenCode session from runtime generation %d: %v", generation, recovered),
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

	return a.cleanupRuntimeResources(shutdownErr, nativeRelease, xdgScratchRelease)
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

func (a *Agent) startSharedRuntime(ctx context.Context) (opencode.Client, func(), func(), error) {
	hooks := a.options.RuntimeResourceHooks

	var xdgScratchRelease func()

	if a.options.Home == "" {
		var err error

		xdgScratchRelease, err = acquireRuntimeResource(ctx, hooks.ReserveScratchRoot, RuntimeResourceRuntime)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	nativeRelease, err := acquireRuntimeResource(ctx, hooks.AcquireNativeRoot, RuntimeResourceRuntime)
	if err != nil {
		err = errors.Join(err, a.cleanupRuntimeResources(nil, nil, xdgScratchRelease))

		return nil, nil, nil, err
	}

	xdg, err := opencode.CreateRuntimeXDGDirs(a.homeRoot())
	if err != nil {
		err = errors.Join(err, a.cleanupRuntimeResources(nil, nativeRelease, xdgScratchRelease))

		return nil, nil, nil, err
	}

	factory := a.options.clientFactory
	if factory == nil {
		factory = runtimeStartServer
	}

	a.observe.RecordOpenCodeProcessStart(ctx)

	runtime, err := factory(ctx, opencode.StartOptions{
		Root: a.homeRoot(), ScratchParent: scratchParent(a.options.ScratchDir),
		ContainmentScratchParent: scratchParent(a.options.ScratchDir),
		DarwinBestEffort:         a.containmentMode == RuntimeContainmentBestEffort,
		ReserveContainmentScratch: func(reservationCtx context.Context) (func(), error) {
			return acquireRuntimeResource(
				reservationCtx,
				hooks.ReserveScratchRoot,
				RuntimeResourceRuntime,
			)
		},
		ExecutablePath: a.options.ExecutablePath,
		Env:            a.observe.InjectTraceEnv(ctx, cloneStringMap(a.options.Env)),
		Pure:           a.options.Pure, QuestionTool: a.options.QuestionTool,
		LogLevel: a.options.LogLevel, MinVersion: minNativeVersion,
		HealthTimeout: a.options.HealthCheckTimeout, Logger: a.log,
		ExistingXDG: xdg, SeedFiles: a.options.SeedFiles,
		ObserveProcess: func(processCtx context.Context, kind string, delta int64) {
			observeRuntimeProcess(processCtx, hooks, RuntimeProcessKind(kind), delta)
		},
		ObserveProcessSnapshot: func(processCtx context.Context, kind string, count int) {
			observeRuntimeProcessSnapshot(processCtx, hooks, RuntimeProcessKind(kind), count)
		},
		ObserveStartupStage: func(stageCtx context.Context, lifecycle, stage string, elapsed time.Duration, stageErr error) {
			observe := hooks.ObserveStartupStage
			if observe != nil {
				observe(stageCtx, RuntimeResourceKind(lifecycle), RuntimeStartupStage(stage), elapsed, stageErr)
			}
		},
	})
	if err != nil {
		err = a.cleanupRuntimeResources(err, nativeRelease, xdgScratchRelease)

		return nil, nil, nil, err
	}

	return runtime, nativeRelease, xdgScratchRelease, nil
}

// cleanupRuntimeResources preserves permit ownership whenever the selected
// native containment boundary does not complete. After the selected boundary
// completes, the adapter-created XDG root is removed before its own reservation
// is released. Its deletion gate is independent from the Darwin generation's
// deletion gate inside the runtime client. A failed XDG removal retains only
// the XDG reservation while allowing the native permit to return to the
// worker-global pool.
func (a *Agent) cleanupRuntimeResources(shutdownErr error, nativeRelease, xdgScratchRelease func()) error {
	if errors.Is(shutdownErr, opencode.ErrProcessContainmentIncomplete) {
		return shutdownErr
	}

	var cleanupErr error

	if a.options.Home == "" {
		if err := runtimeRemoveAll(a.homeRoot()); err != nil {
			cleanupErr = errors.Join(
				errRuntimeScratchCleanup,
				fmt.Errorf("remove adapter-created OpenCode runtime scratch: %w", err),
			)
		} else if xdgScratchRelease != nil {
			xdgScratchRelease()
		}
	} else if xdgScratchRelease != nil {
		// Explicit homes do not acquire XDG scratch, but release an unexpected
		// caller-supplied reservation rather than leak it.
		xdgScratchRelease()
	}

	if nativeRelease != nil {
		nativeRelease()
	}

	return errors.Join(shutdownErr, cleanupErr)
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
