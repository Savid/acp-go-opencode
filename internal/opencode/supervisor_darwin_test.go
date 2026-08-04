//go:build darwin

package opencode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	darwinContainmentHelperEnv    = "ACP_GO_OPENCODE_DARWIN_CONTAINMENT_HELPER"
	darwinContainmentPIDFileEnv   = "ACP_GO_OPENCODE_DARWIN_CONTAINMENT_PID_FILE"
	darwinContainmentReadyFileEnv = "ACP_GO_OPENCODE_DARWIN_CONTAINMENT_READY_FILE"
)

func preserveDarwinSupervisorSeams(t *testing.T) {
	t.Helper()
	oldGetpgid := darwinSupervisorGetpgid
	oldKill := darwinSupervisorKill
	oldNow := darwinSupervisorNow
	oldSleep := darwinSupervisorSleep
	oldFastExitWait := darwinFastExitWait
	oldAbortWait := darwinAbortWait
	oldIdentity := darwinProcessIdentityLookup
	t.Cleanup(func() {
		darwinSupervisorGetpgid = oldGetpgid
		darwinSupervisorKill = oldKill
		darwinSupervisorNow = oldNow
		darwinSupervisorSleep = oldSleep
		darwinFastExitWait = oldFastExitWait
		darwinAbortWait = oldAbortWait
		darwinProcessIdentityLookup = oldIdentity
	})
}

func darwinSupervisorTestConfig(t *testing.T) supervisorConfig {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "acp-go-opencode-runtime-test")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	return supervisorConfig{
		ScratchParent: parent, Scratch: root, LifecycleKind: "runtime", DarwinBestEffort: true,
		IsolationUID: testProcessIsolation().UID, IsolationGID: testProcessIsolation().GID, Isolation: testProcessIsolation(),
	}
}

func TestDarwinContainmentSelectionAndStartFailures(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	if _, err := newGuardianContainment(supervisorConfig{}); err == nil {
		t.Fatal("guardian accepted missing opt-in")
	}
	if err := (&guardianContainment{}).Quiesce(0, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("missing guardian identity = %v", err)
	}
	if _, err := openLivenessContainment(supervisorConfig{}); err == nil {
		t.Fatal("liveness accepted missing opt-in")
	}
	if _, err := openLivenessContainment(supervisorConfig{DarwinBestEffort: true}); err == nil {
		t.Fatal("liveness accepted missing generation root")
	}
	parent := t.TempDir()
	if _, err := openLivenessContainment(supervisorConfig{
		DarwinBestEffort: true, ScratchParent: parent,
		Scratch: filepath.Join(filepath.Dir(parent), "acp-go-opencode-runtime-outside"), LifecycleKind: "runtime",
	}); err == nil {
		t.Fatal("liveness accepted a generation outside its parent")
	}

	config := darwinSupervisorTestConfig(t)
	liveness, err := openLivenessContainment(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := liveness.Start(exec.Command(filepath.Join(t.TempDir(), "missing"))); err == nil {
		t.Fatal("missing command started")
	}
	if err := (&livenessContainment{generation: &DarwinGeneration{}}).Start(exec.Command("/usr/bin/true")); err == nil {
		t.Fatal("invalid generation prepared a command")
	}

	if err := (*livenessContainment)(nil).Start(exec.Command("/usr/bin/true")); err == nil {
		t.Fatal("nil containment started")
	}
	if err := (&livenessContainment{}).Quiesce(0, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := (&livenessContainment{}).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinFastExitFallbackBranches(t *testing.T) {
	t.Run("group absent", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		liveness, err := openLivenessContainment(config)
		if err != nil {
			t.Fatal(err)
		}
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		err = liveness.Start(exec.Command("/usr/bin/true"))
		if err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("fast-exit result = %v", err)
		}
	})

	t.Run("group present cleanup", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		liveness, err := openLivenessContainment(config)
		if err != nil {
			t.Fatal(err)
		}
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
		calls := 0
		signalBeforeWaiter := false
		darwinSupervisorKill = func(int, syscall.Signal) error {
			calls++
			if calls == 1 {
				return nil
			}
			select {
			case <-liveness.waiter.begin:
				t.Fatal("fast-exit waiter started before the first group signal")
			default:
				signalBeforeWaiter = true
			}

			return syscall.ESRCH
		}
		err = liveness.Start(exec.Command("/usr/bin/true"))
		if err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("present fast-exit result = %v", err)
		}
		if !signalBeforeWaiter {
			t.Fatal("fast-exit group signal ordering was not observed")
		}
	})

	t.Run("probe failure", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		liveness, err := openLivenessContainment(config)
		if err != nil {
			t.Fatal(err)
		}
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
		err = liveness.Start(exec.Command("/bin/sleep", "10"))
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("probe failure = %v", err)
		}
	})

	t.Run("reap timeout", func(t *testing.T) {
		preserveDarwinSupervisorSeams(t)
		darwinFastExitWait = time.Millisecond
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		liveness := &livenessContainment{
			generation: &DarwinGeneration{},
			waiter:     &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})},
		}
		if err := liveness.handleFastExit(123); !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("fast-exit timeout = %v", err)
		}
	})
}

