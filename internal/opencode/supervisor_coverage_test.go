//go:build linux || darwin || freebsd || openbsd

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
	"github.com/stretchr/testify/require"
)

type supervisorErrorWriter struct{ err error }

func (writer supervisorErrorWriter) Write([]byte) (int, error) { return 0, writer.err }

type supervisorCloseBuffer struct {
	bytes.Buffer
	closed bool
}

type errorSupervisorIdentityLock struct{ err error }

func (lock errorSupervisorIdentityLock) Close() error       { return lock.err }
func (errorSupervisorIdentityLock) InheritedFile() *os.File { return nil }

type duplicateSupervisorCapability struct {
	file *os.File
	err  error
}

func (capability duplicateSupervisorCapability) Duplicate() (*os.File, error) {
	return capability.file, capability.err
}

func (buffer *supervisorCloseBuffer) Close() error {
	buffer.closed = true

	return nil
}

func preserveSupervisorGlobals(t *testing.T) {
	t.Helper()
	preservePlatformSupervisorGlobals(t)
	oldExecutable := supervisorExecutable
	oldCommand := supervisorExecCommand
	oldRandRead := supervisorRandRead
	oldChmod := supervisorChmod
	oldOpenFile := supervisorOpenFile
	oldPipe := supervisorPipe
	oldCreateTemp := supervisorCreateTemp
	oldRemove := supervisorRemove
	oldOpen := supervisorOpen
	oldEncode := supervisorEncodeConfig
	oldGuardianContainment := supervisorNewGuardianContainment
	oldGuardianName := supervisorGuardianName
	oldGuardianQuiesce := supervisorGuardianQuiesce
	oldLivenessContainment := supervisorOpenLivenessContainment
	oldLivenessQuiesce := supervisorLivenessQuiesce
	oldQuarantineRetry := supervisorQuarantineRetry
	oldGuardianQuarantineRetry := supervisorGuardianQuarantineRetry
	oldReleaseWaiter := supervisorReleaseIndependentWaiter
	oldStartIndependent := supervisorStartIndependent
	oldInput := supervisorInput
	oldOutput := supervisorOutput
	oldError := supervisorError
	oldExit := supervisorExit
	oldProcessSnapshot := supervisorProcessSnapshot
	oldInheritedFile := supervisorInheritedFile
	oldWriteConfig := supervisorWriteConfig
	oldMarkerRoot := supervisorMarkerRoot
	oldAcquireIdentityAuthority := supervisorAcquireIdentityAuthority
	oldVerifyTrustedIdentity := supervisorVerifyTrustedIdentity
	oldAdoptIdentityLock := supervisorAdoptIdentityLock
	oldAdoptAuthorityDomain := supervisorAdoptAuthorityDomain
	oldValidateAdoptedAuthority := supervisorValidateAdoptedAuthority
	oldGuardianPeer := supervisorGuardianPeer
	oldValidateGuardianPeer := supervisorValidateGuardianPeer
	t.Cleanup(func() {
		supervisorExecutable = oldExecutable
		supervisorExecCommand = oldCommand
		supervisorRandRead = oldRandRead
		supervisorChmod = oldChmod
		supervisorOpenFile = oldOpenFile
		supervisorPipe = oldPipe
		supervisorCreateTemp = oldCreateTemp
		supervisorRemove = oldRemove
		supervisorOpen = oldOpen
		supervisorEncodeConfig = oldEncode
		supervisorNewGuardianContainment = oldGuardianContainment
		supervisorGuardianName = oldGuardianName
		supervisorGuardianQuiesce = oldGuardianQuiesce
		supervisorOpenLivenessContainment = oldLivenessContainment
		supervisorLivenessQuiesce = oldLivenessQuiesce
		supervisorQuarantineRetry = oldQuarantineRetry
		supervisorGuardianQuarantineRetry = oldGuardianQuarantineRetry
		supervisorReleaseIndependentWaiter = oldReleaseWaiter
		supervisorStartIndependent = oldStartIndependent
		supervisorInput = oldInput
		supervisorOutput = oldOutput
		supervisorError = oldError
		supervisorExit = oldExit
		supervisorProcessSnapshot = oldProcessSnapshot
		supervisorInheritedFile = oldInheritedFile
		supervisorWriteConfig = oldWriteConfig
		supervisorMarkerRoot = oldMarkerRoot
		supervisorAcquireIdentityAuthority = oldAcquireIdentityAuthority
		supervisorVerifyTrustedIdentity = oldVerifyTrustedIdentity
		supervisorAdoptIdentityLock = oldAdoptIdentityLock
		supervisorAdoptAuthorityDomain = oldAdoptAuthorityDomain
		supervisorValidateAdoptedAuthority = oldValidateAdoptedAuthority
		supervisorGuardianPeer = oldGuardianPeer
		supervisorValidateGuardianPeer = oldValidateGuardianPeer
	})
}

func TestSupervisorConfigAndDispatchFailures(t *testing.T) {
	_, err := readSupervisorConfig(nil)
	require.ErrorContains(t, err, "missing private")

	root := t.TempDir()
	_, err = readSupervisorConfig(strings.NewReader("{"))
	require.ErrorContains(t, err, "decode private")

	_, err = readSupervisorConfig(strings.NewReader(`{"nativePath":"x"}`))
	require.ErrorContains(t, err, "incomplete")

	_, err = writeSupervisorConfig("", supervisorConfig{})
	require.ErrorContains(t, err, "scratch root is required")
	notDir := filepath.Join(root, "not-dir")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	_, err = writeSupervisorConfig(filepath.Join(notDir, "child"), supervisorConfig{})
	require.ErrorContains(t, err, "create private")

	config := supervisorConfig{NativePath: "x", Home: "h", Scratch: "s", IsolationUID: 1, IsolationGID: 2}
	path, err := writeSupervisorConfig(root, config)
	require.NoError(t, err)
	loaded, err := readSupervisorConfig(path)
	require.NoError(t, err)
	require.Equal(t, config.NativePath, loaded.NativePath)

	path, err = writeSupervisorConfig(root, config)
	require.NoError(t, err)
	err = runSupervisor("unknown", path)
	require.ErrorContains(t, err, "unknown internal mode")
}

func TestSupervisorCommandNonceEnvironmentAndProof(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	supervisorExecutable = func() (string, error) { return "", errors.New("lookup failed") }
	_, _, err := supervisorCommand(context.Background(), supervisorConfig{
		Scratch: root, Isolation: testProcessIsolation(),
	})
	require.ErrorContains(t, err, "resolve embedded")

	supervisorExecutable = os.Executable
	cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
		NativePath: "/usr/bin/true", Home: filepath.Join(root, "home"), Scratch: root,
		Isolation: testProcessIsolation(),
	})
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.Nil(t, cmd.Cancel, "trusted supervisor must outlive caller-context cancellation")
	require.NotNil(t, proof)
	require.NotEmpty(t, proof.inventoryIdentity)
	require.Equal(t, []string{supervisorModeEnv + "=" + supervisorModeGuardian}, cmd.Env)
	require.Equal(t, "/", cmd.Dir)
	if cmd.SysProcAttr != nil {
		require.Nil(t, cmd.SysProcAttr.Credential)
	}

	nonce, err := supervisorNonce()
	require.NoError(t, err)
	require.Len(t, nonce, 32)

	require.NoError(t, (*supervisorProof)(nil).awaitCompletion(context.Background()))
	require.ErrorIs(t, (&supervisorProof{
		started: filepath.Join(root, "never-started"), completion: filepath.Join(root, "never-completed"),
	}).awaitCompletion(context.Background()), ErrProcessContainmentIncomplete)

	started := filepath.Join(root, "started")
	completed := filepath.Join(root, "completed")
	require.NoError(t, writeSupervisorMarker(started))
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = writeSupervisorMarker(completed)
	}()
	require.NoError(t, (&supervisorProof{started: started, completion: completed}).awaitCompletion(context.Background()))
	require.NoFileExists(t, started)
	require.NoFileExists(t, completed)

	count, available := (*supervisorProof)(nil).processSnapshot()
	require.Zero(t, count)
	require.False(t, available)
	supervisorProcessSnapshot = func(path string) (int, bool) {
		require.Equal(t, proof.inventoryIdentity, path)

		return 3, true
	}
	count, available = proof.processSnapshot()
	require.Equal(t, 3, count)
	require.True(t, available)
}

