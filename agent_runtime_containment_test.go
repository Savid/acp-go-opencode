package opencodeacp

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
)

func TestAgentContainmentModeAndObservation(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	if got := (*Agent)(nil).ContainmentMode(); got != RuntimeContainmentUnavailable {
		t.Fatalf("nil agent mode = %q", got)
	}

	for _, test := range []struct {
		name    string
		goos    string
		opted   bool
		want    RuntimeContainmentMode
		wantErr bool
	}{
		{name: "linux", goos: platformLinux, want: RuntimeContainmentAuthoritative},
		{name: "windows", goos: platformWindows, want: RuntimeContainmentUnavailable},
		{name: "darwin default", goos: platformDarwin, want: RuntimeContainmentUnavailable},
		{name: "darwin opted", goos: platformDarwin, opted: true, want: RuntimeContainmentBestEffort},
		{name: "unsupported", goos: "plan9", want: RuntimeContainmentUnavailable},
		{name: "off darwin opted", goos: platformLinux, opted: true, want: RuntimeContainmentUnavailable, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeGOOS = test.goos
			options := Options{DarwinBestEffortContainment: test.opted}
			if got := containmentMode(options); got != test.want {
				t.Fatalf("mode = %q, want %q", got, test.want)
			}
			err := validateContainmentOptions(options)
			if (err != nil) != test.wantErr {
				t.Fatalf("validation error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}

	runtimeGOOS = platformDarwin
	if err := validateContainmentOptions(Options{Env: map[string]string{"acp_go_opencode_internal_spoof": "1"}}); err == nil || !strings.Contains(err.Error(), privateAdapterEnvPrefix) {
		t.Fatalf("reserved environment validation error = %v", err)
	}

	var observed []RuntimeContainmentMode
	defaultAgent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
			observed = append(observed, mode)
		},
	}))
	want := RuntimeContainmentUnavailable
	if got := defaultAgent.ContainmentMode(); got != want {
		t.Fatalf("default mode = %q, want %q", got, want)
	}
	if len(observed) != 1 || observed[0] != want {
		t.Fatalf("containment observations = %v", observed)
	}

	var logs bytes.Buffer
	var snapshots int
	opted := NewAgent(
		WithDarwinBestEffortContainment(),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveProcessSnapshot: func(context.Context, RuntimeProcessKind, int) { snapshots++ },
		}),
	)
	if opted.ContainmentMode() != RuntimeContainmentBestEffort {
		t.Fatalf("opted mode = %q", opted.ContainmentMode())
	}
	if !strings.Contains(logs.String(), `"containment":"best_effort"`) || !strings.Contains(logs.String(), "escaped descendants may survive") {
		t.Fatalf("structured best-effort warning = %q", logs.String())
	}
	observeRuntimeProcessSnapshot(t.Context(), opted.options.RuntimeResourceHooks, RuntimeProcessProviderDescendant, 7)
	if snapshots != 0 {
		t.Fatalf("best-effort provider snapshots = %d", snapshots)
	}

	runtimeGOOS = platformLinux
	offDarwin := NewAgent(WithDarwinBestEffortContainment())
	if _, err := offDarwin.Initialize(t.Context(), acp.InitializeRequest{}); err == nil || !strings.Contains(err.Error(), "supported only on darwin") {
		t.Fatalf("off-Darwin opt-in initialization error = %v", err)
	}
}
