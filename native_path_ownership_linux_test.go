//go:build linux

package opencodeacp

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// requireNativeOwnershipRoot skips a case that cannot run unprivileged. Every
// property below turns on an inode belonging to an identity that is not the
// caller, which only a privileged process can arrange.
func requireNativeOwnershipRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
}

// nativeOwnershipTestIsolation is the dropped identity the ownership surface
// hands trees to: unprivileged, and never the identity running the test.
func nativeOwnershipTestIsolation() *ProcessIsolation {
	return &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}}
}

// nativeOwnershipTargetOwnedHome builds a durable native home the ownership
// predicate admits: mode 0700 and owned outright by the dropped identity. It
// sits directly under the temp root, which — unlike /tmp — is not writable by
// anyone but its owner, because the durable validator tolerates no writable
// ancestor at all, sticky or otherwise.
func nativeOwnershipTargetOwnedHome(t *testing.T) string {
	t.Helper()
	requireNativeOwnershipRoot(t)

	home := testNativeOwnedHome(t)
	isolation := nativeOwnershipTestIsolation()
	require.NoError(t, os.Chown(home, int(isolation.UID), int(isolation.GID)))

	return home
}

// nativeOwnershipOwner reports the owning identity of a path as the kernel sees
// it, which is how these cases prove an inode did or did not change hands.
func nativeOwnershipOwner(t *testing.T, path string) (uint32, uint32) {
	t.Helper()

	var stat unix.Stat_t
	require.NoError(t, unix.Stat(path, &stat))

	return stat.Uid, stat.Gid
}

// TestNativeOwnershipTraversalRejectsRelativeRoot proves the traversal refuses a
// relative root outright. A relative walk would resolve against the working
// directory the agent controls rather than against the tree the caller named, so
// the refusal has to land before the first descriptor is opened.
func TestNativeOwnershipTraversalRejectsRelativeRoot(t *testing.T) {
	require.ErrorContains(
		t,
		validateNativeOwnedDirectory("relative/home", nativeOwnershipTestIsolation()),
		"native path must be absolute",
	)
}

// TestNativeOwnershipTraversalRefusesTheFilesystemRootAsATarget proves the
// filesystem root is validated before any component is opened, and that it
// reaches the validator as the final component rather than as an ancestor. The
// refusal therefore lands on the leaf contract rather than the ancestry one —
// "/" does not belong to the dropped identity — and "/" keeps its owner.
func TestNativeOwnershipTraversalRefusesTheFilesystemRootAsATarget(t *testing.T) {
	requireNativeOwnershipRoot(t)

	require.ErrorContains(
		t, validateNativeOwnedDirectory("/", nativeOwnershipTestIsolation()), nativeOwnedHomeRefusal,
	)

	uid, gid := nativeOwnershipOwner(t, "/")
	require.Equal(t, uint32(0), uid, "the filesystem root was handed to the dropped identity")
	require.Equal(t, uint32(0), gid)
}

// TestNativeOwnershipTraversalOpensFilesystemRootItself proves "/" is still a
// walkable target: the empty component the split produces is skipped instead of
// being handed to openat, and the descriptor that comes back is the filesystem
// root itself. No production caller names "/", so the walk is driven directly.
func TestNativeOwnershipTraversalOpensFilesystemRootItself(t *testing.T) {
	var seen []bool

	directory, err := openNativeOwnershipDirectory("/", func(_ unix.Stat_t, final bool) error {
		seen = append(seen, final)

		return nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })
	require.Equal(t, []bool{true}, seen)

	var opened, root unix.Stat_t
	require.NoError(t, unix.Fstat(int(directory.Fd()), &opened))
	require.NoError(t, unix.Stat("/", &root))
	require.Equal(t, root.Ino, opened.Ino)
	require.Equal(t, root.Dev, opened.Dev)
}

