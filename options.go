package opencodeacp

import (
	"io"
)

const (
	otelExporterOTLPEndpointKey = "OTEL_EXPORTER_OTLP_ENDPOINT"
	otelExporterOTLPProtocolKey = "OTEL_EXPORTER_OTLP_PROTOCOL"
	otelResourceAttributesKey   = "OTEL_RESOURCE_ATTRIBUTES"

	defaultTelemetryProtocol        = "http/protobuf"
	defaultTelemetryMetricsInterval = "1000"
	defaultTelemetryDisableTraces   = "session"
)

// Option configures the OpenCode ACP wrapper.
type Option func(*Options)

// TelemetryOptions configures OpenCode-native telemetry environment variables.
type TelemetryOptions struct {
	// Enabled sets OPENCODE_ENABLE_TELEMETRY=1 when true.
	Enabled bool
	// Endpoint sets OPENCODE_OTLP_ENDPOINT.
	Endpoint string
	// Protocol sets OPENCODE_OTLP_PROTOCOL.
	Protocol string
	// MetricsInterval sets OPENCODE_OTLP_METRICS_INTERVAL.
	MetricsInterval string
	// ResourceAttributes sets OPENCODE_RESOURCE_ATTRIBUTES.
	ResourceAttributes string
	// DisableLogs sets OPENCODE_DISABLE_LOGS=1 when true.
	DisableLogs bool
	// DisableTraces sets OPENCODE_DISABLE_TRACES to an OpenCode trace filter,
	// for example "session".
	DisableTraces string
	// Traceparent sets OPENCODE_TRACEPARENT.
	Traceparent string
	// Tracestate sets OPENCODE_TRACESTATE.
	Tracestate string
}

// Options configures the launched `opencode acp` process.
type Options struct {
	// OpenCodePath is the OpenCode executable path. If empty, PATH is searched.
	OpenCodePath string
	// Cwd is passed to `opencode acp --cwd` and used as the child process
	// working directory when non-empty.
	Cwd string
	// Pure runs OpenCode without external plugins.
	Pure bool
	// PrintLogs passes OpenCode's --print-logs flag.
	PrintLogs bool
	// LogLevel passes OpenCode's --log-level flag when non-empty.
	LogLevel string
	// Hostname passes OpenCode's --hostname flag when non-empty.
	Hostname string
	// Port passes OpenCode's --port flag when set. Use WithPort(0) to ask
	// OpenCode to allocate a port.
	Port *int
	// MDNS passes OpenCode's --mdns flag.
	MDNS bool
	// MDNSDomain passes OpenCode's --mdns-domain flag when non-empty.
	MDNSDomain string
	// CORS passes one --cors flag for each configured domain.
	CORS []string
	// QuestionTool sets OPENCODE_ENABLE_QUESTION_TOOL when non-nil.
	QuestionTool *bool
	// Telemetry configures OpenCode-native telemetry environment variables.
	Telemetry TelemetryOptions
	// IsolateTempDir sets TMPDIR, TMP, and TEMP to a wrapper-owned temporary
	// directory that is removed when the process exits.
	IsolateTempDir bool
	// Env is merged into the launched process environment.
	Env map[string]string
	// Stderr receives OpenCode stderr. If nil, stderr is discarded.
	Stderr io.Writer
	// ExtraArgs are appended after wrapper-managed `opencode acp` flags.
	ExtraArgs []string
}

func applyOptions(opts []Option) Options {
	options := Options{IsolateTempDir: true}
	for _, opt := range opts {
		opt(&options)
	}

	return options
}

// WithOpenCodePath sets the OpenCode executable path.
func WithOpenCodePath(path string) Option {
	return func(options *Options) {
		options.OpenCodePath = path
	}
}

// WithCwd sets the OpenCode working directory.
func WithCwd(path string) Option {
	return func(options *Options) {
		options.Cwd = path
	}
}

// WithPure runs OpenCode without external plugins.
func WithPure(enabled bool) Option {
	return func(options *Options) {
		options.Pure = enabled
	}
}

// WithPrintLogs forwards OpenCode logs to the configured stderr writer.
func WithPrintLogs(enabled bool) Option {
	return func(options *Options) {
		options.PrintLogs = enabled
	}
}

