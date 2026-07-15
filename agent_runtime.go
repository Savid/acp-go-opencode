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
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

var (
	runtimeEvalSymlinks      = filepath.EvalSymlinks
	runtimeAbs               = filepath.Abs
	runtimeJSONMarshal       = json.Marshal
	runtimeStartServer       = opencode.StartServer
	runtimeRemoveAll         = os.RemoveAll
	errRuntimeScratchCleanup = errors.New("adapter-created OpenCode runtime scratch cleanup failed")
)

func fatalRuntimeCleanup(err error) bool {
	return errors.Is(err, opencode.ErrProcessTreeUnproven) || errors.Is(err, errRuntimeScratchCleanup)
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

		var (
			generation uint64
			published  bool
		)

		starting := make(chan struct{})
		a.runtimeStarting = starting
		a.mu.Unlock()

		runtime, nativeRelease, scratchRelease, err := a.startSharedRuntime(context.WithoutCancel(ctx))

		a.mu.Lock()

		if err == nil && !a.closed {
			a.runtimeGeneration++
			generation = a.runtimeGeneration
			a.runtime = runtime
			a.runtimeNativeRelease = nativeRelease
			a.runtimeScratchRelease = scratchRelease
			published = true
		} else if err == nil {
			err = acp.NewInvalidRequest(map[string]any{jsonFieldError: errValueAgentClosed})
		}

		a.runtimeStartErr = err
		if fatalRuntimeCleanup(err) {
			a.runtimeFatalErr = err
		}

		a.runtimeStarting = nil

		close(starting)
		a.mu.Unlock()

		if published {
			// The watcher belongs to the published runtime, not the request that
			// happened to start it.
			go a.watchSharedRuntime(runtime, generation)
		}

		if err != nil {
			var shutdownErr error

			if runtime != nil {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), closeTimeout)
				shutdownErr = runtime.Shutdown(shutdownCtx)

				shutdownCancel()
			}

			cleanupErr := shutdownErr
			if runtime != nil || nativeRelease != nil || scratchRelease != nil {
				cleanupErr = a.cleanupRuntimeResources(shutdownErr, nativeRelease, scratchRelease)
			}

			return nil, 0, errors.Join(err, cleanupErr)
		}

		return runtime, generation, nil
	}
}

func (a *Agent) watchSharedRuntime(runtime opencode.Client, generation uint64) {
	exited := runtime.RuntimeExited()
	if exited == nil {
		return
	}

	<-exited
	a.handleSharedRuntimeExit(runtime, generation)
}

func (a *Agent) handleSharedRuntimeExit(runtime opencode.Client, generation uint64) {
	a.mu.Lock()
	if a.closed || a.runtime != runtime || a.runtimeGeneration != generation {
		a.mu.Unlock()

		return
	}

	sessions := make([]*session, 0, len(a.sessions))
	for _, current := range a.sessions {
		sessions = append(sessions, current)
	}

	a.directories = make(map[string]directoryBinding)
	a.runtime = nil
	cleanupDone := make(chan struct{})
	a.runtimeStarting = cleanupDone
	nativeRelease := a.runtimeNativeRelease
	a.runtimeNativeRelease = nil
	scratchRelease := a.runtimeScratchRelease
	a.runtimeScratchRelease = nil
	a.mu.Unlock()

	for _, current := range sessions {
		current.detachRuntime(generation, "shared OpenCode runtime exited")
	}

	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	shutdownErr := runtime.Shutdown(ctx)

	cancel()

	cleanupErr := a.cleanupRuntimeResources(shutdownErr, nativeRelease, scratchRelease)

	a.mu.Lock()
	if fatalRuntimeCleanup(cleanupErr) {
		a.runtimeFatalErr = cleanupErr
	}

	if a.runtimeStarting == cleanupDone {
		a.runtimeStarting = nil
	}

	close(cleanupDone)
	a.mu.Unlock()

	if cleanupErr != nil && a.log != nil {
		a.log.ErrorContext(context.Background(), "clean up exited shared OpenCode runtime", slog.Any("error", cleanupErr))
	}
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
	scratchRelease := func() {}

	if a.options.Home == "" {
		var err error

		scratchRelease, err = acquireRuntimeResource(ctx, hooks.ReserveScratchRoot, RuntimeResourceRuntime)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	nativeRelease, err := acquireRuntimeResource(ctx, hooks.AcquireNativeRoot, RuntimeResourceRuntime)
	if err != nil {
		err = errors.Join(err, a.cleanupRuntimeResources(nil, nil, scratchRelease))

		return nil, nil, nil, err
	}

	xdg, err := opencode.CreateRuntimeXDGDirs(a.homeRoot())
	if err != nil {
		err = errors.Join(err, a.cleanupRuntimeResources(nil, nativeRelease, scratchRelease))

		return nil, nil, nil, err
	}

	factory := a.options.clientFactory
	if factory == nil {
		factory = runtimeStartServer
	}

	a.observe.RecordOpenCodeProcessStart(ctx)

	runtime, err := factory(ctx, opencode.StartOptions{
		Root: a.homeRoot(), ScratchParent: scratchParent(a.options.ScratchDir),
		ExecutablePath: a.options.ExecutablePath,
		Env:            a.observe.InjectTraceEnv(ctx, cloneStringMap(a.options.Env)),
		Pure:           a.options.Pure, QuestionTool: a.options.QuestionTool,
		LogLevel: a.options.LogLevel, ExactVersion: a.options.NativeVersion,
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
		err = a.cleanupRuntimeResources(err, nativeRelease, scratchRelease)

		return nil, nil, nil, err
	}

	return runtime, nativeRelease, scratchRelease, nil
}

// cleanupRuntimeResources preserves permit ownership whenever native-tree
// quiescence is unproven. Once quiescence is proved, adapter-created scratch is
// removed before its reservation is released. A failed removal deliberately
// retains only the scratch reservation while allowing the native permit to
// return to the worker-global pool.
func (a *Agent) cleanupRuntimeResources(shutdownErr error, nativeRelease, scratchRelease func()) error {
	if errors.Is(shutdownErr, opencode.ErrProcessTreeUnproven) {
		return shutdownErr
	}

	var cleanupErr error

	if a.options.Home == "" {
		if err := runtimeRemoveAll(a.homeRoot()); err != nil {
			cleanupErr = errors.Join(
				errRuntimeScratchCleanup,
				fmt.Errorf("remove adapter-created OpenCode runtime scratch: %w", err),
			)
		} else if scratchRelease != nil {
			scratchRelease()
		}
	} else if scratchRelease != nil {
		// Explicit homes never acquire a scratch reservation, but preserve the
		// release-pair invariant for the standalone no-op controller.
		scratchRelease()
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
