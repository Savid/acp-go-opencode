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
	supervisorModeGuardian  = "guardian"
	supervisorModeLiveness  = "liveness"
	supervisorReadyPrefix   = "acp-go-opencode supervisor-ready "
	supervisorConfigPrefix  = "supervisor-config-"
	supervisorQuiesceWindow = 5 * time.Second
	darwinBestEffortJobName = "darwin-best-effort"
)

// ErrProcessContainmentIncomplete marks shutdown failures for which the native process
// tree has not been proved empty. Embedders must retain native-root ownership
// when this sentinel is present.
var ErrProcessContainmentIncomplete = errors.New("OpenCode process containment incomplete")

type supervisorConfig struct {
	NativePath          string            `json:"nativePath"`
	NativeArgs          []string          `json:"nativeArgs"`
	NativeEnv           []string          `json:"nativeEnv"`
	NativeDir           string            `json:"nativeDir,omitempty"`
	Home                string            `json:"home"`
	Scratch             string            `json:"scratch"`
	ScratchParent       string            `json:"scratchParent"`
	LifecycleKind       string            `json:"lifecycleKind"`
	DarwinBestEffort    bool              `json:"darwinBestEffort"`
	JobName             string            `json:"jobName,omitempty"`
	Started             string            `json:"started"`
	Completion          string            `json:"completion"`
	Quarantine          string            `json:"quarantine"`
	NativePIDFile       string            `json:"nativePidFile"`
	InventoryIdentity   string            `json:"inventoryIdentity"`
	IsolationUID        uint32            `json:"isolationUid"`
	IsolationGID        uint32            `json:"isolationGid"`
	StandaloneOwnerID   string            `json:"standaloneOwnerId"`
	StandaloneStateRoot string            `json:"standaloneStateRoot"`
	IdentityLock        bool              `json:"identityLock"`
	AuthorityDomain     bool              `json:"authorityDomain"`
	StandaloneAuthority bool              `json:"standaloneAuthority"`
	Isolation           *ProcessIsolation `json:"-"`
}

type supervisorReady struct {
	NativePID int `json:"nativePid"`
}

var supervisorExecutable = os.Executable
var supervisorExecCommand = exec.Command
var supervisorRandRead = rand.Read
var supervisorChmod = os.Chmod
var supervisorOpenFile = os.OpenFile
var supervisorCreateTemp = os.CreateTemp
var supervisorRemove = os.Remove
var supervisorOpen = os.Open
var supervisorInheritedFile = os.NewFile
var supervisorPipe = os.Pipe
var supervisorEncodeConfig = func(writer io.Writer, config supervisorConfig) error {
	return json.NewEncoder(writer).Encode(config)
}
var supervisorNewGuardianContainment = newGuardianContainment
var supervisorGuardianName = func(containment *guardianContainment) string { return containment.Name() }
var supervisorGuardianQuiesce = func(containment *guardianContainment, nativePID int, timeout time.Duration) error {
	return containment.Quiesce(nativePID, timeout)
}
var supervisorOpenLivenessContainment = openLivenessContainment
var supervisorLivenessQuiesce = func(containment *livenessContainment, nativePID int, timeout time.Duration) error {
	return containment.Quiesce(nativePID, timeout)
}
var supervisorQuarantineRetry func(*livenessContainment) error
var supervisorGuardianQuarantineRetry func(*guardianContainment) error
var supervisorReleaseIndependentWaiter = releaseIndependentSupervisorWaiter
var supervisorStartIndependent = startIndependentSupervisor
var supervisorInput io.Reader = os.Stdin
var supervisorOutput io.Writer = os.Stdout
var supervisorError io.Writer = os.Stderr
var supervisorExit = os.Exit
var supervisorProcessSnapshot = querySupervisorProcessSnapshot
var supervisorWriteConfig = writeSupervisorConfig

