package opencodeacp

import (
	"context"
	"log/slog"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"

	"github.com/savid/acp-go-opencode/internal/defaults"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const defaultAgentName = "acp-go-opencode"

// Option configures the OpenCode ACP agent.
type Option func(*Options)

// ConcurrencyLimits bounds work accepted by one Agent.
type ConcurrencyLimits struct {
	MaxActiveSessions        int
	MaxConcurrentClientCalls int
}

// Options configures the ACP agent process and OpenCode sessions it starts.
type Options struct {
	AgentName    string
	AgentTitle   string
	AgentVersion string

	ExecutablePath string
	Home           string
	DefaultModel   string
	Env            map[string]string

	Logger            *slog.Logger
	TracerProvider    trace.TracerProvider
	MeterProvider     metric.MeterProvider
	TextMapPropagator propagation.TextMapPropagator

	SessionStore            SessionStore
	SessionStoreLoadTimeout time.Duration
	ConcurrencyLimits       ConcurrencyLimits
	SeedFiles               map[string]string

	Pure               bool
	QuestionTool       bool
	LogLevel           string
	MinimumVersion     string
	HealthCheckTimeout time.Duration

	clientFactory func(context.Context, opencode.StartOptions) (opencode.Client, error)
}

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:               defaultAgentName,
		AgentTitle:              defaultAgentName,
		AgentVersion:            "0.1.0",
		SessionStoreLoadTimeout: 10 * time.Second,
		HealthCheckTimeout:      defaults.HealthCheckTimeout,
		MinimumVersion:          "1.17.13",
		clientFactory:           opencode.StartServer,
	}
	for _, opt := range opts {
		opt(&options)
	}

	return options
}

func WithLogger(logger *slog.Logger) Option {
	return func(options *Options) {
		options.Logger = logger
	}
}

func WithAgentName(name string) Option {
	return func(options *Options) {
		options.AgentName = name
	}
}

func WithAgentTitle(title string) Option {
	return func(options *Options) {
		options.AgentTitle = title
	}
}

func WithAgentVersion(version string) Option {
	return func(options *Options) {
		options.AgentVersion = version
	}
}

func WithExecutablePath(path string) Option {
	return func(options *Options) {
		options.ExecutablePath = path
	}
}

// WithHome sets the parent root under which isolated per-session OpenCode XDG
// data, config, cache, and state directories are created.
func WithHome(path string) Option {
	return func(options *Options) {
		options.Home = path
	}
}

func WithDefaultModel(model string) Option {
	return func(options *Options) {
		options.DefaultModel = model
	}
}

func WithEnv(env map[string]string) Option {
	return func(options *Options) {
		options.Env = cloneStringMap(env)
	}
}

func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(options *Options) {
		options.TracerProvider = provider
	}
}

func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(options *Options) {
		options.MeterProvider = provider
	}
}

func WithTextMapPropagator(propagator propagation.TextMapPropagator) Option {
	return func(options *Options) {
		options.TextMapPropagator = propagator
	}
}

func WithSessionStore(store SessionStore) Option {
	return func(options *Options) {
		options.SessionStore = store
	}
}

func WithSessionStoreLoadTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.SessionStoreLoadTimeout = timeout
	}
}

func WithConcurrencyLimits(limits ConcurrencyLimits) Option {
	return func(options *Options) {
		options.ConcurrencyLimits = limits
	}
}

// WithSeedFiles writes files into each session's isolated OpenCode config root
// before launching opencode serve, so the native server reads them as its own
// config. Keys are paths relative to the per-session
// <XDG_CONFIG_HOME>/opencode/ directory mapped to file contents; absolute
// paths, parent-directory escapes, and empty keys are rejected with the uniform
// unsupported error. The seeded opencode.json is deep-merged with the wrapper's
// managed $schema and permission keys (the wrapper wins for those keys, the
// seed supplies the rest, e.g. a custom provider block); every other seeded
// file is written verbatim. The map is cloned like WithEnv.
func WithSeedFiles(files map[string]string) Option {
	return func(options *Options) {
		options.SeedFiles = cloneStringMap(files)
	}
}

func WithOpenCodePure(enabled bool) Option {
	return func(options *Options) {
		options.Pure = enabled
	}
}

func WithOpenCodeQuestionTool(enabled bool) Option {
	return func(options *Options) {
		options.QuestionTool = enabled
	}
}

func WithOpenCodeLogLevel(level string) Option {
	return func(options *Options) {
		options.LogLevel = level
	}
}

func WithOpenCodeMinimumVersion(version string) Option {
	return func(options *Options) {
		options.MinimumVersion = version
	}
}

func WithOpenCodeHealthCheckTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.HealthCheckTimeout = timeout
	}
}
