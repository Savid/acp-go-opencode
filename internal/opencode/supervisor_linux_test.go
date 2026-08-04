//go:build linux

package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
)

type supervisedNative struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	home        string
	rootPID     int
	descPID     int
	livenessPID int
	proof       *supervisorProof
	stderr      *supervisorTestBuffer
	cancel      context.CancelFunc
	attackPath  string
	forgePath   string
}

type supervisorTestBuffer struct {
	mu   sync.Mutex
	data []byte
}

const linuxSecurityLimitsProofEnv = "ACP_GO_OPENCODE_TEST_SECURITY_LIMITS_PROOF"

func TestLinuxSupervisorChildInheritsSecurityLimits(t *testing.T) {
	proofPath := os.Getenv(linuxSecurityLimitsProofEnv)
	if proofPath != "" {
		native := exec.Command("/bin/sh", "-c", `nnp=$(awk '$1 == "NoNewPrivs:" { print $2 }' /proc/self/status); printf '%s %s\n' "$nnp" "$(ulimit -c)" > "$1"`, "sh", proofPath)
		require.NoError(t, startLinuxSecurityLimited(native.Start))
		require.NoError(t, native.Wait())

		return
	}

	proofPath = filepath.Join(t.TempDir(), "security-limits")
	helper := exec.Command(os.Args[0], "-test.run=^TestLinuxSupervisorChildInheritsSecurityLimits$")
	helper.Env = append(os.Environ(), linuxSecurityLimitsProofEnv+"="+proofPath)
	output, err := helper.CombinedOutput()
	require.NoErrorf(t, err, "run security-limits proof helper: %s", output)

	proof, err := os.ReadFile(proofPath)
	require.NoError(t, err)
	require.Equal(t, "1 0", strings.TrimSpace(string(proof)))
}

func TestLinuxSupervisorLaunchesFailClosedWhenSecurityLimitsCannotBeSet(t *testing.T) {
	preservePlatformSupervisorGlobals(t)
	supervisorLinuxCoreLimit = func() error { return errors.New("setrlimit failed") }

	require.ErrorContains(t, startIndependentSupervisor(exec.Command("/bin/true")), "disable core dumps")
	require.ErrorContains(t, (&livenessContainment{}).Start(exec.Command("/bin/true")), "disable core dumps")

	supervisorLinuxCoreLimit = func() error { return nil }
	supervisorLinuxNoNewPrivileges = func() error { return errors.New("prctl failed") }

	require.ErrorContains(t, startIndependentSupervisor(exec.Command("/bin/true")), "disable privilege elevation")
	require.ErrorContains(t, (&livenessContainment{}).Start(exec.Command("/bin/true")), "disable privilege elevation")
}

func (buffer *supervisorTestBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, value...)

	return len(value), nil
}

func (buffer *supervisorTestBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	return string(buffer.data)
}

