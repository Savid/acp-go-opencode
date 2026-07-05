package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"time"

	opencodeacp "github.com/savid/acp-go-opencode"
)

var serve = opencodeacp.Serve
var agentVersion = version
var exit = os.Exit

func main() {
	if code := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); code != 0 {
		exit(code)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("acp-go-opencode", flag.ContinueOnError)
	flags.SetOutput(stderr)

	opencodePath := flags.String("path", "", "path to opencode CLI")
	opencodeHome := flags.String("home", "", "parent root for isolated OpenCode session state")
	model := flags.String("model", "", "default OpenCode model as provider/model")
	debug := flags.Bool("debug", false, "write debug logs to stderr")
	printVersion := flags.Bool("version", false, "print adapter version and exit")
	pure := flags.Bool("opencode-pure", false, "start OpenCode without external plugins")
	questionTool := flags.Bool("opencode-question-tool", false, "enable OpenCode native question tool mapping")
	logLevel := flags.String("opencode-log-level", "", "OpenCode native server log level")
	minimumVersion := flags.String("opencode-minimum-version", "", "minimum accepted OpenCode version")
	healthTimeout := flags.Duration("opencode-health-timeout", 60*time.Second, "OpenCode server readiness timeout")

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

	opts := []opencodeacp.Option{
		opencodeacp.WithAgentVersion(agentVersion()),
		opencodeacp.WithExecutablePath(*opencodePath),
		opencodeacp.WithHome(*opencodeHome),
		opencodeacp.WithDefaultModel(*model),
		opencodeacp.WithLogger(logger),
		opencodeacp.WithOpenCodePure(*pure),
		opencodeacp.WithOpenCodeQuestionTool(*questionTool),
		opencodeacp.WithOpenCodeLogLevel(*logLevel),
		opencodeacp.WithOpenCodeHealthCheckTimeout(*healthTimeout),
	}
	if *minimumVersion != "" {
		opts = append(opts, opencodeacp.WithOpenCodeMinimumVersion(*minimumVersion))
	}

	err := serve(ctx, stdin, stdout, opts...)
	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "acp-go-opencode: %v\n", err)
		return 1
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