func TestGuardianCleansQuarantineMarkersWithoutCaller(t *testing.T) {
	root := t.TempDir()
	config := supervisorConfig{
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		Quarantine: filepath.Join(root, "quarantine"), NativePIDFile: filepath.Join(root, "pid"),
		InventoryIdentity: filepath.Join(root, "inventory"),
	}
	for _, path := range []string{config.Started, config.Quarantine, config.NativePIDFile, config.InventoryIdentity} {
		require.NoError(t, writeSupervisorMarker(path))
	}

	livenessDone := make(chan error)
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- finishQuarantinedLiveness(livenessDone, config) }()
	require.FileExists(t, config.Quarantine)
	livenessDone <- nil
	require.ErrorIs(t, <-cleanupDone, ErrProcessContainmentIncomplete)

	for _, path := range []string{config.Started, config.Completion, config.Quarantine, config.NativePIDFile, config.InventoryIdentity} {
		require.NoFileExists(t, path)
	}
}

func TestSupervisorMarkerPIDReadyAndCopyUtilities(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	require.Error(t, writeSupervisorMarker(""))
	marker := filepath.Join(root, "marker")
	require.NoError(t, writeSupervisorMarker(marker))
	require.NoError(t, writeSupervisorMarker(marker))
	require.Error(t, writeSupervisorMarker(filepath.Join(marker, "child")))

	require.Error(t, writeNativePID("", 1))
	require.Error(t, writeNativePID(filepath.Join(root, "pid"), 0))
	pidPath := filepath.Join(root, "pid")
	require.NoError(t, writeNativePID(pidPath, 123))
	pid, err := readNativePID(pidPath)
	require.NoError(t, err)
	require.Equal(t, 123, pid)
	_, err = readNativePID(filepath.Join(root, "missing"))
	require.Error(t, err)
	require.NoError(t, os.WriteFile(pidPath, []byte("bad"), 0o600))
	_, err = readNativePID(pidPath)
	require.ErrorContains(t, err, "invalid")

	require.Error(t, writeSupervisorInventoryIdentity("", "job"))
	require.Error(t, writeSupervisorInventoryIdentity(filepath.Join(root, "identity"), ""))
	identityPath := filepath.Join(root, "identity")
	require.NoError(t, writeSupervisorInventoryIdentity(identityPath, "job-name"))
	identity, err := os.ReadFile(identityPath)
	require.NoError(t, err)
	require.Equal(t, "job-name\n", string(identity))
	require.Error(t, writeSupervisorInventoryIdentity(identityPath, "job-name"))
	require.Error(t, writeSupervisorInventoryIdentity(filepath.Join(identityPath, "child"), "job-name"))
	supervisorOpenFile = func(string, int, os.FileMode) (*os.File, error) {
		return os.OpenFile("/dev/full", os.O_WRONLY, 0)
	}
	identityErr := writeSupervisorInventoryIdentity(filepath.Join(root, "full"), "job-name")
	require.Error(t, identityErr)
	require.True(t,
		strings.Contains(identityErr.Error(), "create private") || strings.Contains(identityErr.Error(), "write private"),
		"unexpected identity error: %v", identityErr,
	)
	closed, err := os.CreateTemp(t.TempDir(), "closed-")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	supervisorOpenFile = func(string, int, os.FileMode) (*os.File, error) { return closed, nil }
	require.ErrorContains(t, writeSupervisorInventoryIdentity(filepath.Join(root, "closed"), "job-name"), "write private")

	count, available := querySupervisorProcessSnapshot(identityPath)
	require.Zero(t, count)
	require.False(t, available)

	_, err = parseSupervisorReady("bad")
	require.ErrorContains(t, err, "invalid readiness")
	_, err = parseSupervisorReady(supervisorReadyPrefix + "{")
	require.ErrorContains(t, err, "decode readiness")
	_, err = parseSupervisorReady(supervisorReadyPrefix + `{"nativePid":0}`)
	require.ErrorContains(t, err, "omitted")
	ready, err := parseSupervisorReady(supervisorReadyPrefix + `{"nativePid":123}`)
	require.NoError(t, err)
	require.Equal(t, 123, ready.NativePID)

	buffer := new(supervisorCloseBuffer)
	done := make(chan struct{}, 1)
	copySupervisorStream(buffer, strings.NewReader("payload"), done)
	<-done
	require.Equal(t, "payload", buffer.String())
	require.True(t, buffer.closed)

	tries := 0
	err = awaitQuiescence(func() error {
		tries++

		return errors.New("not yet")
	})
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Equal(t, 1, tries)
}

func TestRunLivenessControlEOFAndNativeFailure(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("hello\n")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	config := supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	}
	require.NoError(t, runLiveness(config))
	_, err := os.Stat(config.Completion)
	require.NoError(t, err)

	root = t.TempDir()
	supervisorInput = strings.NewReader("")
	config.Home = filepath.Join(root, "home")
	config.Scratch = root
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "native.pid")
	config.NativePath = filepath.Join(root, "missing")
	err = runLiveness(config)
	require.ErrorContains(t, err, "start contained native root")
}

func TestRunLivenessPublishAndPIDFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = supervisorErrorWriter{err: errors.New("write failed")}
	config := supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "while :; do sleep 1; done"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	}
	err := runLiveness(config)
	require.ErrorContains(t, err, "publish supervisor readiness")

	root = t.TempDir()
	supervisorError = io.Discard
	config.Home = filepath.Join(root, "home")
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "missing", "native.pid")
	err = runLiveness(config)
	require.ErrorContains(t, err, "write private native PID proof")
}

func TestRunGuardianHappyPathAndPreReadinessFailure(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	preserveSupervisorGlobals(t)
	withNeutralSupervisorIdentityHooks(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("payload\n")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	config := withTestSupervisorIdentity(supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	})
	require.NoError(t, runGuardian(config))

	root = t.TempDir()
	supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
	config.Home = filepath.Join(root, "home")
	config.Scratch = root
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "native.pid")
	err := runGuardian(config)
	require.ErrorContains(t, err, "failed before readiness")
}

