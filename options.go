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
	HostAuthority  HostAuthority
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
	Logger                 *slog.Logger
	TracerProvider         trace.TracerProvider
	MeterProvider          metric.MeterProvider
	TextMapPropagator      propagation.TextMapPropagator

	SessionStore            SessionStore
	SessionStoreLoadTimeout time.Duration
	ConcurrencyLimits       ConcurrencyLimits
	SeedFiles               map[string]string
	ImageLimits             ImageLimits

	Pure               bool
	QuestionTool       bool
	LogLevel           string
	HealthCheckTimeout time.Duration
	TurnTimeout        time.Duration
	// PluginSeedDir is the adapter-owned cache of the npm tree OpenCode installs
	// for its plugin loader, copied into each new runtime root before launch.
	// Empty resolves to plugin-seed beneath the adapter's user cache directory.
	// Managed execution leaves the cache unused because executable identity is
	// owned by HostAuthority.
	PluginSeedDir string
	// PluginSeedDisabled turns the plugin seed cache off entirely: every cold
	// runtime root then waits for OpenCode's own install.
	PluginSeedDisabled      bool
	clientFactory           func(context.Context, opencode.StartOptions) (opencode.Client, error)
	implicitEnvironment     map[string]string
	hostAuthorityConfigured bool
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
		implicitEnvironment:     captureAmbientEnvironment(),
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

// WithHostAuthority routes native processes and tree ownership through authority.
func WithHostAuthority(authority HostAuthority) Option {
	return func(options *Options) {
		options.HostAuthority = authority
		options.hostAuthorityConfigured = true
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

func WithDefaultModel(model string) Option {
	return func(options *Options) {
		options.DefaultModel = model
	}
}

// WithEnv adds ordinary variables to the shared native runtime environment.
// Managed home, XDG, database, and config roots are rejected during agent
// initialization.
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
// paths, parent-directory escapes, and empty keys are rejected. All files are
// materialized before a managed runtime tree is prepared for host authority.
// A seeded opencode.json must not contain permission or MCP policy because those
// values are bound to native sessions and directory scopes respectively. The
// map is cloned like WithEnv.
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

func WithOpenCodeHealthCheckTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.HealthCheckTimeout = timeout
	}
}

// WithPluginSeedDir relocates the plugin seed cache. OpenCode installs its
// plugin loader with npm into every fresh runtime root, which costs minutes on
// a cold boot; the adapter keeps one copy of that install per native binary in
// this directory and copies it into each new root before launch. A cache with
// no entry for the running binary is filled by a priming launch — a throwaway
// runtime that performs the install, serves no session, and is torn down —
// before the runtime itself starts, in ordinary and managed execution alike.
// The default is plugin-seed beneath the adapter's directory in the user cache
// directory. The cache holds code OpenCode executes, so it must stay private to
// the user running the adapter.
func WithPluginSeedDir(dir string) Option {
	return func(options *Options) {
		options.PluginSeedDir = dir
	}
}

// WithPluginSeed enables or disables the plugin seed cache. It is enabled by
// default; disabling it leaves every cold runtime root to OpenCode's own
// install and writes nothing beneath the seed directory.
func WithPluginSeed(enabled bool) Option {
	return func(options *Options) {
		options.PluginSeedDisabled = !enabled
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