// TestNativeOwnershipTraversalPropagatesMissingComponent proves a component that
// does not exist surfaces the kernel's own ENOENT rather than being treated as a
// directory that simply has nothing to check. The walk is anchored on a trusted
// ancestry through the filesystem-root seam, so the missing component — and not
// the ancestry above the fixture — is what the walk stops on.
func TestNativeOwnershipTraversalPropagatesMissingComponent(t *testing.T) {
	requireNativeOwnershipRoot(t)

	base, err := os.MkdirTemp("", "acp-go-opencode-absent-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	require.NoError(t, os.Chown(base, 0, 0))
	require.NoError(t, os.Chmod(base, 0o711))

	previous := nativeOwnershipOpenFilesystemRoot
	nativeOwnershipOpenFilesystemRoot = func() (int, error) {
		return unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}

	t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

	require.ErrorIs(
		t, validateNativeOwnedDirectory("/absent", nativeOwnershipTestIsolation()), unix.ENOENT,
	)
}

// TestNativeOwnershipTraversalFailsClosedOnKernelFaults proves every descriptor
// syscall the walk depends on aborts it. These guards cannot be driven through
// a real filesystem — the kernel answers for a descriptor the walk has just
// opened — so each is reached through its seam. A walk that swallowed any of
// them would return a descriptor whose ancestry it never proved, and both
// callers chown or admit everything under that descriptor.
func TestNativeOwnershipTraversalFailsClosedOnKernelFaults(t *testing.T) {
	accept := func(unix.Stat_t, bool) error { return nil }

	t.Run("filesystem root unopenable", func(t *testing.T) {
		previous := nativeOwnershipOpenFilesystemRoot
		nativeOwnershipOpenFilesystemRoot = func() (int, error) { return -1, unix.EMFILE }

		t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

		directory, err := openNativeOwnershipDirectory("/etc", accept)
		require.ErrorIs(t, err, unix.EMFILE)
		require.Nil(t, directory)
	})

	t.Run("filesystem root unstattable", func(t *testing.T) {
		previous := nativeOwnershipFstat
		nativeOwnershipFstat = func(int, *unix.Stat_t) error { return unix.EIO }

		t.Cleanup(func() { nativeOwnershipFstat = previous })

		directory, err := openNativeOwnershipDirectory("/etc", accept)
		require.ErrorIs(t, err, unix.EIO)
		require.Nil(t, directory)
	})

	t.Run("component unstattable", func(t *testing.T) {
		previous := nativeOwnershipFstat
		calls := 0
		nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
			calls++
			if calls == 1 {
				return previous(fd, stat)
			}

			return unix.EIO
		}

		t.Cleanup(func() { nativeOwnershipFstat = previous })

		directory, err := openNativeOwnershipDirectory("/etc", accept)
		require.ErrorIs(t, err, unix.EIO)
		require.Nil(t, directory)
		require.Equal(t, 2, calls, "the walk statted past the faulted component")
	})

	t.Run("parent descriptor unreleasable", func(t *testing.T) {
		previous := nativeOwnershipClose
		nativeOwnershipClose = func(fd int) error {
			_ = previous(fd)

			return unix.EIO
		}

		t.Cleanup(func() { nativeOwnershipClose = previous })

		directory, err := openNativeOwnershipDirectory("/etc", accept)
		require.ErrorIs(t, err, unix.EIO)
		require.Nil(t, directory)
	})
}