func TestSupervisorBootstrapAndUnixContainmentBranches(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorExit = func(int) {}
	supervisorError = io.Discard
	t.Setenv(supervisorModeEnv, "")
	supervisorBootstrap()

	containmentConfig := supervisorConfig{
		DarwinBestEffort: true,
		ScratchParent:    t.TempDir(),
		LifecycleKind:    "runtime",
	}
	containmentConfig.Scratch = filepath.Join(containmentConfig.ScratchParent, "acp-go-opencode-runtime-test")
	require.NoError(t, os.Mkdir(containmentConfig.Scratch, 0o700))

	guardian, err := newGuardianContainment(containmentConfig)
	require.NoError(t, err)
	require.NoError(t, guardian.Close())
	liveness, err := openLivenessContainment(containmentConfig)
	require.NoError(t, err)
	require.NoError(t, liveness.Close())

	cmd := exec.Command("/usr/bin/true")
	configureIndependentSupervisor(cmd)
	require.True(t, cmd.SysProcAttr.Setpgid)

	require.ErrorContains(t, quiesceProcessGroup(0, time.Second), "required")
	oldKill := openCodeSyscallKill
	t.Cleanup(func() { openCodeSyscallKill = oldKill })
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, signalProcessGroup(123, syscall.SIGTERM))
	alive, err := processGroupAlive(123)
	require.NoError(t, err)
	require.False(t, alive)
	require.NoError(t, quiesceProcessGroup(123, time.Second))

	openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
		if signal == 0 {
			return syscall.EPERM
		}

		return nil
	}
	alive, err = processGroupAlive(123)
	require.NoError(t, err)
	require.True(t, alive)

	openCodeSyscallKill = func(int, syscall.Signal) error { return errors.New("probe failed") }
	_, err = processGroupAlive(123)
	require.ErrorContains(t, err, "probe native process group")
	require.Error(t, signalProcessGroup(123, syscall.SIGTERM))
}

func TestSupervisorBootstrapDispatchesMissingAndSuccessfulLiveness(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		t.Setenv(supervisorModeEnv, supervisorModeGuardian)
		supervisorInheritedFile = func(uintptr, string) *os.File { return nil }
		var exits []int
		supervisorExit = func(code int) { exits = append(exits, code) }
		supervisorError = io.Discard
		supervisorBootstrap()
		require.Equal(t, []int{1}, exits)
	})

	t.Run("missing guardian peer", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		t.Setenv(supervisorModeEnv, supervisorModeLiveness)
		configFile, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.NoError(t, err)
		supervisorInheritedFile = func(fd uintptr, _ string) *os.File {
			if fd == 3 {
				return configFile
			}

			return nil
		}
		var exits []int
		supervisorExit = func(code int) { exits = append(exits, code) }
		supervisorError = io.Discard
		supervisorBootstrap()
		require.Equal(t, []int{1}, exits)
	})

	t.Run("liveness", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withNeutralSupervisorIdentityHooks(t)
		t.Setenv(supervisorModeEnv, supervisorModeLiveness)
		root := t.TempDir()
		config := withTestSupervisorIdentity(supervisorConfig{
			NativePath: "/bin/sh", NativeArgs: []string{"-c", "sleep 0.01"}, NativeEnv: os.Environ(),
			Home: filepath.Join(root, "home"), Scratch: root, Started: filepath.Join(root, "started"),
			Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		configFile, err := writeSupervisorConfig(root, config)
		require.NoError(t, err)
		peer, err := os.CreateTemp(t.TempDir(), "peer")
		require.NoError(t, err)
		supervisorInheritedFile = func(fd uintptr, _ string) *os.File {
			if fd == 3 {
				return configFile
			}

			return peer
		}
		// The peer is a plain file here. Linux requires a live guardian pipe,
		// which only a real supervised launch provides and which the
		// supervised-native cases in supervisor_linux_test.go prove.
		supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error { return nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		var exits []int
		supervisorExit = func(code int) { exits = append(exits, code) }
		supervisorBootstrap()
		require.Equal(t, []int{0}, exits)
	})
}

func TestSupervisorDispatchIdentityAdoptionBranches(t *testing.T) {
	preserveSupervisorGlobals(t)
	validateAdoptedAuthority := supervisorValidateAdoptedAuthority
	valid := supervisorConfig{NativePath: "/bin/sh", NativeEnv: os.Environ(), Home: t.TempDir(), Scratch: t.TempDir(), IsolationUID: 1, IsolationGID: 2}
	encoded := func(config supervisorConfig) io.Reader {
		data, err := json.Marshal(config)
		require.NoError(t, err)

		return bytes.NewReader(data)
	}
	require.Error(t, runSupervisor(supervisorModeLiveness, strings.NewReader("{")))
	guardian := valid
	guardianRoot := t.TempDir()
	guardianFile := filepath.Join(guardianRoot, "file")
	require.NoError(t, os.WriteFile(guardianFile, []byte("x"), 0o600))
	guardian.Home = filepath.Join(guardianFile, "home")
	require.Error(t, runSupervisor(supervisorModeGuardian, encoded(guardian)))
	bad := valid
	bad.IdentityLock = true
	require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(bad)), "inconsistent")
	bad.AuthorityDomain = true
	supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) { return nil, errors.New("identity") }
	require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(bad)), "identity")
	supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) { return errorSupervisorIdentityLock{}, nil }
	supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) { return nil, errors.New("domain") }
	require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(bad)), "domain")
	supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) { return errorSupervisorIdentityLock{}, nil }
	supervisorValidateAdoptedAuthority = func(supervisorConfig) error { return errors.New("disposition") }
	require.ErrorContains(t, runSupervisor(supervisorModeLiveness, encoded(bad)), "disposition")
	supervisorValidateAdoptedAuthority = validateAdoptedAuthority
	supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
		return errorSupervisorIdentityLock{err: errors.New("close identity")}, nil
	}
	supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
		return errorSupervisorIdentityLock{err: errors.New("close domain")}, nil
	}
	err := runSupervisor(supervisorModeLiveness, encoded(bad))
	require.ErrorContains(t, err, "close identity")
	require.ErrorContains(t, err, "close domain")
}

