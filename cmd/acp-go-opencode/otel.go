package main

import (
	"context"
	"log/slog"

	"github.com/savid/acp-go-core/observer/exporters"
	opencodeacp "github.com/savid/acp-go-opencode"
)

// telemetryConfig is the exporter bundle this binary hands to the agent.
type telemetryConfig struct {
	logger   *slog.Logger
	options  []opencodeacp.Option
	shutdown func(context.Context) error
}

// configureTelemetry reads the OTEL_* environment and maps the providers it
// enables onto agent options.
func configureTelemetry(ctx context.Context, baseLogger *slog.Logger, version string) (telemetryConfig, error) {
	bundle, err := exporters.Configure(ctx, exporters.Config{Vendor: "opencode", Version: version, Logger: baseLogger})
	if err != nil {
		return telemetryConfig{}, err
	}

	config := telemetryConfig{logger: bundle.Logger, shutdown: bundle.Shutdown}
	if bundle.Propagator != nil {
		config.options = append(config.options, opencodeacp.WithTextMapPropagator(bundle.Propagator))
	}

	if bundle.TracerProvider != nil {
		config.options = append(config.options, opencodeacp.WithTracerProvider(bundle.TracerProvider))
	}

	if bundle.MeterProvider != nil {
		config.options = append(config.options, opencodeacp.WithMeterProvider(bundle.MeterProvider))
	}

	return config, nil
}
