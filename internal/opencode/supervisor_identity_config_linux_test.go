//go:build linux

package opencode

import (
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestLinuxSupervisorConfigWriteFailsClosedAndKeepsNoDescriptor proves every
// step of the private config write is fail-closed. The descriptor this function
// returns is inherited by the guardian as descriptor 3 and is the only thing
// that tells it which native process to supervise under which identity, so a
// descriptor that was not created, not restricted to its owner, not sealed
// against later edits, or not rewound for the child to read must never escape.
// Each case faults one step and requires both halves of that: the call hands
// back no file, and the descriptor it had opened is gone from this process, so
// there is nothing left for a child to inherit.
func TestLinuxSupervisorConfigWriteFailsClosedAndKeepsNoDescriptor(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		linuxSupervisorIdentitySeams(t)
		want := errors.New("no anonymous memory")
		linuxSupervisorMemfdCreate = func(string, int) (int, error) { return -1, want }

		file, err := writeLinuxSupervisorConfig("", supervisorConfig{NativeExecutable: testNativeExecutable(t, "/bin/true")})
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "create private supervisor config descriptor")
		require.Nil(t, file)
	})

	t.Run("restrict", func(t *testing.T) {
		linuxSupervisorIdentitySeams(t)
		// An O_PATH descriptor names a file without granting any operation on
		// its contents, so the kernel refuses fchmod on it with EBADF. It is a
		// descriptor the write cannot restrict to its owner, which is the one
		// condition the guard exists for.
		opened := -1
		linuxSupervisorMemfdCreate = func(string, int) (int, error) {
			fd, err := unix.Open(t.TempDir(), unix.O_PATH|unix.O_CLOEXEC, 0)
			opened = fd

			return fd, err
		}

		file, err := writeLinuxSupervisorConfig("", supervisorConfig{NativeExecutable: testNativeExecutable(t, "/bin/true")})
		require.ErrorIs(t, err, unix.EBADF)
		require.ErrorContains(t, err, "secure private supervisor config descriptor")
		require.Nil(t, file)
		require.NotEqual(t, -1, opened)
		require.True(t, supervisorIdentityDescriptorClosed(opened), "an unrestricted config descriptor must not survive the refusal")
	})

	t.Run("seal", func(t *testing.T) {
		linuxSupervisorIdentitySeams(t)
		preserveSupervisorGlobals(t)
		opened := -1
		linuxSupervisorMemfdCreate = func(name string, flags int) (int, error) {
			fd, err := unix.MemfdCreate(name, flags)
			opened = fd

			return fd, err
		}
		// Sealing the descriptor shut during the encode leaves the write with a
		// descriptor whose seal set can no longer be completed: the kernel
		// answers the production F_ADD_SEALS with EPERM.
		supervisorEncodeConfig = func(writer io.Writer, _ supervisorConfig) error {
			file, ok := writer.(*os.File)
			require.True(t, ok)
			_, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL)
			require.NoError(t, err)

			return nil
		}

		file, err := writeLinuxSupervisorConfig("", supervisorConfig{NativeExecutable: testNativeExecutable(t, "/bin/true")})
		require.ErrorIs(t, err, unix.EPERM)
		require.ErrorContains(t, err, "seal private supervisor config descriptor")
		require.Nil(t, file)
		require.NotEqual(t, -1, opened)
		require.True(t, supervisorIdentityDescriptorClosed(opened), "an unsealed config descriptor must not survive the refusal")
	})

	t.Run("rewind", func(t *testing.T) {
		linuxSupervisorIdentitySeams(t)
		opened := -1
		linuxSupervisorMemfdCreate = func(name string, flags int) (int, error) {
			fd, err := unix.MemfdCreate(name, flags)
			opened = fd

			return fd, err
		}
		want := errors.New("descriptor will not rewind")
		linuxSupervisorSeek = func(*os.File, int64, int) (int64, error) { return 0, want }

		file, err := writeLinuxSupervisorConfig("", supervisorConfig{NativeExecutable: testNativeExecutable(t, "/bin/true")})
		require.ErrorIs(t, err, want)
		require.ErrorContains(t, err, "rewind private supervisor config descriptor")
		require.Nil(t, file)
		require.NotEqual(t, -1, opened)
		require.True(t, supervisorIdentityDescriptorClosed(opened), "a config descriptor the child cannot read from the start must not survive the refusal")
	})
}

// TestPollFDRefusesToTruncateAnUnrepresentableDescriptor proves pollFD does not
// alias. unix.PollFd carries an int32, and a descriptor number that does not fit
// in one has a low half that names a different, live descriptor; polling that
// half would report a stranger's readiness as the guardian peer's or the control
// pipe's own hang-up. The case builds exactly that collision — an unrepresentable
// number whose low 32 bits are a pipe with data waiting — and requires pollFD to
// yield -1 instead, a value poll observes nothing through.
func TestPollFDRefusesToTruncateAnUnrepresentableDescriptor(t *testing.T) {
	linuxSupervisorIdentitySeams(t)

	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = writeEnd.Close()
		_ = readEnd.Close()
	})
	_, err = writeEnd.Write([]byte("x"))
	require.NoError(t, err)

	live := int32(readEnd.Fd())
	require.Positive(t, live)

	var high uintptr = 1
	unrepresentable := readEnd.Fd() + high<<32
	require.Greater(t, unrepresentable, uintptr(math.MaxInt32))
	require.Equal(t, live, int32(unrepresentable), "the fixture must collide with a live descriptor when truncated")

	pollFDSource = func(*os.File) uintptr { return unrepresentable }
	fenced := pollFD(readEnd)
	require.Equal(t, int32(-1), fenced)

	aliased := []unix.PollFd{{Fd: live, Events: unix.POLLIN}}
	ready, err := unix.Poll(aliased, 0)
	require.NoError(t, err)
	require.Equal(t, 1, ready, "the descriptor the truncation would have named is genuinely ready")
	require.NotZero(t, aliased[0].Revents&unix.POLLIN)

	refused := []unix.PollFd{{Fd: fenced, Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
	ready, err = unix.Poll(refused, 0)
	require.NoError(t, err)
	require.Zero(t, ready, "the fenced descriptor must report nothing at all")
	require.Zero(t, refused[0].Revents)
}
