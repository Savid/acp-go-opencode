//go:build linux

package opencode

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const linuxAgentIdentityNamespace = "/var/lib/acp-go/agent-identities"
const linuxSupervisorProofNamespace = "/run/acp-go/supervisor-proofs"

// Seams for the private supervisor config descriptor. The anonymous memory the
// kernel just handed back always accepts a mode change and always rewinds, so
// the fail-closed guards around those steps cannot be reached through the real
// primitives; tests swap these to prove the guards abort and leave no
// descriptor behind.
var linuxSupervisorMemfdCreate = unix.MemfdCreate
var linuxSupervisorSeek = (*os.File).Seek

func writeLinuxSupervisorConfig(_ string, config supervisorConfig) (*os.File, error) {
	fd, err := linuxSupervisorMemfdCreate("acp-go-opencode-supervisor-config", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
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

	if _, err := linuxSupervisorSeek(file, 0, io.SeekStart); err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("rewind private supervisor config descriptor: %w", err)
	}

	return file, nil
}

// supervisorTrustedEffectiveUID is the seam the trusted-root assertion reads.
// It is deliberately not the seam the shared arm is selected through: a case
// that pins the isolated refusals has to be able to place this process at the
// trusted identity without also moving the identity the arm compares the native
// uid against, or the isolated arm becomes unreachable off root.
var supervisorTrustedEffectiveUID = os.Geteuid

func verifyLinuxTrustedSupervisorIdentity(uid uint32) error {
	// The supervisor drops privilege to reach the native identity, so it has to
	// hold a higher one first. When the native identity is the one it already
	// runs as there is no descent to make, and demanding root would refuse the
	// only launch such a deployment can perform.
	if sharedNativeIdentity(uid) {
		return nil
	}

	if supervisorTrustedEffectiveUID() != 0 || uid == 0 || effectiveUID() == uid {
		return errors.New("OpenCode liveness supervisor requires a distinct trusted root identity")
	}

	return nil
}

func acquireLinuxAgentIdentityAuthority(
	uid uint32,
	gid uint32,
	ownerID string,
	stateRoot string,
	control io.Reader,
) (supervisorIdentityLock, supervisorIdentityLock, error) {
	canceled, stop := linuxSupervisorControlCancellation(control)
	defer stop()

	standalone, err := acquireAgentStandaloneIdentity(uid, gid, ownerID, stateRoot, false, "", canceled, nil)
	if err != nil {
		return nil, nil, err
	}

	return standalone.identity, standalone.authority, nil
}

func adoptLinuxAgentIdentityLock(uid uint32) (supervisorIdentityLock, error) {
	file := supervisorInheritedFile(4, "opencode-agent-identity-lock")

	return adoptAgentIdentityLock(file, uid, false, "")
}

func adoptLinuxAgentAuthorityDomain(uint32) (supervisorIdentityLock, error) {
	file := supervisorInheritedFile(5, "opencode-agent-authority-domain")

	return adoptAgentAuthorityDomain(file, false, "")
}

func linuxSupervisorControlCancellation(control io.Reader) (<-chan struct{}, func()) {
	file, ok := control.(*os.File)
	if !ok {
		return nil, func() {}
	}

	canceled := make(chan struct{})
	stop := make(chan struct{})

	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				poll := []unix.PollFd{{Fd: pollFD(file), Events: unix.POLLHUP | unix.POLLERR}}

				count, pollErr := unix.Poll(poll, 0)
				if pollErr == nil && count > 0 && poll[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
					close(canceled)

					return
				}
			}
		}
	}()

	return canceled, func() { close(stop) }
}

func validateLinuxSupervisorGuardianPeer(peer *os.File, done <-chan struct{}) error {
	if peer == nil {
		return nil
	}

	select {
	case <-done:
		return errors.New("OpenCode guardian exited before native launch")
	default:
	}

	poll := []unix.PollFd{{
		Fd:     pollFD(peer),
		Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
	}}

	ready, err := supervisorLinuxPoll(poll, 0)
	if err != nil {
		return fmt.Errorf("poll OpenCode guardian before native launch: %w", err)
	}

	if ready != 0 || poll[0].Revents != 0 {
		return errors.New("OpenCode guardian exited before native launch")
	}

	return nil
}

func linuxSupervisorMarkerRoot(config supervisorConfig) (string, error) {
	// The proof namespace under /run is root-owned and root-created, and it
	// exists to keep the markers out of reach of the identity the native process
	// runs as. A shared identity is that identity, so the namespace would prove
	// nothing it does not already hold; the markers stay in the adapter-owned
	// scratch root the launch created for itself.
	if config.SharedIdentity {
		return config.Scratch, nil
	}

	fd, err := openLinuxSupervisorProofDirectory()
	if err != nil {
		return "", err
	}

	if err := supervisorLinuxClose(fd); err != nil {
		return "", fmt.Errorf("close supervisor proof namespace: %w", err)
	}

	return linuxSupervisorProofNamespace, nil
}

func openLinuxSupervisorProofDirectory() (int, error) {
	if effectiveUIDSource() != 0 {
		return -1, errors.New("supervisor proof namespace requires a trusted root supervisor")
	}

	runFD, err := openLinuxSupervisorProofComponent(unix.AT_FDCWD, "/run", 0o755, false)
	if err != nil {
		return -1, err
	}
	defer unix.Close(runFD)

	acpGoFD, err := openLinuxSupervisorProofComponent(runFD, "acp-go", 0o700, true)
	if err != nil {
		return -1, err
	}
	defer unix.Close(acpGoFD)

	return openLinuxSupervisorProofComponent(acpGoFD, "supervisor-proofs", 0o700, true)
}

// Seam for the fail-closed guard over a proof component's own stat. A directory
// descriptor openat has just returned always stats, so the guard cannot be
// reached through the real syscall; tests swap this to prove that a component
// the kernel stops answering for aborts the whole namespace open and leaks no
// descriptor.
var linuxSupervisorProofFstat = unix.Fstat

func openLinuxSupervisorProofComponent(parentFD int, name string, mode uint32, create bool) (int, error) {
	if create {
		err := unix.Mkdirat(parentFD, name, mode)
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return -1, fmt.Errorf("create supervisor proof directory %q: %w", name, err)
		}
	}

	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open supervisor proof directory %q: %w", name, err)
	}

	var stat unix.Stat_t
	if err := linuxSupervisorProofFstat(fd, &stat); err != nil {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("stat supervisor proof directory %q: %w", name, err)
	}

	if stat.Uid != 0 || stat.Gid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 || create && stat.Mode&0o777 != 0o700 {
		_ = unix.Close(fd)

		return -1, fmt.Errorf("supervisor proof directory %q is not root-owned and protected", name)
	}

	return fd, nil
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

// Seam for the fail-closed guard in pollFD. Linux hands out small descriptors,
// so the guard is unreachable through a real *os.File; tests swap this to reach it.
var pollFDSource = (*os.File).Fd

// pollFD narrows a descriptor to the int32 unix.PollFd carries. Linux hands out
// small non-negative descriptors, so the guard never fires; when the value
// cannot be represented it yields -1, which poll reports as EBADF rather than
// aliasing onto a live descriptor.
func pollFD(file *os.File) int32 {
	fd := pollFDSource(file)
	if fd > math.MaxInt32 {
		return -1
	}

	return int32(fd)
}
