package opencodeacp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

type authorityTrace struct {
	mu       sync.Mutex
	events   []string
	prepared map[string]bool
	hidden   map[string]string
	process  *authorityTraceProcess
	startErr error
	hideTree bool
}

func newAuthorityTrace() *authorityTrace {
	authority := &authorityTrace{prepared: map[string]bool{}, hidden: map[string]string{}}
	authority.process = &authorityTraceProcess{
		authority: authority,
		terminal:  make(chan struct{}),
		stdin:     &authorityTraceInput{authority: authority},
	}

	return authority
}

func (a *authorityTrace) record(event string) {
	a.mu.Lock()
	a.events = append(a.events, event)
	a.mu.Unlock()
}

func (a *authorityTrace) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.events...)
}

func (a *authorityTrace) NativeEnvironment() map[string]string {
	a.record("environment")

	return map[string]string{"PATH": "/authority/bin", "AUTHORITY_CANARY": "present"}
}

func (a *authorityTrace) PrepareNativeTree(_ context.Context, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.hideTree {
		hidden := path + ".authority"
		if err := os.Rename(path, hidden); err != nil {
			return err
		}

		a.hidden[path] = hidden
	}

	a.events = append(a.events, "prepare:"+filepath.Base(path))
	a.prepared[path] = true

	return nil
}

func (a *authorityTrace) ReclaimNativeTree(_ context.Context, path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.process.settled {
		return errors.New("reclaim before terminal wait")
	}
	if !a.prepared[path] {
		return errors.New("reclaim of unprepared tree")
	}

	if hidden := a.hidden[path]; hidden != "" {
		if err := os.Rename(hidden, path); err != nil {
			return err
		}

		delete(a.hidden, path)
	}

	a.events = append(a.events, "reclaim:"+filepath.Base(path))
	delete(a.prepared, path)

	return nil
}

func (a *authorityTrace) StartNative(_ context.Context, request NativeRequest) (NativeProcess, error) {
	if a.startErr != nil {
		a.record("start-refused")

		return nil, a.startErr
	}
	a.mu.Lock()
	prepared := a.prepared[request.WorkingDirectory]
	a.events = append(a.events, "start:"+request.Executable)
	a.mu.Unlock()
	if !prepared {
		return nil, errors.New("native start outside prepared tree")
	}

	return a.process, nil
}

type authorityTraceInput struct {
	authority *authorityTrace
	closed    bool
}

func (w *authorityTraceInput) Write(value []byte) (int, error) { return len(value), nil }
func (w *authorityTraceInput) Close() error {
	if !w.closed {
		w.closed = true
		w.authority.record("protocol-close")
	}

	return nil
}

type authorityTraceProcess struct {
	authority *authorityTrace
	stdin     *authorityTraceInput
	terminal  chan struct{}
	revoke    sync.Once
	settled   bool
}

func (p *authorityTraceProcess) Stdin() io.WriteCloser { return p.stdin }
func (*authorityTraceProcess) Stdout() io.ReadCloser   { return io.NopCloser(&emptyReader{}) }
func (*authorityTraceProcess) Stderr() io.ReadCloser   { return io.NopCloser(&emptyReader{}) }

func (p *authorityTraceProcess) Wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-p.terminal:
		p.authority.mu.Lock()
		p.settled = true
		p.authority.events = append(p.authority.events, "wait:terminal")
		p.authority.mu.Unlock()

		return NativeResult{Revoked: true}, nil
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (p *authorityTraceProcess) Revoke(context.Context) error {
	p.revoke.Do(func() {
		p.authority.record("revoke")
		close(p.terminal)
	})

	return nil
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func runManagedTrace(t *testing.T, agent *Agent, authority *authorityTrace, remove bool) ([]string, string, error) {
	t.Helper()

	authority.mu.Lock()
	authority.events = nil
	authority.mu.Unlock()

	var root string
	agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		root = options.Root
		require.NotNil(t, options.PrepareTree)
		require.NotNil(t, options.ReclaimTree)
		require.NotNil(t, options.StartProcess)
		require.Equal(t, "present", options.NativeEnvironment()["AUTHORITY_CANARY"])
		require.NoError(t, options.PrepareTree(ctx, root))

		process, err := options.StartProcess(ctx, "opencode", []string{"serve"}, []string{"AUTHORITY_CANARY=present"}, root)
		if err != nil {
			return nil, err
		}
		require.NoError(t, process.Input.Close())
		require.NoError(t, process.Stop(ctx))
		_, err = process.Await(ctx)
		if err != nil {
			return nil, err
		}
		if err := options.ReclaimTree(ctx, root); err != nil {
			return nil, err
		}
		if remove {
			authority.record("remove:" + filepath.Base(root))
			if err := os.RemoveAll(root); err != nil {
				return nil, err
			}
		}

		return newFakeOpenCodeClient(), nil
	}

	_, err := agent.startSharedRuntime(context.Background())

	return authority.snapshot(), root, err
}

func TestHostAuthorityManagedLaunchTrace(t *testing.T) {
	authority := newAuthorityTrace()
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, _, err := runManagedTrace(t, agent, authority, false)
	require.NoError(t, err)
	require.Equal(t, []string{
		"environment", "prepare:acp-go-opencode-runtime-", "start:opencode",
		"protocol-close", "revoke", "wait:terminal", "reclaim:acp-go-opencode-runtime-",
	}, normalizeAuthorityTrace(events))
	require.Contains(t, events[1], "prepare:")
	require.Contains(t, events[2], "start:")
	require.Contains(t, events[5], "wait:")
	require.Contains(t, events[6], "reclaim:")
}