func TestSupervisorQuarantineCompletionAndStreamBranches(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	completion := filepath.Join(root, "complete")
	require.NoError(t, completeOrQuarantineLiveness(supervisorConfig{Completion: completion}, nil, nil))
	require.FileExists(t, completion)
	proof := errors.New("proof")
	require.ErrorIs(t, completeOrQuarantineLiveness(supervisorConfig{}, nil, proof), proof)
	require.NoError(t, writeGuardianQuarantineMarker(supervisorConfig{}))
	guardianCompletion := filepath.Join(root, "guardian-complete")
	require.NoError(t, completeOrQuarantineGuardian(supervisorConfig{Completion: guardianCompletion}, nil, nil))
	require.FileExists(t, guardianCompletion)
	require.ErrorIs(t, completeOrQuarantineGuardian(supervisorConfig{}, nil, proof), proof)

	quarantine := filepath.Join(root, "quarantine")
	supervisorQuarantineRetry = func(*livenessContainment) error { return errors.New("retry") }
	supervisorInput, supervisorOutput, supervisorError = strings.NewReader(""), io.Discard, io.Discard
	err := completeOrQuarantineLiveness(supervisorConfig{Quarantine: quarantine}, nil, proof)
	require.ErrorContains(t, err, "retry")
	require.FileExists(t, quarantine)

	guardianQuarantine := filepath.Join(root, "guardian-quarantine")
	supervisorGuardianQuarantineRetry = func(*guardianContainment) error { return errors.New("guardian retry") }
	err = completeOrQuarantineGuardian(supervisorConfig{Quarantine: guardianQuarantine}, nil, proof)
	require.ErrorContains(t, err, "guardian retry")

	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	err = completeOrQuarantineLiveness(supervisorConfig{Quarantine: filepath.Join(notDirectory, "child")}, nil, proof)
	require.Error(t, err)
	err = completeOrQuarantineGuardian(supervisorConfig{Quarantine: filepath.Join(notDirectory, "child")}, nil, proof)
	require.Error(t, err)

	done := make(chan error, 1)
	done <- nil
	waitErr, quarantined, terminalErr := awaitLivenessTerminal(done, "")
	require.NoError(t, waitErr)
	require.False(t, quarantined)
	require.NoError(t, terminalErr)
	present := filepath.Join(root, "present")
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o600))
	waitErr, quarantined, terminalErr = awaitLivenessTerminal(make(chan error), present)
	require.NoError(t, waitErr)
	require.True(t, quarantined)
	require.NoError(t, terminalErr)
	waitErr, quarantined, terminalErr = awaitLivenessTerminal(make(chan error), filepath.Join(notDirectory, "child"))
	require.NoError(t, waitErr)
	require.False(t, quarantined)
	require.Error(t, terminalErr)

	input, err := os.CreateTemp(t.TempDir(), "input")
	require.NoError(t, err)
	output, err := os.CreateTemp(t.TempDir(), "output")
	require.NoError(t, err)
	errorFile, err := os.CreateTemp(t.TempDir(), "error")
	require.NoError(t, err)
	supervisorInput, supervisorOutput, supervisorError = input, output, errorFile
	closeSupervisorQuarantineStreams()
	require.Error(t, input.Close())
	require.Error(t, output.Close())
	require.Error(t, errorFile.Close())
}

func TestSupervisorDefaultHooksAndConfigWriteFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	withNeutralSupervisorIdentityHooks(t)
	isolation := testProcessIsolation()
	lock, domain, err := supervisorAcquireIdentityAuthority(
		isolation.UID, isolation.GID, isolation.StandaloneOwnerID, isolation.StandaloneStateRoot, strings.NewReader(""),
	)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	require.NoError(t, domain.Close())
	require.NoError(t, supervisorVerifyTrustedIdentity(isolation.UID))
	lock, err = supervisorAdoptIdentityLock(isolation.UID)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	domain, err = supervisorAdoptAuthorityDomain(isolation.UID)
	require.NoError(t, err)
	require.NoError(t, domain.Close())

	want := errors.New("filesystem")
	supervisorChmod = func(string, os.FileMode) error { return want }
	_, err = writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.ErrorIs(t, err, want)
	supervisorChmod = os.Chmod

	supervisorCreateTemp = func(string, string) (*os.File, error) { return nil, want }
	_, err = writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.ErrorIs(t, err, want)
	supervisorCreateTemp = os.CreateTemp

	closed, err := os.CreateTemp(t.TempDir(), "closed")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	supervisorCreateTemp = func(string, string) (*os.File, error) { return closed, nil }
	_, err = writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.ErrorContains(t, err, "secure private")
	supervisorCreateTemp = os.CreateTemp

	supervisorEncodeConfig = func(writer io.Writer, _ supervisorConfig) error {
		file, ok := writer.(*os.File)
		require.True(t, ok)
		require.NoError(t, file.Close())

		return nil
	}
	_, err = writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.ErrorContains(t, err, "rewind")
	supervisorEncodeConfig = func(writer io.Writer, config supervisorConfig) error {
		return json.NewEncoder(writer).Encode(config)
	}

	supervisorRemove = func(string) error { return want }
	_, err = writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.ErrorContains(t, err, "unlink")
}

func TestSupervisorCommandValidationAndCapabilityFailures(t *testing.T) {
	valid := func(t *testing.T) supervisorConfig {
		t.Helper()

		return supervisorConfig{Scratch: t.TempDir(), Isolation: testProcessIsolation()}
	}

	t.Run("context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, err := supervisorCommand(ctx, valid(t))
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("isolation", func(t *testing.T) {
		_, _, err := supervisorCommand(context.Background(), supervisorConfig{})
		require.Error(t, err)
	})
	t.Run("mixed capabilities", func(t *testing.T) {
		config := valid(t)
		config.Isolation.IdentityLock = duplicateSupervisorCapability{}
		_, _, err := supervisorCommand(context.Background(), config)
		require.ErrorContains(t, err, "together")
	})
	t.Run("trusted identity", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("identity")
		supervisorVerifyTrustedIdentity = func(uint32) error { return want }
		_, _, err := supervisorCommand(context.Background(), valid(t))
		require.ErrorIs(t, err, want)
	})
	t.Run("marker root", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("marker")
		supervisorMarkerRoot = func(supervisorConfig) (string, error) { return "", want }
		_, _, err := supervisorCommand(context.Background(), valid(t))
		require.ErrorIs(t, err, want)
	})
	t.Run("config write", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("config")
		supervisorWriteConfig = func(string, supervisorConfig) (*os.File, error) { return nil, want }
		_, _, err := supervisorCommand(context.Background(), valid(t))
		require.ErrorIs(t, err, want)
	})
	t.Run("executable", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("executable")
		supervisorExecutable = func() (string, error) { return "", want }
		_, _, err := supervisorCommand(context.Background(), valid(t))
		require.ErrorIs(t, err, want)
	})
	t.Run("executable policy", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorExecutable = func() (string, error) { return "relative", nil }
		_, _, err := supervisorCommand(context.Background(), valid(t))
		require.ErrorContains(t, err, "through process policy")
	})
	t.Run("identity duplicate", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		config := valid(t)
		borrowedTestIsolation(config.Isolation)
		want := errors.New("duplicate")
		config.Isolation.IdentityLock = duplicateSupervisorCapability{err: want}
		config.Isolation.AuthorityDomain = duplicateSupervisorCapability{}
		_, _, err := supervisorCommand(context.Background(), config)
		require.ErrorIs(t, err, want)
	})
	t.Run("domain duplicate", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		config := valid(t)
		borrowedTestIsolation(config.Isolation)
		file, err := os.CreateTemp(t.TempDir(), "identity")
		require.NoError(t, err)
		want := errors.New("duplicate")
		config.Isolation.IdentityLock = duplicateSupervisorCapability{file: file}
		config.Isolation.AuthorityDomain = duplicateSupervisorCapability{err: want}
		_, _, err = supervisorCommand(context.Background(), config)
		require.ErrorIs(t, err, want)
	})
	t.Run("borrowed capabilities", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		config := valid(t)
		borrowedTestIsolation(config.Isolation)
		identity, err := os.CreateTemp(t.TempDir(), "identity")
		require.NoError(t, err)
		domain, err := os.CreateTemp(t.TempDir(), "domain")
		require.NoError(t, err)
		config.Isolation.IdentityLock = duplicateSupervisorCapability{file: identity}
		config.Isolation.AuthorityDomain = duplicateSupervisorCapability{file: domain}
		_, proof, err := supervisorCommand(context.Background(), config)
		require.NoError(t, err)
		require.Len(t, proof.inherited, 3)
		require.NoError(t, proof.closeInherited())
	})
}

