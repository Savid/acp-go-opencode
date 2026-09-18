package opencodeacp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const sessionShutdownTimeout = 10 * time.Second
const sessionShutdownGrace = 2 * time.Second

const serverStartupTimeout = 2 * time.Minute
const serverHealthTimeout = 2 * time.Second

// runtime owns one shared server, SSE stream, and native home lock.
type runtime struct {
	proc        *process.Process
	client      *opencode.Client
	executable  string
	environment []string
	stream      *opencode.Stream
	root        string
	lock        *process.FileLock
	cancel      context.CancelFunc
	done        chan struct{}
	closeOnce   sync.Once
	mu          sync.Mutex
	bindings    map[string]*binding
}

func (rt *runtime) alive() bool {
	select {
	case <-rt.done:
		return false
	case <-rt.proc.Done():
		return false
	default:
		return true
	}
}

func (a *Agent) ensureRuntime(ctx context.Context) (*runtime, error) {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()

	if a.runtime != nil && a.runtime.alive() {
		return a.runtime, nil
	}

	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	if a.runtime != nil {
		a.runtime.close()
	}

	rt, err := a.startRuntime(ctx)
	if err != nil {
		a.log.ErrorContext(ctx, "opencode server startup failed", slog.String("reason", err.Error()))

		if refusal := wire.SeedFileRefusal(err); refusal != nil {
			return nil, refusal
		}

		if a.runtime != nil {
			return nil, wire.RuntimeUnavailable(vendor)
		}

		return nil, wire.InternalFailure(vendor, internalClassNativeStart)
	}

	a.runtime = rt

	return rt, nil
}
func (a *Agent) startRuntime(ctx context.Context) (*runtime, error) {
	executable, err := a.ensureExecutable(ctx)
	if err != nil {
		return nil, err
	}

	client, err := opencode.NewClient()
	if err != nil {
		return nil, err
	}

	root, err := a.scratchDir("plugin")
	if err != nil {
		return nil, err
	}

	transferred := false
	defer func() {
		if !transferred {
			_ = os.RemoveAll(root)
		}
	}()

	environment := a.environment(nil, nil)

	base, err := environment.Build()
	if err != nil {
		return nil, err
	}

	lookup := func(key string) (string, bool) { return process.Lookup(base, key) }

	lock, err := process.LockFile(filepath.Join(opencode.DataDir(lookup), ".acp-go-opencode.lock"))
	if err != nil {
		return nil, err
	}

	defer func() {
		if !transferred {
			_ = lock.Close()
		}
	}()

	if seedErr := process.WriteSeedFiles(opencode.ConfigDir(lookup), a.options.SeedFiles); seedErr != nil {
		return nil, seedErr
	}

	config, _ := lookup("OPENCODE_CONFIG_CONTENT")

	config, err = opencode.WritePlugin(root, config, a.options.Home != "")
	if err != nil {
		return nil, err
	}

	owned := map[string]string{"OPENCODE_SERVER_USERNAME": opencode.ServerUsername, "OPENCODE_SERVER_PASSWORD": client.Password, "OPENCODE_CONFIG_CONTENT": config, "OPENCODE_ENABLE_QUESTION_TOOL": "1"}

	env, err := a.environment(nil, owned).Build()
	if err != nil {
		return nil, err
	}

	proc, err := process.Start(ctx, process.Request{Executable: executable, Args: client.Args(), Env: env})
	if err != nil {
		return nil, err
	}

	go func() { _, _ = io.Copy(io.Discard, proc.Stdout()) }()

	defer func() {
		if !transferred {
			// The lock defer registered above runs after this one, so the
			// child is reaped before the native home lock is released: two
			// servers must never hold one native data directory.
			shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), sessionShutdownTimeout)
			defer shutdownCancel()

			_ = proc.Shutdown(shutdownCtx, sessionShutdownGrace)
			_ = proc.Close()
		}
	}()

	started := time.Now()

	readyCtx, cancel := context.WithTimeout(ctx, serverStartupTimeout)
	defer cancel()

	if healthErr := a.waitForServerHealth(readyCtx, client, proc); healthErr != nil {
		return nil, fmt.Errorf("native health after %s: %w", time.Since(started).Round(time.Millisecond), healthErr)
	}

	healthDuration := time.Since(started)

	var doc struct {
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if docErr := client.Do(readyCtx, "", http.MethodGet, "/doc", nil, &doc); docErr != nil {
		return nil, fmt.Errorf("native schema after %s: %w", time.Since(started).Round(time.Millisecond), docErr)
	}

	if doc.Components.Schemas["OutputFormatJsonSchema"] == nil {
		return nil, errors.New("native structured output schema missing")
	}

	a.log.InfoContext(ctx, "opencode server ready",
		slog.Duration("health_duration", healthDuration), slog.Duration("schema_duration", time.Since(started)-healthDuration))

	runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(ctx))

	stream, err := client.Subscribe(runtimeCtx)
	if err != nil {
		runtimeCancel()

		return nil, err
	}

	rt := &runtime{proc: proc, client: client, executable: executable, environment: base, stream: stream, root: root, lock: lock, cancel: runtimeCancel, done: make(chan struct{}), bindings: map[string]*binding{}}
	transferred = true

	go rt.pump(runtimeCtx)

	return rt, nil
}

func (a *Agent) waitForServerHealth(ctx context.Context, client *opencode.Client, proc *process.Process) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		// A request accepted during startup can remain unanswered after the server becomes healthy.
		probeCtx, cancel := context.WithTimeout(ctx, serverHealthTimeout)

		var health struct {
			Healthy bool `json:"healthy"`
		}

		err := client.Do(probeCtx, "", http.MethodGet, "/global/health", nil, &health)

		cancel()

		if err == nil && health.Healthy {
			return nil
		}

		if err == nil {
			err = errors.New("native server reports unhealthy")
		}

		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			a.log.InfoContext(ctx, "opencode health request stalled; retrying", slog.Duration("request_timeout", serverHealthTimeout))
		}

		select {
		case <-proc.Done():
			message := "opencode server exited before it was ready"
			if line := proc.StderrLastLine(); line != "" {
				message += ": " + line
			}

			return errors.New(message)
		case <-ctx.Done():
			return fmt.Errorf("last health request: %v: %w", err, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (rt *runtime) pump(ctx context.Context) {
	defer close(rt.done)

	for event := range rt.stream.Events {
		id := event.SessionID()
		if id == "" {
			continue
		}

		rt.mu.Lock()

		b := rt.bindings[id]
		if b != nil {
			select {
			case b.events <- event:
			default:
				b.cancel()
			}
		}
		rt.mu.Unlock()
	}

	rt.cancel()
	rt.stream.Close()

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionShutdownTimeout)
	defer cancel()

	if err := rt.proc.Shutdown(shutdownCtx, sessionShutdownGrace); err != nil {
		_ = rt.proc.Kill()
	}

	_ = rt.proc.Close()
	_ = rt.lock.Close()
	rt.mu.Lock()
	for _, b := range rt.bindings {
		b.cancel()
	}

	clear(rt.bindings)
	rt.mu.Unlock()
	_ = os.RemoveAll(rt.root)
}
func (rt *runtime) close() { rt.closeOnce.Do(func() { rt.cancel(); rt.stream.Close(); <-rt.done }) }
func (a *Agent) stopRuntime() {
	a.runtimeMu.Lock()
	rt := a.runtime
	a.runtimeMu.Unlock()

	if rt != nil {
		rt.close()
	}
}
