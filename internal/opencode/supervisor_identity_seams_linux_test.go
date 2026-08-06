//go:build linux

package opencode

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// linuxSupervisorIdentitySeams restores the Linux supervisor identity seams that
// the cases in this package's supervisor_identity_*_test.go files swap. The
// seams stand for primitives that cannot fail for the descriptors the code has
// just validated, so faulting them is the only way to reach the guards that
// keep a half-built identity from being handed to a child.
func linuxSupervisorIdentitySeams(t *testing.T) {
	t.Helper()
	memfdCreate := linuxSupervisorMemfdCreate
	seek := linuxSupervisorSeek
	proofFstat := linuxSupervisorProofFstat
	effectiveUID := effectiveUIDSource
	pollSource := pollFDSource
	t.Cleanup(func() {
		linuxSupervisorMemfdCreate = memfdCreate
		linuxSupervisorSeek = seek
		linuxSupervisorProofFstat = proofFstat
		effectiveUIDSource = effectiveUID
		pollFDSource = pollSource
	})
}

// supervisorIdentityDescriptorClosed reports whether this process has stopped
// holding descriptor fd. It is the observable form of "the write closed what it
// opened": a descriptor the kernel no longer knows cannot be inherited, mapped
// or read by anything the supervisor goes on to start.
func supervisorIdentityDescriptorClosed(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)

	return errors.Is(err, unix.EBADF)
}

// supervisorIdentityProtectedDir materializes a root-owned 0700 directory that
// satisfies every check openLinuxSupervisorProofComponent makes of a parent, so
// a case can name exactly the one property it means to break.
func supervisorIdentityProtectedDir(t *testing.T) *os.File {
	t.Helper()
	path := t.TempDir()
	require.NoError(t, os.Chmod(path, 0o700))
	directory, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })

	return directory
}
