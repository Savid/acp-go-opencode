package opencodeacp

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
)

func carrierOptions(meta map[string]any) map[string]any {
	return map[string]any{opencodeMetaKey: map[string]any{metaOptionsKey: meta}}
}

func TestSessionEnvMetaAcceptsBothShapesAndPreservesValues(t *testing.T) {
	meta, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{
		metaEnvKey: map[string]any{"WAGIE_API_TOKEN": "bearer", "CLEARED": ""},
	}))
	require.NoError(t, err)
	require.True(t, meta.EnvSet)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer", "CLEARED": ""}, meta.Env)

	// An in-process host hands the map over already typed.
	meta, err = sessionMetaFromVendorOptions(carrierOptions(map[string]any{
		metaEnvKey: map[string]string{"WAGIE_API_TOKEN": "bearer"},
	}))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer"}, meta.Env)

	// An empty map is a value, not an omission: it clears the environment.
	meta, err = sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: map[string]any{}}))
	require.NoError(t, err)
	require.True(t, meta.EnvSet)
	require.Empty(t, meta.Env)

	// An absent option leaves the recorded value in place.
	meta, err = sessionMetaFromVendorOptions(carrierOptions(map[string]any{}))
	require.NoError(t, err)
	require.False(t, meta.EnvSet)
	require.Nil(t, meta.Env)
}

func TestSessionEnvMetaRefusesEveryInvalidEntry(t *testing.T) {
	managedOpenCodeRoot, managedXDGRoot := managedEnvOpenCodeConfigDir, managedEnvXDGDataHome

	tests := []struct {
		name  string
		value any
		field string
	}{
		{"not an object", "WAGIE_API_TOKEN=bearer", envOptionPath},
		{"value is not a string", map[string]any{"WAGIE_API_TOKEN": 1}, envOptionPath + ".WAGIE_API_TOKEN"},
		{"value is null", map[string]any{"WAGIE_API_TOKEN": nil}, envOptionPath + ".WAGIE_API_TOKEN"},
		{"empty name", map[string]any{"": "bearer"}, envOptionPath + "."},
		{"name carries an equals sign", map[string]any{"A=B": "bearer"}, envOptionPath + ".A=B"},
		{"name carries a NUL", map[string]any{"A\x00B": "bearer"}, envOptionPath + ".A\x00B"},
		{"raw PATH", map[string]any{envPathKey: "/attacker/bin"}, envOptionPath + "." + envPathKey},
		{"managed OpenCode root", map[string]any{managedOpenCodeRoot: "/elsewhere"}, envOptionPath + "." + managedOpenCodeRoot},
		{"managed XDG root", map[string]any{managedXDGRoot: "/elsewhere"}, envOptionPath + "." + managedXDGRoot},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{metaEnvKey: test.value}))
			require.Equal(t, unsupportedField(test.field), err)
		})
	}
}

// A session may not own the search path, and only the platform decides which
// spellings address it: Windows resolves environment names case-insensitively,
// so Path is PATH there and an ordinary variable of its own everywhere else.
func TestSessionEnvMetaRefusesThePathVariableByEnvironmentIdentity(t *testing.T) {
	for _, spelling := range []string{"path", "Path", "PaTh"} {
		meta, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{
			metaEnvKey: map[string]any{spelling: "/attacker/bin"},
		}))

		if runtime.GOOS == "windows" {
			require.Equal(t, unsupportedField(envOptionPath+"."+spelling), err)

			continue
		}

		require.NoError(t, err)
		require.Equal(t, map[string]string{spelling: "/attacker/bin"}, meta.Env)
	}
}

func TestLifecycleMetaAllowsTheCarrierOptionsOnly(t *testing.T) {
	_, err := sessionMetaFromVendorOptions(carrierOptions(map[string]any{
		metaEnvKey:           map[string]any{"WAGIE_API_TOKEN": "bearer"},
		metaExtraPathDirsKey: []any{"/session/bin"},
	}))
	require.NoError(t, err)

	_, err = sessionMetaFromVendorOptions(carrierOptions(map[string]any{"envs": map[string]any{}}))
	require.Equal(t, unsupportedField("_meta.opencode.options.envs"), err)
}

