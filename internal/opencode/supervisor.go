package opencode

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/savid/acp-go-opencode/internal/homelock"
)

const (
	supervisorModeEnv       = "ACP_GO_OPENCODE_INTERNAL_MODE"
	supervisorConfigEnv     = "ACP_GO_OPENCODE_INTERNAL_HELPER"
	supervisorModeGuardian  = "guardian"
	supervisorModeLiveness  = "liveness"
	supervisorReadyPrefix   = "acp-go-opencode supervisor-ready "
	supervisorConfigPrefix  = "supervisor-config-"
	supervisorQuiesceWindow = 5 * time.Second
)

// ErrProcessContainmentIncomplete marks shutdown failures for which the native process
// tree has not been proved empty. Embedders must retain native-root ownership
// when this sentinel is present.
var ErrProcessContainmentIncomplete = errors.New("OpenCode process containment incomplete")

type supervisorConfig struct {
	NativePath        string   `json:"nativePath"`
	NativeArgs        []string `json:"nativeArgs"`
	NativeEnv         []string `json:"nativeEnv"`
	NativeDir         string   `json:"nativeDir,omitempty"`
	Home              string   `json:"home"`
	Scratch           string   `json:"scratch"`
	ScratchParent     string   `json:"scratchParent"`
	LifecycleKind     string   `json:"lifecycleKind"`
	DarwinBestEffort  bool     `json:"darwinBestEffort"`
	JobName           string   `json:"jobName,omitempty"`
	Started           string   `json:"started"`
	Completion        string   `json:"completion"`
	NativePIDFile     string   `json:"nativePidFile"`
	InventoryIdentity string   `json:"inventoryIdentity"`
}

type supervisorReady struct {
	NativePID int `json:"nativePid"`
}

var supervisorExecutable = os.Executable
var supervisorExecCommand = exec.Command
var supervisorRandRead = rand.Read
var supervisorChmod = os.Chmod
var supervisorOpenFile = os.OpenFile
var supervisorEncodeConfig = func(writer io.Writer, config supervisorConfig) error {
	return json.NewEncoder(writer).Encode(config)
}
var supervisorNewGuardianContainment = newGuardianContainment
var supervisorGuardianQuiesce = func(containment *guardianContainment, nativePID int, timeout time.Duration) error {
	return containment.Quiesce(nativePID, timeout)
}
var supervisorOpenLivenessContainment = openLivenessContainment
var supervisorInput io.Reader = os.Stdin
var supervisorOutput io.Writer = os.Stdout
var supervisorError io.Writer = os.Stderr
var supervisorExit = os.Exit
var supervisorProcessSnapshot = querySupervisorProcessSnapshot

type supervisorProof struct {
	started           string
	completion        string
	inventoryIdentity string
}

// init turns the embedding command itself into either member of the
// supervisor pair. No separate helper binary is installed, and these modes
// run before the host's main package can open any adapter state.
func init() {
	supervisorBootstrap()
}

func supervisorBootstrap() {
	mode := os.Getenv(supervisorModeEnv)
	if mode == "" {
		return
	}

	err := runSupervisor(mode, os.Getenv(supervisorConfigEnv))
	if err != nil {
		_, _ = fmt.Fprintln(supervisorError, "acp-go-opencode runtime supervisor:", err)

		supervisorExit(1)

		return
	}

	supervisorExit(0)
}

func runSupervisor(mode string, configPath string) error {
	config, err := readSupervisorConfig(configPath)
	if err != nil {
		return err
	}

	switch mode {
	case supervisorModeGuardian:
		return runGuardian(config)
	case supervisorModeLiveness:
		return runLiveness(config)
	default:
		return fmt.Errorf("unknown internal mode %q", mode)
	}
}

func readSupervisorConfig(path string) (supervisorConfig, error) {
	if path == "" {
		return supervisorConfig{}, errors.New("missing private supervisor config")
	}

	file, err := os.Open(path) //nolint:gosec // Path comes from the private environment entry written by this process.
	if err != nil {
		return supervisorConfig{}, fmt.Errorf("open private supervisor config: %w", err)
	}
	defer file.Close()
	defer os.Remove(path)

	var config supervisorConfig
	if err := json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&config); err != nil {
		return supervisorConfig{}, fmt.Errorf("decode private supervisor config: %w", err)
	}

	if config.NativePath == "" || config.Home == "" || config.Scratch == "" {
		return supervisorConfig{}, errors.New("private supervisor config is incomplete")
	}

	return config, nil
}