// The identity seams below carry no initializer: every platform installs its own
// through configureSupervisorPlatform, because the answers they stand for are
// what differs between platforms. Linux binds the real agent identity registry;
// everywhere else the supervisor holds no identity to bind.
var supervisorMarkerRoot func(config supervisorConfig) (string, error)
var supervisorAcquireIdentityAuthority func(
	uint32, uint32, string, string, io.Reader,
) (supervisorIdentityLock, supervisorIdentityLock, error)
var supervisorVerifyTrustedIdentity func(uint32) error
var supervisorAdoptIdentityLock func(uint32) (supervisorIdentityLock, error)
var supervisorAdoptAuthorityDomain func(uint32) (supervisorIdentityLock, error)
var supervisorValidateAdoptedAuthority func(supervisorConfig) error
var supervisorGuardianPeer *os.File
var supervisorValidateGuardianPeer func(*os.File, <-chan struct{}) error

type supervisorIdentityLock interface {
	io.Closer
	InheritedFile() *os.File
}

type noopSupervisorIdentityLock struct{}

func (noopSupervisorIdentityLock) Close() error            { return nil }
func (noopSupervisorIdentityLock) InheritedFile() *os.File { return nil }

type supervisorProof struct {
	started           string
	completion        string
	quarantine        string
	nativePIDFile     string
	inventoryIdentity string
	inherited         []*os.File
}

func (p *supervisorProof) closeInherited() error {
	if p == nil {
		return nil
	}

	var result error
	for _, file := range p.inherited {
		result = errors.Join(result, file.Close())
	}

	p.inherited = nil

	return result
}

func supervisorBootstrap() {
	mode := os.Getenv(supervisorModeEnv)
	if mode == "" {
		return
	}

	var err error

	configFile := supervisorInheritedFile(3, "acp-go-opencode-supervisor-config")
	if err == nil && configFile == nil {
		err = errors.New("process supervisor inherited config descriptor is unavailable")
	}

	if err == nil {
		err = closeInheritedOnExec(configFile)
	}

	var guardianPeer *os.File
	if err == nil && mode == supervisorModeLiveness {
		guardianPeer = supervisorInheritedFile(6, "acp-go-opencode-supervisor-guardian-peer")
		if guardianPeer == nil {
			err = errors.New("liveness supervisor inherited guardian peer descriptor is unavailable")
		} else {
			err = closeInheritedOnExec(guardianPeer)
		}
	}

	if err == nil {
		supervisorGuardianPeer = guardianPeer
		err = runSupervisor(mode, configFile)
	}

	if configFile != nil {
		_ = configFile.Close()
	}

	if guardianPeer != nil {
		_ = guardianPeer.Close()
	}

	if err != nil {
		_, _ = fmt.Fprintln(supervisorError, "acp-go-opencode runtime supervisor:", err)

		supervisorExit(1)

		return
	}

	supervisorExit(0)
}