func TestSupervisorProofAndIdentityAuthorityRemainingBranches(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	proof := &supervisorProof{completion: filepath.Join(root, "missing"), quarantine: filepath.Join(notDirectory, "child"), started: filepath.Join(root, "started")}
	require.ErrorContains(t, proof.awaitCompletion(context.Background()), "quarantine proof")
	quarantined := filepath.Join(root, "quarantined")
	require.NoError(t, os.WriteFile(quarantined, []byte("x"), 0o600))
	require.ErrorContains(t, (&supervisorProof{completion: filepath.Join(root, "missing-completion"), quarantine: quarantined}).awaitCompletion(context.Background()), "quarantining")
	require.False(t, func() bool {
		present, _ := (*supervisorProof)(nil).quarantineDetected()

		return present
	}())
	(*supervisorProof)(nil).removeTerminalMarkers()

	require.NoError(t, writeSupervisorMarker(proof.started))
	proof.quarantine = filepath.Join(root, "quarantine")
	go func() {
		time.Sleep(15 * time.Millisecond)
		_ = os.WriteFile(proof.quarantine, []byte("x"), 0o600)
	}()
	require.ErrorContains(t, proof.awaitCompletion(context.Background()), "quarantining")

	lock, domain, err := acquireGuardianIdentityAuthority(supervisorConfig{}, strings.NewReader(""))
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	require.NoError(t, domain.Close())
	want := errors.New("verify")
	supervisorVerifyTrustedIdentity = func(uint32) error { return want }
	_, _, err = acquireGuardianIdentityAuthority(supervisorConfig{IsolationUID: 1}, strings.NewReader(""))
	require.ErrorIs(t, err, want)
	supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
	supervisorAcquireIdentityAuthority = func(uint32, uint32, string, string, io.Reader) (supervisorIdentityLock, supervisorIdentityLock, error) {
		return nil, nil, want
	}
	_, _, err = acquireGuardianIdentityAuthority(supervisorConfig{IsolationUID: 1}, strings.NewReader(""))
	require.ErrorIs(t, err, want)
}

func TestGuardianAndLivenessEarlyFailureBranches(t *testing.T) {
	t.Run("guardian adopted identity", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("identity")
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) { return nil, want }
		err := runGuardian(supervisorConfig{IdentityLock: true, IsolationUID: 1})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian adopted domain", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("domain")
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) { return errorSupervisorIdentityLock{}, nil }
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) { return nil, want }
		err := runGuardian(supervisorConfig{IdentityLock: true, IsolationUID: 1})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian adopted authority", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("disposition")
		supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) { return errorSupervisorIdentityLock{}, nil }
		supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) { return errorSupervisorIdentityLock{}, nil }
		supervisorValidateAdoptedAuthority = func(supervisorConfig) error { return want }
		err := runGuardian(supervisorConfig{IdentityLock: true, IsolationUID: 1})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian authority", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("authority")
		supervisorVerifyTrustedIdentity = func(uint32) error { return want }
		err := runGuardian(supervisorConfig{IsolationUID: 1})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian claim", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		file := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
		err := runGuardian(supervisorConfig{Home: filepath.Join(file, "home")})
		require.Error(t, err)
	})
	t.Run("guardian containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("containment")
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) { return nil, want }
		err := runGuardian(supervisorConfig{Home: filepath.Join(t.TempDir(), "home")})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian pipe", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("pipe")
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) { return &guardianContainment{}, nil }
		supervisorPipe = func() (*os.File, *os.File, error) { return nil, nil, want }
		err := runGuardian(supervisorConfig{Home: filepath.Join(t.TempDir(), "home")})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian identity placeholder", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("open")
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) { return &guardianContainment{}, nil }
		supervisorOpen = func(string) (*os.File, error) { return nil, want }
		err := runGuardian(supervisorConfig{Home: filepath.Join(t.TempDir(), "home")})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian authority placeholder", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("open")
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) { return &guardianContainment{}, nil }
		calls := 0
		supervisorOpen = func(string) (*os.File, error) {
			calls++
			if calls == 2 {
				return nil, want
			}

			return os.Open("/dev/null")
		}
		err := runGuardian(supervisorConfig{Home: filepath.Join(t.TempDir(), "home")})
		require.ErrorIs(t, err, want)
	})
	t.Run("guardian start", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("start")
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) { return &guardianContainment{}, nil }
		supervisorExecutable = func() (string, error) { return "/usr/bin/true", nil }
		supervisorStartIndependent = func(*exec.Cmd) error { return want }
		err := runGuardian(supervisorConfig{Home: filepath.Join(t.TempDir(), "home"), Scratch: t.TempDir(), NativeEnv: []string{"PATH=/usr/bin"}})
		require.ErrorIs(t, err, want)
	})

	t.Run("liveness trusted identity", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("identity")
		supervisorVerifyTrustedIdentity = func(uint32) error { return want }
		err := runLiveness(supervisorConfig{IsolationUID: 1})
		require.ErrorIs(t, err, want)
	})
	t.Run("liveness marker", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		file := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
		err := runLiveness(supervisorConfig{Started: filepath.Join(file, "started")})
		require.Error(t, err)
	})
	t.Run("liveness containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		want := errors.New("containment")
		supervisorOpenLivenessContainment = func(supervisorConfig) (*livenessContainment, error) { return nil, want }
		root := t.TempDir()
		err := runLiveness(supervisorConfig{Home: filepath.Join(root, "home"), Started: filepath.Join(root, "started")})
		require.ErrorIs(t, err, want)
	})
	t.Run("liveness credential", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		generation := filepath.Join(root, "generation")
		require.NoError(t, os.Mkdir(generation, 0o700))
		err := runLiveness(supervisorConfig{
			Home: filepath.Join(root, "home"), Started: filepath.Join(root, "started"), NativePath: "/usr/bin/true",
			IsolationUID: 1, IsolationGID: 0, NativeEnv: []string{"PATH=/usr/bin"}, DarwinBestEffort: true,
			ScratchParent: root, Scratch: generation, LifecycleKind: darwinLifecycleRuntime,
		})
		require.ErrorContains(t, err, "apply supervised")
	})
	t.Run("liveness guardian peer", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		generation := filepath.Join(root, "generation")
		require.NoError(t, os.Mkdir(generation, 0o700))
		want := errors.New("guardian peer")
		supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error { return want }
		supervisorLivenessQuiesce = func(*livenessContainment, int, time.Duration) error { return nil }
		err := runLiveness(supervisorConfig{
			Home: filepath.Join(root, "home"), Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			NativePath: "/usr/bin/true", NativeEnv: []string{"PATH=/usr/bin"}, DarwinBestEffort: true,
			ScratchParent: root, Scratch: generation, LifecycleKind: darwinLifecycleRuntime,
		})
		require.ErrorIs(t, err, want)
	})
	t.Run("liveness final guardian peer", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		generation := filepath.Join(root, "generation")
		require.NoError(t, os.Mkdir(generation, 0o700))
		want := errors.New("guardian peer")
		calls := 0
		supervisorValidateGuardianPeer = func(*os.File, <-chan struct{}) error {
			calls++
			if calls == 2 {
				return want
			}

			return nil
		}
		supervisorLivenessQuiesce = func(*livenessContainment, int, time.Duration) error { return nil }
		err := runLiveness(supervisorConfig{
			Home: filepath.Join(root, "home"), Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			NativePath: "/usr/bin/true", NativeEnv: []string{"PATH=/usr/bin"}, DarwinBestEffort: true,
			ScratchParent: root, Scratch: generation, LifecycleKind: darwinLifecycleRuntime,
		})
		require.ErrorIs(t, err, want)
	})
}

