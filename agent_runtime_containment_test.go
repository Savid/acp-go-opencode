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
		{name: "linux", goos: platformLinux, want: RuntimeContainmentSharedIdentity},
		{name: "windows", goos: platformWindows, want: RuntimeContainmentSharedIdentity},
		{name: "freebsd", goos: "freebsd", want: RuntimeContainmentSharedIdentity},
		{name: "darwin default", goos: platformDarwin, want: RuntimeContainmentSharedIdentity},
		{name: "darwin opted", goos: platformDarwin, opted: true, want: RuntimeContainmentBestEffort},
		{name: "unsupported", goos: "plan9", want: RuntimeContainmentSharedIdentity},
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
	want := RuntimeContainmentSharedIdentity
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
	originalGOOS := runtimeGOOS
	runtimeGOOS = platformLinux
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

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

// TestContainmentModeReportsANonAuthoritativeSharedIdentity proves the
// reported boundary names what was actually selected. Omitting the policy is
// ordinary same-identity execution: a posture that needs no privilege and works
// wherever the native launch works, so it is reported on every platform and at
// root exactly as at an unprivileged account. An explicit policy is the
// hardened Linux boundary and nothing else, so it reports authoritative there
// and unavailable everywhere it cannot be honored — never the ordinary arm.
func TestContainmentModeReportsANonAuthoritativeSharedIdentity(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	explicit := &ProcessIsolation{UID: 65534, GID: 65534}

	for _, test := range []struct {
		name      string
		goos      string
		isolation *ProcessIsolation
		want      RuntimeContainmentMode
	}{
		{name: "omitted linux", goos: platformLinux, want: RuntimeContainmentSharedIdentity},
		{name: "omitted darwin", goos: platformDarwin, want: RuntimeContainmentSharedIdentity},
		{name: "omitted windows", goos: platformWindows, want: RuntimeContainmentSharedIdentity},
		{name: "omitted freebsd", goos: "freebsd", want: RuntimeContainmentSharedIdentity},
		{name: "explicit linux", goos: platformLinux, isolation: explicit, want: RuntimeContainmentAuthoritative},
		{name: "explicit darwin", goos: platformDarwin, isolation: explicit, want: RuntimeContainmentUnavailable},
		{name: "explicit windows", goos: platformWindows, isolation: explicit, want: RuntimeContainmentUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeGOOS = test.goos
			if got := containmentMode(Options{ProcessIsolation: test.isolation}); got != test.want {
				t.Fatalf("mode = %q, want %q", got, test.want)
			}
		})
	}

	runtimeGOOS = platformLinux

	var observed []RuntimeContainmentMode
	agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
		ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
			observed = append(observed, mode)
		},
	}))
	if got := agent.ContainmentMode(); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("ordinary mode = %q", got)
	}
	if len(observed) != 1 || observed[0] != RuntimeContainmentSharedIdentity {
		t.Fatalf("containment observations = %v", observed)
	}
}

// TestOrdinaryExecutionPublishesNoProviderInventory proves the ordinary default
// makes no whole-tree claim on any platform: the provider-descendant snapshot
// hook is withheld for the Agent's whole life, so not even a terminal zero can
// be read as a quiescence proof. Only the hardened Linux boundary, which can
// enumerate what it contains, keeps the hook.
func TestOrdinaryExecutionPublishesNoProviderInventory(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	for _, goos := range []string{platformLinux, platformDarwin, platformWindows, "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			runtimeGOOS = goos

			snapshots := 0
			agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
				ObserveProcessSnapshot: func(context.Context, RuntimeProcessKind, int) { snapshots++ },
			}))
			if agent.ContainmentMode() != RuntimeContainmentSharedIdentity {
				t.Fatalf("ordinary mode = %q", agent.ContainmentMode())
			}
			if agent.options.RuntimeResourceHooks.ObserveProcessSnapshot != nil {
				t.Fatal("ordinary execution retained a provider-descendant snapshot hook")
			}
			observeRuntimeProcessSnapshot(t.Context(), agent.options.RuntimeResourceHooks, RuntimeProcessProviderDescendant, 0)
			if snapshots != 0 {
				t.Fatalf("ordinary provider snapshots = %d", snapshots)
			}
		})
	}

	runtimeGOOS = platformLinux

	snapshots := 0
	hardened := NewAgent(
		WithProcessIsolation(ProcessIsolation{
			UID: 65534, GID: 65534, BaseEnvironment: map[string]string{},
			StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go-opencode",
		}),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveProcessSnapshot: func(context.Context, RuntimeProcessKind, int) { snapshots++ },
		}),
	)
	if hardened.ContainmentMode() != RuntimeContainmentAuthoritative {
		t.Fatalf("hardened mode = %q", hardened.ContainmentMode())
	}
	observeRuntimeProcessSnapshot(t.Context(), hardened.options.RuntimeResourceHooks, RuntimeProcessProviderDescendant, 3)
	if snapshots != 1 {
		t.Fatalf("authoritative provider snapshots = %d", snapshots)
	}
}

// TestExplicitProcessIsolationRefusesEveryUnsupportedSelection proves the
// public option is fail-closed rather than best effort. Off Linux it refuses,
// and combined with the Darwin opt-in it refuses on Darwin too, because a
// hardened identity policy cannot be downgraded to a process-group boundary.
func TestExplicitProcessIsolationRefusesEveryUnsupportedSelection(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	isolation := ProcessIsolation{
		UID: 65534, GID: 65534, BaseEnvironment: map[string]string{},
		StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go-opencode",
	}

	for _, goos := range []string{platformDarwin, platformWindows, "freebsd"} {
		t.Run(goos, func(t *testing.T) {
			runtimeGOOS = goos
			agent := NewAgent(WithProcessIsolation(isolation))
			if _, err := agent.Initialize(t.Context(), acp.InitializeRequest{}); err == nil ||
				!strings.Contains(err.Error(), "explicit process isolation is supported only on linux") {
				t.Fatalf("off-Linux explicit policy error = %v", err)
			}
			if agent.ContainmentMode() != RuntimeContainmentUnavailable {
				t.Fatalf("off-Linux explicit mode = %q", agent.ContainmentMode())
			}
		})
	}

	runtimeGOOS = platformDarwin
	combined := NewAgent(WithProcessIsolation(isolation), WithDarwinBestEffortContainment())
	if _, err := combined.Initialize(t.Context(), acp.InitializeRequest{}); err == nil ||
		!strings.Contains(err.Error(), "cannot be combined with darwin best-effort containment") {
		t.Fatalf("combined selection error = %v", err)
	}
	if combined.ContainmentMode() != RuntimeContainmentUnavailable {
		t.Fatalf("combined mode = %q", combined.ContainmentMode())
	}
}

func TestProcessIsolatedDurableHomeRequiresCanonicalPath(t *testing.T) {
	originalGOOS := runtimeGOOS
	runtimeGOOS = platformLinux
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

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
