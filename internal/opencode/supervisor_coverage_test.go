//go:build linux || darwin || freebsd || openbsd

package opencode

import (
	"bytes"
	"context"
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
	oldEncode := supervisorEncodeConfig
	oldGuardianContainment := supervisorNewGuardianContainment
	oldGuardianName := supervisorGuardianName
	oldGuardianQuiesce := supervisorGuardianQuiesce
	oldLivenessContainment := supervisorOpenLivenessContainment
	oldLivenessQuiesce := supervisorLivenessQuiesce
	oldReleaseWaiter := supervisorReleaseIndependentWaiter
	oldInput := supervisorInput
	oldOutput := supervisorOutput
	oldError := supervisorError
	oldExit := supervisorExit
	oldProcessSnapshot := supervisorProcessSnapshot
	t.Cleanup(func() {
		supervisorExecutable = oldExecutable
		supervisorExecCommand = oldCommand
		supervisorRandRead = oldRandRead
		supervisorChmod = oldChmod
		supervisorOpenFile = oldOpenFile
		supervisorEncodeConfig = oldEncode
		supervisorNewGuardianContainment = oldGuardianContainment
		supervisorGuardianName = oldGuardianName
		supervisorGuardianQuiesce = oldGuardianQuiesce
		supervisorOpenLivenessContainment = oldLivenessContainment
		supervisorLivenessQuiesce = oldLivenessQuiesce
		supervisorReleaseIndependentWaiter = oldReleaseWaiter
		supervisorInput = oldInput
		supervisorOutput = oldOutput
		supervisorError = oldError
		supervisorExit = oldExit
		supervisorProcessSnapshot = oldProcessSnapshot
	})
}

func TestSupervisorConfigAndDispatchFailures(t *testing.T) {
	_, err := readSupervisorConfig("")
	require.ErrorContains(t, err, "missing private")
	_, err = readSupervisorConfig(filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "open private")

	root := t.TempDir()
	badJSON := filepath.Join(root, "bad.json")
	require.NoError(t, os.WriteFile(badJSON, []byte("{"), 0o600))
	_, err = readSupervisorConfig(badJSON)
	require.ErrorContains(t, err, "decode private")
	_, statErr := os.Stat(badJSON)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	incomplete := filepath.Join(root, "incomplete.json")
	require.NoError(t, os.WriteFile(incomplete, []byte(`{"nativePath":"x"}`), 0o600))
	_, err = readSupervisorConfig(incomplete)
	require.ErrorContains(t, err, "incomplete")

	_, err = writeSupervisorConfig("", supervisorConfig{})
	require.ErrorContains(t, err, "scratch root is required")
	notDir := filepath.Join(root, "not-dir")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o600))
	_, err = writeSupervisorConfig(filepath.Join(notDir, "child"), supervisorConfig{})
	require.ErrorContains(t, err, "create private")

	config := supervisorConfig{NativePath: "x", Home: "h", Scratch: "s"}
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
	_, _, err := supervisorCommand(context.Background(), supervisorConfig{Scratch: root})
	require.ErrorContains(t, err, "resolve embedded")

	supervisorExecutable = os.Executable
	cmd, proof, err := supervisorCommand(context.Background(), supervisorConfig{
		NativePath: "/usr/bin/true", Home: filepath.Join(root, "home"), Scratch: root,
	})
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.NotNil(t, proof)
	require.NotEmpty(t, proof.inventoryIdentity)
	require.Contains(t, strings.Join(cmd.Env, "\n"), supervisorModeEnv+"="+supervisorModeGuardian)

	nonce, err := supervisorNonce()
	require.NoError(t, err)
	require.Len(t, nonce, 32)

	t.Setenv(supervisorModeEnv, "old")
	t.Setenv(supervisorConfigEnv, "old")
	env := supervisorEnv("new", "config")
	require.Contains(t, env, supervisorModeEnv+"=new")
	require.Contains(t, env, supervisorConfigEnv+"=config")
	require.NotContains(t, env, supervisorModeEnv+"=old")

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
	preserveSupervisorGlobals(t)
	root := t.TempDir()
	supervisorInput = strings.NewReader("payload\n")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	config := supervisorConfig{
		NativePath: "/bin/sh", NativeArgs: []string{"-c", "cat"}, NativeEnv: os.Environ(),
		Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"),
		NativePIDFile: filepath.Join(root, "native.pid"),
	}
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
	t.Setenv(supervisorModeEnv, "bad")
	t.Setenv(supervisorConfigEnv, "")
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

func TestSupervisorEntropyAndProofStatFailures(t *testing.T) {
	preserveSupervisorGlobals(t)
	supervisorRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	_, err := supervisorNonce()
	require.ErrorContains(t, err, "marker nonce")
	_, err = writeSupervisorConfig(t.TempDir(), supervisorConfig{})
	require.ErrorContains(t, err, "config nonce")

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
		require.ErrorContains(t, err, "start liveness supervisor")
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
	require.ErrorContains(t, quiesceProcessGroup(123, time.Millisecond), "did not become quiescent")

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
		supervisorOpenFile = func(string, int, os.FileMode) (*os.File, error) { return nil, errors.New("open failed") }
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
	root := t.TempDir()
	config := supervisorConfig{
		NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
		Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
	}
	path, err := writeSupervisorConfig(root, config)
	require.NoError(t, err)
	supervisorInput = strings.NewReader("")
	supervisorOutput = io.Discard
	supervisorError = io.Discard
	require.NoError(t, runSupervisor(supervisorModeLiveness, path))

	exitCode := -1
	supervisorExit = func(code int) { exitCode = code }
	root = t.TempDir()
	config.Home = filepath.Join(root, "home")
	config.Scratch = root
	config.Started = filepath.Join(root, "started")
	config.Completion = filepath.Join(root, "complete")
	config.NativePIDFile = filepath.Join(root, "pid")
	path, err = writeSupervisorConfig(root, config)
	require.NoError(t, err)
	t.Setenv(supervisorModeEnv, supervisorModeLiveness)
	t.Setenv(supervisorConfigEnv, path)
	// A native root can win runLiveness's exit race before its stdin copier
	// observes EOF. Give this independent bootstrap invocation its own reader.
	supervisorInput = strings.NewReader("")
	supervisorBootstrap()
	require.Equal(t, 0, exitCode)

	supervisorRandRead = func([]byte) (int, error) { return 0, errors.New("entropy failed") }
	_, _, err = supervisorCommand(context.Background(), supervisorConfig{Scratch: t.TempDir()})
	require.ErrorContains(t, err, "marker nonce")
	supervisorRandRead = func(value []byte) (int, error) {
		for index := range value {
			value[index] = 1
		}

		return len(value), nil
	}
	_, _, err = supervisorCommand(context.Background(), supervisorConfig{Scratch: ""})
	require.ErrorContains(t, err, "scratch root")

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
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		config := supervisorConfig{
			NativePath: "/usr/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		}
		path, err := writeSupervisorConfig(root, config)
		require.NoError(t, err)
		require.NoError(t, runSupervisor(supervisorModeGuardian, path))
	})

	t.Run("guardian config", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		err := runGuardian(supervisorConfig{Home: t.TempDir(), Scratch: ""})
		require.ErrorContains(t, err, "scratch root")
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