func writeSupervisorConfig(root string, config supervisorConfig) (string, error) {
	if root == "" {
		return "", errors.New("private supervisor scratch root is required")
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create private supervisor scratch root: %w", err)
	}

	if err := supervisorChmod(root, 0o700); err != nil {
		return "", fmt.Errorf("chmod private supervisor scratch root: %w", err)
	}

	var nonce [16]byte
	if _, err := supervisorRandRead(nonce[:]); err != nil {
		return "", fmt.Errorf("create private supervisor config nonce: %w", err)
	}

	path := filepath.Join(root, supervisorConfigPrefix+hex.EncodeToString(nonce[:])+".json")

	file, err := supervisorOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create private supervisor config: %w", err)
	}

	encodeErr := supervisorEncodeConfig(file, config)

	closeErr := file.Close()
	if err := errors.Join(encodeErr, closeErr); err != nil {
		_ = os.Remove(path)

		return "", fmt.Errorf("write private supervisor config: %w", err)
	}

	return path, nil
}

func supervisorCommand(ctx context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
	if config.ScratchParent == "" && config.Scratch != "" {
		config.ScratchParent = filepath.Dir(config.Scratch)
	}

	if config.LifecycleKind == "" {
		config.LifecycleKind = darwinLifecycleRuntime
	}

	markerNonce, err := supervisorNonce()
	if err != nil {
		return nil, nil, err
	}

	config.Started = filepath.Join(config.Scratch, "supervisor-started-"+markerNonce)
	config.Completion = filepath.Join(config.Scratch, "supervisor-complete-"+markerNonce)
	config.NativePIDFile = filepath.Join(config.Scratch, "supervisor-native-pid-"+markerNonce)
	config.InventoryIdentity = filepath.Join(config.Scratch, "supervisor-inventory-"+markerNonce)

	path, err := writeSupervisorConfig(config.Scratch, config)
	if err != nil {
		return nil, nil, err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = os.Remove(path)

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor: %w", err)
	}

	cmd := openCodeCommandContext(ctx, executable)
	cmd.Env = supervisorEnv(supervisorModeGuardian, path)

	return cmd, &supervisorProof{
		started: config.Started, completion: config.Completion, inventoryIdentity: config.InventoryIdentity,
	}, nil
}

func supervisorNonce() (string, error) {
	var nonce [16]byte
	if _, err := supervisorRandRead(nonce[:]); err != nil {
		return "", fmt.Errorf("create private supervisor marker nonce: %w", err)
	}

	return hex.EncodeToString(nonce[:]), nil
}

// awaitCompletion closes the guardian-SIGKILL gap for a still-running adapter.
// Shutdown calls it only after the guardian has exited. Only a completion
// marker proves the selected backend's containment boundary completed.
func (p *supervisorProof) awaitCompletion(ctx context.Context) error {
	if p == nil {
		return nil
	}

	if _, err := os.Stat(p.completion); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness completion proof: %w", err))
	}

	if _, err := os.Stat(p.started); errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrProcessContainmentIncomplete, errors.New("liveness start and completion proofs are both absent"))
	} else if err != nil {
		return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness start proof: %w", err))
	}

	for {
		if _, err := os.Stat(p.completion); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness completion proof: %w", err))
		}

		select {
		case <-ctx.Done():
			return errors.Join(ErrProcessContainmentIncomplete, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (p *supervisorProof) processSnapshot() (int, bool) {
	if p == nil {
		return 0, false
	}

	return supervisorProcessSnapshot(p.inventoryIdentity)
}

func supervisorEnv(mode string, configPath string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, supervisorModeEnv+"=") || strings.HasPrefix(entry, supervisorConfigEnv+"=") {
			continue
		}

		env = append(env, entry)
	}

	return append(env, supervisorModeEnv+"="+mode, supervisorConfigEnv+"="+configPath)
}