// WithLogLevel sets OpenCode's log level. Supported values are defined by the
// installed OpenCode CLI.
func WithLogLevel(level string) Option {
	return func(options *Options) {
		options.LogLevel = level
	}
}

// WithHostname sets OpenCode's ACP listener hostname.
func WithHostname(hostname string) Option {
	return func(options *Options) {
		options.Hostname = hostname
	}
}

// WithPort sets OpenCode's ACP listener port. Use 0 to ask OpenCode to
// allocate a port.
func WithPort(port int) Option {
	return func(options *Options) {
		copied := port
		options.Port = &copied
	}
}

// WithMDNS enables OpenCode mDNS service discovery.
func WithMDNS(enabled bool) Option {
	return func(options *Options) {
		options.MDNS = enabled
	}
}

// WithMDNSDomain sets OpenCode's mDNS domain.
func WithMDNSDomain(domain string) Option {
	return func(options *Options) {
		options.MDNSDomain = domain
	}
}

// WithCORS adds OpenCode CORS domains.
func WithCORS(domains ...string) Option {
	return func(options *Options) {
		options.CORS = append([]string(nil), domains...)
	}
}

// WithQuestionTool configures OpenCode's question tool environment flag.
func WithQuestionTool(enabled bool) Option {
	return func(options *Options) {
		copied := enabled
		options.QuestionTool = &copied
	}
}

// WithTelemetry configures OpenCode-native telemetry environment variables.
func WithTelemetry(telemetry TelemetryOptions) Option {
	return func(options *Options) {
		options.Telemetry = telemetry
	}
}

// WithTelemetryFromEnv maps common OpenTelemetry process environment variables
// into OpenCode-native telemetry variables. When any supported OTEL variable is
// present, it applies OpenCode-friendly defaults for protocol, metrics
// interval, log disabling, and session trace disabling.
func WithTelemetryFromEnv(env map[string]string) Option {
	telemetry := TelemetryOptionsFromEnv(env)

	return func(options *Options) {
		options.Telemetry = telemetry
	}
}

// TelemetryOptionsFromEnv converts common OTEL environment variables into
// OpenCode telemetry options. When any supported OTEL variable is present, the
// returned options enable telemetry, default the protocol to "http/protobuf",
// set the metrics interval to "1000", disable OpenCode logs, and disable
// OpenCode session traces.
func TelemetryOptionsFromEnv(env map[string]string) TelemetryOptions {
	if len(env) == 0 {
		return TelemetryOptions{}
	}

	telemetry := TelemetryOptions{
		Endpoint:           env[otelExporterOTLPEndpointKey],
		Protocol:           env[otelExporterOTLPProtocolKey],
		ResourceAttributes: env[otelResourceAttributesKey],
	}
	if telemetry.Endpoint != "" || telemetry.Protocol != "" || telemetry.ResourceAttributes != "" {
		telemetry.Enabled = true
		if telemetry.Protocol == "" {
			telemetry.Protocol = defaultTelemetryProtocol
		}
		telemetry.MetricsInterval = defaultTelemetryMetricsInterval
		telemetry.DisableLogs = true
		telemetry.DisableTraces = defaultTelemetryDisableTraces
	}

	return telemetry
}

// WithTraceContext injects W3C trace context into OpenCode's process
// environment.
func WithTraceContext(traceparent string, tracestate string) Option {
	return func(options *Options) {
		options.Telemetry.Traceparent = traceparent
		options.Telemetry.Tracestate = tracestate
	}
}

// WithIsolatedTempDir controls wrapper-owned temporary directory isolation.
// It is enabled by default.
func WithIsolatedTempDir(enabled bool) Option {
	return func(options *Options) {
		options.IsolateTempDir = enabled
	}
}

// WithEnv merges environment variables into the launched OpenCode process.
func WithEnv(env map[string]string) Option {
	return func(options *Options) {
		options.Env = cloneStringMap(env)
	}
}

// WithStderr sets the writer used for OpenCode stderr.
func WithStderr(stderr io.Writer) Option {
	return func(options *Options) {
		options.Stderr = stderr
	}
}

// WithExtraArgs appends raw arguments after wrapper-managed `opencode acp`
// flags. Use this as an escape hatch for new OpenCode flags.
func WithExtraArgs(args ...string) Option {
	return func(options *Options) {
		options.ExtraArgs = append([]string(nil), args...)
	}
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}

	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}

	return cloned
}