func TestSupervisorControlEOFKillsTreeBeforeUnlock(t *testing.T) {
	runtime := startSupervisedNative(t)
	_, err := homelock.Acquire(runtime.home)
	require.Error(t, err, "second claimant must fail while the tree is live")

	require.NoError(t, runtime.stdin.Close())
	require.NoError(t, waitSupervisor(runtime, 10*time.Second))
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func TestTrustedSupervisorDeniesNativeAuthorityAttacks(t *testing.T) {
	runtime := startSupervisedNative(t)
	raw, err := os.ReadFile(runtime.attackPath)
	require.NoError(t, err)
	require.Equal(t, []string{"denied", "denied", "denied", "denied", "0", "65534", "65534", "0"}, strings.Fields(string(raw)))
	require.NoFileExists(t, runtime.forgePath)

	require.NoError(t, runtime.stdin.Close())
	require.NoError(t, waitSupervisor(runtime, 10*time.Second))
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
}

func TestLinuxAgentIdentityLockSerializesAndCancels(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the trusted root supervisor identity")
	}

	uid := uint32(63000 + os.Getpid()%1000)
	first, err := acquireLinuxAgentIdentityLock(uid, strings.NewReader(""))
	require.NoError(t, err)
	defer first.Close()

	controlRead, controlWrite, err := os.Pipe()
	require.NoError(t, err)
	defer controlRead.Close()
	result := make(chan error, 1)
	go func() {
		lock, lockErr := acquireLinuxAgentIdentityLock(uid, controlRead)
		if lock != nil {
			_ = lock.Close()
		}
		result <- lockErr
	}()

	select {
	case err := <-result:
		t.Fatalf("contending identity lock completed early: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	require.NoError(t, controlWrite.Close())
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	case <-time.After(time.Second):
		t.Fatal("contending identity lock ignored closed control")
	}
}

func TestSupervisorGuardianSIGKILLLeavesLivenessLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	require.NoError(t, syscall.Kill(-runtime.rootPID, syscall.SIGSTOP))
	require.NoError(t, runtime.cmd.Process.Kill())
	_ = runtime.cmd.Wait()

	claim, err := homelock.AcquireClaim(runtime.home)
	require.NoError(t, err, "guardian death must release only claim")
	defer func() { require.NoError(t, claim.Release()) }()
	_, err = homelock.AcquireLiveness(runtime.home)
	require.Error(t, err, "surviving liveness supervisor must retain its lock")

	liveness := acquireLivenessEventually(t, runtime.home)
	require.NoError(t, liveness.Release())
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
}