func runGuardian(config supervisorConfig) error {
	input := supervisorInput
	output := supervisorOutput
	errorOutput := supervisorError

	claim, err := homelock.AcquireClaim(config.Home)
	if err != nil {
		return err
	}
	defer func() { _ = claim.Release() }()

	containment, err := supervisorNewGuardianContainment(config)
	if err != nil {
		return err
	}
	defer containment.Close()

	config.JobName = containment.Name()
	if config.JobName == "darwin-best-effort" {
		config.DarwinBestEffort = true

		if config.ScratchParent == "" {
			config.ScratchParent = filepath.Dir(config.Scratch)
		}

		if config.LifecycleKind == "" {
			config.LifecycleKind = darwinLifecycleRuntime
		}
	}

	if config.JobName != "" {
		_ = writeSupervisorInventoryIdentity(config.InventoryIdentity, config.JobName)
	}

	livenessConfig, err := writeSupervisorConfig(config.Scratch, config)
	if err != nil {
		return err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("resolve liveness supervisor executable: %w", err)
	}

	cmd := supervisorExecCommand(executable)
	cmd.Env = supervisorEnv(supervisorModeLiveness, livenessConfig)
	configureIndependentSupervisor(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("open liveness control input: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("open liveness data output: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("open liveness control output: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("start liveness supervisor: %w", err)
	}

	livenessWaiter := newSupervisorWaiter(cmd, true)
	if _, err := releaseIndependentSupervisorWaiter(cmd, livenessWaiter); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = os.Remove(livenessConfig)

		return fmt.Errorf("capture liveness supervisor identity: %w", err)
	}

	livenessDone := livenessWaiter.result()

	control := bufio.NewReader(stderr)
	readyLine, readyErr := control.ReadString('\n')

	ready, parseErr := parseSupervisorReady(readyLine)
	if readyErr != nil || parseErr != nil {
		_ = stdin.Close()
		waitErr := <-livenessDone
		_, _ = io.Copy(errorOutput, control)

		var proofErr error

		if _, completeErr := os.Stat(config.Completion); completeErr != nil {
			if !errors.Is(completeErr, os.ErrNotExist) {
				return errors.Join(waitErr, completeErr)
			}

			if _, startedErr := os.Stat(config.Started); startedErr == nil {
				nativePID, pidErr := readNativePID(config.NativePIDFile)
				quiesceErr := awaitQuiescence(func() error {
					return supervisorGuardianQuiesce(containment, nativePID, supervisorQuiesceWindow)
				})

				proofErr = errors.Join(pidErr, quiesceErr)
				if proofErr == nil {
					proofErr = writeSupervisorMarker(config.Completion)
				}
			} else if !errors.Is(startedErr, os.ErrNotExist) {
				return errors.Join(waitErr, startedErr)
			}
		}

		return errors.Join(fmt.Errorf("liveness supervisor failed before readiness: %w", errors.Join(readyErr, parseErr)), waitErr, proofErr)
	}

	copyDone := make(chan struct{}, 3)
	go copySupervisorStream(stdin, input, copyDone)
	go copySupervisorStream(output, stdout, copyDone)
	go copySupervisorStream(errorOutput, control, copyDone)

	waitErr := <-livenessDone
	_ = stdin.Close()

	var proofErr error

	if _, completeErr := os.Stat(config.Completion); completeErr != nil {
		if !errors.Is(completeErr, os.ErrNotExist) {
			return errors.Join(waitErr, completeErr)
		}

		proofErr = awaitQuiescence(func() error {
			return supervisorGuardianQuiesce(containment, ready.NativePID, supervisorQuiesceWindow)
		})
		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}
	}

	if proofErr != nil {
		return errors.Join(waitErr, proofErr)
	}

	if waitErr != nil {
		return fmt.Errorf("liveness supervisor exited: %w", waitErr)
	}

	return nil
}