func TestCarrierFromMetaReplacesOnlyThePresentHalf(t *testing.T) {
	recorded := newSessionCarrier(map[string]string{"WAGIE_API_TOKEN": "old"}, []string{"/old/bin"})

	unchanged := carrierFromMeta(sessionMeta{}, recorded)
	require.Equal(t, recorded, unchanged)

	rotated := carrierFromMeta(sessionMeta{
		EnvSet: true, Env: map[string]string{"WAGIE_API_TOKEN": "new"},
		ExtraPathDirsSet: true, ExtraPathDirs: []string{"/new/bin"},
	}, recorded)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "new"}, rotated.Env)
	require.Equal(t, []string{"/new/bin"}, rotated.ExtraPathDirs)
	require.NotEqual(t, recorded, rotated, "the recorded carrier must not be mutated in place")
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "old"}, recorded.Env)

	cleared := carrierFromMeta(sessionMeta{EnvSet: true, ExtraPathDirsSet: true}, recorded)
	require.Empty(t, cleared.Env)
	require.Empty(t, cleared.ExtraPathDirs)

	// The clone is deep: a caller that keeps one cannot reach into the other.
	source := newSessionCarrier(map[string]string{"A": "1"}, []string{"/bin"})
	copied := source.clone()
	copied.Env["A"] = "2"
	copied.ExtraPathDirs[0] = "/other"
	require.Equal(t, map[string]string{"A": "1"}, source.Env)
	require.Equal(t, []string{"/bin"}, source.ExtraPathDirs)
	require.NotEqual(t, source, copied)
}

func TestCarrierScopeOptionsClone(t *testing.T) {
	carrier := newSessionCarrier(map[string]string{"A": "1"}, []string{"/bin"})
	options := carrier.scopeOptions("/cwd", []opencode.MCPServerConfig{{Name: "tools"}})
	require.Equal(t, "/cwd", options.Directory)
	options.Env["A"] = "2"
	options.ExtraPathDirs[0] = "/other"
	require.Equal(t, map[string]string{"A": "1"}, carrier.Env)
	require.Equal(t, []string{"/bin"}, carrier.ExtraPathDirs)
}

// TestConcurrentSessionsCarryDistinctBearersAndDirectories is the isolation
// proof: one shared runtime, two live sessions, and neither one's values reach
// the other's addressed native scope.
func TestConcurrentSessionsCarryDistinctBearersAndDirectories(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()

	var created atomic.Int64

	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession(fmt.Sprintf("native-%d", created.Add(1))), nil
	}
	client.agents = []opencode.NativeAgent{{Name: "build"}}

	var factoryCalls atomic.Int64

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			factoryCalls.Add(1)

			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	start := func(token string, dirs ...string) acp.SessionId {
		response, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": token}),
			WithOpenCodeExtraPathDirs(dirs...),
		))))
		require.NoError(t, err)

		return response.SessionId
	}

	first := start("bearer-one", "/one", "/shared")
	second := start("bearer-two", "/two", "/shared")
	require.NotEqual(t, first, second)
	require.EqualValues(t, 1, factoryCalls.Load(), "one runtime serves both sessions")

	scopes := client.scopes()
	require.Len(t, scopes, 2)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-one"}, scopes[0].Env)
	require.Equal(t, []string{"/one", "/shared"}, scopes[0].ExtraPathDirs)
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-two"}, scopes[1].Env)
	require.Equal(t, []string{"/two", "/shared"}, scopes[1].ExtraPathDirs)

	require.NoError(t, agent.Close())
}

// TestRebindRotatesOneSessionAndLeavesItsPeerAlone covers what a rebind owes a
// peer: a live peer keeps its bearer while the rebound session's previous
// bearer disappears entirely.
func TestRebindRotatesOneSessionAndLeavesItsPeerAlone(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()

	var created atomic.Int64

	client.createSessionFunc = func(context.Context, string) (opencode.NativeSession, error) {
		return testNativeSession(fmt.Sprintf("native-%d", created.Add(1))), nil
	}
	client.getSession = testNativeSession("native-1")
	client.agents = []opencode.NativeAgent{{Name: "build"}}

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	rotatingCwd := t.TempDir()

	rotating, err := agent.NewSession(ctx, NewSessionRequest(rotatingCwd, WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "bearer-one", "OPERATION": "first"}),
		WithOpenCodeExtraPathDirs("/first/bin"),
	))))
	require.NoError(t, err)

	_, err = agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "bearer-peer"}),
		WithOpenCodeExtraPathDirs("/peer/bin"),
	))))
	require.NoError(t, err)

	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: rotating.SessionId})
	require.NoError(t, err)

	_, err = agent.ResumeSession(ctx, ResumeSessionRequest(rotating.SessionId, rotatingCwd,
		WithSessionOpenCodeOptions(NewOpenCodeOptions(
			WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "bearer-rotated"}),
			WithOpenCodeExtraPathDirs("/rotated/bin"),
		)),
	))
	require.NoError(t, err)

	scopes := client.scopes()
	require.Len(t, scopes, 3)

	rebound := scopes[2]
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-rotated"}, rebound.Env)
	require.Equal(t, []string{"/rotated/bin"}, rebound.ExtraPathDirs)
	require.NotContains(t, rebound.Env, "OPERATION", "the replaced environment must not retain a stale key")

	peer := scopes[1]
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-peer"}, peer.Env)
	require.Equal(t, []string{"/peer/bin"}, peer.ExtraPathDirs)

	require.NoError(t, agent.Close())
}

