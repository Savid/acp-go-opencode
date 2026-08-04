//go:build linux

package opencode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const linuxAgentIdentityNamespace = "/run/acp-go/agent-identities"
const linuxSupervisorProofNamespace = "/run/acp-go/supervisor-proofs"

type linuxAgentIdentityLock struct {
	file *os.File
}

func init() {
	supervisorWriteConfig = writeLinuxSupervisorConfig
	supervisorMarkerRoot = linuxSupervisorMarkerRoot
	supervisorAcquireIdentityLock = acquireLinuxAgentIdentityLock
	supervisorVerifyTrustedIdentity = verifyLinuxTrustedSupervisorIdentity
	supervisorAdoptIdentityLock = adoptLinuxAgentIdentityLock
	supervisorQuarantineRetry = retryLinuxLivenessContainment
	supervisorGuardianQuarantineRetry = retryLinuxGuardianContainment
}

func writeLinuxSupervisorConfig(_ string, config supervisorConfig) (*os.File, error) {
	fd, err := unix.MemfdCreate("acp-go-opencode-supervisor-config", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create private supervisor config descriptor: %w", err)
	}

	file := os.NewFile(uintptr(fd), "acp-go-opencode-supervisor-config")
	if err := unix.Fchmod(fd, 0o600); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("secure private supervisor config descriptor: %w", err)
	}
	if err := supervisorEncodeConfig(file, config); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("write private supervisor config descriptor: %w", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("seal private supervisor config descriptor: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("rewind private supervisor config descriptor: %w", err)
	}

	return file, nil
}

func verifyLinuxTrustedSupervisorIdentity(uid uint32) error {
	if os.Geteuid() != 0 || uid == 0 || uint32(os.Geteuid()) == uid {
		return errors.New("OpenCode liveness supervisor requires a distinct trusted root identity")
	}

	return nil
}

func linuxSupervisorMarkerRoot(supervisorConfig) (string, error) {
	fd, err := openLinuxACPGoDirectory("supervisor-proofs")
	if err != nil {
		return "", err
	}
	if err := unix.Close(fd); err != nil {
		return "", fmt.Errorf("close agent identity lock namespace: %w", err)
	}

	return linuxSupervisorProofNamespace, nil
}

func acquireLinuxAgentIdentityLock(uid uint32, control io.Reader) (supervisorIdentityLock, error) {
	namespaceFD, err := openLinuxAgentIdentityNamespace()
	if err != nil {
		return nil, err
	}

	name := strconv.FormatUint(uint64(uid), 10) + ".lock"
	fd, err := unix.Openat(namespaceFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	_ = unix.Close(namespaceFD)
	if err != nil {
		return nil, fmt.Errorf("open agent identity lock %q: %w", name, err)
	}
	if err := hardenLinuxAgentIdentityLock(fd); err != nil {
		_ = unix.Close(fd)

		return nil, err
	}

	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &linuxAgentIdentityLock{file: os.NewFile(uintptr(fd), name)}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = unix.Close(fd)

			return nil, fmt.Errorf("acquire agent identity lock for uid %d: %w", uid, err)
		}
		if linuxSupervisorControlClosed(control) {
			_ = unix.Close(fd)

			return nil, fmt.Errorf("%w: control closed while waiting for agent identity uid %d", ErrProcessContainmentIncomplete, uid)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func adoptLinuxAgentIdentityLock() (io.Closer, error) {
	file := supervisorInheritedFile(4, "acp-go-opencode-agent-identity-lock")
	if file == nil {
		return nil, errors.New("inherited agent identity lock descriptor is unavailable")
	}
	if err := closeInheritedOnExec(file); err != nil {
		_ = file.Close()

		return nil, err
	}

	return file, nil
}

func openLinuxAgentIdentityNamespace() (int, error) {
	return openLinuxACPGoDirectory("agent-identities")
}

func openLinuxACPGoDirectory(name string) (int, error) {
	if os.Geteuid() != 0 {
		return -1, errors.New("agent identity namespace requires a trusted root supervisor")
	}

	runFD, err := openLinuxAgentIdentityDirectory(unix.AT_FDCWD, "/run", 0o755, false)
	if err != nil {
		return -1, err
	}
	defer unix.Close(runFD)

	acpGoFD, err := openLinuxAgentIdentityDirectory(runFD, "acp-go", 0o700, true)
	if err != nil {
		return -1, err
	}
	defer unix.Close(acpGoFD)

	return openLinuxAgentIdentityDirectory(acpGoFD, name, 0o700, true)
}

func openLinuxAgentIdentityDirectory(parentFD int, name string, mode uint32, create bool) (int, error) {
	if create {
		err := unix.Mkdirat(parentFD, name, mode)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, fmt.Errorf("create agent identity directory %q: %w", name, err)
		}
	}

	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open agent identity directory %q: %w", name, err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("stat agent identity directory %q: %w", name, err)
	}
	if stat.Uid != 0 || stat.Gid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 || create && stat.Mode&0o777 != 0o700 {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("agent identity directory %q is not root-owned and protected", name)
	}

	return fd, nil
}

func hardenLinuxAgentIdentityLock(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("stat agent identity lock: %w", err)
	}

	return validateLinuxAgentIdentityLock(stat)
}

func validateLinuxAgentIdentityLock(stat unix.Stat_t) error {
	if stat.Uid != 0 || stat.Gid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Mode&0o777 != 0o600 {
		return errors.New("agent identity lock is not a root-owned mode-0600 single-link regular file")
	}

	return nil
}

func linuxSupervisorControlClosed(control io.Reader) bool {
	file, ok := control.(*os.File)
	if !ok {
		return false
	}
	poll := []unix.PollFd{{Fd: int32(file.Fd()), Events: unix.POLLHUP | unix.POLLERR}} //nolint:gosec // inherited descriptors fit pollfd.
	count, err := unix.Poll(poll, 0)

	return err == nil && count > 0 && poll[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0
}

func (lock *linuxAgentIdentityLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil

	return file.Close()
}

func retryLinuxLivenessContainment(containment *livenessContainment) error {
	for {
		if err := supervisorLivenessQuiesce(containment, 0, supervisorQuiesceWindow); err == nil {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func retryLinuxGuardianContainment(containment *guardianContainment) error {
	for {
		if err := supervisorGuardianQuiesce(containment, 0, supervisorQuiesceWindow); err == nil {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}
}

func (lock *linuxAgentIdentityLock) InheritedFile() *os.File {
	if lock == nil {
		return nil
	}

	return lock.file
}
