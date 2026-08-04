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

// RuntimeContainmentMode reports the selected native process boundary.
type RuntimeContainmentMode string

const (
	// RuntimeContainmentAuthoritative identifies a platform proof boundary.
	RuntimeContainmentAuthoritative RuntimeContainmentMode = "authoritative"
	// RuntimeContainmentBestEffort identifies explicitly accepted Darwin process-group containment.
	RuntimeContainmentBestEffort RuntimeContainmentMode = "best_effort"
	// RuntimeContainmentUnavailable identifies a platform with no selected boundary.
	RuntimeContainmentUnavailable RuntimeContainmentMode = "unavailable"
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
	ObserveContainment     func(context.Context, RuntimeContainmentMode)
}

// Option configures the OpenCode ACP agent.
type Option func(*Options)

// ProcessIsolation defines the complete operating-system identity and base
// environment inherited by every native OpenCode process.
type ProcessIsolation struct {
	UID             uint32
	GID             uint32
	BaseEnvironment map[string]string
}

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
	ScratchDir string
	// InputHandoffRoot is the only directory a prompt image may be read from.
	// Empty rejects the handoff input form outright.
	InputHandoffRoot string
	// ProviderAuthRoot is the durable host-owned directory holding the
	// values-free provider-auth ledger. Empty leaves every provider-auth method
	// unadvertised.
	ProviderAuthRoot string
	// ProviderAuthDirectHome names a canonical native home an account-level
	// provider-auth leg may read or clear. OpenCode removes a credential with a
	// scoped per-provider call and has no such leg, so a configured value is
	// rejected at session start.
	ProviderAuthDirectHome string
	DefaultModel           string
	Env                    map[string]string
	ProcessIsolation       *ProcessIsolation

	Logger            *slog.Logger
	TracerProvider    trace.TracerProvider
	MeterProvider     metric.MeterProvider
	TextMapPropagator propagation.TextMapPropagator

	SessionStore            SessionStore
	SessionStoreLoadTimeout time.Duration
	ConcurrencyLimits       ConcurrencyLimits
	SeedFiles               map[string]string
	ImageLimits             ImageLimits

	Pure                        bool
	QuestionTool                bool
	LogLevel                    string
	HealthCheckTimeout          time.Duration
	TurnTimeout                 time.Duration
	RuntimeResourceHooks        RuntimeResourceHooks
	DarwinBestEffortContainment bool

	clientFactory func(context.Context, opencode.StartOptions) (opencode.Client, error)
}

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:               defaultAgentName,
		AgentTitle:              defaultAgentName,
		AgentVersion:            "0.1.0",
		SessionStoreLoadTimeout: 10 * time.Second,
		HealthCheckTimeout:      opencode.HealthCheckTimeout,
		ImageLimits:             defaultImageLimits(),
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

// WithProcessIsolation requires every native process to run as the supplied
// uid/gid with no supplementary groups. BaseEnvironment is the complete native
// environment base; the adapter never overlays os.Environ.
func WithProcessIsolation(isolation ProcessIsolation) Option {
	return func(options *Options) {
		cloned := isolation
		cloned.BaseEnvironment = cloneStringMap(isolation.BaseEnvironment)
		options.ProcessIsolation = &cloned
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

// WithInputHandoffRoot sets the absolute directory a prompt image may be read
// from when it arrives in the handoff form: an image block with empty data, a
// file URI, and a digest envelope. It is a read root only — the wrapper never
// writes, moves, or deletes anything beneath it, and it materializes nothing,
// so it carries no scratch semantics. Unset rejects every handoff-form block.
func WithInputHandoffRoot(dir string) Option {
	return func(options *Options) {
		options.InputHandoffRoot = dir
	}
}

// WithProviderAuthRoot sets the absolute durable directory holding the
// values-free provider-auth ledger. It sits outside session scratch, outlives
// every session and native generation, and carries no config or
// auth-resolution semantics. Unset — or set alongside no Home — leaves every
// provider-auth method unadvertised.
func WithProviderAuthRoot(path string) Option {
	return func(options *Options) {
		options.ProviderAuthRoot = path
	}
}

// WithProviderAuthDirectHome names the canonical native home an operator
// consents to an account-level provider-auth leg reading or clearing. OpenCode
// removes a credential through a scoped per-provider call, so it has no leg to
// gate and rejects any configured value at session start.
func WithProviderAuthDirectHome(path string) Option {
	return func(options *Options) {
		options.ProviderAuthDirectHome = path
	}
}

// WithDarwinBestEffortContainment explicitly accepts Darwin process-group containment.
func WithDarwinBestEffortContainment() Option {
	return func(options *Options) {
		options.DarwinBestEffortContainment = true
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