// TestDurableNativeAncestorStatesEachRefusal pins the exact reason the
// native-owned ancestry validator refuses each unsafe shape. Its contract is
// deliberately stricter than the generated-tree one, because the durable home
// outlives the process it is handed to: every ancestor must be a directory the
// wrapper itself owns and nobody else may write to — sticky protection does not
// buy an exception here as it does for a generated tree — the leaf must belong
// outright to the native identity at exactly 0700, and every level must stay
// traversable by that identity.
func TestDurableNativeAncestorStatesEachRefusal(t *testing.T) {
	const (
		trustedUID = uint32(0)
		trustedGID = uint32(0)
		targetUID  = uint32(65534)
		targetGID  = uint32(65534)
	)

	directory := func(mode uint32, uid uint32, gid uint32) unix.Stat_t {
		return unix.Stat_t{Mode: unix.S_IFDIR | mode, Uid: uid, Gid: gid}
	}

	for _, testCase := range []struct {
		name  string
		stat  unix.Stat_t
		final bool
		want  string
	}{
		{
			name: "not a directory",
			stat: unix.Stat_t{Mode: unix.S_IFREG | 0o700},
			want: "native-owned path ancestry is not a directory",
		},
		{
			name: "ancestor owned by a third identity",
			stat: directory(0o755, 4242, 4242),
			want: "native-owned path ancestor is uid=4242 gid=4242",
		},
		{
			name: "ancestor owned by the native identity rather than the wrapper",
			stat: directory(0o755, targetUID, targetGID),
			want: "native-owned path ancestor is uid=65534 gid=65534",
		},
		{
			name: "group-writable ancestor",
			stat: directory(0o771, trustedUID, trustedGID),
			want: "native-owned path ancestor mode 0771 is writable",
		},
		{
			name: "world-writable ancestor even with sticky protection",
			stat: directory(0o1777, trustedUID, trustedGID),
			want: "native-owned path ancestor mode 01777 is writable",
		},
		{
			name:  "leaf owned by the wrapper rather than the native identity",
			stat:  directory(0o700, trustedUID, trustedGID),
			final: true,
			want:  "native-owned directory is not owned by the target identity",
		},
		{
			name:  "leaf that is not exactly 0700",
			stat:  directory(0o750, targetUID, targetGID),
			final: true,
			want:  "not safely owned by the target identity",
		},
		{
			name: "ancestor the native identity cannot traverse",
			stat: directory(0o700, trustedUID, trustedGID),
			want: "native-owned path ancestry is not traversable by the target identity",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateDurableNativeAncestor(
				testCase.stat, testCase.final, trustedUID, trustedGID, targetUID, targetGID,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	require.NoError(t, validateDurableNativeAncestor(
		directory(0o711, trustedUID, trustedGID), false, trustedUID, trustedGID, targetUID, targetGID,
	))
	require.NoError(t, validateDurableNativeAncestor(
		directory(0o700, targetUID, targetGID), true, trustedUID, trustedGID, targetUID, targetGID,
	))
}

// TestDurableNativeAncestorUnderASharedIdentityAcceptsOnlyRootAncestors proves
// the same bound on the native-owned ancestry rule, which stays the stricter of
// the two: the ownership relaxation buys a root-owned ancestor nothing on the
// mode contract, so the sticky world-writable directories the generated walk
// admits are still refused here. The leaf contract is untouched — it belongs to
// the native identity outright at exactly 0700 — and only its ancestry admits
// the root-owned components a home directory hangs from.
func TestDurableNativeAncestorUnderASharedIdentityAcceptsOnlyRootAncestors(t *testing.T) {
	const (
		sharedUID = uint32(1000)
		sharedGID = uint32(1000)
	)

	directory := func(mode uint32, uid uint32, gid uint32) unix.Stat_t {
		return unix.Stat_t{Mode: unix.S_IFDIR | mode, Uid: uid, Gid: gid}
	}

	for _, testCase := range []struct {
		name  string
		stat  unix.Stat_t
		final bool
		want  string
	}{
		{
			name: "not a directory",
			stat: unix.Stat_t{Mode: unix.S_IFREG | 0o700},
			want: "native-owned path ancestry is not a directory",
		},
		{
			name: "ancestor owned by a third identity",
			stat: directory(0o755, 4242, 4242),
			want: "native-owned path ancestor is uid=4242 gid=4242; " +
				"run the supervisor as root to isolate the agent identity, " +
				"or place the native directory under a path the agent identity owns",
		},
		{
			name: "ancestor owned by root with a foreign group",
			stat: directory(0o755, 0, 4242),
			want: "native-owned path ancestor is uid=0 gid=4242",
		},
		{
			name: "group-writable root-owned ancestor",
			stat: directory(0o775, 0, 0),
			want: "native-owned path ancestor mode 0775 is writable",
		},
		{
			name: "world-writable root-owned ancestor even with sticky protection",
			stat: directory(0o1777, 0, 0),
			want: "native-owned path ancestor mode 01777 is writable",
		},
		{
			name: "root-owned ancestor the shared identity cannot traverse",
			stat: directory(0o700, 0, 0),
			want: "native-owned path ancestry is not traversable by the target identity",
		},
		{
			name:  "leaf still owned by root",
			stat:  directory(0o700, 0, 0),
			final: true,
			want:  nativeOwnedHomeRefusal,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateDurableNativeAncestor(
				testCase.stat, testCase.final, sharedUID, sharedGID, sharedUID, sharedGID,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	require.NoError(t, validateDurableNativeAncestor(
		directory(0o755, 0, 0), false, sharedUID, sharedGID, sharedUID, sharedGID,
	), "the root-owned ancestry every home directory is reached through was refused")
	require.NoError(t, validateDurableNativeAncestor(
		directory(0o711, 0, 0), false, sharedUID, sharedGID, sharedUID, sharedGID,
	))
	require.NoError(t, validateDurableNativeAncestor(
		directory(0o711, sharedUID, sharedGID), false, sharedUID, sharedGID, sharedUID, sharedGID,
	))
	require.NoError(t, validateDurableNativeAncestor(
		directory(0o700, sharedUID, sharedGID), true, sharedUID, sharedGID, sharedUID, sharedGID,
	))

	require.EqualError(t, validateDurableNativeAncestor(
		directory(0o755, 0, 0), false, sharedUID, sharedGID, 65534, 65534,
	), "native-owned path ancestor is uid=0 gid=0")
}

// TestNativeOwnershipWalksARootOwnedAncestryUnderASharedIdentity proves the
// traversal accepts the shape a wrapper that never dropped privilege presents:
// its own identity is the isolated identity, and the home it validates hangs
// from a root-owned directory it will never own. The effective identity is
// staged through its seams so the proof does not depend on which identity runs
// the tests, and the filesystem root is substituted through its seam so the
// ancestry above the fixture — /tmp is world-writable, which the native-owned
// contract refuses outright — cannot decide the outcome. The chowns that give
// the home its owner still need the root the rest of this file requires.
func TestNativeOwnershipWalksARootOwnedAncestryUnderASharedIdentity(t *testing.T) {
	requireNativeOwnershipRoot(t)

	const (
		sharedUID = uint32(65534)
		sharedGID = uint32(65534)
	)

	base, err := os.MkdirTemp("", "acp-go-opencode-shared-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	require.NoError(t, os.Chmod(base, 0o711))

	native := filepath.Join(base, "native")
	require.NoError(t, os.Mkdir(native, 0o700))

	seed := filepath.Join(native, "input")
	require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))
	require.NoError(t, os.Chown(seed, int(sharedUID), int(sharedGID)))
	require.NoError(t, os.Chown(native, int(sharedUID), int(sharedGID)))

	previousRoot := nativeOwnershipOpenFilesystemRoot
	nativeOwnershipOpenFilesystemRoot = func() (int, error) {
		return unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}

	t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previousRoot })

	previousUID, previousGID := effectiveUIDSource, effectiveGIDSource
	effectiveUIDSource = func() int { return int(sharedUID) }
	effectiveGIDSource = func() int { return int(sharedGID) }

	t.Cleanup(func() { effectiveUIDSource, effectiveGIDSource = previousUID, previousGID })

	isolation := &ProcessIsolation{UID: sharedUID, GID: sharedGID, BaseEnvironment: map[string]string{}}
	require.NoError(t, validateNativeOwnedDirectory("/native", isolation))

	uid, gid := nativeOwnershipOwner(t, seed)
	require.Equal(t, sharedUID, uid, "the home the shared identity already owned changed hands")
	require.Equal(t, sharedGID, gid)

	require.NoError(t, os.Chown(base, 4242, 4242))

	validationErr := validateNativeOwnedDirectory("/native", isolation)
	require.ErrorContains(t, validationErr, "native-owned path ancestor is uid=4242 gid=4242")
	require.ErrorContains(t, validationErr, "place the native directory under a path the agent identity owns")
}

// TestNativeIdentityTraversalUsesTheApplicableModeClass proves traversability is
// decided by the single mode class the kernel would apply — owner, then group,
// then other — and never by a union of them. Reading the wrong class would admit
// a path the dropped identity cannot enter, or refuse one it can.
func TestNativeIdentityTraversalUsesTheApplicableModeClass(t *testing.T) {
	const (
		uid = uint32(65534)
		gid = uint32(65535)
	)

	for _, testCase := range []struct {
		name string
		stat unix.Stat_t
		want bool
	}{
		{name: "owner execute", stat: unix.Stat_t{Uid: uid, Gid: 0, Mode: 0o100}, want: true},
		{name: "owner without execute ignores group", stat: unix.Stat_t{Uid: uid, Gid: gid, Mode: 0o011}},
		{name: "group execute", stat: unix.Stat_t{Uid: 0, Gid: gid, Mode: 0o010}, want: true},
		{name: "group without execute ignores other", stat: unix.Stat_t{Uid: 0, Gid: gid, Mode: 0o101}},
		{name: "other execute", stat: unix.Stat_t{Uid: 0, Gid: 0, Mode: 0o001}, want: true},
		{name: "other without execute", stat: unix.Stat_t{Uid: 0, Gid: 0, Mode: 0o110}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			require.Equal(t, testCase.want, nativeIdentityCanTraverse(testCase.stat, uid, gid))
		})
	}
}

// TestNativeOwnedDirectoryRecheckDisagreeingWithTheWalkIsRefused proves the
// final inspection of the opened descriptor is load-bearing rather than a
// restatement of the walk. The walk validates the home one component at a time
// and the leaf can be replaced between the last openat and the moment the
// descriptor is used, so the check re-reads the descriptor it actually holds and
// refuses on any disagreement instead of admitting a home it never proved. The
// disagreement is staged on the fstat seam because a real filesystem cannot make
// one descriptor answer twice with two different inodes. The home on disk is
// asserted unchanged after each refusal, so the refusal is provably about the
// re-read rather than about damage the walk did.
func TestNativeOwnedDirectoryRecheckDisagreeingWithTheWalkIsRefused(t *testing.T) {
	home := nativeOwnershipTargetOwnedHome(t)
	isolation := nativeOwnershipTestIsolation()
	require.NoError(t, validateNativeOwnedDirectory(home, isolation))

	baseline := nativeOwnershipFstat
	total := 0
	nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
		total++

		return baseline(fd, stat)
	}

	require.NoError(t, validateNativeOwnedDirectory(home, isolation))
	nativeOwnershipFstat = baseline
	require.Greater(t, total, 1, "the walk made no ancestor stat before the final inspection")

	faultFinalStat := func(t *testing.T, replace func(*unix.Stat_t) error) {
		t.Helper()

		calls := 0
		nativeOwnershipFstat = func(fd int, stat *unix.Stat_t) error {
			calls++
			if statErr := baseline(fd, stat); statErr != nil {
				return statErr
			}
			if calls == total {
				return replace(stat)
			}

			return nil
		}

		t.Cleanup(func() { nativeOwnershipFstat = baseline })
	}

	for _, testCase := range []struct {
		name    string
		replace func(*unix.Stat_t) error
		want    string
	}{
		{
			name:    "descriptor stops answering",
			replace: func(*unix.Stat_t) error { return unix.EIO },
			want:    "inspect native-owned directory",
		},
		{
			name: "descriptor is no longer a directory",
			replace: func(stat *unix.Stat_t) error {
				stat.Mode = unix.S_IFREG | 0o700

				return nil
			},
			want: "native-owned path is not a directory",
		},
		{
			name: "descriptor is owned by another identity",
			replace: func(stat *unix.Stat_t) error {
				stat.Uid = 4242

				return nil
			},
			want: "native-owned directory is uid=4242",
		},
		{
			name: "descriptor became writable by others",
			replace: func(stat *unix.Stat_t) error {
				stat.Mode = unix.S_IFDIR | 0o702

				return nil
			},
			want: "native-owned directory mode 0702 is unsafe",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			faultFinalStat(t, testCase.replace)

			require.ErrorContains(t, validateNativeOwnedDirectory(home, isolation), testCase.want)

			nativeOwnershipFstat = baseline

			uid, gid := nativeOwnershipOwner(t, home)
			require.Equal(t, isolation.UID, uid, "the refused home changed hands")
			require.Equal(t, isolation.GID, gid)
			require.NoError(t, validateNativeOwnedDirectory(home, isolation))
		})
	}
}

