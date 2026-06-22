package opencode

// TelemetryOptions configures OpenCode-native telemetry environment variables.
type TelemetryOptions struct {
	Enabled            bool
	Endpoint           string
	Protocol           string
	MetricsInterval    string
	ResourceAttributes string
	DisableLogs        bool
	DisableTraces      string
	Traceparent        string
	Tracestate         string
}

// Options configures one `opencode acp` process.
type Options struct {
	CLIPath        string
	Cwd            string
	Pure           bool
	PrintLogs      bool
	LogLevel       string
	Hostname       string
	Port           *int
	MDNS           bool
	MDNSDomain     string
	CORS           []string
	QuestionTool   *bool
	Telemetry      TelemetryOptions
	IsolateTempDir bool
	TempDir        string
	Env            map[string]string
	ExtraArgs      []string
}
