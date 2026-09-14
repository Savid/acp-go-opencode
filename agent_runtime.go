package opencodeacp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

const nativeSyncReplayPath = "/sync/replay"
const nativeSyncHistoryPath = "/sync/history"
const sessionShutdownTimeout = 10 * time.Second
const sessionShutdownGrace = 2 * time.Second
const stderrTailBytes = 8 << 10

// runtime owns one shared server, SSE stream, and native home lock.
type runtime struct {
	proc      *process.Process
	client    *opencode.Client
	stream    *opencode.Stream
	root      string
	lock      *process.FileLock
	stderr    *stderrTail
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	bindings  map[string]*binding
}

type stderrTail struct {
	mu   sync.Mutex
	data []byte
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.data = append(t.data, p...)
	if len(t.data) > stderrTailBytes {
		t.data = t.data[len(t.data)-stderrTailBytes:]
	}

	return len(p), nil
}
func (t *stderrTail) lastLine() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	lines := strings.Split(strings.TrimSpace(string(t.data)), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
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

	parent := a.options.ScratchDir
	if parent != "" {
		if mkdirErr := os.MkdirAll(parent, 0o700); mkdirErr != nil {
			return nil, mkdirErr
		}
	}

	root, err := os.MkdirTemp(parent, "acp-go-opencode-")
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
	if seedErr := process.WriteSeedFiles(opencode.ConfigDir(lookup), a.options.SeedFiles); seedErr != nil {
		return nil, seedErr
	}

	config, _ := lookup("OPENCODE_CONFIG_CONTENT")

	config, err = opencode.WritePlugin(root, config, a.options.Home != "")
	if err != nil {
		return nil, err
	}

	owned := map[string]string{"OPENCODE_SERVER_USERNAME": vendor, "OPENCODE_SERVER_PASSWORD": client.Password, "OPENCODE_CONFIG_CONTENT": config, "OPENCODE_ENABLE_QUESTION_TOOL": "1"}

	env, err := a.environment(nil, owned).Build()
	if err != nil {
		return nil, err
	}

	lock, err := process.LockFile(filepath.Join(opencode.DataDir(lookup), ".acp-go-opencode.lock"))
	if err != nil {
		return nil, err
	}

	defer func() {
		if !transferred {
			_ = lock.Close()
		}
	}()

	proc, err := process.Start(ctx, process.Request{Executable: executable, Args: client.Args(), Env: env})
	if err != nil {
		return nil, err
	}

	tail := &stderrTail{}
	go func() { _, _ = io.Copy(tail, proc.Stderr()) }()
	go func() { _, _ = io.Copy(io.Discard, proc.Stdout()) }()

	defer func() {
		if !transferred {
			_ = proc.Kill()
			_ = proc.Close()
		}
	}()

	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		var health struct {
			Healthy bool   `json:"healthy"`
			Version string `json:"version"`
		}
		if healthErr := client.Do(readyCtx, "", http.MethodGet, "/global/health", nil, &health); healthErr == nil && health.Healthy {
			break
		}

		select {
		case <-readyCtx.Done():
			return nil, readyCtx.Err()
		case <-ticker.C:
		}
	}

	var doc struct {
		Paths      map[string]any `json:"paths"`
		Components struct {
			Schemas map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if docErr := client.Do(readyCtx, "", http.MethodGet, "/doc", nil, &doc); docErr != nil {
		return nil, docErr
	}

	for _, path := range []string{nativeSyncHistoryPath, nativeSyncReplayPath, nativeCommandPath, "/session/{sessionID}/message", "/session/{sessionID}/command", "/permission/{requestID}/reply", "/question/{requestID}/reply"} {
		if doc.Paths[path] == nil {
			return nil, errors.New("required native route missing: " + path)
		}
	}

	if doc.Components.Schemas["OutputFormatJsonSchema"] == nil {
		return nil, errors.New("native structured output schema missing")
	}

	runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(ctx))

	stream, err := client.Subscribe(runtimeCtx)
	if err != nil {
		runtimeCancel()

		return nil, err
	}

	rt := &runtime{proc: proc, client: client, stream: stream, root: root, lock: lock, stderr: tail, cancel: runtimeCancel, done: make(chan struct{}), bindings: map[string]*binding{}}
	transferred = true

	go rt.pump(runtimeCtx)

	return rt, nil
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
