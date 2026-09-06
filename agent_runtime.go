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
		errors.Is(err, ErrContainmentIncomplete)
}

func retryableRuntimeCleanup(err error) bool {
	return !errors.Is(err, ErrHostAuthorityUnavailable) &&
		(errors.Is(err, opencode.ErrProcessContainmentIncomplete) ||
			errors.Is(err, ErrNativeTreeBusy) ||
			errors.Is(err, opencode.ErrRuntimeScratchCleanup))
}

func classifyRuntimeContainment(err error) error {
	if errors.Is(err, opencode.ErrProcessContainmentIncomplete) && !errors.Is(err, ErrContainmentIncomplete) {
		return errors.Join(ErrContainmentIncomplete, err)
	}

	return err
}

func classifyRuntimeShutdown(ctx context.Context, err error) error {
	err = classifyRuntimeContainment(err)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return errors.Join(ErrContainmentIncomplete, opencode.ErrProcessContainmentIncomplete, err)
	}

	return err
}

func (a *Agent) sharedRuntimeBinding(
	ctx context.Context,
) (opencode.Client, uint64, error) {
	for {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()

			return nil, 0, acp.NewInvalidRequest(map[string]any{jsonFieldError: valAgentClosed})
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
			err = acp.NewInvalidRequest(map[string]any{jsonFieldError: valAgentClosed})
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

	cause := valSharedRuntimeExited
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

	if retryableRuntimeCleanup(cleanupErr) {
		a.runtimeSequencing = retirement
	}

	if fatalRuntimeCleanup(cleanupErr) && !retryableRuntimeCleanup(cleanupErr) {
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

	err := classifyRuntimeShutdown(ctx, retirement.runtime.Shutdown(ctx))

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

	if fatalRuntimeCleanup(err) && !retryableRuntimeCleanup(err) {
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

	shutdownErr := errors.Join(classifyRuntimeShutdown(ctx, runtime.Shutdown(ctx)), detachErr)

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
	closeCtx, closeCancel := context.WithTimeout(context.Background(), closeTimeout)
	closeErr := session.Close(closeCtx)

	closeCancel()

	if closeErr != nil {
		a.quarantineRuntimeConfiguration(session.runtimeGeneration, closeErr)
	}

	return closeErr
}

func (a *Agent) startSharedRuntime(ctx context.Context) (opencode.Client, error) {
	a.nativeAdmissionMu.Lock()
	defer a.nativeAdmissionMu.Unlock()

	if a.options.hostAuthorityConfigured && a.options.Home != "" {
		seedPaths := make([]string, 0, len(a.options.SeedFiles))
		for path := range a.options.SeedFiles {
			seedPaths = append(seedPaths, path)
		}

		slices.Sort(seedPaths)

		for _, path := range seedPaths {
			if path != "opencode.json" {
				return nil, unsupportedField("seedFiles[" + path + "]")
			}
		}
	}

	var managedEnvironment map[string]string

	if a.options.hostAuthorityConfigured {
		if err := a.retryRetiredNativeTrees(ctx); err != nil {
			return nil, err
		}

		environment, err := hostAuthorityEnvironment(a.options.HostAuthority)
		if err != nil {
			return nil, err
		}

		managedEnvironment = environment
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
		SeedFiles: a.options.SeedFiles, PluginSeedDir: a.pluginSeedDir(ctx),
	}

	if a.options.hostAuthorityConfigured {
		authority := a.options.HostAuthority
		startOptions.NativeEnvironment = func() map[string]string {
			return cloneStringMap(managedEnvironment)
		}
		startOptions.PrepareTree = func(prepareCtx context.Context, path string) (err error) {
			defer func() {
				if recover() != nil {
					err = opencode.MarkPrepareOpaque(errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete))
				}
			}()

			if err := authority.PrepareNativeTree(prepareCtx, path); err != nil {
				return opencode.MarkPrepareOpaque(errors.Join(ErrContainmentIncomplete, err))
			}

			return nil
		}
		startOptions.ReclaimTree = func(reclaimCtx context.Context, path string) error {
			return reclaimManagedNativeTree(reclaimCtx, authority, path)
		}
		startOptions.RetainTree = a.retainNativeTree
		startOptions.StartProcess = authorityProcessStarter(authority)
	}

	factory := a.options.clientFactory
	if factory == nil {
		factory = runtimeStartServer
	}

	a.observe.RecordOpenCodeProcessStart(ctx)

	runtime, err := factory(ctx, startOptions)
	if err != nil {
		err = classifyRuntimeContainment(err)
		if generated && !fatalRuntimeCleanup(err) && !opencode.NativeCleanupRetained(err) {
			err = errors.Join(err, runtimeRemoveAll(root), runtimeRemoveAll(opencode.ControlRootForXDG(root)))
		}

		return nil, startupFailure(err)
	}

	return runtime, nil
}

func reclaimManagedNativeTree(ctx context.Context, authority HostAuthority, path string) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete)
		}
	}()

	if err := authority.ReclaimNativeTree(ctx, path); err != nil {
		if errors.Is(err, ErrNativeTreeBusy) {
			return opencode.MarkTreeReclaimPending(err)
		}

		return errors.Join(ErrContainmentIncomplete, err)
	}

	return nil
}