func runSupervisor(mode string, configInput io.Reader) (runErr error) {
	config, err := readSupervisorConfig(configInput)
	if err != nil {
		return err
	}

	if config.IdentityLock != config.AuthorityDomain {
		return errors.New("supervisor UID lock and authority domain are inconsistent")
	}

	if mode == supervisorModeLiveness && config.IdentityLock {
		lock, lockErr := supervisorAdoptIdentityLock(config.IsolationUID)
		if lockErr != nil {
			return lockErr
		}

		defer func() { runErr = errors.Join(runErr, lock.Close()) }()

		domain, domainErr := supervisorAdoptAuthorityDomain(config.IsolationUID)
		if domainErr != nil {
			return domainErr
		}

		defer func() { runErr = errors.Join(runErr, domain.Close()) }()

		if validationErr := supervisorValidateAdoptedAuthority(config); validationErr != nil {
			return validationErr
		}
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

func readSupervisorConfig(reader io.Reader) (supervisorConfig, error) {
	if reader == nil {
		return supervisorConfig{}, errors.New("missing private supervisor config descriptor")
	}

	var config supervisorConfig
	if err := json.NewDecoder(io.LimitReader(reader, 8<<20)).Decode(&config); err != nil {
		return supervisorConfig{}, fmt.Errorf("decode private supervisor config: %w", err)
	}

	if config.NativePath == "" || config.Home == "" || config.Scratch == "" || config.IsolationUID == 0 || config.IsolationGID == 0 {
		return supervisorConfig{}, errors.New("private supervisor config is incomplete")
	}

	return config, nil
}

func writeSupervisorConfig(root string, config supervisorConfig) (*os.File, error) {
	if root == "" {
		return nil, errors.New("private supervisor scratch root is required")
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create private supervisor scratch root: %w", err)
	}

	if err := supervisorChmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("chmod private supervisor scratch root: %w", err)
	}

	file, err := supervisorCreateTemp(root, supervisorConfigPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create private supervisor config: %w", err)
	}

	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = supervisorRemove(path)

		return nil, fmt.Errorf("secure private supervisor config: %w", err)
	}

	encodeErr := supervisorEncodeConfig(file, config)
	if encodeErr != nil {
		_ = file.Close()
		_ = supervisorRemove(path)

		return nil, fmt.Errorf("write private supervisor config: %w", encodeErr)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		_ = supervisorRemove(path)

		return nil, fmt.Errorf("rewind private supervisor config: %w", err)
	}

	if err := supervisorRemove(path); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("unlink private supervisor config: %w", err)
	}

	return file, nil
}