func TestDarwinStartValidationFailureBranches(t *testing.T) {
	for _, test := range []struct {
		name     string
		getpgid  func(int) (int, error)
		identity bool
	}{
		{name: "getpgid", getpgid: func(int) (int, error) { return 0, syscall.EIO }},
		{name: "wrong group", getpgid: func(pid int) (int, error) { return pid + 1, nil }},
		{name: "identity", getpgid: func(pid int) (int, error) { return pid, nil }, identity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveDarwinSupervisorSeams(t)
			config := darwinSupervisorTestConfig(t)
			liveness, err := openLivenessContainment(config)
			if err != nil {
				t.Fatal(err)
			}
			darwinSupervisorGetpgid = test.getpgid
			signalBeforeWaiter := false
			if test.identity {
				darwinProcessIdentityLookup = func(int) (darwinProcessIdentity, error) {
					return darwinProcessIdentity{}, errors.New("identity failed")
				}
				darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
					if signal == syscall.SIGTERM {
						select {
						case <-liveness.waiter.begin:
							t.Fatal("record-activation waiter started before the first group signal")
						default:
							signalBeforeWaiter = true
						}
					}

					return syscall.ESRCH
				}
			}
			err = liveness.Start(exec.Command("/bin/sleep", "10"))
			if test.identity && liveness.process != nil {
				_ = liveness.process.Kill()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, _ = liveness.waiter.await(ctx)
				cancel()
			}
			if !errors.Is(err, ErrProcessContainmentIncomplete) && test.name != "identity" {
				t.Fatalf("validation failure = %v", err)
			}
			if test.identity && err == nil {
				t.Fatal("identity failure was accepted")
			}
			if test.identity && !signalBeforeWaiter {
				t.Fatal("record-activation group signal ordering was not observed")
			}
		})
	}
}

func TestQuiesceDarwinOriginalGroupBranches(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	if err := quiesceDarwinOriginalGroup(0, nil, nil, nil, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("invalid group = %v", err)
	}

	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if err := quiesceDarwinOriginalGroup(123, nil, nil, nil, time.Millisecond); err != nil {
		t.Fatalf("short group cleanup = %v", err)
	}
	if err := quiesceDarwinOriginalGroup(123, nil, nil, nil, time.Second); err != nil {
		t.Fatal(err)
	}

	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
	if err := quiesceDarwinOriginalGroup(123, nil, nil, nil, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("term failure = %v", err)
	}

	calls := 0
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		calls++
		if calls == 1 {
			return syscall.EPERM
		}
		if signal == 0 {
			return syscall.EIO
		}

		return nil
	}
	if err := quiesceDarwinOriginalGroup(123, nil, nil, nil, time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("probe failure = %v", err)
	}

	base := time.Now()
	tick := 0
	darwinSupervisorNow = func() time.Time {
		value := base.Add(time.Duration(tick) * 250 * time.Millisecond)
		tick++

		return value
	}
	darwinSupervisorSleep = func(time.Duration) {}
	calls = 0
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		calls++
		if signal == syscall.SIGKILL {
			return syscall.EIO
		}

		return syscall.EPERM
	}
	if err := quiesceDarwinOriginalGroup(123, nil, nil, nil, 5*time.Second); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("kill failure = %v", err)
	}

	tick = 0
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGKILL {
			return syscall.ESRCH
		}

		return syscall.EPERM
	}
	if err := quiesceDarwinOriginalGroup(123, nil, nil, nil, 5*time.Second); err != nil {
		t.Fatalf("kill ESRCH = %v", err)
	}
}