func TestSupervisorLivenessSIGKILLLeavesClaimLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	require.NoError(t, syscall.Kill(-runtime.rootPID, syscall.SIGSTOP))
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGKILL))

	_, err := homelock.AcquireClaim(runtime.home)
	require.Error(t, err, "surviving guardian must retain claim while it kills the tree")
	require.Error(t, waitSupervisor(runtime, 10*time.Second), "guardian must report its killed liveness child")

	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func TestCancelledShutdownContainsStubbornSetsidDescendant(t *testing.T) {
	runtime := startSupervisedNative(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownSupervisedRuntime(t, runtime, ctx)
}

func TestTurnTimeoutShutdownContainsStubbornSetsidDescendant(t *testing.T) {
	runtime := startSupervisedNative(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	shutdownSupervisedRuntime(t, runtime, ctx)
}

func startSupervisedNative(t *testing.T) *supervisedNative {
	t.Helper()
	root, err := os.MkdirTemp("", "acp-go-opencode-authority-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	require.NoError(t, os.Chmod(root, 0o777))
	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	rootPIDPath := filepath.Join(root, "root.pid")
	descPIDPath := filepath.Join(root, "desc.pid")
	attackPath := filepath.Join(root, "attacks")
	forgePath := filepath.Join(linuxSupervisorProofNamespace, fmt.Sprintf("native-forge-%d", os.Getpid()))
	_ = os.Remove(forgePath)
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "descendant" ]; then
  echo "$$" > "$DESC_PID_FILE"
  trap '' TERM
  while :; do sleep 1; done
fi
setsid "$0" descendant &
echo "$$" > "$ROOT_PID_FILE"
parent=$PPID
stop=denied; kill -STOP "$parent" 2>/dev/null && stop=allowed
kill_result=denied; kill -KILL "$parent" 2>/dev/null && kill_result=allowed
forge=denied; : > "$FORGE_PATH" 2>/dev/null && forge=allowed
config=denied; cat /proc/$parent/fd/3 >/dev/null 2>&1 && config=allowed
printf '%s %s %s %s %s %s %s %s\n' "$stop" "$kill_result" "$forge" "$config" "$(awk '$1 == "Uid:" {print $2}' /proc/$parent/status)" "$(id -u)" "$(id -g)" "$(awk '$1 == "Groups:" {print NF-1}' /proc/self/status)" > "$ATTACK_PATH"
trap '' TERM
while IFS= read -r line; do printf '%s\n' "$line"; done
`), 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	nativeEnv := append(os.Environ(),
		"ROOT_PID_FILE="+rootPIDPath,
		"DESC_PID_FILE="+descPIDPath,
		"ATTACK_PATH="+attackPath,
		"FORGE_PATH="+forgePath,
	)
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv:  nativeEnv,
		Home:       home,
		Scratch:    scratch,
		Isolation:  &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: environmentMap(nativeEnv)},
	})
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(supervisorTestBuffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	configureOpenCodeProcess(cmd)
	require.NoError(t, cmd.Start())
	require.NoError(t, proof.closeInherited())

	runtime := &supervisedNative{
		cmd:        cmd,
		stdin:      stdin,
		home:       home,
		stderr:     stderr,
		cancel:     cancel,
		proof:      proof,
		attackPath: attackPath,
		forgePath:  forgePath,
	}
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		if runtime.rootPID > 0 {
			_ = syscall.Kill(-runtime.rootPID, syscall.SIGKILL)
		}
		if runtime.descPID > 0 {
			_ = syscall.Kill(runtime.descPID, syscall.SIGKILL)
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	runtime.rootPID = waitPIDFile(t, rootPIDPath)
	runtime.descPID = waitPIDFile(t, descPIDPath)
	descendantSession, descendantGroup := processSessionAndGroup(t, runtime.descPID)
	require.Equal(t, runtime.descPID, descendantSession, "descendant must escape into a new session")
	require.Equal(t, runtime.descPID, descendantGroup, "descendant must escape into a new process group")
	require.NotEqual(t, runtime.rootPID, descendantGroup)
	runtime.livenessPID = parentPID(t, runtime.rootPID)
	require.Positive(t, runtime.livenessPID)

	return runtime
}

func shutdownSupervisedRuntime(t *testing.T, runtime *supervisedNative, ctx context.Context) {
	t.Helper()
	waitDone := make(chan error, 1)
	go func() { waitDone <- runtime.cmd.Wait() }()
	server := &openCodeServer{
		cmd: runtime.cmd, supervisorControl: runtime.stdin, supervisor: runtime.proof,
		waitDone: waitDone, runtimeShutdown: newRuntimeShutdownState(), runtimeClosed: make(chan struct{}),
	}
	require.NoError(t, server.Shutdown(ctx))
	runtime.cancel()
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func processSessionAndGroup(t *testing.T, pid int) (int, int) {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	closeParen := strings.LastIndexByte(string(raw), ')')
	require.Greater(t, closeParen, 0)
	fields := strings.Fields(string(raw[closeParen+1:]))
	require.GreaterOrEqual(t, len(fields), 4)
	group, err := strconv.Atoi(fields[2])
	require.NoError(t, err)
	session, err := strconv.Atoi(fields[3])
	require.NoError(t, err)

	return session, group
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not published", path)

	return 0
}

func parentPID(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	closeParen := strings.LastIndexByte(string(raw), ')')
	require.Greater(t, closeParen, 0)
	fields := strings.Fields(string(raw[closeParen+1:]))
	require.GreaterOrEqual(t, len(fields), 2)
	parent, err := strconv.Atoi(fields[1])
	require.NoError(t, err)

	return parent
}

func waitSupervisor(runtime *supervisedNative, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- runtime.cmd.Wait() }()
	select {
	case err := <-done:
		runtime.cancel()

		proofCtx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()

		return errors.Join(err, runtime.proof.awaitCompletion(proofCtx))
	case <-time.After(timeout):
		return fmt.Errorf("supervisor did not exit; stderr=%s", runtime.stderr.String())
	}
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived supervisor containment", pid)
}

func assertHomeReacquires(t *testing.T, home string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		lock, err := homelock.Acquire(home)
		if err == nil {
			require.NoError(t, lock.Release())

			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("OpenCode home %q did not become claimable after tree exit", home)
}

func acquireLivenessEventually(t *testing.T, home string) *homelock.Lock {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lock, err := homelock.AcquireLiveness(home)
		if err == nil {
			return lock
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("liveness lock was not released after tree quiescence")

	return nil
}