func supervisorCommand(ctx context.Context, config supervisorConfig) (*exec.Cmd, *supervisorProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	if err := validateProcessIsolation(config.Isolation); err != nil {
		return nil, nil, err
	}

	if (config.Isolation.IdentityLock == nil) != (config.Isolation.AuthorityDomain == nil) {
		return nil, nil, errors.New("OpenCode supervisor requires the UID lock and authority domain together")
	}

	if err := supervisorVerifyTrustedIdentity(config.Isolation.UID); err != nil {
		return nil, nil, err
	}

	config.IsolationUID = config.Isolation.UID
	config.IsolationGID = config.Isolation.GID
	config.StandaloneOwnerID = config.Isolation.StandaloneOwnerID
	config.StandaloneStateRoot = config.Isolation.StandaloneStateRoot
	config.IdentityLock = config.Isolation.IdentityLock != nil
	config.AuthorityDomain = config.Isolation.AuthorityDomain != nil
	config.StandaloneAuthority = config.Isolation.IdentityLock == nil

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

	markerRoot, err := supervisorMarkerRoot(config)
	if err != nil {
		return nil, nil, err
	}

	config.Started = filepath.Join(markerRoot, "supervisor-started-"+markerNonce)
	config.Completion = filepath.Join(markerRoot, "supervisor-complete-"+markerNonce)
	config.Quarantine = filepath.Join(markerRoot, "supervisor-quarantine-"+markerNonce)
	config.NativePIDFile = filepath.Join(markerRoot, "supervisor-native-pid-"+markerNonce)
	config.InventoryIdentity = filepath.Join(markerRoot, "supervisor-inventory-"+markerNonce)

	configFile, err := supervisorWriteConfig(config.Scratch, config)
	if err != nil {
		return nil, nil, err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = configFile.Close()

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor: %w", err)
	}

	helperEnv := []string{supervisorModeEnv + "=" + supervisorModeGuardian}

	executable, err = resolveProcessExecutable(executable, helperEnv)
	if err != nil {
		_ = configFile.Close()

		return nil, nil, fmt.Errorf("resolve embedded runtime supervisor through process policy: %w", err)
	}

	cmd := openCodeCommandContext(ctx, executable)
	cmd.Cancel = nil
	cmd.WaitDelay = 0
	cmd.Env = helperEnv
	cmd.Dir = "/"

	cmd.ExtraFiles = []*os.File{configFile}
	inherited := []*os.File{configFile}

	if config.Isolation.IdentityLock != nil {
		identityLock, duplicateErr := config.Isolation.IdentityLock.Duplicate()
		if duplicateErr != nil {
			_ = configFile.Close()

			return nil, nil, fmt.Errorf("duplicate OpenCode agent identity lock: %w", duplicateErr)
		}

		cmd.ExtraFiles = append(cmd.ExtraFiles, identityLock)
		inherited = append(inherited, identityLock)

		authorityDomain, duplicateErr := config.Isolation.AuthorityDomain.Duplicate()
		if duplicateErr != nil {
			_ = identityLock.Close()
			_ = configFile.Close()

			return nil, nil, fmt.Errorf("duplicate OpenCode agent authority domain: %w", duplicateErr)
		}

		cmd.ExtraFiles = append(cmd.ExtraFiles, authorityDomain)
		inherited = append(inherited, authorityDomain)
	}

	return cmd, &supervisorProof{
		started: config.Started, completion: config.Completion, quarantine: config.Quarantine,
		nativePIDFile: config.NativePIDFile, inventoryIdentity: config.InventoryIdentity,
		inherited: inherited,
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
		p.removeTerminalMarkers()

		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness completion proof: %w", err))
	}

	if quarantined, err := supervisorMarkerExists(p.quarantine); err != nil {
		return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness quarantine proof: %w", err))
	} else if quarantined {
		return errors.Join(ErrProcessContainmentIncomplete, errors.New("liveness supervisor retained the identity lock while quarantining descendants"))
	}

	if _, err := os.Stat(p.started); errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrProcessContainmentIncomplete, errors.New("liveness start and completion proofs are both absent"))
	} else if err != nil {
		return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness start proof: %w", err))
	}

	for {
		if _, err := os.Stat(p.completion); err == nil {
			p.removeTerminalMarkers()

			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness completion proof: %w", err))
		}

		if quarantined, err := supervisorMarkerExists(p.quarantine); err != nil {
			return errors.Join(ErrProcessContainmentIncomplete, fmt.Errorf("stat liveness quarantine proof: %w", err))
		} else if quarantined {
			return errors.Join(ErrProcessContainmentIncomplete, errors.New("liveness supervisor retained the identity lock while quarantining descendants"))
		}

		select {
		case <-ctx.Done():
			return errors.Join(ErrProcessContainmentIncomplete, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (p *supervisorProof) removeTerminalMarkers() {
	if p == nil {
		return
	}

	removeSupervisorMarkers(p.started, p.completion, p.quarantine, p.nativePIDFile, p.inventoryIdentity)
}

func (p *supervisorProof) quarantineDetected() (bool, error) {
	if p == nil {
		return false, nil
	}

	return supervisorMarkerExists(p.quarantine)
}

func (p *supervisorProof) processSnapshot() (int, bool) {
	if p == nil {
		return 0, false
	}

	return supervisorProcessSnapshot(p.inventoryIdentity)
}

func runGuardian(config supervisorConfig) (runErr error) { //nolint:gocyclo // Guardian startup owns the ordered containment handoff and recovery sequence.
	input := supervisorInput
	output := supervisorOutput
	errorOutput := supervisorError

	var (
		identityLock    supervisorIdentityLock
		authorityDomain supervisorIdentityLock = noopSupervisorIdentityLock{}
	)

	if config.IdentityLock {
		var adoptErr error

		identityLock, adoptErr = supervisorAdoptIdentityLock(config.IsolationUID)
		if adoptErr != nil {
			return adoptErr
		}

		authorityDomain, adoptErr = supervisorAdoptAuthorityDomain(config.IsolationUID)
		if adoptErr != nil {
			return adoptErr
		}

		if validationErr := supervisorValidateAdoptedAuthority(config); validationErr != nil {
			return validationErr
		}
	} else {
		var err error

		identityLock, authorityDomain, err = acquireGuardianIdentityAuthority(config, input)
		if err != nil {
			return err
		}
	}

	defer func() { runErr = errors.Join(runErr, identityLock.Close(), authorityDomain.Close()) }()

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

	config.JobName = supervisorGuardianName(containment)
	if config.JobName == darwinBestEffortJobName {
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

	lockFile := identityLock.InheritedFile()
	domainFile := authorityDomain.InheritedFile()
	config.IdentityLock = lockFile != nil
	config.AuthorityDomain = domainFile != nil

	peerRead, peerWrite, err := supervisorPipe()
	if err != nil {
		return fmt.Errorf("open liveness guardian peer: %w", err)
	}
	defer peerRead.Close()
	defer peerWrite.Close()

	identityExtra := lockFile
	if identityExtra == nil {
		identityExtra, err = supervisorOpen("/dev/null")
		if err != nil {
			return fmt.Errorf("open liveness identity placeholder: %w", err)
		}
		defer identityExtra.Close()
	}

	domainExtra := domainFile
	if domainExtra == nil {
		domainExtra, err = supervisorOpen("/dev/null")
		if err != nil {
			return fmt.Errorf("open liveness authority placeholder: %w", err)
		}
		defer domainExtra.Close()
	}

	livenessConfig, err := supervisorWriteConfig(config.Scratch, config)
	if err != nil {
		return err
	}

	executable, err := supervisorExecutable()
	if err != nil {
		_ = livenessConfig.Close()

		return fmt.Errorf("resolve liveness supervisor executable: %w", err)
	}

	executable, err = resolveProcessExecutable(executable, config.NativeEnv)
	if err != nil {
		_ = livenessConfig.Close()

		return fmt.Errorf("resolve liveness supervisor executable through process policy: %w", err)
	}

	cmd := supervisorExecCommand(executable)
	cmd.Env = []string{supervisorModeEnv + "=" + supervisorModeLiveness}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{livenessConfig, identityExtra, domainExtra, peerRead}

	configureIndependentSupervisor(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = livenessConfig.Close()

		return fmt.Errorf("open liveness control input: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = livenessConfig.Close()

		return fmt.Errorf("open liveness data output: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = livenessConfig.Close()

		return fmt.Errorf("open liveness control output: %w", err)
	}

	if err := supervisorStartIndependent(cmd); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = livenessConfig.Close()

		return fmt.Errorf("start liveness supervisor: %w", err)
	}

	_ = livenessConfig.Close()
	_ = peerRead.Close()

	livenessWaiter := newSupervisorWaiter(cmd, true)
	if _, err := supervisorReleaseIndependentWaiter(cmd, livenessWaiter); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		return fmt.Errorf("capture liveness supervisor identity: %w", err)
	}

	livenessDone := livenessWaiter.result()

	control := bufio.NewReader(stderr)
	readyLine, readyErr := control.ReadString('\n')

	ready, parseErr := parseSupervisorReady(readyLine)
	if readyErr != nil || parseErr != nil {
		_ = stdin.Close()

		waitErr, quarantined, terminalErr := awaitLivenessTerminal(livenessDone, config.Quarantine)
		if quarantined || terminalErr != nil {
			_ = stdout.Close()
			_ = stderr.Close()

			return errors.Join(terminalErr, finishQuarantinedLiveness(livenessDone, config))
		}

		_, _ = io.Copy(errorOutput, control)

		var proofErr error

		if _, completeErr := os.Stat(config.Completion); completeErr != nil {
			if !errors.Is(completeErr, os.ErrNotExist) {
				return errors.Join(waitErr, completeErr)
			}

			if _, startedErr := os.Stat(config.Started); startedErr == nil {
				nativePID, pidErr := readNativePID(config.NativePIDFile)
				if markerErr := writeGuardianQuarantineMarker(config); markerErr != nil {
					return errors.Join(waitErr, markerErr)
				}

				quiesceErr := awaitQuiescence(func() error {
					return supervisorGuardianQuiesce(containment, nativePID, supervisorQuiesceWindow)
				})

				proofErr = errors.Join(pidErr, quiesceErr)
				proofErr = completeOrQuarantineGuardian(config, containment, proofErr)
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

	waitErr, quarantined, terminalErr := awaitLivenessTerminal(livenessDone, config.Quarantine)

	_ = stdin.Close()
	if quarantined || terminalErr != nil {
		_ = stdout.Close()
		_ = stderr.Close()

		return errors.Join(terminalErr, finishQuarantinedLiveness(livenessDone, config))
	}

	return finishGuardianLiveness(config, containment, ready.NativePID, waitErr)
}

func finishGuardianLiveness(config supervisorConfig, containment *guardianContainment, nativePID int, waitErr error) error {
	var proofErr error

	if _, completeErr := os.Stat(config.Completion); completeErr != nil {
		if !errors.Is(completeErr, os.ErrNotExist) {
			return errors.Join(waitErr, completeErr)
		}

		if markerErr := writeGuardianQuarantineMarker(config); markerErr != nil {
			return errors.Join(waitErr, markerErr)
		}

		proofErr = awaitQuiescence(func() error {
			return supervisorGuardianQuiesce(containment, nativePID, supervisorQuiesceWindow)
		})
		proofErr = completeOrQuarantineGuardian(config, containment, proofErr)
	}

	if proofErr != nil {
		return errors.Join(waitErr, proofErr)
	}

	if waitErr != nil {
		return fmt.Errorf("liveness supervisor exited: %w", waitErr)
	}

	return nil
}

func acquireGuardianIdentityAuthority(
	config supervisorConfig,
	control io.Reader,
) (supervisorIdentityLock, supervisorIdentityLock, error) {
	if config.IsolationUID == 0 {
		return noopSupervisorIdentityLock{}, noopSupervisorIdentityLock{}, nil
	}

	if err := supervisorVerifyTrustedIdentity(config.IsolationUID); err != nil {
		return nil, nil, err
	}

	return supervisorAcquireIdentityAuthority(
		config.IsolationUID,
		config.IsolationGID,
		config.StandaloneOwnerID,
		config.StandaloneStateRoot,
		control,
	)
}

func runLiveness(config supervisorConfig) error {
	input := supervisorInput
	output := supervisorOutput
	errorOutput := supervisorError

	if config.IsolationUID != 0 {
		if verifyErr := supervisorVerifyTrustedIdentity(config.IsolationUID); verifyErr != nil {
			return verifyErr
		}
	}

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

	guardianDone := make(chan struct{})

	if supervisorGuardianPeer != nil {
		go func() {
			_, _ = io.Copy(io.Discard, supervisorGuardianPeer)

			close(guardianDone)
		}()
	}

	cmd := supervisorExecCommand(config.NativePath, config.NativeArgs...)
	cmd.Env = config.NativeEnv
	cmd.Dir = config.NativeDir

	if config.IsolationUID != 0 || config.IsolationGID != 0 {
		if credentialErr := applyProcessCredential(cmd, supervisedNativeIsolation(config)); credentialErr != nil {
			return fmt.Errorf("apply supervised OpenCode native identity: %w", credentialErr)
		}
	}

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

	if peerErr := supervisorValidateGuardianPeer(supervisorGuardianPeer, guardianDone); peerErr != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow)
		})
		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return errors.Join(peerErr, proofErr)
	}

	var finalPeerErr error

	containment.beforeStart = func() error {
		finalPeerErr = supervisorValidateGuardianPeer(supervisorGuardianPeer, guardianDone)

		return finalPeerErr
	}

	if startErr := containment.Start(cmd); startErr != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		if finalPeerErr != nil {
			proofErr := awaitQuiescence(func() error {
				return supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow)
			})
			proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

			return errors.Join(finalPeerErr, proofErr)
		}

		return fmt.Errorf("start contained native root: %w", startErr)
	}

	waitDone := containment.Wait()

	pidErr := writeNativePID(config.NativePIDFile, cmd.Process.Pid)
	if pidErr != nil {
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return errors.Join(pidErr, proofErr)
	}

	// This fixed, integer-only object has no JSON encoding failure mode.
	ready := fmt.Appendf(nil, "{\"nativePid\":%d}", cmd.Process.Pid)

	if _, err := fmt.Fprintln(errorOutput, supervisorReadyPrefix+string(ready)); err != nil {
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

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
			return supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow)
		})

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)
		if proofErr != nil {
			return errors.Join(waitErr, proofErr)
		}

		if waitErr != nil {
			return fmt.Errorf("native root exited: %w", waitErr)
		}

		return nil
	case <-controlDone:
		proofErr := awaitQuiescence(func() error {
			return supervisorLivenessQuiesce(containment, cmd.Process.Pid, supervisorQuiesceWindow)
		})

		<-waitDone

		proofErr = completeOrQuarantineLiveness(config, containment, proofErr)

		return proofErr
	}
}