func (a *Agent) retainNativeTree(path string, reclaimed bool, cleanup func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	existing, ok := a.retiredNativeTrees[path]
	if ok {
		reclaimed = existing.reclaimed || reclaimed
		if cleanup == nil {
			cleanup = existing.cleanup
		}
	}

	if a.retiredNativeTrees == nil {
		a.retiredNativeTrees = make(map[string]retiredNativeTree)
	}

	a.retiredNativeTrees[path] = retiredNativeTree{reclaimed: reclaimed, cleanup: cleanup}
}

func (a *Agent) retryRetiredNativeTrees(ctx context.Context) error {
	a.mu.Lock()

	trees := make(map[string]retiredNativeTree, len(a.retiredNativeTrees))
	for path, tree := range a.retiredNativeTrees {
		trees[path] = tree
	}
	a.mu.Unlock()

	var result error

	for path, tree := range trees {
		if !tree.reclaimed {
			err := reclaimManagedNativeTree(ctx, a.options.HostAuthority, path)
			if err != nil {
				result = errors.Join(result, err)

				continue
			}

			tree.reclaimed = true

			a.mu.Lock()

			current, ok := a.retiredNativeTrees[path]
			if ok {
				current.reclaimed = true
				a.retiredNativeTrees[path] = current
			}
			a.mu.Unlock()
		}

		if tree.cleanup != nil {
			if err := tree.cleanup(); err != nil {
				result = errors.Join(result, opencode.ErrRuntimeScratchCleanup, err)

				continue
			}
		}

		a.mu.Lock()
		delete(a.retiredNativeTrees, path)
		a.mu.Unlock()
	}

	return result
}

// bindDirectory admits one logical session to a canonical directory's MCP
// principal. parentID is the requesting session's ACP fork parent, or empty for
// a lineage root; it is what lets a fork join the principal its parent already
// holds. Every other pair of sessions still owns a directory exclusively.
func (a *Agent) bindDirectory(
	id acp.SessionId,
	parentID acp.SessionId,
	cwd string,
	servers []opencode.MCPServerConfig,
) (func(), error) {
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

	binding, ok := a.directories[canonical]
	if ok {
		// A differing MCP set is a conflict whoever asks: one directory has one
		// registered tool catalog, and two sessions cannot disagree about it.
		if binding.MCPFingerprint != fingerprint {
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: "mcp_principal_conflict", jsonFieldCwd: canonical})
		}

		if !a.admitsDirectoryHolderLocked(binding, id, parentID) {
			return nil, acp.NewInvalidRequest(map[string]any{jsonFieldError: valBackpressure, jsonFieldLimit: "directory_mcp_principal"})
		}
	} else {
		binding = directoryBinding{
			Holders:        make(map[acp.SessionId]directoryBindingIncarnation),
			MCPFingerprint: fingerprint,
		}
	}

	incarnation := a.nextDirectoryBindingIncarnationLocked()
	binding.Holders[id] = incarnation
	a.directories[canonical] = binding

	return func() { a.releaseDirectory(canonical, id, incarnation) }, nil
}

// admitsDirectoryHolderLocked reports whether the requesting session may join a
// principal another session already holds. The same session rebinding always
// may; another session may only when the two are in one fork lineage. The caller
// must hold a.mu.
func (a *Agent) admitsDirectoryHolderLocked(binding directoryBinding, id, parentID acp.SessionId) bool {
	if _, held := binding.Holders[id]; held {
		return true
	}

	root := a.lineageRootLocked(id, parentID)

	for holder := range binding.Holders {
		holderParent := acp.SessionId("")
		if member := a.sessions[holder]; member != nil {
			holderParent = member.parentSessionID()
		}

		if a.lineageRootLocked(holder, holderParent) == root {
			return true
		}
	}

	return false
}

// lineageRootLocked names the session a fork lineage descends from. The chain is
// walked over durable parent identity rather than over live sessions, so a
// lineage keeps its root while an ancestor is closed: a fork whose parent is no
// longer loaded still roots at that parent's id. The caller must hold a.mu.
func (a *Agent) lineageRootLocked(id, parentID acp.SessionId) acp.SessionId {
	root := id
	for parentID != "" {
		root = parentID

		member := a.sessions[root]
		if member == nil {
			break
		}

		parentID = member.parentSessionID()
	}

	return root
}

// releaseDirectory drops one holder. The principal itself survives while a
// co-holder remains; the surviving holders re-register the directory's MCP
// servers before their next prompt, because the leaving scope's teardown
// unregisters them for the whole directory.
func (a *Agent) releaseDirectory(canonical string, id acp.SessionId, incarnation directoryBindingIncarnation) {
	a.mu.Lock()

	binding, ok := a.directories[canonical]
	if !ok || binding.Holders[id] != incarnation {
		a.mu.Unlock()

		return
	}

	delete(binding.Holders, id)

	var survivors []*session

	if len(binding.Holders) == 0 {
		delete(a.directories, canonical)
	} else {
		a.directories[canonical] = binding

		if binding.MCPFingerprint != "" {
			for holder := range binding.Holders {
				if member := a.sessions[holder]; member != nil {
					survivors = append(survivors, member)
				}
			}
		}
	}

	a.mu.Unlock()

	for _, member := range survivors {
		member.requireMCPRefresh()
	}
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
