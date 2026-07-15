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
	oldExecutable := supervisorExecutable
	oldCommand := supervisorExecCommand
	oldRandRead := supervisorRandRead
	oldChmod := supervisorChmod
	oldOpenFile := supervisorOpenFile
	oldEncode := supervisorEncodeConfig
	oldGuardianContainment := supervisorNewGuardianContainment
	oldLivenessContainment := supervisorOpenLivenessContainment
	oldInput := supervisorInput
	oldOutput := supervisorOutput
	oldError := supervisorError
	oldExit := supervisorExit
	t.Cleanup(func() {
		supervisorExecutable = oldExecutable
		supervisorExecCommand = oldCommand
		supervisorRandRead = oldRandRead
		supervisorChmod = oldChmod
		supervisorOpenFile = oldOpenFile
		supervisorEncodeConfig = oldEncode
		supervisorNewGuardianContainment = oldGuardianContainment
		supervisorOpenLivenessContainment = oldLivenessContainment
		supervisorInput = oldInput
		supervisorOutput = oldOutput
		supervisorError = oldError
		supervisorExit = oldExit
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
		NativePath: "/bin/true", Home: filepath.Join(root, "home"), Scratch: root,
	})
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.NotNil(t, proof)
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
	require.NoError(t, (&supervisorProof{
		started: filepath.Join(root, "never-started"), completion: filepath.Join(root, "never-completed"),
	}).awaitCompletion(context.Background()))

	started := filepath.Join(root, "started")
	completed := filepath.Join(root, "completed")
	require.NoError(t, writeSupervisorMarker(started))
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = writeSupervisorMarker(completed)
	}()
	require.NoError(t, (&supervisorProof{started: started, completion: completed}).awaitCompletion(context.Background()))
}

func TestSupervisorMarkerPIDReadyAndCopyUtilities(t *testing.T) {
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
	awaitQuiescence(func() error {
		tries++
		if tries < 2 {
			return errors.New("not yet")
		}

		return nil
	})
	require.Equal(t, 2, tries)
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
	supervisorExecutable = func() (string, error) { return "/bin/false", nil }
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

	guardian, err := newGuardianContainment()
	require.NoError(t, err)
	require.Empty(t, guardian.Name())
	require.NoError(t, guardian.Close())
	liveness, err := openLivenessContainment("")
	require.NoError(t, err)
	require.NoError(t, liveness.Close())

	cmd := exec.Command("/bin/true")
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
	require.ErrorIs(t, err, ErrProcessTreeUnproven)
	err = (&supervisorProof{started: filepath.Join(notDirectory, "child"), completion: filepath.Join(root, "missing")}).awaitCompletion(context.Background())
	require.ErrorContains(t, err, "stat liveness start")
	require.ErrorIs(t, err, ErrProcessTreeUnproven)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = (&supervisorProof{started: filepath.Join(root, "absent-start"), completion: filepath.Join(root, "absent-complete")}).awaitCompletion(cancelled)
	require.ErrorIs(t, err, ErrProcessTreeUnproven)
	require.ErrorIs(t, err, context.Canceled)

	started := filepath.Join(root, "started")
	require.NoError(t, writeSupervisorMarker(started))
	err = (&supervisorProof{started: started, completion: filepath.Join(root, "still-absent")}).awaitCompletion(cancelled)
	require.ErrorIs(t, err, ErrProcessTreeUnproven)
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
			supervisorExecutable = func() (string, error) { return "/bin/true", nil }
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
				NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
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
			NativePath: "/bin/false", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
		})
		require.ErrorContains(t, err, "native root exited")
	})
}

func TestUnixQuiescenceSignalEscalationAndTimeout(t *testing.T) {
	oldKill := openCodeSyscallKill
	oldGetpgid := openCodeSyscallGetpgid
	t.Cleanup(func() {
		openCodeSyscallKill = oldKill
		openCodeSyscallGetpgid = oldGetpgid
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

	command := &exec.Cmd{Process: &os.Process{Pid: 123}}
	openCodeSyscallGetpgid = func(int) (int, error) { return 123, nil }
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, terminateIndependentSupervisor(command))

	liveness, err := openLivenessContainment("")
	require.NoError(t, err)
	command = exec.Command("/bin/true")
	require.NoError(t, liveness.Start(command))
	require.NoError(t, command.Wait())
	openCodeSyscallKill = func(int, syscall.Signal) error { return syscall.ESRCH }
	require.NoError(t, liveness.Quiesce(command.Process.Pid, time.Second))
	guardian, err := newGuardianContainment()
	require.NoError(t, err)
	require.NoError(t, guardian.Quiesce(command.Process.Pid, time.Second))
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
		supervisorNewGuardianContainment = func() (*guardianContainment, error) { return nil, errors.New("containment failed") }
		root := t.TempDir()
		err := runGuardian(supervisorConfig{Home: filepath.Join(root, "home"), Scratch: root})
		require.ErrorContains(t, err, "containment failed")
	})

	t.Run("liveness containment", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		supervisorOpenLivenessContainment = func(string) (*livenessContainment, error) { return nil, errors.New("containment failed") }
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
		NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
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
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
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
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
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
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root, Started: started,
			Completion: filepath.Join(root, "complete"), NativePIDFile: pid,
		})
		require.Error(t, err)
		_, statErr := os.Stat(filepath.Join(root, "complete"))
		require.NoError(t, statErr)
	})

	t.Run("completion appears while pid absent", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		started := filepath.Join(root, "started")
		completion := filepath.Join(root, "complete")
		require.NoError(t, writeSupervisorMarker(started))
		supervisorExecutable = func() (string, error) { return "/bin/false", nil }
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
	t.Run("guardian dispatch", func(t *testing.T) {
		preserveSupervisorGlobals(t)
		root := t.TempDir()
		supervisorInput = strings.NewReader("")
		supervisorOutput = io.Discard
		supervisorError = io.Discard
		config := supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
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
		err := runGuardian(supervisorConfig{
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
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
		err := runGuardian(supervisorConfig{
			Home: filepath.Join(root, "home"), Scratch: root,
			Started: filepath.Join(root, "started"), Completion: filepath.Join(root, "complete"), NativePIDFile: filepath.Join(root, "pid"),
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
			NativePath: "/bin/true", NativeEnv: os.Environ(), Home: filepath.Join(root, "home"), Scratch: root,
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
		require.ErrorIs(t, err, ErrProcessTreeUnproven)
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
