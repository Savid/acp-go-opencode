package opencodeacp

import (
	"log/slog"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
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
		WithVersion("1.2.3"),
		WithOpenCodeHealthCheckTimeout(time.Second),
	})
	if opts.AgentName != "name" || opts.AgentTitle != "title" || opts.ExecutablePath != "opencode" ||
		opts.Env["A"] != "1" || !opts.Pure || !opts.QuestionTool || opts.SessionStore != store {
		t.Fatalf("options = %#v", opts)
	}
	if opts.Home != "/tmp/home" || opts.ScratchDir != "/tmp/scratch" {
		t.Fatalf("home/scratch options = %q / %q", opts.Home, opts.ScratchDir)
	}
	if opts.SeedFiles["opencode.json"] != `{"provider":{}}` {
		t.Fatalf("seed files = %#v", opts.SeedFiles)
	}
	seedSource["opencode.json"] = "mutated"
	if opts.SeedFiles["opencode.json"] != `{"provider":{}}` {
		t.Fatalf("WithSeedFiles did not clone the map: %#v", opts.SeedFiles)
	}
}
