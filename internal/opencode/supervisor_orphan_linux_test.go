//go:build linux

package opencode

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireNoTestSubreaper refuses to run a containment proof from a process that
// would adopt the supervisor pair's orphans. A subreaper here keeps a killed
// guardian's liveness supervisor parented inside this session, so the group
// never orphans and the kernel signal the production topology turns on is never
// sent. This fails rather than skips: a skip is as fatal as a failure in the
// privileged lanes, and a proof that quietly did not run is worse than both.
func requireNoTestSubreaper(t *testing.T) {
	t.Helper()
	require.Zero(t, processSubreaper(t),
		"this process adopts the supervisor pair's orphans, so nothing observed here is production topology")
}

func processSubreaper(t *testing.T) int {
	t.Helper()

	var subreaper int

	require.NoError(t, unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&subreaper)), 0, 0, 0))

	return subreaper
}

// preserveProcessSubreaper restores the flag a case set on the test host. The
// production containment constructors make whatever process calls them a
// subreaper, and a test host that keeps it silently adopts the supervisor
// pair's orphans for every case that runs afterwards.
func preserveProcessSubreaper(t *testing.T) {
	t.Helper()

	previous := processSubreaper(t)
	t.Cleanup(func() {
		require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0))
	})
}

// startOrdinarySupervisedNative starts the policy-omitted guardian/liveness
// pair over a native script that escapes into its own session and ignores
// SIGTERM. Omission needs no privilege, so the same real topology — an
// independent liveness process group inside the guardian's session — is
// reachable from any account.
func startOrdinarySupervisedNative(t *testing.T) *supervisedNative {
	t.Helper()
	requireNoTestSubreaper(t)

	root, err := os.MkdirTemp("", "acp-go-opencode-ordinary-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	home := filepath.Join(root, "home")
	scratch := filepath.Join(root, "scratch")
	require.NoError(t, os.MkdirAll(scratch, 0o700))

	rootPIDPath := filepath.Join(root, "root.pid")
	descPIDPath := filepath.Join(root, "desc.pid")
	identityPath := filepath.Join(root, "identity")
	environmentPath := filepath.Join(root, "environment")
	readyPath := filepath.Join(root, "ready")
	script := filepath.Join(root, "native.sh")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = "descendant" ]; then
  echo "$$" > "$DESC_PID_FILE"
  trap '' TERM
  while :; do sleep 1; done
fi
setsid "$0" descendant &
echo "$$" > "$ROOT_PID_FILE"
printf '%s %s %s\n' "$(id -u)" "$(id -g)" "$(id -G)" > "$IDENTITY_FILE"
env > "$ENVIRONMENT_FILE"
touch "$READY_FILE"
trap '' TERM
while IFS= read -r line; do printf '%s\n' "$line"; done
`), 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

	nativeEnvironment, err := buildProcessEnvironmentFrom(nil, nil, map[string]string{
		"ROOT_PID_FILE":    rootPIDPath,
		"DESC_PID_FILE":    descPIDPath,
		"IDENTITY_FILE":    identityPath,
		"ENVIRONMENT_FILE": environmentPath,
		"READY_FILE":       readyPath,
	})
	require.NoError(t, err)

	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativeExecutable: testNativeExecutable(t, script),
		NativeArgs:       []string{"root"},
		NativeEnv:        envMapToSlice(nativeEnvironment),
		Home:             home,
		Scratch:          scratch,
	})
	require.NoError(t, err)
	require.Nil(t, cmd.SysProcAttr, "ordinary execution requests no credential change")

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(supervisorTestBuffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	waiter, err := startOpenCodeProcess(cmd)
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())

	uid := uint32(os.Geteuid()) // Kernel IDs fit the wire width.
	supervised := &supervisedNative{
		cmd: cmd, waiter: waiter, stdin: stdin, root: root, home: home,
		stderr: stderr, cancel: cancel, proof: proof, isolationUID: uid,
		identityPath: identityPath, environmentPath: environmentPath,
	}
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		killSupervisedGroup(supervised.rootPID)
		killSupervisedGroup(supervised.descPID)

		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	supervised.rootPID = waitPIDFile(t, rootPIDPath, stderr)
	supervised.descPID = waitPIDFile(t, descPIDPath, stderr)
	waitFile(t, readyPath)
	supervised.livenessPID = parentPID(t, supervised.rootPID)
	require.Positive(t, supervised.livenessPID)

	return supervised
}

// hangupCanaryScript places a second member in the liveness supervisor's
// process group and reports the kernel's orphan signals from inside it. The
// seed shell is the process the test can place; the shell it backgrounds
// inherits the group and outlives it, so the canary's parent becomes init and
// the group's orphan test is not spoiled by this session.
const hangupCanaryScript = `sh -c 'trap "printf hangup > $CANARY_RECEIPT_FILE; exit 0" HUP
echo $$ > $CANARY_PID_FILE
while :; do sleep 1; done' &
`

// startHangupCanary returns the receipt path the canary writes when the kernel
// delivers SIGHUP and SIGCONT to its group. The canary is left stopped, which
// is the condition the orphan rule requires before it signals at all, and it
// catches SIGHUP so that delivery leaves evidence instead of only a corpse.
func startHangupCanary(t *testing.T, root string, group int) string {
	t.Helper()

	receipt := filepath.Join(root, "canary.hangup")
	pidPath := filepath.Join(root, "canary.pid")

	seed := exec.Command("/bin/sh", "-c", hangupCanaryScript)
	seed.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	seed.Env = append(os.Environ(), "CANARY_RECEIPT_FILE="+receipt, "CANARY_PID_FILE="+pidPath)
	require.NoError(t, seed.Run())

	pid := waitPIDFile(t, pidPath, new(supervisorTestBuffer))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	_, canaryGroup := processSessionAndGroup(t, pid)
	require.Equal(t, group, canaryGroup, "the canary must observe the liveness supervisor's own process group")

	stopSupervisedProcess(t, pid)

	return receipt
}

func processIsLive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// TestSupervisorGuardianSIGKILLOrphansTheLivenessGroupWithoutHangingItUp is the
// unsuppressed orphan proof. Nothing here adopts the pair's orphans, so a killed
// guardian reparents the liveness supervisor out of this session exactly as it
// does in production, its process group orphans, and the kernel resumes that
// group with SIGHUP followed by SIGCONT because a member is stopped. A canary
// sharing the group records the delivery, so the signal is observed rather than
// assumed.
//
// The supervisor must survive that hangup with its locks, quiesce only when the
// guardian's control channel closes, and exit behind its completion proof. Under
// the default disposition it would die at the SIGHUP instead, stranding the
// setsid descendant and the home locks.
func TestSupervisorGuardianSIGKILLOrphansTheLivenessGroupWithoutHangingItUp(t *testing.T) {
	runtime := startOrdinarySupervisedNative(t)
	guardianPID := runtime.cmd.Process.Pid

	// An unauthenticated hangup decides nothing while the guardian is alive.
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGHUP))
	require.NoError(t, syscall.Kill(guardianPID, syscall.SIGHUP))
	require.Never(t, func() bool {
		_, err := os.Stat(runtime.proof.completion)

		return err == nil || !processIsLive(guardianPID) || !processIsLive(runtime.livenessPID)
	}, 500*time.Millisecond, 25*time.Millisecond,
		"SIGHUP must neither terminate a supervisor nor start quiescence")
	require.True(t, processIsLive(runtime.descPID), "the contained tree must outlive an unauthenticated hangup")

	_, err := homelock.AcquireLiveness(runtime.home)
	require.Error(t, err, "a hung-up supervisor must still hold its liveness lock")

	// The tree ignores SIGTERM and is frozen, so quiescence spends its whole
	// graceful phase there and the survivor can be observed while it works.
	stopSupervisedGroup(t, runtime.rootPID)
	receipt := startHangupCanary(t, runtime.root, runtime.livenessPID)

	require.NoError(t, runtime.cmd.Process.Kill())
	runtime.waiter.start()
	<-runtime.waiter.result()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(receipt)

		return statErr == nil
	}, 10*time.Second, 5*time.Millisecond,
		"the kernel delivered no SIGHUP to the orphaned liveness group: this environment reparents the "+
			"supervisor inside its own session, so the orphan topology this proof is about never formed")

	require.True(t, processIsLive(runtime.livenessPID), "SIGHUP must not terminate the orphaned liveness supervisor")
	_, err = homelock.AcquireLiveness(runtime.home)
	require.Error(t, err, "the orphaned supervisor must still hold its liveness lock")
	require.True(t, processIsLive(runtime.descPID), "the contained tree must outlive the hangup")

	claim, err := homelock.AcquireClaim(runtime.home)
	require.NoError(t, err, "guardian death releases only the claim")
	require.NoError(t, claim.Release())

	require.Eventually(t, func() bool {
		return !processIsLive(runtime.livenessPID)
	}, 20*time.Second, 10*time.Millisecond, "the orphaned supervisor never finished containment")

	// The completion proof is written only after the whole tree returns ECHILD,
	// so its presence at the survivor's exit is what says the boundary held.
	require.FileExists(t, runtime.proof.completion)
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}
