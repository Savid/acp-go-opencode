package opencodeacp

import (
	"bytes"
	"reflect"
	"testing"
)

func TestOptions(t *testing.T) {
	t.Parallel()

	env := map[string]string{"A": "1"}
	var stderr bytes.Buffer
	options := applyOptions([]Option{
		WithOpenCodePath("/bin/opencode"),
		WithCwd("/workspace"),
		WithPure(true),
		WithPrintLogs(true),
		WithLogLevel("DEBUG"),
		WithHostname("127.0.0.1"),
		WithPort(0),
		WithMDNS(true),
		WithMDNSDomain("opencode.local"),
		WithCORS("https://example.com"),
		WithQuestionTool(true),
		WithTelemetryFromEnv(map[string]string{
			otelExporterOTLPEndpointKey: "ignored-by-explicit-telemetry",
		}),
		WithTelemetry(TelemetryOptions{
			Enabled:            true,
			Endpoint:           "http://otel",
			Protocol:           "http/protobuf",
			MetricsInterval:    "1000",
			ResourceAttributes: "service.name=test",
			DisableLogs:        true,
			DisableTraces:      "session",
		}),
		WithTraceContext("00-trace-span-01", "state"),
		WithIsolatedTempDir(false),
		WithEnv(env),
		WithStderr(&stderr),
		WithExtraArgs("--port", "0"),
	})
	env["A"] = "changed"

	if options.OpenCodePath != "/bin/opencode" ||
		options.Cwd != "/workspace" ||
		!options.Pure ||
		!options.PrintLogs ||
		options.LogLevel != "DEBUG" ||
		options.Hostname != "127.0.0.1" ||
		options.Port == nil ||
		*options.Port != 0 ||
		!options.MDNS ||
		options.MDNSDomain != "opencode.local" ||
		options.QuestionTool == nil ||
		!*options.QuestionTool ||
		!options.Telemetry.Enabled ||
		options.Telemetry.Endpoint != "http://otel" ||
		options.Telemetry.Traceparent != "00-trace-span-01" ||
		options.Telemetry.Tracestate != "state" ||
		options.IsolateTempDir ||
		options.Stderr != &stderr {
		t.Fatalf("unexpected options: %#v", options)
	}
	if !reflect.DeepEqual(options.CORS, []string{"https://example.com"}) {
		t.Fatalf("CORS = %#v", options.CORS)
	}
	if !reflect.DeepEqual(options.Env, map[string]string{"A": "1"}) {
		t.Fatalf("Env = %#v", options.Env)
	}
	if !reflect.DeepEqual(options.ExtraArgs, []string{"--port", "0"}) {
		t.Fatalf("ExtraArgs = %#v", options.ExtraArgs)
	}

	envTelemetry := applyOptions([]Option{WithTelemetryFromEnv(map[string]string{
		otelExporterOTLPEndpointKey: "http://from-env",
	})})
	if !envTelemetry.Telemetry.Enabled || envTelemetry.Telemetry.Endpoint != "http://from-env" {
		t.Fatalf("WithTelemetryFromEnv options = %#v", envTelemetry.Telemetry)
	}
}

func TestDefaultOptions(t *testing.T) {
	t.Parallel()

	options := applyOptions(nil)
	if !options.IsolateTempDir {
		t.Fatal("IsolateTempDir default is false")
	}
}

func TestTelemetryOptionsFromEnv(t *testing.T) {
	t.Parallel()

	empty := TelemetryOptionsFromEnv(nil)
	if empty.Enabled {
		t.Fatalf("empty telemetry enabled: %#v", empty)
	}

	telemetry := TelemetryOptionsFromEnv(map[string]string{
		otelExporterOTLPEndpointKey: "http://otel",
		otelResourceAttributesKey:   "service.name=test",
	})
	if !telemetry.Enabled ||
		telemetry.Endpoint != "http://otel" ||
		telemetry.Protocol != "http/protobuf" ||
		telemetry.MetricsInterval != "1000" ||
		!telemetry.DisableLogs ||
		telemetry.DisableTraces != "session" ||
		telemetry.ResourceAttributes != "service.name=test" {
		t.Fatalf("telemetry = %#v", telemetry)
	}
}

func TestCloneStringMap(t *testing.T) {
	t.Parallel()

	if cloneStringMap(nil) != nil {
		t.Fatal("cloneStringMap(nil) returned non-nil map")
	}

	source := map[string]string{"A": "1"}
	cloned := cloneStringMap(source)
	source["A"] = "2"
	if cloned["A"] != "1" {
		t.Fatalf("cloned map changed: %#v", cloned)
	}
}
