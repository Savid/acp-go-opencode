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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type supervisedNative struct {
	cmd          *exec.Cmd
	waiter       *supervisorWaiter
	stdin        io.WriteCloser
	home         string
	rootPID      int
	descPID      int
	livenessPID  int
	proof        *supervisorProof
	stderr       *supervisorTestBuffer
	cancel       context.CancelFunc
	attackPath   string
	forgePath    string
	isolationUID uint32
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

func TestSupervisedNativeIsolationDropsStandaloneFieldsAfterAuthorityAdoption(t *testing.T) {
	config := supervisorConfig{
		IsolationUID: 123, IsolationGID: 456,
		NativeEnv:           []string{"PATH=/usr/bin:/bin"},
		IdentityLock:        true,
		AuthorityDomain:     true,
		StandaloneOwnerID:   "standalone-owner",
		StandaloneStateRoot: "/var/lib/standalone-owner",
	}

	isolation := supervisedNativeIsolation(config)
	require.True(t, isolation.identityAuthorityAdopted)
	require.Empty(t, isolation.StandaloneOwnerID)
	require.Empty(t, isolation.StandaloneStateRoot)
	require.NoError(t, validateProcessIsolation(isolation))
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

func TestLinuxLivenessContainmentUsesCreatorThreadWait(t *testing.T) {
	preservePlatformSupervisorGlobals(t)

	command := exec.Command("/bin/sh", "-c", "exit 0")
	containment := &livenessContainment{}
	require.NoError(t, containment.Start(command))
	require.NoError(t, <-containment.Wait())
	require.NotNil(t, command.ProcessState)
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

func TestLinuxSupervisorConfigIsSealed(t *testing.T) {
	file, err := writeLinuxSupervisorConfig("", supervisorConfig{NativePath: "/bin/true"})
	require.NoError(t, err)
	defer file.Close()

	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	require.NoError(t, err)
	require.Equal(t, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL, seals)
	_, err = file.WriteAt([]byte("x"), 0)
	require.Error(t, err)
}

func TestPersistentProofFailureRetainsIdentityLockUntilRecovery(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

	lockPath := filepath.Join(t.TempDir(), "identity.lock")
	guardianFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(guardianFile.Fd()), unix.LOCK_EX|unix.LOCK_NB))

	survivorFD, err := unix.Dup(int(guardianFile.Fd()))
	require.NoError(t, err)
	survivorFile := os.NewFile(uintptr(survivorFD), "surviving-identity-lock")
	require.NotNil(t, survivorFile)
	require.NoError(t, (&agentIdentityLock{file: guardianFile}).Close())

	contender, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer contender.Close()
	require.ErrorIs(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)

	recoverProof := make(chan struct{})
	var attempts atomic.Int32
	supervisorLivenessQuiesce = func(*livenessContainment, int, time.Duration) error {
		attempts.Add(1)
		select {
		case <-recoverProof:
			return nil
		default:
			return ErrProcessContainmentIncomplete
		}
	}
	supervisorQuarantineRetry = retryLinuxLivenessContainment

	root := t.TempDir()
	config := supervisorConfig{
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		Quarantine: filepath.Join(root, "quarantine"), NativePIDFile: filepath.Join(root, "pid"),
		InventoryIdentity: filepath.Join(root, "inventory"),
	}
	require.NoError(t, writeSupervisorMarker(config.Started))

	livenessDone := make(chan error)
	go func() {
		livenessDone <- completeOrQuarantineLiveness(config, &livenessContainment{}, ErrProcessContainmentIncomplete)
		_ = survivorFile.Close()
	}()
	guardianDone := make(chan error, 1)
	go func() { guardianDone <- finishQuarantinedLiveness(livenessDone, config) }()
	require.Eventually(t, func() bool {
		_, err := os.Stat(config.Quarantine)

		return err == nil
	}, time.Second, 10*time.Millisecond)

	startedAt := time.Now()
	proofCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	proofErr := (&supervisorProof{
		started: config.Started, completion: config.Completion, quarantine: config.Quarantine,
		nativePIDFile: config.NativePIDFile, inventoryIdentity: config.InventoryIdentity,
	}).awaitCompletion(proofCtx)
	require.ErrorIs(t, proofErr, ErrProcessContainmentIncomplete)
	require.Less(t, time.Since(startedAt), time.Second)
	require.FileExists(t, config.Quarantine)
	require.FileExists(t, config.Started)
	require.ErrorIs(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK)
	require.GreaterOrEqual(t, attempts.Load(), int32(1))

	close(recoverProof)
	require.ErrorIs(t, <-guardianDone, ErrProcessContainmentIncomplete)
	require.NoFileExists(t, config.Quarantine)
	require.NoFileExists(t, config.Started)
	require.Eventually(t, func() bool {
		return unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil
	}, time.Second, 10*time.Millisecond)
}

func TestGuardianPersistentProofFailureQuarantinesUntilRecovery(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard

	recoverProof := make(chan struct{})
	supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error {
		select {
		case <-recoverProof:
			return nil
		default:
			return ErrProcessContainmentIncomplete
		}
	}
	supervisorGuardianQuarantineRetry = retryLinuxGuardianContainment

	root := t.TempDir()
	config := supervisorConfig{
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		Quarantine: filepath.Join(root, "quarantine"), NativePIDFile: filepath.Join(root, "pid"),
	}
	done := make(chan error, 1)
	go func() {
		done <- completeOrQuarantineGuardian(config, &guardianContainment{}, ErrProcessContainmentIncomplete)
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(config.Quarantine)

		return err == nil
	}, time.Second, 10*time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("guardian quarantine returned before kernel recovery: %v", err)
	default:
	}

	close(recoverProof)
	require.ErrorIs(t, <-done, ErrProcessContainmentIncomplete)
}

