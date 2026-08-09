//go:build unix

package opencode

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

var inheritedDescriptorFcntl = unix.FcntlInt

// Seams for the identity this process is actually running as. The shared arm is
// selected through them and nothing else, so a case can place the launch on
// either arm without the account the suite happens to run under deciding for
// it. They are deliberately not the seams the trusted-root assertion reads.
var (
	processEffectiveUID = os.Geteuid
	processEffectiveGID = os.Getegid
)

func validateProcessIsolationPlatform() error { return nil }

// sharedNativeIdentity reports whether the native identity is the identity the
// supervisor already runs as. Nothing separates the two ends of the launch in
// that shape, so every step that exists to cross the boundary has nothing to
// cross. A zero effective uid never qualifies: the supervisor holds the trusted
// identity there, and a nonzero native uid is required everywhere, so the two
// can never name the same identity. Only the Linux backend recognises the
// shape; the Darwin backend states its own boundary and is left as it is.
func sharedNativeIdentity(uid uint32) bool {
	if processIsolationGOOS != processIsolationLinux {
		return false
	}

	effective := processEffectiveUID()

	return effective > 0 && uint64(uid) == uint64(effective)
}

func sharedProcessIdentity(isolation *ProcessIsolation) bool {
	return isolation != nil && sharedNativeIdentity(isolation.UID)
}

func applyProcessCredential(cmd *exec.Cmd, isolation *ProcessIsolation) error {
	if err := validateProcessIsolation(isolation); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	// Requesting no credential change at all is the only honest instruction
	// when the native identity is already the running one. The supplementary
	// groups belong to the account the supervisor was started under, and an
	// unprivileged process can neither shed them nor re-enter them.
	if sharedProcessIdentity(isolation) {
		effectiveGID := processEffectiveGID()
		if effectiveGID < 0 || uint64(isolation.GID) != uint64(effectiveGID) {
			return fmt.Errorf(
				"native group %d cannot be entered from group %d; %s",
				isolation.GID, effectiveGID, sharedIdentitySupervisorRemedy,
			)
		}

		return nil
	}

	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: isolation.UID, Gid: isolation.GID, Groups: []uint32{}, NoSetGroups: false}

	return nil
}

func closeInheritedOnExec(file *os.File) error {
	if file == nil {
		return fmt.Errorf("inherited config descriptor is unavailable")
	}

	flags, err := inheritedDescriptorFcntl(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("read inherited descriptor flags: %w", err)
	}

	if _, err = inheritedDescriptorFcntl(file.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("protect inherited descriptor from native exec: %w", err)
	}

	return nil
}