func TestDarwinSyscallFailuresStillReapDirectChild(t *testing.T) {
	for _, test := range []struct {
		name    string
		install func()
	}{
		{
			name: "term",
			install: func() {
				darwinSupervisorKill = func(pid int, signal syscall.Signal) error {
					if pid < 0 && signal == syscall.SIGTERM {
						return syscall.EIO
					}

					return syscall.EPERM
				}
			},
		},
		{
			name: "probe",
			install: func() {
				darwinSupervisorKill = func(pid int, signal syscall.Signal) error {
					if pid < 0 && signal == 0 {
						return syscall.EIO
					}

					return nil
				}
			},
		},
		{
			name: "kill",
			install: func() {
				base := time.Now()
				tick := 0
				darwinSupervisorNow = func() time.Time {
					value := base.Add(time.Duration(tick) * 250 * time.Millisecond)
					tick++

					return value
				}
				darwinSupervisorSleep = func(time.Duration) {}
				darwinSupervisorKill = func(pid int, signal syscall.Signal) error {
					if pid < 0 && signal == syscall.SIGKILL {
						return syscall.EIO
					}

					return syscall.EPERM
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveDarwinSupervisorSeams(t)
			command := exec.Command("/bin/sleep", "10")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waiter := newSupervisorWaiter(command, false)
			t.Cleanup(func() {
				_ = command.Process.Kill()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, _ = waiter.await(ctx)
				cancel()
			})

			test.install()
			err := quiesceDarwinOriginalGroup(command.Process.Pid, command.Process, waiter, nil, 5*time.Second)
			if !errors.Is(err, ErrProcessContainmentIncomplete) {
				t.Fatalf("syscall failure = %v", err)
			}
			select {
			case <-waiter.done:
			default:
				t.Fatal("direct-child waiter was not joined")
			}
		})
	}
}

func TestDarwinAbortUnvalidatedTimeout(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	darwinAbortWait = time.Millisecond
	waiter := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
	liveness := &livenessContainment{
		generation: &DarwinGeneration{},
		process:    &os.Process{Pid: 99999999},
		waiter:     waiter,
	}
	if err := liveness.abortUnvalidated(nil, errors.New("invalid")); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("abort timeout = %v", err)
	}
	if err := terminateAndReapDarwinDirectChild(nil, nil, time.Second); err == nil {
		t.Fatal("missing waiter was accepted")
	}
	if err := terminateAndReapDarwinDirectChild(nil, waiter, 0); err == nil {
		t.Fatal("zero direct-child deadline was accepted")
	}

	completed := newSupervisorWaiterFunc(nil, false)
	<-completed.done
	if _, done := awaitDarwinWaiterUntil(completed, time.Now().Add(-time.Second)); !done {
		t.Fatal("expired deadline missed completed waiter")
	}
	if err := reapDarwinDirectChild(nil, completed, time.Now().Add(time.Second)); err == nil {
		t.Fatal("missing process handle was accepted")
	}
	blocked := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
	if err := reapDarwinDirectChild(nil, blocked, time.Now().Add(time.Millisecond)); err == nil {
		t.Fatal("unreaped direct child was accepted")
	}
}