func TestHostAuthorityPreparedTreeExclusivity(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	var root string
	agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		root = options.Root
		require.NoError(t, options.PrepareTree(ctx, root))
		_, statErr := os.Stat(root)
		require.ErrorIs(t, statErr, os.ErrNotExist)

		process, err := options.StartProcess(ctx, "opencode", []string{"serve"}, []string{"PATH=/authority/bin"}, root)
		require.NoError(t, err)
		_, statErr = os.Stat(root)
		require.ErrorIs(t, statErr, os.ErrNotExist)
		require.NoError(t, process.Input.Close())
		require.NoError(t, process.Stop(ctx))
		_, err = process.Await(ctx)
		require.NoError(t, err)
		require.NoError(t, options.ReclaimTree(ctx, root))

		return newFakeOpenCodeClient(), nil
	}
	_, err := agent.startSharedRuntime(context.Background())
	require.NoError(t, err)
	_, statErr := os.Stat(root)
	require.NoError(t, statErr)
	authority.mu.Lock()
	defer authority.mu.Unlock()
	require.Empty(t, authority.prepared, "the adapter retained a prepared tree after reclaim")
	require.NotContains(t, authority.prepared, opencode.ControlRootForXDG(root),
		"the runtime-owned control lock entered the prepared tree set")
}

func TestHostAuthorityReclaimPrecedesRemoval(t *testing.T) {
	authority := newAuthorityTrace()
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, root, err := runManagedTrace(t, agent, authority, true)
	require.NoError(t, err)
	normalized := normalizeAuthorityTrace(events)
	require.Less(t, indexOfAuthorityEvent(normalized, "wait:terminal"), indexOfAuthorityEvent(normalized, "reclaim:acp-go-opencode-runtime-"))
	require.Less(t, indexOfAuthorityEvent(normalized, "reclaim:acp-go-opencode-runtime-"), indexOfAuthorityEvent(normalized, "remove:acp-go-opencode-runtime-"))
	_, statErr := os.Stat(root)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestHostAuthorityForcedRevokeLeavesSQLiteWALReopenable(t *testing.T) {
	authority := newAuthorityTrace()
	authority.hideTree = true
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))

	agent.options.clientFactory = func(ctx context.Context, options opencode.StartOptions) (opencode.Client, error) {
		database := filepath.Join(options.Root, "data", "opencode.db")
		wal := database + "-wal"
		require.NoError(t, os.MkdirAll(filepath.Dir(database), 0o700))
		require.NoError(t, os.WriteFile(database, []byte("SQLite format 3\x00"), 0o600))
		require.NoError(t, os.WriteFile(wal, []byte("wal-before-revoke"), 0o600))
		require.NoError(t, options.PrepareTree(ctx, options.Root))

		process, err := options.StartProcess(ctx, "opencode", []string{"serve"}, nil, options.Root)
		require.NoError(t, err)
		require.NoError(t, process.Input.Close())
		require.NoError(t, process.Stop(ctx))
		_, err = process.Await(ctx)
		require.NoError(t, err)
		require.NoError(t, options.ReclaimTree(ctx, options.Root))

		for _, path := range []string{database, wal} {
			file, openErr := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
			require.NoError(t, openErr)
			_, writeErr := file.Write([]byte("reopened"))
			require.NoError(t, writeErr)
			require.NoError(t, file.Sync())
			require.NoError(t, file.Close())
		}

		return newFakeOpenCodeClient(), nil
	}

	_, err := agent.startSharedRuntime(context.Background())
	require.NoError(t, err)
}

func TestHostAuthorityNoOrdinaryFallback(t *testing.T) {
	authority := newAuthorityTrace()
	authority.startErr = errors.New("authority refused")
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()))
	events, _, err := runManagedTrace(t, agent, authority, false)
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.Contains(t, events, "start-refused")

	called := false
	agent = NewAgent(WithHostAuthority(nil))
	agent.options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
		called = true

		return nil, errors.New("unexpected factory call")
	}
	_, err = agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.False(t, called)
}

func normalizeAuthorityTrace(events []string) []string {
	result := make([]string, len(events))
	for index, event := range events {
		if len(event) >= len("prepare:acp-go-opencode-runtime-") && event[:len("prepare:acp-go-opencode-runtime-")] == "prepare:acp-go-opencode-runtime-" {
			result[index] = "prepare:acp-go-opencode-runtime-"

			continue
		}
		if len(event) >= len("reclaim:acp-go-opencode-runtime-") && event[:len("reclaim:acp-go-opencode-runtime-")] == "reclaim:acp-go-opencode-runtime-" {
			result[index] = "reclaim:acp-go-opencode-runtime-"

			continue
		}
		if len(event) >= len("remove:acp-go-opencode-runtime-") && event[:len("remove:acp-go-opencode-runtime-")] == "remove:acp-go-opencode-runtime-" {
			result[index] = "remove:acp-go-opencode-runtime-"

			continue
		}
		result[index] = event
	}

	return result
}

func indexOfAuthorityEvent(events []string, target string) int {
	for index, event := range events {
		if event == target {
			return index
		}
	}

	return -1
}
