package opencodeacp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestApplyOptions(t *testing.T) {
	if options := applyOptions(nil); options.HealthCheckTimeout != opencode.HealthCheckTimeout {
		t.Fatalf("default health timeout = %s, want %s", options.HealthCheckTimeout, opencode.HealthCheckTimeout)
	}

	store := NewInMemorySessionStore()
	seedSource := map[string]string{"opencode.json": `{"provider":{}}`}
	opts := applyOptions([]Option{
		WithLogger(slog.New(slog.DiscardHandler)),
		WithAgentName("name"),
		WithAgentTitle("title"),
		WithAgentVersion("version"),
		WithExecutablePath("opencode"),
		WithHome("/tmp/home"),
		WithScratchDir("/tmp/scratch"),
		WithDefaultModel("openai/gpt"),
		WithEnv(map[string]string{"A": "1"}),
		WithTracerProvider(tracenoop.NewTracerProvider()),
		WithMeterProvider(metricnoop.NewMeterProvider()),
		WithTextMapPropagator(propagation.TraceContext{}),
		WithSessionStore(store),
		WithSessionStoreLoadTimeout(time.Second),
		WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1, MaxConcurrentClientCalls: 3}),
		WithSeedFiles(seedSource),
		WithOpenCodePure(true),
		WithOpenCodeQuestionTool(true),
		WithOpenCodeLogLevel("INFO"),
		WithOpenCodeHealthCheckTimeout(time.Second),
	})
	if opts.AgentName != "name" || opts.AgentTitle != "title" || opts.ExecutablePath != "opencode" ||
		opts.Env["A"] != "1" || !opts.Pure || !opts.QuestionTool || opts.SessionStore != store {
		t.Fatalf("options = %#v", opts)
	}
	if scratch := (&Agent{options: opts}).scratchParent(); opts.Home != "/tmp/home" || scratch != "/tmp/scratch" {
		t.Fatalf("home/scratch options = %q / %q", opts.Home, scratch)
	}
	if opts.SeedFiles["opencode.json"] != `{"provider":{}}` {
		t.Fatalf("seed files = %#v", opts.SeedFiles)
	}
	seedSource["opencode.json"] = "mutated"
	if opts.SeedFiles["opencode.json"] != `{"provider":{}}` {
		t.Fatalf("WithSeedFiles did not clone the map: %#v", opts.SeedFiles)
	}
}

type optionTestAuthority struct{}

func (optionTestAuthority) NativeEnvironment() map[string]string            { return map[string]string{} }
func (optionTestAuthority) PrepareNativeTree(context.Context, string) error { return nil }
func (optionTestAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (optionTestAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return nil, errors.New("unused authority")
}

func TestWithHostAuthorityMarksTheManagedBoundary(t *testing.T) {
	authority := optionTestAuthority{}
	options := applyOptions([]Option{WithHostAuthority(authority)})
	if !options.hostAuthorityConfigured || options.HostAuthority != authority {
		t.Fatal("WithHostAuthority did not retain the authority boundary")
	}
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