func TestDarwinAbortUnvalidatedEscalatesAndReapsTermIgnoringChild(t *testing.T) {
	if os.Getenv(darwinContainmentHelperEnv) == "term-ignore" {
		signal.Ignore(syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv(darwinContainmentReadyFileEnv), []byte("ready"), 0o600); err != nil {
			os.Exit(93)
		}
		for {
			time.Sleep(time.Second)
		}
	}

	preserveDarwinSupervisorSeams(t)
	darwinAbortWait = 2 * time.Second
	readyFile := filepath.Join(t.TempDir(), "term-ready")
	command := exec.Command(os.Args[0], "-test.run=^TestDarwinAbortUnvalidatedEscalatesAndReapsTermIgnoringChild$")
	command.Env = replaceTestEnv(os.Environ(), darwinContainmentHelperEnv, "term-ignore")
	command.Env = replaceTestEnv(command.Env, darwinContainmentReadyFileEnv, readyFile)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waiter := newSupervisorWaiter(command, true)
	t.Cleanup(func() {
		_ = command.Process.Kill()
		waiter.start()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = waiter.await(ctx)
		cancel()
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyFile); err != nil {
		t.Fatalf("TERM-ignore helper did not become ready: %v", err)
	}

	liveness := &livenessContainment{
		generation: &DarwinGeneration{},
		process:    command.Process,
		waiter:     waiter,
	}
	started := time.Now()
	err := liveness.abortUnvalidated(command, errors.New("identity failed"))
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("abort result = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond || elapsed >= darwinAbortWait {
		t.Fatalf("TERM-ignore escalation did not honor the grace and outer deadline: %v, result=%v", elapsed, err)
	}
	select {
	case <-waiter.done:
	default:
		t.Fatal("TERM-ignore direct child was not reaped")
	}
}

func TestReleaseIndependentDarwinSupervisorWaiterBranches(t *testing.T) {
	preserveDarwinSupervisorSeams(t)
	if _, err := releaseIndependentSupervisorWaiter(nil, nil); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("missing direct child = %v", err)
	}
	if count, authoritative := (&livenessContainment{}).DescendantCount(); count != 0 || authoritative {
		t.Fatalf("descendant count = %d, %v", count, authoritative)
	}

	completedWaiter := func(waitErr error) *supervisorWaiter {
		waiter := newSupervisorWaiterFunc(func() error { return waitErr }, false)
		<-waiter.done

		return waiter
	}
	command := &exec.Cmd{Process: &os.Process{Pid: 99999991}}

	darwinSupervisorGetpgid = func(pid int) (int, error) { return pid, nil }
	if pgid, err := releaseIndependentSupervisorWaiter(command, completedWaiter(nil)); err != nil || pgid != command.Process.Pid {
		t.Fatalf("valid direct child = %v", err)
	}

	darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	if _, err := releaseIndependentSupervisorWaiter(command, completedWaiter(nil)); err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("absent fast exit = %v", err)
	}

	calls := 0
	signalBeforeWaiter := false
	pausedWaiter := newSupervisorWaiterFunc(nil, true)
	darwinSupervisorKill = func(_ int, signal syscall.Signal) error {
		calls++
		if calls == 1 && signal == 0 {
			return nil
		}
		select {
		case <-pausedWaiter.begin:
			t.Fatal("independent-supervisor waiter started before the first group signal")
		default:
			signalBeforeWaiter = true
		}

		return syscall.ESRCH
	}
	if _, err := releaseIndependentSupervisorWaiter(command, pausedWaiter); err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("observable fast exit = %v", err)
	}
	if !signalBeforeWaiter {
		t.Fatal("independent-supervisor group signal ordering was not observed")
	}

	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
	if _, err := releaseIndependentSupervisorWaiter(command, completedWaiter(nil)); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("fast-exit probe failure = %v", err)
	}

	darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.EIO }
	if _, err := releaseIndependentSupervisorWaiter(command, completedWaiter(nil)); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("group inspection failure = %v", err)
	}
	darwinSupervisorGetpgid = func(pid int) (int, error) { return pid + 1, nil }
	if _, err := releaseIndependentSupervisorWaiter(command, completedWaiter(nil)); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("unexpected group = %v", err)
	}

	darwinFastExitWait = time.Millisecond
	darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.ESRCH }
	darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	blocked := &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
	if _, err := releaseIndependentSupervisorWaiter(command, blocked); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("fast-exit reap timeout = %v", err)
	}

	darwinAbortWait = time.Millisecond
	blocked = &supervisorWaiter{begin: make(chan struct{}), done: make(chan struct{})}
	if err := abortIndependentSupervisor(command.Process, blocked, errors.New("invalid")); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("abort timeout = %v", err)
	}
	if err := abortIndependentSupervisor(nil, completedWaiter(errors.New("wait")), errors.New("invalid")); !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("abort without process = %v", err)
	}
}

