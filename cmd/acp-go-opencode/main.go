package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	opencodeacp "github.com/savid/acp-go-opencode"
	"github.com/savid/acp-go-opencode/internal/opencode"
)

var serve = opencodeacp.Serve
var agentVersion = version
var exit = os.Exit
var shutdownOpenTelemetry = shutdownTelemetry

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("acp-go-opencode", flag.ContinueOnError)
	flags.SetOutput(stderr)

	opencodePath := flags.String("path", "", "path to opencode CLI")
	opencodeHome := flags.String("home", "", "exclusive shared OpenCode XDG runtime root")
	scratchDir := flags.String("scratch-dir", "", "parent for a generated runtime root and transient scratch")
	model := flags.String("model", "", "default OpenCode model as provider/model")
	debug := flags.Bool("debug", false, "write debug logs to stderr")
	printVersion := flags.Bool("version", false, "print adapter version and exit")
	pure := flags.Bool("opencode-pure", false, "start OpenCode without external plugins")
	questionTool := flags.Bool("opencode-question-tool", false, "enable OpenCode native question tool mapping")
	logLevel := flags.String("opencode-log-level", "", "OpenCode native server log level")
	nativeVersion := flags.String("opencode-version", "1.18.1", "exact OpenCode version required by the sync-event store")
	healthTimeout := flags.Duration("opencode-health-timeout", opencode.HealthCheckTimeout, "OpenCode server readiness timeout")

	var seedFiles seedFileFlag

	flags.Var(&seedFiles, "seed-file", "seed a file into the shared runtime config root as <relpath>=<hostpath> (repeatable)")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *printVersion {
		_, _ = fmt.Fprintln(stdout, agentVersion())

		return 0
	}

	logger := slog.New(slog.DiscardHandler)
	if *debug {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	signals := forwardedSignals()
	receivedSignals := make(chan os.Signal, 1)

	signal.Notify(receivedSignals, signals...)
	defer signal.Stop(receivedSignals)

	ctx, stop := signal.NotifyContext(ctx, signals...)
	defer stop()

	version := agentVersion()

	telemetry, err := configureTelemetry(ctx, logger, version)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-opencode: configure OpenTelemetry: %v\n", err)

		return 1
	}

	logger = telemetry.logger

	opts := make([]opencodeacp.Option, 0, 10+len(telemetry.options))

	opts = append(opts,
		opencodeacp.WithAgentVersion(version),
		opencodeacp.WithExecutablePath(*opencodePath),
		opencodeacp.WithHome(*opencodeHome),
		opencodeacp.WithScratchDir(*scratchDir),
		opencodeacp.WithDefaultModel(*model),
		opencodeacp.WithLogger(logger),
		opencodeacp.WithOpenCodePure(*pure),
		opencodeacp.WithOpenCodeQuestionTool(*questionTool),
		opencodeacp.WithOpenCodeLogLevel(*logLevel),
		opencodeacp.WithOpenCodeHealthCheckTimeout(*healthTimeout),
	)

	if *nativeVersion != "" {
		opts = append(opts, opencodeacp.WithVersion(*nativeVersion))
	}

	if len(seedFiles.files) > 0 {
		opts = append(opts, opencodeacp.WithSeedFiles(seedFiles.files))
	}

	opts = append(opts, telemetry.options...)

	err = serve(ctx, stdin, stdout, opts...)

	shutdownErr := shutdownOpenTelemetry(context.Background(), telemetry.shutdown)
	if shutdownErr != nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-opencode: shutdown OpenTelemetry: %v\n", shutdownErr)

		return 1
	}

	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-opencode: %v\n", err)

		return 1
	}

	if sig := pendingSignal(receivedSignals); sig != nil {
		return signalCode(sig)
	}

	return 0
}

// seedFileFlag is a repeatable -seed-file flag. Each value is
// <relpath>=<hostpath>: the host file is read at parse time and its contents are
// mapped to the relative path passed to WithSeedFiles.
type seedFileFlag struct {
	files map[string]string
}

func (s *seedFileFlag) String() string {
	return ""
}

func (s *seedFileFlag) Set(value string) error {
	rel, host, ok := strings.Cut(value, "=")
	if !ok || rel == "" || host == "" {
		return fmt.Errorf("seed-file must be <relpath>=<hostpath>, got %q", value)
	}

	contents, err := os.ReadFile(host)
	if err != nil {
		return fmt.Errorf("read seed file %q: %w", host, err)
	}

	if s.files == nil {
		s.files = make(map[string]string, 1)
	}

	s.files[rel] = string(contents)

	return nil
}

func pendingSignal(signals <-chan os.Signal) os.Signal {
	select {
	case sig := <-signals:
		return sig
	default:
		return nil
	}
}