// TestNativeOwnershipTrustsNothingOnAnUnrepresentableEffectiveIdentity proves
// the width guards in effectiveUID and effectiveGID fail closed. Every ownership
// decision compares the caller's effective identity against an inode owner, so
// an identity the adapter cannot represent has to become one no inode can carry
// rather than truncating into an accidental match. Linux cannot report such an
// identity, so the guard is reached through the source seam that exists for it;
// what is asserted is the outcome — the walk trusts nothing and the tree keeps
// its owner.
func TestNativeOwnershipTrustsNothingOnAnUnrepresentableEffectiveIdentity(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		fault   func(*testing.T)
		guarded func() uint32
	}{
		{
			name: "uid",
			fault: func(t *testing.T) {
				t.Helper()

				previous := effectiveUIDSource
				effectiveUIDSource = func() int { return -1 }

				t.Cleanup(func() { effectiveUIDSource = previous })
			},
			guarded: effectiveUID,
		},
		{
			name: "gid",
			fault: func(t *testing.T) {
				t.Helper()

				previous := effectiveGIDSource
				effectiveGIDSource = func() int { return -1 }

				t.Cleanup(func() { effectiveGIDSource = previous })
			},
			guarded: effectiveGID,
		},
		{
			name: "uid above the 32-bit range",
			fault: func(t *testing.T) {
				t.Helper()

				previous := effectiveUIDSource
				effectiveUIDSource = func() int { return math.MaxUint32 + 1 }

				t.Cleanup(func() { effectiveUIDSource = previous })
			},
			guarded: effectiveUID,
		},
		{
			name: "gid above the 32-bit range",
			fault: func(t *testing.T) {
				t.Helper()

				previous := effectiveGIDSource
				effectiveGIDSource = func() int { return math.MaxUint32 + 1 }

				t.Cleanup(func() { effectiveGIDSource = previous })
			},
			guarded: effectiveGID,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home := nativeOwnershipTargetOwnedHome(t)
			isolation := nativeOwnershipTestIsolation()

			testCase.fault(t)
			require.Equal(t, uint32(math.MaxUint32), testCase.guarded())

			require.ErrorContains(
				t,
				validateNativeOwnedDirectory(home, isolation),
				"native-owned path ancestor is uid=0 gid=0",
			)

			uid, gid := nativeOwnershipOwner(t, home)
			require.Equal(t, isolation.UID, uid, "the home changed hands on an unrepresentable identity")
			require.Equal(t, isolation.GID, gid)
		})
	}
}

// nativeOwnedHomeRefusal is why Linux refuses a durable native home owned by
// someone other than the isolated identity: the real ownership walk reaches the
// home and finds the wrong owner.
const nativeOwnedHomeRefusal = "native-owned directory is not owned by the target identity"