func TestFinishGuardianLivenessRemainingBranches(t *testing.T) {
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	err := finishGuardianLiveness(supervisorConfig{Completion: filepath.Join(notDirectory, "complete")}, nil, 0, errors.New("wait"))
	require.Error(t, err)
	err = finishGuardianLiveness(supervisorConfig{Completion: filepath.Join(root, "missing"), Quarantine: filepath.Join(notDirectory, "quarantine")}, nil, 0, errors.New("wait"))
	require.Error(t, err)
	supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error { return nil }
	err = finishGuardianLiveness(supervisorConfig{Completion: filepath.Join(root, "completion")}, nil, 0, errors.New("wait"))
	require.ErrorContains(t, err, "liveness supervisor exited")
}

func TestSupervisorEntropyAndProofStatFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	_, err := supervisorNonce()
	require.ErrorContains(t, err, "marker nonce")
	configFile, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.NoError(t, err)
	configPath := configFile.Name()
	require.NoError(t, configFile.Close())
	_, statErr := os.Stat(configPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	err = (&supervisorProof{completion: filepath.Join(notDirectory, "child")}).awaitCompletion(context.Background())
	require.ErrorContains(t, err, "stat liveness completion")
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	err = (&supervisorProof{started: filepath.Join(notDirectory, "child"), completion: filepath.Join(root, "missing")}).awaitCompletion(context.Background())
	require.ErrorContains(t, err, "stat liveness start")
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = (&supervisorProof{started: filepath.Join(root, "absent-start"), completion: filepath.Join(root, "absent-complete")}).awaitCompletion(cancelled)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	started := filepath.Join(root, "started")
	require.NoError(t, writeSupervisorMarker(started))
	err = (&supervisorProof{started: started, completion: filepath.Join(root, "still-absent")}).awaitCompletion(cancelled)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunGuardianPipeAndStartFailures(t *testing.T) {
	tests := map[string]func(*exec.Cmd){
		"stdin":  func(command *exec.Cmd) { command.Stdin = strings.NewReader("") },
		"stdout": func(command *exec.Cmd) { command.Stdout = io.Discard },
		"stderr": func(command *exec.Cmd) { command.Stderr = io.Discard },
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			root := t.TempDir()
			supervisorExecutable = func() (string, error) { return "/usr/bin/true", nil }
			supervisorExecCommand = func(name string, args ...string) *exec.Cmd {
				command := exec.Command(name, args...)
				configure(command)

				return command
			}
			err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
			require.ErrorContains(t, err, "open liveness")
		})
	}

	t.Run("start", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		supervisorExecutable = func() (string, error) { return "/missing", nil }
		supervisorExecCommand = func(string, ...string) *exec.Cmd { return exec.Command(filepath.Join(root, "missing")) }
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "resolve liveness supervisor executable through process policy")
	})

	t.Run("executable", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		supervisorExecutable = func() (string, error) { return "", errors.New("lookup failed") }
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "resolve liveness")
	})
}

func TestRunLivenessPipeAndExitFailures(t *testing.T) {
	tests := map[string]func(*exec.Cmd){
		"stdin":  func(command *exec.Cmd) { command.Stdin = strings.NewReader("") },
		"stdout": func(command *exec.Cmd) { command.Stdout = io.Discard },
		"stderr": func(command *exec.Cmd) { command.Stderr = io.Discard },
	}
	for name, configure := range tests {
		t.Run(name, func(t *testing.T) {
			preserveSupervisorGlobals(t)
			root := t.TempDir()
			supervisorExecCommand = func(path string, args ...string) *exec.Cmd {
				command := exec.Command(path, args...)
				configure(command)

				return command
			}
			err := runLiveness(supervisorConfig{
				NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
				Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
			})
			require.ErrorContains(t, err, "open native")
		})
	}

	t.Run("native exit", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		input, inputWriter := io.Pipe()
		t.Cleanup(func() { _ = inputWriter.Close() })
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runLiveness(supervisorConfig{
			NativePath: "/usr/bin/false", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorContains(t, err, "native root exited")
	})
}

func TestUnixQuiescenceSignalEscalationAndTimeout(t *testing.T) {
	oldKill := openCodeSyscallKill
	t.Cleanup(func() {
		openCodeSyscallKill = oldKill
	})

	probes := 0
	openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
		if signal == 0 {
			probes++
			if probes > 1 {
				return syscall.ESRCH
			}
		}

		return nil
	}
	require.NoError(t, quiesceProcessGroup(123, time.Second))

	openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
		if signal == 0 {
			return nil
		}

		return nil
	}
	// A group that never stops answering exhausts the deadline. Darwin routes
	// quiescence through the containment boundary and reports its sentinel;
	// every other platform here reports the process-group timeout directly.
	stubborn := quiesceProcessGroup(123, time.Millisecond)
	if runtime.GOOS == "darwin" {
		require.ErrorIs(t, stubborn, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, stubborn, "original process group 123 remained observable")
	} else {
		require.ErrorContains(t, stubborn, "native process group 123 did not become quiescent")
	}

	containmentConfig := supervisorConfig{
		DarwinBestEffort: true,
		ScratchParent:    t.TempDir(),
		LifecycleKind:    "runtime",
	}
	containmentConfig.Scratch = filepath.Join(containmentConfig.ScratchParent, "acp-go-opencode-runtime-test")
	require.NoError(t, os.Mkdir(containmentConfig.Scratch, 0o700))

	liveness, err := openLivenessContainment(containmentConfig)
	require.NoError(t, err)
	command := exec.Command("/usr/bin/true")
	require.NoError(t, liveness.Start(command))
	require.NoError(t, <-liveness.Wait())
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, liveness.Quiesce(command.Process.Pid, time.Second))
	guardian, err := newGuardianContainment(containmentConfig)
	require.NoError(t, err)
	guardianErr := guardian.Quiesce(command.Process.Pid, time.Second)
	if runtime.GOOS == "darwin" {
		require.ErrorIs(t, guardianErr, ErrProcessContainmentIncomplete)
	} else {
		require.NoError(t, guardianErr)
	}
}

func TestSupervisorInjectedFilesystemAndContainmentFailures(t *testing.T) {
	t.Run("chmod", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorChmod = func(string, os.FileMode) error { return errors.New("chmod failed") }
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "chmod private")
	})

	t.Run("open file", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorCreateTemp = func(string, string) (*os.File, error) { return nil, errors.New("open failed") }
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "create private supervisor config")
	})

	t.Run("encode", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorEncodeConfig = func(io.Writer, supervisorConfig) error { return errors.New("encode failed") }
		_, err := writeSupervisorConfig(t.TempDir(), supervisorConfig{})
		require.ErrorContains(t, err, "write private supervisor config")
	})

	t.Run("guardian containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorNewGuardianContainment = func(supervisorConfig) (*guardianContainment, error) {
			return nil, errors.New("containment failed")
		}
		root := t.TempDir()
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "containment failed")
	})

	t.Run("liveness containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorOpenLivenessContainment = func(supervisorConfig) (*livenessContainment, error) {
			return nil, errors.New("containment failed")
		}
		root := t.TempDir()
		err := runLiveness(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		})
		require.ErrorContains(t, err, "containment failed")
	})
}