func runLiveness(config supervisorConfig) error {
	input := supervisorInput
	output := supervisorOutput
	errorOutput := supervisorError

	if err := writeSupervisorMarker(config.Started); err != nil {
		return err
	}

	liveness, err := homelock.AcquireLiveness(config.Home)
	if err != nil {
		return err
	}
	defer func() { _ = liveness.Release() }()

	containment, err := supervisorOpenLivenessContainment(config)
	if err != nil {
		return err
	}
	defer containment.Close()

	cmd := supervisorExecCommand(config.NativePath, config.NativeArgs...)
	cmd.Env = config.NativeEnv
	cmd.Dir = config.NativeDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open native data input: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return fmt.Errorf("open native data output: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return fmt.Errorf("open native error output: %w", err)
	}

	if startErr := containment.Start(cmd); startErr != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		return fmt.Errorf("start contained native root: %w", startErr)
	}

	waitDone := containment.Wait()

	pidErr := writeNativePID(config.NativePIDFile, cmd.Process.Pid)
	if pidErr != nil {
		proofErr := awaitQuiescence(func() error {
			return containment.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		return errors.Join(pidErr, proofErr)
	}

	// This fixed, integer-only object has no JSON encoding failure mode.
	ready := fmt.Appendf(nil, "{\"nativePid\":%d}", cmd.Process.Pid)

	if _, err := fmt.Fprintln(errorOutput, supervisorReadyPrefix+string(ready)); err != nil {
		proofErr := awaitQuiescence(func() error {
			return containment.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		return errors.Join(fmt.Errorf("publish supervisor readiness: %w", err), proofErr)
	}

	controlDone := make(chan struct{})

	go func() {
		_, _ = io.Copy(stdin, input)
		_ = stdin.Close()

		close(controlDone)
	}()
	go func() { _, _ = io.Copy(output, stdout) }()
	go func() { _, _ = io.Copy(errorOutput, stderr) }()

	select {
	case waitErr := <-waitDone:
		proofErr := awaitQuiescence(func() error {
			// The root has already been reaped. The containment retains the
			// captured original identity and must not rediscover a reused PID.
			return containment.Quiesce(0, supervisorQuiesceWindow)
		})
		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		if proofErr != nil {
			return errors.Join(waitErr, proofErr)
		}

		if waitErr != nil {
			return fmt.Errorf("native root exited: %w", waitErr)
		}

		return nil
	case <-controlDone:
		proofErr := awaitQuiescence(func() error {
			return containment.Quiesce(cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		if proofErr == nil {
			proofErr = writeSupervisorMarker(config.Completion)
		}

		return proofErr
	}
}

func awaitQuiescence(probe func() error) error {
	err := probe()
	if err == nil || errors.Is(err, ErrProcessContainmentIncomplete) {
		return err
	}

	return fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, err)
}

func writeSupervisorMarker(path string) error {
	if path == "" {
		return errors.New("private supervisor proof path is required")
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("write private supervisor proof: %w", err)
	}

	return file.Close()
}

func writeNativePID(path string, pid int) error {
	if path == "" || pid <= 0 {
		return errors.New("private native PID proof is invalid")
	}

	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o600); err != nil {
		return fmt.Errorf("write private native PID proof: %w", err)
	}

	return nil
}

func writeSupervisorInventoryIdentity(path string, identity string) error {
	if path == "" || identity == "" {
		return errors.New("private supervisor inventory identity is invalid")
	}

	file, err := supervisorOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private supervisor inventory identity: %w", err)
	}

	_, writeErr := fmt.Fprintln(file, identity)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)

		return fmt.Errorf("write private supervisor inventory identity: %w", err)
	}

	return nil
}

func readNativePID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	var pid int
	if _, err := fmt.Sscanf(string(raw), "%d", &pid); err != nil || pid <= 0 {
		return 0, errors.New("private native PID proof is invalid")
	}

	return pid, nil
}

func parseSupervisorReady(line string) (supervisorReady, error) {
	if !strings.HasPrefix(line, supervisorReadyPrefix) {
		return supervisorReady{}, fmt.Errorf("invalid readiness frame %q", strings.TrimSpace(line))
	}

	var ready supervisorReady
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, supervisorReadyPrefix))), &ready); err != nil {
		return supervisorReady{}, fmt.Errorf("decode readiness frame: %w", err)
	}

	if ready.NativePID <= 0 {
		return supervisorReady{}, errors.New("readiness frame omitted native PID")
	}

	return ready, nil
}

func copySupervisorStream(dst io.Writer, src io.Reader, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	if closer, ok := dst.(io.Closer); ok {
		_ = closer.Close()
	}

	done <- struct{}{}
}