func TestSupervisorGuardianSIGKILLLeavesLivenessLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	stopSupervisedGroup(t, runtime.rootPID)
	stopSupervisedProcess(t, runtime.livenessPID)
	t.Cleanup(func() { _ = syscall.Kill(runtime.livenessPID, syscall.SIGCONT) })
	require.NoError(t, runtime.cmd.Process.Kill())
	runtime.waiter.start()
	<-runtime.waiter.result()

	claim, err := homelock.AcquireClaim(runtime.home)
	require.NoError(t, err, "guardian death must release only claim")
	defer func() { require.NoError(t, claim.Release()) }()
	_, err = homelock.AcquireLiveness(runtime.home)
	require.Error(t, err, "surviving liveness supervisor must retain its lock")
	require.NoError(t, syscall.Kill(runtime.descPID, 0), "setsid descendant must still be live during authority contention")
	assertAgentIdentityAuthorityLocked(t, runtime.isolationUID)
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGCONT))

	liveness := acquireLivenessEventually(t, runtime.home)
	require.NoError(t, liveness.Release())
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertAgentIdentityAuthorityReacquires(t, runtime.isolationUID)
}

func TestSupervisorGuardianSIGKILLBeforeNativeLaunchRefusesStartAndCompletesAfterECHILD(t *testing.T) {
	preserveSupervisorGlobals(t)
	preservePlatformSupervisorGlobals(t)

	peerRead, peerWrite, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = peerWrite.Close()
		_ = peerRead.Close()
	})

	oldValidator := supervisorValidateGuardianPeer
	oldPeer := supervisorGuardianPeer
	t.Cleanup(func() {
		supervisorValidateGuardianPeer = oldValidator
		supervisorGuardianPeer = oldPeer
	})
	supervisorGuardianPeer = peerRead
	peerChecks := 0
	supervisorValidateGuardianPeer = func(peer *os.File, done <-chan struct{}) error {
		peerChecks++
		peerErr := validateLinuxSupervisorGuardianPeer(peer, done)
		if peerChecks == 1 && peerErr == nil {
			if closeErr := peerWrite.Close(); closeErr != nil {
				return closeErr
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				return errors.New("guardian peer did not close")
			}
		}

		return peerErr
	}

	root := t.TempDir()
	config := supervisorConfig{
		NativePath:    "/bin/true",
		NativeArgs:    []string{"true"},
		NativeEnv:     os.Environ(),
		Home:          filepath.Join(root, "home"),
		Started:       filepath.Join(root, "started"),
		Completion:    filepath.Join(root, "completion"),
		Quarantine:    filepath.Join(root, "quarantine"),
		NativePIDFile: filepath.Join(root, "native-pid"),
	}
	var native *exec.Cmd
	supervisorExecCommand = func(string, ...string) *exec.Cmd {
		native = exec.Command("/bin/true")

		return native
	}

	err = runLiveness(config)
	require.ErrorContains(t, err, "guardian exited before native launch")
	require.Equal(t, 2, peerChecks, "guardian must be fenced again at the native Start boundary")
	require.NotNil(t, native)
	require.Nil(t, native.Process, "native command must not start after the guardian peer fence fails")
	require.FileExists(t, config.Completion)
	require.NoFileExists(t, config.NativePIDFile)
}