func TestSupervisorDispatchBootstrapAndEarlyFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	withNeutralSupervisorIdentityHooks(t)
	isolation := testProcessIsolation()
	root := t.TempDir()
	config := withTestSupervisorIdentity(supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "sleep 0.1"}, NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
	})
	path, err := writeSupervisorConfig(root, config)
	require.NoError(t, err)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	require.NoError(t, runSupervisor(supervisorModeLiveness, path))

	root = t.TempDir()
	config.Home = filepath.Join(root, "home")
	config.Scratch = root
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "pid")
	supervisorRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	_, _, err = supervisorCommand(context.Background(), supervisorConfig{Scratch: t.TempDir(), Isolation: isolation})
	require.ErrorContains(t, err, "marker nonce")
	supervisorRandRead = func(value []byte) (int, error) {
		for index := range value {
			value[index] = 1
		}

		return len(value), nil
	}
	requireSupervisorCommandWithoutScratchRoot(t, isolation)

	root = t.TempDir()
	claim, err := homelock.AcquireClaim(filepath.Join(root, "home"))
	require.NoError(t, err)
	err = runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
	require.Error(t, err)
	require.NoError(t, claim.Release())

	root = t.TempDir()
	liveness, err := homelock.AcquireLiveness(filepath.Join(root, "home"))
	require.NoError(t, err)
	err = runLiveness(supervisorConfig{
		Home: filepath.Join(root, "home"), Scratch: root, Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
	})
	require.Error(t, err)
	require.NoError(t, liveness.Release())

	root = t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	err = runLiveness(supervisorConfig{Started: filepath.Join(notDirectory, "child"), Completion: filepath.Join(root, "complete")})
	require.Error(t, err)
}

func TestGuardianPreReadinessRecoveryProofBranches(t *testing.T) {
	t.Run("completion stat", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		notDirectory := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Completion: filepath.Join(notDirectory, "child"),
		})
		require.Error(t, err)
	})

	t.Run("started stat", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		notDirectory := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(notDirectory, "child"), Completion: filepath.Join(root, "missing"),
		})
		require.Error(t, err)
	})

	t.Run("started pid proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		pid := filepath.Join(root, "pid")
		require.NoError(t, writeSupervisorMarker(started))
		require.NoError(t, writeNativePID(pid, 99999999))
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: filepath.Join(root, "complete"), NativePIDFile: pid,
		})
		require.Error(t, err)
		_, statErr := os.Stat(filepath.Join(root, "complete"))
		if runtime.GOOS == "darwin" {
			require.ErrorIs(t, statErr, os.ErrNotExist)
		} else {
			require.NoError(t, statErr)
		}
	})

	t.Run("recoverable platform publishes proof", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		completion := filepath.Join(root, "complete")
		pid := filepath.Join(root, "pid")
		require.NoError(t, writeSupervisorMarker(started))
		require.NoError(t, writeNativePID(pid, 99999999))
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
		supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error { return nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: completion, NativePIDFile: pid,
		})
		require.Error(t, err)
		_, statErr := os.Stat(completion)
		require.NoError(t, statErr)
	})

	t.Run("completion appears while pid absent", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		completion := filepath.Join(root, "complete")
		require.NoError(t, writeSupervisorMarker(started))
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = writeSupervisorMarker(completion)
		}()
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: completion, NativePIDFile: filepath.Join(root, "missing-pid"),
		})
		require.Error(t, err)
	})
}

func TestSupervisorFinalRemainingBranches(t *testing.T) {
	t.Run("guardian Darwin metadata", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		want := errors.New("stop after metadata")
		supervisorGuardianName = func(*guardianContainment) string { return "darwin-best-effort" }
		supervisorEncodeConfig = func(_ io.Writer, config supervisorConfig) error {
			require.True(t, config.DarwinBestEffort)
			require.Equal(t, filepath.Dir(root), config.ScratchParent)
			require.Equal(t, darwinLifecycleRuntime, config.LifecycleKind)
			require.Equal(t, "darwin-best-effort", config.JobName)

			return want
		}
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			InventoryIdentity: filepath.Join(root, "inventory"),
		})
		require.ErrorIs(t, err, want)
	})

	t.Run("guardian waiter release", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		want := errors.New("release failed")
		supervisorExecutable = func() (string, error) { return "/bin/sh", nil }
		supervisorReleaseIndependentWaiter = func(*exec.Cmd, *supervisorWaiter) (int, error) {
			return 0, want
		}
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorIs(t, err, want)
	})

	t.Run("guardian post-readiness proof failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		liveness := filepath.Join(root, "liveness")
		require.NoError(t, os.WriteFile(liveness, []byte("#!/bin/sh\nprintf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":99999999}`+"' >&2\nexit 0\n"), 0o700))
		supervisorExecutable = func() (string, error) { return liveness, nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		want := errors.New("proof failed")
		supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error { return want }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Completion: filepath.Join(root, "complete"),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, want.Error())
	})

	t.Run("guardian pre-readiness quarantine", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		liveness := filepath.Join(root, "liveness")
		require.NoError(t, os.WriteFile(liveness, []byte("#!/bin/sh\nprintf 'invalid\\n' >&2\nsleep 0.05\n"), 0o700))
		supervisorExecutable = func() (string, error) { return liveness, nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		quarantine := filepath.Join(root, "quarantine")
		require.NoError(t, writeSupervisorMarker(quarantine))
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			Quarantine: quarantine, NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})

	t.Run("guardian pre-readiness quarantine marker failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		require.NoError(t, writeSupervisorMarker(started))
		quarantineParent := filepath.Join(root, "quarantine-parent")
		require.NoError(t, os.Mkdir(quarantineParent, 0o700))
		pidFIFO := filepath.Join(root, "native-pid")
		require.NoError(t, syscall.Mkfifo(pidFIFO, 0o600))
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.Remove(quarantineParent)
			_ = os.WriteFile(quarantineParent, []byte("x"), 0o600)
			writer, openErr := os.OpenFile(pidFIFO, os.O_WRONLY, 0)
			if openErr == nil {
				_, _ = io.WriteString(writer, "99999999\n")
				_ = writer.Close()
			}
		}()
		supervisorExecutable = func() (string, error) { return "/usr/bin/false", nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: filepath.Join(root, "complete"), Quarantine: filepath.Join(quarantineParent, "marker"),
			NativePIDFile: pidFIFO,
		})
		require.ErrorContains(t, err, "quarantine")
	})

	t.Run("guardian post-readiness quarantine", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		liveness := filepath.Join(root, "liveness")
		require.NoError(t, os.WriteFile(liveness, []byte("#!/bin/sh\nprintf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":99999999}`+"' >&2\nsleep 0.05\n"), 0o700))
		supervisorExecutable = func() (string, error) { return liveness, nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		quarantine := filepath.Join(root, "quarantine")
		require.NoError(t, writeSupervisorMarker(quarantine))
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
			Quarantine: quarantine, NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})

	t.Run("liveness post-exit proof failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		input, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		want := errors.New("proof failed")
		supervisorLivenessQuiesce = func(*livenessContainment, int, time.Duration) error { return want }
		err := runLiveness(supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
		require.ErrorContains(t, err, want.Error())
	})

	t.Run("guardian dispatch", func(t *testing.T) {
		skipUnprivilegedDarwinIsolation(t)
		preserveSupervisorGlobals(t)
		withNeutralSupervisorIdentityHooks(t)
		root := t.TempDir()
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		config := withTestSupervisorIdentity(supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		path, err := writeSupervisorConfig(root, config)
		require.NoError(t, err)
		require.NoError(t, runSupervisor(supervisorModeGuardian, path))
	})

	t.Run("guardian config", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		withNeutralSupervisorIdentityHooks(t)
		err := runGuardian(withTestSupervisorIdentity(supervisorConfig{Home: t.TempDir(), Scratch: ""}))
		require.ErrorContains(t, err, guardianWithoutScratchRootRefusal)
	})

	t.Run("guardian completion publish", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		notDirectory := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error { return nil }
		err := runGuardian(supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(notDirectory, "child"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.Error(t, err)
	})

	t.Run("guardian reports post-readiness liveness failure", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		liveness := filepath.Join(root, "liveness")
		require.NoError(t, os.WriteFile(liveness, []byte("#!/bin/sh\nprintf '%s\\n' '"+supervisorReadyPrefix+`{"nativePid":99999999}`+"' >&2\nexit 7\n"), 0o700))
		supervisorExecutable = func() (string, error) { return liveness, nil }
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		completion := filepath.Join(root, "complete")
		require.NoError(t, writeSupervisorMarker(completion))
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: completion, NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorContains(t, err, "liveness supervisor exited")
	})

	t.Run("liveness native success", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		input, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })
		supervisorInput = input
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		err := runLiveness(supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.NoError(t, err)
	})

	t.Run("proof second stat", func(t *testing.T) {
		root := t.TempDir()
		started := filepath.Join(root, "started")
		completionParent := filepath.Join(root, "parent")
		completion := filepath.Join(completionParent, "child")
		require.NoError(t, writeSupervisorMarker(started))
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = os.WriteFile(completionParent, []byte("x"), 0o600)
		}()
		err := (&supervisorProof{started: started, completion: completion}).awaitCompletion(context.Background())
		require.ErrorContains(t, err, "stat liveness completion")
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})

	t.Run("proof second quarantine stat", func(t *testing.T) {
		root := t.TempDir()
		started := filepath.Join(root, "started")
		require.NoError(t, writeSupervisorMarker(started))
		quarantineParent := filepath.Join(root, "quarantine-parent")
		require.NoError(t, os.Mkdir(quarantineParent, 0o700))
		go func() {
			time.Sleep(5 * time.Millisecond)
			_ = os.Remove(quarantineParent)
			_ = os.WriteFile(quarantineParent, []byte("x"), 0o600)
		}()
		err := (&supervisorProof{
			started: started, completion: filepath.Join(root, "missing"),
			quarantine: filepath.Join(quarantineParent, "marker"),
		}).awaitCompletion(context.Background())
		require.ErrorContains(t, err, "stat liveness quarantine")
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	})
}

