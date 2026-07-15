package opencodeacp

import (
	"context"
	"log/slog"
	"time"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const defaultAgentName = "acp-go-opencode"

type RuntimeResourceKind string

const (
	RuntimeResourceRuntime   RuntimeResourceKind = "runtime"
	RuntimeResourceSession   RuntimeResourceKind = "session"
	RuntimeResourcePrompt    RuntimeResourceKind = "prompt"
	RuntimeResourceDiscovery RuntimeResourceKind = "discovery"
)

type RuntimeProcessKind string

const (
	RuntimeProcessHomeLockSupervisor RuntimeProcessKind = "home_lock_supervisor"
	RuntimeProcessProviderDescendant RuntimeProcessKind = "provider_descendant"
)

type RuntimeStartupStage string

const (
	RuntimeStartupSpawn         RuntimeStartupStage = "spawn"
	RuntimeStartupReadiness     RuntimeStartupStage = "readiness"
	RuntimeStartupConfiguration RuntimeStartupStage = "configuration"
	RuntimeStartupSession       RuntimeStartupStage = "session"
)

// RuntimeResourceHooks lets an embedding worker account for native roots and
// adapter-created scratch roots in its worker-global permit pools. A nil hook
// selects the standalone, unbounded controller.
type RuntimeResourceHooks struct {
	AcquireNativeRoot      func(context.Context, RuntimeResourceKind) (func(), error)
	ReserveScratchRoot     func(context.Context, RuntimeResourceKind) (func(), error)
	ObserveProcess         func(context.Context, RuntimeProcessKind, int64)
	ObserveProcessSnapshot func(context.Context, RuntimeProcessKind, int)
	ObserveStartupStage    func(context.Context, RuntimeResourceKind, RuntimeStartupStage, time.Duration, error)
}

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
	// Home is the exclusive shared XDG root owned by this Agent runtime. Empty
	// creates one beneath ScratchDir.
	Home string
	// ScratchDir is the parent for a generated shared runtime root and transient
	// adapter scratch material. It is never a per-session OpenCode home.
	ScratchDir   string
	DefaultModel string
	Env          map[string]string

	Logger            *slog.Logger
	TracerProvider    trace.TracerProvider
	MeterProvider     metric.MeterProvider
	TextMapPropagator propagation.TextMapPropagator

	SessionStore            SessionStore
	SessionStoreLoadTimeout time.Duration
	ConcurrencyLimits       ConcurrencyLimits
	SeedFiles               map[string]string

	Pure                 bool
	QuestionTool         bool
	LogLevel             string
	NativeVersion        string
	HealthCheckTimeout   time.Duration
	TurnTimeout          time.Duration
	RuntimeResourceHooks RuntimeResourceHooks

	clientFactory func(context.Context, opencode.StartOptions) (opencode.Client, error)
}

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:               defaultAgentName,
		AgentTitle:              defaultAgentName,
		AgentVersion:            "0.1.0",
		SessionStoreLoadTimeout: 10 * time.Second,
		HealthCheckTimeout:      opencode.HealthCheckTimeout,
		NativeVersion:           "1.17.18",
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

// WithHome selects the exclusive shared OpenCode XDG root owned by the Agent.
func WithHome(path string) Option {
	return func(options *Options) {
		options.Home = path
	}
}

// WithScratchDir sets the parent directory for all ephemeral on-disk
// materialization, including a generated shared runtime root. An empty value
// uses the system temporary directory.
func WithScratchDir(dir string) Option {
	return func(options *Options) {
		options.ScratchDir = dir
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

// WithSessionStoreLoadTimeout bounds session store reads (load, list, and
// subkey enumeration). Store writes use a separate fixed bound.
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

// WithSeedFiles writes immutable bootstrap files into the shared OpenCode
// runtime config root before launching opencode serve. Keys are paths relative to
// <XDG_CONFIG_HOME>/opencode/ directory mapped to file contents; absolute
// paths, parent-directory escapes, and empty keys are rejected. A seeded
// opencode.json must not contain permission or MCP policy because those values
// are bound to native sessions and directory scopes respectively. The map is
// cloned like WithEnv.
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

// WithVersion selects the exact native version required by the sync
// event store. Version ranges and minimum-version fallbacks are unsupported.
func WithVersion(version string) Option {
	return func(options *Options) {
		options.NativeVersion = version
	}
}

func WithRuntimeResourceHooks(hooks RuntimeResourceHooks) Option {
	return func(options *Options) {
		options.RuntimeResourceHooks = hooks
	}
}

func WithOpenCodeHealthCheckTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.HealthCheckTimeout = timeout
	}
}

// WithTurnTimeout bounds how long a single prompt turn may run before the
// wrapper aborts the native turn and fails the prompt with a turn-failure error
// carrying cause "timeout". The default of 0 disables the deadline.
func WithTurnTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.TurnTimeout = timeout
	}
}