func TestStartServerDarwinDirectChildIdentityFailure(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	restoreOpenCodeClientSeams(t)
	preserveSupervisorGlobals(t)
	preserveDarwinSupervisorSeams(t)
	originalMkdirTemp := darwinRuntimeRootMkdirTemp
	t.Cleanup(func() { darwinRuntimeRootMkdirTemp = originalMkdirTemp })

	var generationRoot string
	darwinRuntimeRootMkdirTemp = func(parent, pattern string) (string, error) {
		root, err := originalMkdirTemp(parent, pattern)
		generationRoot = root

		return root, err
	}

	var cleanupCalls int
	openCodeRemoveAll = func(path string) error {
		cleanupCalls++

		return os.RemoveAll(path)
	}

	openCodeCommandContext = func(context.Context, string, ...string) *exec.Cmd {
		return exec.Command("/bin/sleep", "10")
	}
	darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.EIO }
	parent := t.TempDir()
	var reservations, releases int
	_, err := StartServer(context.Background(), StartOptions{
		ExistingXDG:              testXDGDirs(t),
		ProcessIsolation:         testProcessIsolation(),
		DarwinBestEffort:         true,
		ContainmentScratchParent: parent,
		ReserveContainmentScratch: func(context.Context) (func(), error) {
			reservations++

			return func() { releases++ }, nil
		},
	})
	if !errors.Is(err, ErrProcessContainmentIncomplete) {
		t.Fatalf("direct child identity error = %v", err)
	}
	if reservations != 1 || releases != 0 {
		t.Fatalf("generation reservation = acquired %d, released %d", reservations, releases)
	}
	if cleanupCalls != 0 {
		t.Fatalf("post-start containment failure attempted %d generation cleanups", cleanupCalls)
	}
	if generationRoot == "" || !pathWithin(parent, generationRoot) {
		t.Fatalf("generation root = %q, parent %q", generationRoot, parent)
	}
	if _, statErr := os.Stat(generationRoot); statErr != nil {
		t.Fatalf("retained generation root %q: %v", generationRoot, statErr)
	}
}

func TestDarwinSupervisorProofFailureBranches(t *testing.T) {
	for _, test := range []struct {
		name       string
		pidContent string
	}{
		{name: "missing native pid"},
		{name: "invalid native pid", pidContent: "not-a-pid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			preserveDarwinSupervisorSeams(t)
			root := t.TempDir()
			started := filepath.Join(root, "started")
			completion := filepath.Join(root, "complete")
			pidFile := filepath.Join(root, "pid")
			if err := writeSupervisorMarker(started); err != nil {
				t.Fatal(err)
			}
			if test.pidContent != "" {
				if err := os.WriteFile(pidFile, []byte(test.pidContent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			helper := filepath.Join(root, "liveness-without-readiness")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nsleep 0.05\nexit 1\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			supervisorExecutable = func() (string, error) { return helper, nil }
			supervisorInput = io.NopCloser(&emptyReader{})
			supervisorOutput = io.Discard
			supervisorError = io.Discard
			err := runGuardian(supervisorConfig{
				Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
				LifecycleKind: "runtime", DarwinBestEffort: true,
				Started: started, Completion: completion, NativePIDFile: pidFile,
			})
			if !errors.Is(err, ErrProcessContainmentIncomplete) {
				t.Fatalf("pre-readiness PID failure = %v", err)
			}
			if _, markerErr := os.Stat(completion); !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf("pre-readiness PID failure published completion: %v", markerErr)
			}
		})
	}

	t.Run("guardian direct child identity", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		root := t.TempDir()
		supervisorExecutable = func() (string, error) { return "/usr/bin/true", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd { return exec.Command("/bin/sleep", "10") }
		supervisorInput = io.NopCloser(&emptyReader{})
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorGetpgid = func(int) (int, error) { return 0, syscall.EIO }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: "runtime", DarwinBestEffort: true,
		})
		if !errors.Is(err, ErrProcessContainmentIncomplete) || !strings.Contains(err.Error(), "capture liveness supervisor identity") {
			t.Fatalf("guardian identity error = %v", err)
		}
	})

	t.Run("guardian cleanup proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		root := t.TempDir()
		helper := filepath.Join(root, "liveness")
		data := []byte("#!/bin/sh\nprintf '%s\\n' '" + supervisorReadyPrefix + "{\"nativePid\":123}' >&2\nexit 0\n")
		if err := os.WriteFile(helper, data, 0o700); err != nil {
			t.Fatal(err)
		}
		supervisorExecutable = func() (string, error) { return helper, nil }
		supervisorInput = io.NopCloser(&emptyReader{})
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: "runtime", DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		})
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("guardian proof error = %v", err)
		}
		if _, markerErr := os.Stat(filepath.Join(root, "complete")); !errors.Is(markerErr, os.ErrNotExist) {
			t.Fatalf("guardian fallback published completion: %v", markerErr)
		}
	})

	t.Run("guardian cannot replace liveness proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		root := t.TempDir()
		helper := filepath.Join(root, "liveness")
		data := []byte("#!/bin/sh\nprintf '%s\\n' '" + supervisorReadyPrefix + "{\"nativePid\":123}' >&2\nexit 0\n")
		if err := os.WriteFile(helper, data, 0o700); err != nil {
			t.Fatal(err)
		}
		completion := filepath.Join(root, "complete")
		supervisorExecutable = func() (string, error) { return helper, nil }
		supervisorInput = io.NopCloser(&emptyReader{})
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.ESRCH }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, ScratchParent: filepath.Dir(root),
			LifecycleKind: "runtime", DarwinBestEffort: true,
			Started: filepath.Join(root, "started"), Completion: completion,
		})
		if !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("guardian fallback result = %v", err)
		}
		if _, markerErr := os.Stat(completion); !errors.Is(markerErr, os.ErrNotExist) {
			t.Fatalf("guardian fallback published completion: %v", markerErr)
		}
	})

	t.Run("liveness natural cleanup", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		preserveDarwinSupervisorSeams(t)
		config := darwinSupervisorTestConfig(t)
		config.NativePath = "/bin/sh"
		config.NativeArgs = []string{"-c", "sleep 0.05"}
		config.NativeEnv = os.Environ()
		config.Home = filepath.Join(config.Scratch, "home")
		config.Started = filepath.Join(config.Scratch, "started")
		config.Completion = filepath.Join(config.Scratch, "complete")
		config.NativePIDFile = filepath.Join(config.Scratch, "pid")
		input, writer := io.Pipe()
		defer writer.Close()
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		darwinSupervisorKill = func(int, syscall.Signal) error { return syscall.EIO }
		if err := runLiveness(config); !errors.Is(err, ErrProcessContainmentIncomplete) {
			t.Fatalf("liveness proof error = %v", err)
		}
		if _, markerErr := os.Stat(config.Completion); !errors.Is(markerErr, os.ErrNotExist) {
			t.Fatalf("failed liveness cleanup published completion: %v", markerErr)
		}
	})
}

