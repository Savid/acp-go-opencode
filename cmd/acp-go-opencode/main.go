package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	opencodeacp "github.com/savid/acp-go-opencode"
)

var (
	serve        = opencodeacp.Serve
	agentVersion = version
	exit         = os.Exit
)

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("acp-go-opencode", flag.ContinueOnError)
	flags.SetOutput(stderr)

	var cors stringListFlag

	opencodePath := flags.String("opencode", "", "path to opencode CLI")
	cwd := flags.String("cwd", "", "OpenCode working directory")
	pure := flags.Bool("pure", false, "run OpenCode without external plugins")
	printLogs := flags.Bool("print-logs", false, "print OpenCode logs to stderr")
	logLevel := flags.String("log-level", "", "OpenCode log level: DEBUG, INFO, WARN, or ERROR")
	hostname := flags.String("hostname", "", "OpenCode ACP listener hostname")
	port := flags.Int("port", -1, "OpenCode ACP listener port; use 0 to allocate a port")
	mdns := flags.Bool("mdns", false, "enable OpenCode mDNS service discovery")
	mdnsDomain := flags.String("mdns-domain", "", "OpenCode mDNS domain")
	flags.Var(&cors, "cors", "OpenCode CORS domain; may be repeated")
	questionTool := flags.Bool("question-tool", false, "enable OpenCode question tool")
	isolateTempDir := flags.Bool("isolate-temp-dir", true, "isolate OpenCode TMPDIR/TMP/TEMP and clean it up on exit")
	enableTelemetry := flags.Bool("enable-telemetry", false, "set OPENCODE_ENABLE_TELEMETRY=1")
	otlpEndpoint := flags.String("otlp-endpoint", "", "OpenCode OTLP endpoint")
	otlpProtocol := flags.String("otlp-protocol", "", "OpenCode OTLP protocol")
	otlpMetricsInterval := flags.String("otlp-metrics-interval", "", "OpenCode OTLP metrics interval")
	resourceAttributes := flags.String("resource-attributes", "", "OpenCode resource attributes")
	disableOpenCodeLogs := flags.Bool("disable-opencode-logs", false, "set OPENCODE_DISABLE_LOGS=1")
	disableTraces := flags.String("disable-traces", "", "set OPENCODE_DISABLE_TRACES")
	traceparent := flags.String("traceparent", "", "set OPENCODE_TRACEPARENT")
	tracestate := flags.String("tracestate", "", "set OPENCODE_TRACESTATE")
	debug := flags.Bool("debug", false, "print OpenCode debug logs to stderr")
	showVersion := flags.Bool("version", false, "show version and exit")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, agentVersion())

		return 0
	}
	if *debug {
		*printLogs = true
		if *logLevel == "" {
			*logLevel = "DEBUG"
		}
	}

	signals := forwardedSignals()
	receivedSignals := make(chan os.Signal, 1)
	signal.Notify(receivedSignals, signals...)
	defer signal.Stop(receivedSignals)

	ctx, stop := signal.NotifyContext(ctx, signals...)
	defer stop()

	serveOptions := []opencodeacp.Option{
		opencodeacp.WithOpenCodePath(*opencodePath),
		opencodeacp.WithCwd(*cwd),
		opencodeacp.WithPure(*pure),
		opencodeacp.WithPrintLogs(*printLogs),
		opencodeacp.WithLogLevel(*logLevel),
		opencodeacp.WithHostname(*hostname),
		opencodeacp.WithMDNS(*mdns),
		opencodeacp.WithMDNSDomain(*mdnsDomain),
		opencodeacp.WithCORS(cors...),
		opencodeacp.WithIsolatedTempDir(*isolateTempDir),
		opencodeacp.WithTelemetry(opencodeacp.TelemetryOptions{
			Enabled:            *enableTelemetry,
			Endpoint:           *otlpEndpoint,
			Protocol:           *otlpProtocol,
			MetricsInterval:    *otlpMetricsInterval,
			ResourceAttributes: *resourceAttributes,
			DisableLogs:        *disableOpenCodeLogs,
			DisableTraces:      *disableTraces,
			Traceparent:        *traceparent,
			Tracestate:         *tracestate,
		}),
		opencodeacp.WithStderr(stderr),
		opencodeacp.WithExtraArgs(flags.Args()...),
	}
	if *port >= 0 {
		serveOptions = append(serveOptions, opencodeacp.WithPort(*port))
	}
	if flagWasSet(flags, "question-tool") {
		serveOptions = append(serveOptions, opencodeacp.WithQuestionTool(*questionTool))
	}

	err := serve(ctx, stdin, stdout,
		serveOptions...,
	)
	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-opencode: %v\n", err)

		return commandExitCode(err)
	}
	if sig := pendingSignal(receivedSignals); sig != nil {
		return signalCode(sig)
	}

	return 0
}

func pendingSignal(signals <-chan os.Signal) os.Signal {
	select {
	case sig := <-signals:
		return sig
	default:
		return nil
	}
}

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code >= 0 {
			return code
		}
		if code := signalExitCode(exitErr); code > 0 {
			return code
		}
	}

	return 1
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	found := false
	flags.Visit(func(flag *flag.Flag) {
		if flag.Name == name {
			found = true
		}
	})

	return found
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	if f == nil {
		return ""
	}

	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)

	return nil
}
