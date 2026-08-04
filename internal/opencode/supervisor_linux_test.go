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
}

type supervisorTestBuffer struct {
	mu   sync.Mutex
	data []byte
}

const linuxNoNewPrivilegesProofEnv = "ACP_GO_OPENCODE_TEST_NO_NEW_PRIVILEGES_PROOF"

func TestLinuxSupervisorChildInheritsNoNewPrivileges(t *testing.T) {
	proofPath := os.Getenv(linuxNoNewPrivilegesProofEnv)
	if proofPath != "" {
		native := exec.Command("/bin/sh", "-c", `awk '$1 == "NoNewPrivs:" { print $2 }' /proc/self/status > "$1"`, "sh", proofPath)
		require.NoError(t, startLinuxNoNewPrivileges(native.Start))
		require.NoError(t, native.Wait())

		return
	}

	proofPath = filepath.Join(t.TempDir(), "no-new-privileges")
	helper := exec.Command(os.Args[0], "-test.run=^TestLinuxSupervisorChildInheritsNoNewPrivileges$")
	helper.Env = append(os.Environ(), linuxNoNewPrivilegesProofEnv+"="+proofPath)
	output, err := helper.CombinedOutput()
	require.NoErrorf(t, err, "run no-new-privileges proof helper: %s", output)

	proof, err := os.ReadFile(proofPath)
	require.NoError(t, err)
	require.Equal(t, "1", strings.TrimSpace(string(proof)))
}

func TestLinuxSupervisorLaunchesFailClosedWhenNoNewPrivilegesCannotBeSet(t *testing.T) {
	preservePlatformSupervisorGlobals(t)
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
	root := t.TempDir()
	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))
	rootPIDPath := filepath.Join(root, "root.pid")
	descPIDPath := filepath.Join(root, "desc.pid")
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "descendant" ]; then
  echo "$$" > "$DESC_PID_FILE"
  trap '' TERM
  while :; do sleep 1; done
fi
setsid "$0" descendant &
echo "$$" > "$ROOT_PID_FILE"
trap '' TERM
while IFS= read -r line; do printf '%s\n' "$line"; done
`), 0o700))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv: append(os.Environ(),
			"ROOT_PID_FILE="+rootPIDPath,
			"DESC_PID_FILE="+descPIDPath,
		),
		Home:    home,
		Scratch: scratch,
	})
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(supervisorTestBuffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	configureOpenCodeProcess(cmd)
	require.NoError(t, cmd.Start())

	runtime := &supervisedNative{
		cmd:    cmd,
		stdin:  stdin,
		home:   home,
		stderr: stderr,
		cancel: cancel,
		proof:  proof,
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

		return err
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