func TestSupervisorLivenessSIGKILLLeavesClaimLockedUntilTreeExit(t *testing.T) {
	runtime := startSupervisedNative(t)
	stopSupervisedGroup(t, runtime.rootPID)
	stopSupervisedProcess(t, runtime.cmd.Process.Pid)
	t.Cleanup(func() { _ = syscall.Kill(runtime.cmd.Process.Pid, syscall.SIGCONT) })
	require.NoError(t, syscall.Kill(runtime.livenessPID, syscall.SIGKILL))

	_, err := homelock.AcquireClaim(runtime.home)
	require.Error(t, err, "surviving guardian must retain claim while it kills the tree")
	require.NoError(t, syscall.Kill(runtime.descPID, 0), "setsid descendant must still be live during authority contention")
	assertAgentIdentityAuthorityLocked(t, runtime.isolationUID)
	require.NoError(t, syscall.Kill(runtime.cmd.Process.Pid, syscall.SIGCONT))
	require.Error(t, waitSupervisor(runtime, 10*time.Second), "guardian must report its killed liveness child")

	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
	assertAgentIdentityAuthorityReacquires(t, runtime.isolationUID)
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
	// Adopt the supervisor pair's orphans. Without this the kernel reparents a
	// killed guardian's liveness supervisor to init, which orphans the process
	// group configureIndependentSupervisor gave it; POSIX then resumes an
	// orphaned group that holds stopped jobs with SIGHUP and SIGCONT, so a
	// frozen survivor thaws before the test can observe it. The runner reaches
	// this only when the test's session differs from init's, which is why it
	// reproduces on a hosted VM and not under a container whose PID 1 shares
	// the session.
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0))

	const standaloneStateRoot = "/var/lib/acp-go-opencode-test"
	require.NoError(t, os.MkdirAll(standaloneStateRoot, 0o700))
	require.NoError(t, os.Chown(standaloneStateRoot, 65534, 65534))
	require.NoError(t, os.Chmod(standaloneStateRoot, 0o700))

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
	attackReadyPath := filepath.Join(root, "attacks.ready")
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
forge=denied; touch "$FORGE_PATH" 2>/dev/null && forge=allowed
config=denied; cat /proc/$parent/fd/3 >/dev/null 2>&1 && config=allowed
printf '%s %s %s %s %s %s %s %s\n' "$stop" "$kill_result" "$forge" "$config" "$(awk '$1 == "Uid:" {print $2}' /proc/$parent/status)" "$(id -u)" "$(id -g)" "$(awk '$1 == "Groups:" {print NF-1}' /proc/self/status)" > "$ATTACK_PATH"
touch "$ATTACK_READY_FILE"
trap '' TERM
while IFS= read -r line; do printf '%s\n' "$line"; done
`), 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	nativeEnv := append(os.Environ(),
		"ROOT_PID_FILE="+rootPIDPath,
		"DESC_PID_FILE="+descPIDPath,
		"ATTACK_PATH="+attackPath,
		"ATTACK_READY_FILE="+attackReadyPath,
		"FORGE_PATH="+forgePath,
	)
	const isolationUID = 65534
	cmd, proof, err := supervisorCommand(ctx, supervisorConfig{
		NativePath: script,
		NativeArgs: []string{"root"},
		NativeEnv:  nativeEnv,
		Home:       home,
		Scratch:    scratch,
		Isolation: &ProcessIsolation{
			UID: isolationUID, GID: 65534, BaseEnvironment: environmentMap(nativeEnv),
			StandaloneOwnerID: "test-owner", StandaloneStateRoot: standaloneStateRoot,
		},
	})
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr := new(supervisorTestBuffer)
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	waiter, err := startOpenCodeProcess(cmd)
	require.NoError(t, err)
	require.NoError(t, proof.closeInherited())

	runtime := &supervisedNative{
		cmd:          cmd,
		waiter:       waiter,
		stdin:        stdin,
		home:         home,
		stderr:       stderr,
		cancel:       cancel,
		proof:        proof,
		attackPath:   attackPath,
		forgePath:    forgePath,
		isolationUID: isolationUID,
	}
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		// The setsid descendant leads its own group and forks its own children,
		// so signalling its PID alone strands them under the agent identity
		// every test here shares.
		killSupervisedGroup(runtime.rootPID)
		killSupervisedGroup(runtime.descPID)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	runtime.rootPID = waitPIDFile(t, rootPIDPath, stderr)
	runtime.descPID = waitPIDFile(t, descPIDPath, stderr)
	waitFile(t, attackReadyPath)
	descendantSession, descendantGroup := processSessionAndGroup(t, runtime.descPID)
	require.Equal(t, runtime.descPID, descendantSession, "descendant must escape into a new session")
	require.Equal(t, runtime.descPID, descendantGroup, "descendant must escape into a new process group")
	require.NotEqual(t, runtime.rootPID, descendantGroup)
	runtime.livenessPID = parentPID(t, runtime.rootPID)
	require.Positive(t, runtime.livenessPID)

	return runtime
}

func assertAgentIdentityAuthorityLocked(t *testing.T, uid uint32) {
	t.Helper()
	for _, name := range []string{strconv.FormatUint(uint64(uid), 10) + ".lock", "domain.lock"} {
		fd, err := unix.Open(filepath.Join(linuxAgentIdentityNamespace, name), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		require.NoError(t, err)
		lockErr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if lockErr == nil {
			_ = unix.Close(fd)
			t.Fatalf("authority lock %s became available before survivor containment", name)
		}
		require.True(t, errors.Is(lockErr, unix.EWOULDBLOCK) || errors.Is(lockErr, unix.EAGAIN), "contend %s: %v", name, lockErr)
		require.NoError(t, unix.Close(fd))
	}
}

func assertAgentIdentityAuthorityReacquires(t *testing.T, uid uint32) {
	t.Helper()
	for _, name := range []string{strconv.FormatUint(uint64(uid), 10) + ".lock", "domain.lock"} {
		require.Eventually(t, func() bool {
			fd, err := unix.Open(filepath.Join(linuxAgentIdentityNamespace, name), unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return false
			}
			defer unix.Close(fd)

			return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil
		}, 5*time.Second, 10*time.Millisecond, "authority lock %s did not release after ECHILD", name)
	}
}

func shutdownSupervisedRuntime(t *testing.T, runtime *supervisedNative, ctx context.Context) {
	t.Helper()
	runtime.waiter.start()
	server := &openCodeServer{
		cmd: runtime.cmd, supervisorControl: runtime.stdin, supervisor: runtime.proof,
		waitDone: runtime.waiter.result(), runtimeShutdown: newRuntimeShutdownState(), runtimeClosed: make(chan struct{}),
	}
	require.NoError(t, server.Shutdown(ctx))
	runtime.cancel()
	assertProcessGone(t, runtime.rootPID)
	assertProcessGone(t, runtime.descPID)
	assertHomeReacquires(t, runtime.home)
}

func processSessionAndGroup(t *testing.T, pid int) (int, int) {
	t.Helper()
	fields, err := processStatFields(pid)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(fields), 4)
	group, err := strconv.Atoi(fields[2])
	require.NoError(t, err)
	session, err := strconv.Atoi(fields[3])
	require.NoError(t, err)

	return session, group
}

// taskStatFields returns the stat fields that follow the comm field, so a
// process name containing spaces or parentheses cannot shift the offsets.
func taskStatFields(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	closeParen := strings.LastIndexByte(string(raw), ')')
	if closeParen <= 0 {
		return nil, fmt.Errorf("%s has no comm field", path)
	}

	return strings.Fields(string(raw[closeParen+1:])), nil
}

func processStatFields(pid int) ([]string, error) {
	return taskStatFields(fmt.Sprintf("/proc/%d/stat", pid))
}

func stopSupervisedProcess(t *testing.T, pid int) {
	t.Helper()
	require.NoError(t, syscall.Kill(pid, syscall.SIGSTOP))
	awaitSupervisedProcessStopped(t, pid)
}

// stopSupervisedGroup stops the process group pid leads. Only the leader has to
// latch: the supervised tree keeps every other member in its own group.
func stopSupervisedGroup(t *testing.T, pid int) {
	t.Helper()
	require.NoError(t, syscall.Kill(-pid, syscall.SIGSTOP))
	awaitSupervisedProcessStopped(t, pid)
}

// awaitSupervisedProcessStopped blocks until every task of the process has
// entered the group stop. kill returns once the signal is queued, not once the
// target has acted on it, and a supervisor is a multi-threaded Go process whose
// thread group leader can read as stopped while another task still runs. A
// supervisor that is only nominally frozen still observes its peer's death,
// releases its runtime lock and quiesces the tree, which is precisely the
// behaviour these tests freeze it to exclude.
func awaitSupervisedProcessStopped(t *testing.T, pid int) {
	t.Helper()
	taskRoot := fmt.Sprintf("/proc/%d/task", pid)
	require.Eventually(t, func() bool {
		tasks, err := os.ReadDir(taskRoot)
		if err != nil || len(tasks) == 0 {
			return false
		}

		for _, task := range tasks {
			fields, statErr := taskStatFields(filepath.Join(taskRoot, task.Name(), "stat"))
			if statErr != nil || len(fields) == 0 || fields[0] != "T" {
				return false
			}
		}

		return true
	}, 5*time.Second, 5*time.Millisecond, "process %d did not enter the stopped state", pid)
}

func killSupervisedGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		info, err := os.Stat(path)

		return err == nil && info.Mode().IsRegular()
	}, 5*time.Second, 10*time.Millisecond, "file %s was not published", path)
}

func waitPIDFile(t *testing.T, path string, stderr *supervisorTestBuffer) int {
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
	t.Fatalf("PID file %s was not published; supervisor stderr: %s", path, stderr.String())

	return 0
}

func parentPID(t *testing.T, pid int) int {
	t.Helper()
	fields, err := processStatFields(pid)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(fields), 2)
	parent, err := strconv.Atoi(fields[1])
	require.NoError(t, err)

	return parent
}

func waitSupervisor(runtime *supervisedNative, timeout time.Duration) error {
	runtime.waiter.start()

	select {
	case err := <-runtime.waiter.result():
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
