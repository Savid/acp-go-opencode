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
	for _, key := range []string{
		"acp_go_opencode_internal_spoof", "HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME",
		managedEnvXDGDataHome, "XDG_RUNTIME_DIR", "XDG_STATE_HOME", "OPENCODE_CONFIG",
		"OPENCODE_CONFIG_CONTENT", managedEnvOpenCodeConfigDir, "OPENCODE_DB",
	} {
		if err := validateContainmentOptions(Options{Env: map[string]string{key: "1"}}); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("reserved environment validation error for %q = %v", key, err)
		}
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

func TestStandaloneIsolationDefaultsAndFencesDurableHome(t *testing.T) {
	const stateRoot = "/var/lib/acp-go-opencode"
	isolation := ProcessIsolation{StandaloneOwnerID: "deployment-1", StandaloneStateRoot: stateRoot}

	defaulted := NewAgent(WithProcessIsolation(isolation))
	if defaulted.options.Home != stateRoot {
		t.Fatalf("default home = %q, want %q", defaulted.options.Home, stateRoot)
	}

	explicit := NewAgent(WithHome(stateRoot), WithProcessIsolation(isolation))
	if explicit.optionsErr != nil {
		t.Fatalf("matching standalone home error = %v", explicit.optionsErr)
	}

	mismatched := NewAgent(WithHome("/var/lib/other"), WithProcessIsolation(isolation))
	if _, err := mismatched.Initialize(t.Context(), acp.InitializeRequest{}); err == nil ||
		!strings.Contains(err.Error(), "WithHome must equal ProcessIsolation.StandaloneStateRoot") {
		t.Fatalf("mismatched standalone home error = %v", err)
	}
}

// TestContainmentModeReportsASharedAgentIdentity proves the reported boundary
// names what was actually proven. A Linux supervisor that launches the native
// process under the identity it already runs as still proves whole-tree
// lifecycle — the subreaper, the descendant reaping and the group teardown are
// unchanged — but it does not separate the agent's credentials from its own, so
// reporting the authoritative boundary would overstate it. Root can never
// select the arm, a native identity that differs still reports authoritative,
// and no other platform is touched.
func TestContainmentModeReportsASharedAgentIdentity(t *testing.T) {
	originalGOOS := runtimeGOOS
	originalUID := containmentEffectiveUID
	t.Cleanup(func() {
		runtimeGOOS = originalGOOS
		containmentEffectiveUID = originalUID
	})

	for _, test := range []struct {
		name      string
		goos      string
		effective int
		isolation *ProcessIsolation
		want      RuntimeContainmentMode
	}{
		{name: "no isolation", goos: platformLinux, effective: 1000, want: RuntimeContainmentAuthoritative},
		{
			name: "own identity", goos: platformLinux, effective: 1000,
			isolation: &ProcessIsolation{UID: 1000, GID: 1000}, want: RuntimeContainmentSharedIdentity,
		},
		{
			name: "own identity in another group", goos: platformLinux, effective: 1000,
			isolation: &ProcessIsolation{UID: 1000, GID: 2000}, want: RuntimeContainmentSharedIdentity,
		},
		{
			name: "distinct identity", goos: platformLinux, effective: 1000,
			isolation: &ProcessIsolation{UID: 65534, GID: 65534}, want: RuntimeContainmentAuthoritative,
		},
		{
			name: "trusted root", goos: platformLinux, effective: 0,
			isolation: &ProcessIsolation{UID: 0, GID: 0}, want: RuntimeContainmentAuthoritative,
		},
		{
			name: "darwin", goos: platformDarwin, effective: 1000,
			isolation: &ProcessIsolation{UID: 1000, GID: 1000}, want: RuntimeContainmentUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeGOOS = test.goos
			containmentEffectiveUID = func() int { return test.effective }
			if got := containmentMode(Options{ProcessIsolation: test.isolation}); got != test.want {
				t.Fatalf("mode = %q, want %q", got, test.want)
			}
		})
	}

	runtimeGOOS = platformLinux
	containmentEffectiveUID = func() int { return 1000 }

	var observed []RuntimeContainmentMode
	agent := NewAgent(
		WithProcessIsolation(ProcessIsolation{UID: 1000, GID: 1000}),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
				observed = append(observed, mode)
			},
		}),
	)
	if got := agent.ContainmentMode(); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("shared identity mode = %q", got)
	}
	if len(observed) != 1 || observed[0] != RuntimeContainmentSharedIdentity {
		t.Fatalf("containment observations = %v", observed)
	}
}

func TestProcessIsolatedDurableHomeRequiresCanonicalPath(t *testing.T) {
	for _, path := range []string{"relative", "/", "/var/lib/opencode/", "/var/lib/../opencode", "/var/lib/open\ncode", "/" + strings.Repeat("a", 4097)} {
		t.Run(path, func(t *testing.T) {
			agent := NewAgent(
				WithHome(path),
				WithProcessIsolation(ProcessIsolation{}),
			)
			_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
			if err == nil || !strings.Contains(err.Error(), "OpenCode runtime home") {
				t.Fatalf("home validation error = %v", err)
			}
		})
	}
}