func TestDarwinNativeSetsidEscape(t *testing.T) {
	switch os.Getenv(darwinContainmentHelperEnv) {
	case "setsid-root":
		child := exec.Command(os.Args[0], "-test.run=^TestDarwinNativeSetsidEscape$")
		child.Env = replaceTestEnv(os.Environ(), darwinContainmentHelperEnv, "setsid-escaped")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(os.Getenv(darwinContainmentPIDFileEnv), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(92)
		}
		for {
			time.Sleep(time.Second)
		}
	case "setsid-escaped":
		for {
			time.Sleep(time.Second)
		}
	}

	preserveDarwinSupervisorSeams(t)
	config := darwinSupervisorTestConfig(t)
	liveness, err := openLivenessContainment(config)
	if err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	command := exec.Command(os.Args[0], "-test.run=^TestDarwinNativeSetsidEscape$")
	command.Env = replaceTestEnv(os.Environ(), darwinContainmentHelperEnv, "setsid-root")
	command.Env = replaceTestEnv(command.Env, darwinContainmentPIDFileEnv, pidFile)
	if startErr := liveness.Start(command); startErr != nil {
		t.Fatal(startErr)
	}

	escapedPID := 0
	defer func() {
		if escapedPID > 0 {
			_ = syscall.Kill(escapedPID, syscall.SIGKILL)
		}
		_ = liveness.Quiesce(command.Process.Pid, supervisorQuiesceWindow)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			escapedPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err == nil && escapedPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if escapedPID <= 0 {
		t.Fatal("setsid descendant did not publish its pid")
	}

	if err := liveness.Quiesce(command.Process.Pid, supervisorQuiesceWindow); err != nil {
		t.Fatalf("selected process-group boundary did not complete: %v", err)
	}
	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Fatalf("setsid descendant did not survive selected-boundary cleanup: %v", err)
	}
}

func replaceTestEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}

	return append(result, prefix+value)
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
func (*emptyReader) Close() error             { return nil }
