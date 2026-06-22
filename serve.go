package opencodeacp

import (
	"context"
	"errors"
	"io"

	"github.com/savid/acp-go-opencode/internal/opencode"
)

var runACP = opencode.RunACP

// Serve runs `opencode acp` behind a small ACP JSON-RPC proxy.
//
// When input is not an *os.File, Serve copies it to the child through a pipe.
// Embedders should pass a reader that eventually returns data or EOF; a
// permanently blocked reader can keep that copy goroutine parked after shutdown.
//
// Native OpenCode ACP messages are forwarded by default. The proxy handles
// wrapper-owned extension methods and augments initialize metadata.
func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) error {
	if ctx == nil {
		return errors.New("context is nil")
	}
	if input == nil {
		return errors.New("input is nil")
	}
	if output == nil {
		return errors.New("output is nil")
	}

	options := applyOptions(opts)
	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	return serveProxy(ctx, input, output, stderr, opts, opencodeOptions(options))
}

func opencodeTelemetry(telemetry TelemetryOptions) opencode.TelemetryOptions {
	return opencode.TelemetryOptions{
		Enabled:            telemetry.Enabled,
		Endpoint:           telemetry.Endpoint,
		Protocol:           telemetry.Protocol,
		MetricsInterval:    telemetry.MetricsInterval,
		ResourceAttributes: telemetry.ResourceAttributes,
		DisableLogs:        telemetry.DisableLogs,
		DisableTraces:      telemetry.DisableTraces,
		Traceparent:        telemetry.Traceparent,
		Tracestate:         telemetry.Tracestate,
	}
}