func TestUnixQuiescenceRemainingProbeBranches(t *testing.T) {
	oldKill := openCodeSyscallKill
	t.Cleanup(func() { openCodeSyscallKill = oldKill })

	openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
		if signal == 0 {
			return errors.New("term probe failed")
		}

		return nil
	}
	require.ErrorContains(t, quiesceProcessGroup(123, time.Second), "term probe failed")

	killed := false
	openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGKILL {
			killed = true
		}
		if signal == 0 && killed {
			return errors.New("kill probe failed")
		}

		return nil
	}
	require.ErrorContains(t, quiesceProcessGroup(123, 600*time.Millisecond), "kill probe failed")

	killed = false
	killProbes := 0
	openCodeSyscallKill = func(_ int, signal syscall.Signal) error {
		if signal == syscall.SIGKILL {
			killed = true
		}
		if signal == 0 && killed {
			killProbes++
			if killProbes > 1 {
				return syscall.ESRCH
			}
		}

		return nil
	}
	require.NoError(t, quiesceProcessGroup(123, 600*time.Millisecond))
}

// TestRunGuardianWritesCompletionProofWhenLivenessLeftNone covers the
// guardian's post-readiness proof path: a liveness supervisor that published
// readiness and exited without a completion marker leaves the guardian to
// prove quiescence and write the marker itself.
func TestRunGuardianWritesCompletionProofWhenLivenessLeftNone(t *testing.T) {
	preserveSupervisorGlobals(t)

	root := t.TempDir()
	script := filepath.Join(root, "liveness.sh")
	require.NoError(t, os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf '"+supervisorReadyPrefix+"{\"nativePid\":4242}\\n' >&2\n",
	), 0o700))

	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	supervisorExecutable = func() (string, error) { return script, nil }
	supervisorGuardianQuiesce = func(*guardianContainment, int, time.Duration) error { return nil }

	completion := filepath.Join(root, "complete")
	require.NoError(t, runGuardian(supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: completion,
		NativePIDFile: filepath.Join(root, "native.pid"),
	}))
	require.FileExists(t, completion)
}

// withTestSupervisorIdentity fills the identity a private supervisor config
// must carry. readSupervisorConfig rejects a config whose isolation IDs are
// zero, and on Linux the guardian binds the standalone owner and state root
// before it dispatches, so a config naming neither never reaches the branch the
// case is about. Every fixture claims the one package identity: the authority
// binds a UID to a single owner and state root permanently.
func withTestSupervisorIdentity(config supervisorConfig) supervisorConfig {
	isolation := testProcessIsolation()
	config.IsolationUID = isolation.UID
	config.IsolationGID = isolation.GID
	config.StandaloneOwnerID = isolation.StandaloneOwnerID
	config.StandaloneStateRoot = isolation.StandaloneStateRoot
	config.Isolation = isolation

	return config
}

// borrowedTestIsolation drops the standalone owner fields a borrowed process
// identity must not carry: the policy refuses a config that claims capabilities
// and owner fields at once.
func borrowedTestIsolation(isolation *ProcessIsolation) *ProcessIsolation {
	isolation.StandaloneOwnerID = ""
	isolation.StandaloneStateRoot = ""

	return isolation
}

// withNeutralSupervisorIdentityHooks installs the platform-neutral identity
// hooks for cases that exercise supervisor dispatch rather than the authority
// itself. Linux replaces these at init with the real agent authority, which
// claims one identity exclusively and hands the claim down an inherited
// descriptor: a single test process cannot establish and release that claim
// repeatedly, so an in-process dispatch case dies on the authority long before
// the branch it names. The authority itself is proven end to end by the
// supervised-native cases in supervisor_linux_test.go.
func withNeutralSupervisorIdentityHooks(t *testing.T) {
	t.Helper()
	supervisorAcquireIdentityAuthority = func(
		uint32, uint32, string, string, io.Reader,
	) (supervisorIdentityLock, supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, noopSupervisorIdentityLock{}, nil
	}
	supervisorVerifyTrustedIdentity = func(uint32) error { return nil }
	supervisorAdoptIdentityLock = func(uint32) (supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, nil
	}
	supervisorAdoptAuthorityDomain = func(uint32) (supervisorIdentityLock, error) {
		return noopSupervisorIdentityLock{}, nil
	}
	supervisorValidateAdoptedAuthority = func(supervisorConfig) error { return nil }
}