func supervisedNativeIsolation(config supervisorConfig) *ProcessIsolation {
	isolation := &ProcessIsolation{
		UID:             config.IsolationUID,
		GID:             config.IsolationGID,
		BaseEnvironment: environmentMap(config.NativeEnv),
	}
	if config.IdentityLock && config.AuthorityDomain {
		isolation.identityAuthorityAdopted = true

		return isolation
	}

	isolation.StandaloneOwnerID = config.StandaloneOwnerID
	isolation.StandaloneStateRoot = config.StandaloneStateRoot

	return isolation
}

func completeOrQuarantineLiveness(config supervisorConfig, containment *livenessContainment, proofErr error) error {
	if proofErr == nil {
		return writeSupervisorMarker(config.Completion)
	}

	if supervisorQuarantineRetry == nil || config.Quarantine == "" {
		return proofErr
	}

	if err := writeSupervisorMarker(config.Quarantine); err != nil {
		return errors.Join(proofErr, err)
	}

	closeSupervisorQuarantineStreams()

	retryErr := supervisorQuarantineRetry(containment)

	removeSupervisorMarkers(config.Started, config.Completion, config.NativePIDFile, config.InventoryIdentity)

	return errors.Join(proofErr, retryErr)
}

func writeGuardianQuarantineMarker(config supervisorConfig) error {
	if config.Quarantine == "" {
		return nil
	}

	return writeSupervisorMarker(config.Quarantine)
}

