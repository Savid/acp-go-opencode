//go:build linux

package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const linuxSupervisorProofRefusalPrefix = "supervisor proof directory "

// TestLinuxSupervisorProofNamespaceRequiresATrustedRootSupervisor proves the
// proof namespace is opened only by a supervisor the kernel reports as root, and
// that a marker root refused for that reason is refused outright. This is the
// property that separates Linux from every other platform: elsewhere the marker
// root falls back to whatever scratch directory the caller handed in, and an
// unprivileged process could then write its own start, completion and
// quarantine proofs. Here it must get a path at all.
func TestLinuxSupervisorProofNamespaceRequiresATrustedRootSupervisor(t *testing.T) {
	linuxSupervisorIdentitySeams(t)
	scratch := t.TempDir()
	effectiveUIDSource = func() int { return 65534 }

	fd, err := openLinuxSupervisorProofDirectory()
	require.EqualError(t, err, "supervisor proof namespace requires a trusted root supervisor")
	require.Equal(t, -1, fd)

	root, err := linuxSupervisorMarkerRoot(supervisorConfig{Scratch: scratch})
	require.EqualError(t, err, "supervisor proof namespace requires a trusted root supervisor")
	require.Empty(t, root, "a refused proof namespace must not fall back to the caller's scratch root")
}

// TestLinuxSupervisorMarkerRootFailsClosedWhenTheNamespaceCannotBeReleased
// proves the marker root is only published once the supervisor has finished
// with the namespace descriptor it proved. If releasing that descriptor fails
// the process cannot claim it left the namespace in a known state, so it must
// report the failure and name no marker root rather than hand back a path it
// still holds an unaccounted descriptor on.
func TestLinuxSupervisorMarkerRootFailsClosedWhenTheNamespaceCannotBeReleased(t *testing.T) {
	preservePlatformSupervisorGlobals(t)
	want := errors.New("descriptor will not close")
	released := 0
	supervisorLinuxClose = func(fd int) error {
		released++
		_ = unix.Close(fd)

		return want
	}

	root, err := linuxSupervisorMarkerRoot(supervisorConfig{Scratch: t.TempDir()})
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "close supervisor proof namespace")
	require.Empty(t, root)
	require.Equal(t, 1, released, "the namespace descriptor must be released exactly once")
}

// TestLinuxSupervisorProofNamespaceAbortsWhenAComponentStopsAnswering proves the
// namespace walk is fail-closed at every level. /run, /run/acp-go and
// /run/acp-go/supervisor-proofs are each opened and then re-proved through the
// descriptor itself, so that no rename or swap between resolving a name and
// trusting it can slip a different directory in. When the kernel stops
// answering for a component the walk has already opened, the walk must abort
// with no descriptor at all rather than continue into a directory it never
// proved, and it must not leave the descriptor it opened behind.
func TestLinuxSupervisorProofNamespaceAbortsWhenAComponentStopsAnswering(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		component string
		faultAt   int
	}{
		{name: "run", component: "/run", faultAt: 1},
		{name: "acp-go", component: "acp-go", faultAt: 2},
		{name: "supervisor-proofs", component: "supervisor-proofs", faultAt: 3},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			linuxSupervisorIdentitySeams(t)
			want := errors.New("component stopped answering")
			calls, faulted := 0, -1
			linuxSupervisorProofFstat = func(fd int, stat *unix.Stat_t) error {
				calls++
				if calls == testCase.faultAt {
					faulted = fd

					return want
				}

				return unix.Fstat(fd, stat)
			}

			fd, err := openLinuxSupervisorProofDirectory()
			require.ErrorIs(t, err, want)
			require.ErrorContains(t, err, linuxSupervisorProofRefusalPrefix+`"`+testCase.component+`"`)
			require.Equal(t, -1, fd)
			require.NotEqual(t, -1, faulted)
			require.True(t, supervisorIdentityDescriptorClosed(faulted), "an unproved component descriptor must not survive the refusal")
		})
	}
}

// TestLinuxSupervisorProofComponentRefusesUnusableParentsAndUnprotectedTargets
// proves the three refusals a single namespace component makes on its own: it
// will not create a component under a parent that is not a directory, it will
// not adopt a component that is not there, and it will not adopt one that is not
// root-owned and closed to group and other. The last is the security property
// the whole namespace rests on — a directory anyone else can write is a
// directory anyone else can plant a supervisor's completion proof in — and the
// descriptor opened to discover it must not survive the refusal.
func TestLinuxSupervisorProofComponentRefusesUnusableParentsAndUnprotectedTargets(t *testing.T) {
	t.Run("parent is not a directory", func(t *testing.T) {
		parent, err := os.CreateTemp(t.TempDir(), "parent")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, parent.Close()) })

		fd, err := openLinuxSupervisorProofComponent(int(parent.Fd()), "child", 0o700, true)
		require.ErrorIs(t, err, unix.ENOTDIR)
		require.ErrorContains(t, err, `create `+linuxSupervisorProofRefusalPrefix+`"child"`)
		require.Equal(t, -1, fd)
	})

	t.Run("component is absent", func(t *testing.T) {
		parent := supervisorIdentityProtectedDir(t)

		fd, err := openLinuxSupervisorProofComponent(int(parent.Fd()), "absent", 0o700, false)
		require.ErrorIs(t, err, unix.ENOENT)
		require.ErrorContains(t, err, `open `+linuxSupervisorProofRefusalPrefix+`"absent"`)
		require.Equal(t, -1, fd)
	})

	t.Run("component is writable by others", func(t *testing.T) {
		linuxSupervisorIdentitySeams(t)
		parent := supervisorIdentityProtectedDir(t)
		exposed := filepath.Join(parent.Name(), "exposed")
		require.NoError(t, os.Mkdir(exposed, 0o700))
		require.NoError(t, os.Chmod(exposed, 0o777))

		inspected := -1
		linuxSupervisorProofFstat = func(fd int, stat *unix.Stat_t) error {
			inspected = fd

			return unix.Fstat(fd, stat)
		}

		fd, err := openLinuxSupervisorProofComponent(int(parent.Fd()), "exposed", 0o700, false)
		require.EqualError(t, err, linuxSupervisorProofRefusalPrefix+`"exposed" is not root-owned and protected`)
		require.Equal(t, -1, fd)
		require.NotEqual(t, -1, inspected)
		require.True(t, supervisorIdentityDescriptorClosed(inspected), "an unprotected component descriptor must not survive the refusal")
	})
}
