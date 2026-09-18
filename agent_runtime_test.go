package opencodeacp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/process"
	"github.com/savid/acp-go-core/wire"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func TestStartupRetriesStalledHealthRequest(t *testing.T) {
	t.Parallel()
	held := filepath.Join(t.TempDir(), "health-request")
	a := NewAgent(testOptions(t, WithEnv(map[string]string{fakeOpenCodeEnv: "1", fakeOpenCodeEnvHealthHold: held}))...)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := a.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	created, err := a.NewSession(ctx, wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	require.NotEmpty(t, created.SessionId)
	require.FileExists(t, held)
}

// A seed file the adapter cannot own is refused as an invalid option naming
// seedFiles, not reported as a native start failure.
func TestInvalidSeedFileRefusesTheStart(t *testing.T) {
	t.Parallel()

	a := NewAgent(testOptions(t, WithSeedFiles(map[string]string{"../outside.json": "{}"}))...)
	t.Cleanup(func() { _ = a.Close() })

	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	_, err = a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.Equal(t, -32602, requestErrorCode(t, err))

	data := requestErrorData(t, err)
	require.Equal(t, wire.VerdictUnsupported, data[wire.FieldError])
	require.Equal(t, "seedFiles", data[wire.FieldField])
}

func TestLockedHomeMustNotBeSeeded(t *testing.T) {
	home := t.TempDir()
	agent := NewAgent(testOptions(t, WithHome(home), WithSeedFiles(map[string]string{"seed-config.txt": "replacement"}))...)
	t.Cleanup(func() { _ = agent.Close() })
	env, err := agent.environment(nil, nil).Build()
	require.NoError(t, err)
	lookup := func(key string) (string, bool) { return process.Lookup(env, key) }
	config := opencode.ConfigDir(lookup)
	require.NoError(t, process.WriteSeedFiles(config, map[string]string{"seed-config.txt": "original"}))
	held, err := process.LockFile(filepath.Join(opencode.DataDir(lookup), ".acp-go-opencode.lock"))
	require.NoError(t, err)
	defer held.Close()
	_, err = agent.startRuntime(t.Context())
	require.Error(t, err)
	t.Logf("refused runtime: %v", err)
	content, err := os.ReadFile(filepath.Join(config, "seed-config.txt"))
	require.NoError(t, err)
	require.Equal(t, "original", string(content), "startup mutated the locked home's config before refusing its lock")
}