func completeOrQuarantineGuardian(config supervisorConfig, containment *guardianContainment, proofErr error) error {
	if proofErr == nil {
		return writeSupervisorMarker(config.Completion)
	}

	if supervisorGuardianQuarantineRetry == nil || config.Quarantine == "" {
		return proofErr
	}

	if err := writeSupervisorMarker(config.Quarantine); err != nil {
		return errors.Join(proofErr, err)
	}

	closeSupervisorQuarantineStreams()

	retryErr := supervisorGuardianQuarantineRetry(containment)

	removeSupervisorMarkers(config.Started, config.Completion, config.Quarantine, config.NativePIDFile, config.InventoryIdentity)

	return errors.Join(proofErr, retryErr)
}

func finishQuarantinedLiveness(done <-chan error, config supervisorConfig) error {
	waitErr := <-done

	removeSupervisorMarkers(config.Started, config.Completion, config.Quarantine, config.NativePIDFile, config.InventoryIdentity)

	return errors.Join(waitErr, fmt.Errorf("%w: liveness supervisor completed containment quarantine", ErrProcessContainmentIncomplete))
}

func awaitLivenessTerminal(done <-chan error, quarantine string) (error, bool, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return err, false, nil
		case <-ticker.C:
			present, err := supervisorMarkerExists(quarantine)
			if err != nil {
				return nil, false, fmt.Errorf("stat liveness quarantine proof: %w", err)
			}

			if present {
				return nil, true, nil
			}
		}
	}
}

func supervisorMarkerExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}

	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

func removeSupervisorMarkers(paths ...string) {
	for _, path := range paths {
		if path != "" {
			_ = os.Remove(path)
		}
	}
}

func closeSupervisorQuarantineStreams() {
	for _, stream := range []any{supervisorInput, supervisorOutput, supervisorError} {
		if file, ok := stream.(*os.File); ok {
			_ = file.Close()
		}
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