// TestRecoveredSessionKeepsItsCarrier proves a runtime restart rebinds the
// session to the carrier it was admitted under rather than to an empty one.
func TestRecoveredSessionKeepsItsCarrier(t *testing.T) {
	ctx := context.Background()
	first := newFakeOpenCodeClient()
	first.createSession = testNativeSession("native-first")
	first.agents = []opencode.NativeAgent{{Name: "build"}}
	second := newFakeOpenCodeClient()
	second.xdg = first.xdg
	second.getSession = testNativeSession("native-first")
	second.agents = []opencode.NativeAgent{{Name: "build"}}

	var (
		factoryCalls atomic.Int64
		startedMu    sync.Mutex
		started      []opencode.StartOptions
	)

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(_ context.Context, start opencode.StartOptions) (opencode.Client, error) {
			startedMu.Lock()
			started = append(started, start)
			startedMu.Unlock()

			if factoryCalls.Add(1) == 1 {
				return first, nil
			}

			return second, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "bearer-recovered"}),
		WithOpenCodeExtraPathDirs("/session/bin"),
	))))
	require.NoError(t, err)
	close(first.runtimeExited)

	response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, "recovery-turn", "continue after restart"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	startedMu.Lock()
	require.Len(t, started, 2)

	for _, options := range started {
		require.NotContains(t, options.Env, "WAGIE_API_TOKEN",
			"a session bearer must never reach the shared runtime process")
	}
	startedMu.Unlock()

	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-recovered"}, second.scopes()[0].Env)
	require.Equal(t, []string{"/session/bin"}, second.scopes()[0].ExtraPathDirs)
	require.NoError(t, agent.Close())
}

// TestSecondTurnKeepsTheCarrier proves the carrier is a property of the session
// rather than of the turn that installed it.
func TestSecondTurnKeepsTheCarrier(t *testing.T) {
	ctx := context.Background()
	client := newFakeOpenCodeClient()
	client.createSession = testNativeSession("native-turns")
	client.agents = []opencode.NativeAgent{{Name: "build"}}

	agent := NewAgent(WithHome(t.TempDir()), func(options *Options) {
		options.clientFactory = func(context.Context, opencode.StartOptions) (opencode.Client, error) {
			return client, nil
		}
	})
	agent.setAgentClient(newRecordingAgentClient())

	created, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionOpenCodeOptions(NewOpenCodeOptions(
		WithOpenCodeEnv(map[string]string{"WAGIE_API_TOKEN": "bearer-turns"}),
		WithOpenCodeExtraPathDirs("/turns/bin"),
	))))
	require.NoError(t, err)

	for _, nonce := range []string{"turn-one", "turn-two"} {
		response, err := agent.Prompt(ctx, TextPromptRequest(created.SessionId, nonce, "work"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	}

	snapshot := agent.sessions[created.SessionId].snapshot()
	require.Equal(t, map[string]string{"WAGIE_API_TOKEN": "bearer-turns"}, snapshot.carrier.Env)
	require.Equal(t, []string{"/turns/bin"}, snapshot.carrier.ExtraPathDirs)
	require.NoError(t, agent.Close())
}

func TestCarrierBearerIsRedactedFromTheDurableSnapshot(t *testing.T) {
	agent := NewAgent()
	client := newFakeOpenCodeClient()
	agent.runtime = client
	member := testSession(t, agent, client)
	member.carrier = newSessionCarrier(map[string]string{
		"WAGIE_API_TOKEN": "bearer-secret",
		"OPERATION_NAME":  "ordinary",
	}, nil)
	agent.sessions[member.id] = member

	needles := agent.graphSecretNeedles([]*session{member})
	require.Contains(t, needles, "bearer-secret")
	require.NotContains(t, needles, "ordinary")
}
