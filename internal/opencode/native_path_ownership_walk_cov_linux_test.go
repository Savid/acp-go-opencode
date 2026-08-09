//go:build linux

package opencode

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The refusals the ownership surface owes its callers, named once so a repeated
// literal never lands on top of the production string it is asserting.
const (
	nativeOwnershipUntrustedAncestry = "generated native path ancestry is not a trusted directory"
	nativeOwnershipUnsafeRootMode    = "generated native root mode %#o is unsafe"
	nativeOwnershipWritableAncestor  = "is writable without sticky protection"
	nativeOwnershipUnprovenHandoff   = "generated native inode ownership handoff could not be proven"
	nativeOwnershipEntryOpenRefusal  = "open generated native entry"
	nativeOwnershipInodeTypeRefusal  = "generated native inode type changed"
	nativeOwnershipSharedAncestor    = "generated native path ancestor is uid=%d gid=%d"
	nativeOwnershipSharedRemedy      = "run the supervisor as root to isolate the agent identity, " +
		"or place the native directory under a path the agent identity owns"
)

// nativeOwnershipTarget is the dropped identity every case hands a tree to. It
// is never the identity running the test, which is what makes an ownership
// change observable.
const (
	nativeOwnershipTargetUID = uint32(65534)
	nativeOwnershipTargetGID = uint32(65534)
)

// nativeOwnershipRequireRoot skips a case that cannot run unprivileged. Every
// property below turns on an inode belonging to an identity that is not the
// caller, which only a privileged process can arrange.
func nativeOwnershipRequireRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}
}

// nativeOwnershipIsolation is the isolation policy the handoff reads: only the
// target identity matters to this surface.
func nativeOwnershipIsolation() *ProcessIsolation {
	return &ProcessIsolation{
		UID:             nativeOwnershipTargetUID,
		GID:             nativeOwnershipTargetGID,
		BaseEnvironment: map[string]string{},
	}
}

// nativeOwnershipGeneratedRoot builds a trusted 0700 generated tree under a
// 0711 caller root, which is the shape the generated-tree handoff accepts, and
// returns both so a case can assert the handoff never climbed above the tree.
func nativeOwnershipGeneratedRoot(t *testing.T) (string, string) {
	t.Helper()
	nativeOwnershipRequireRoot(t)

	parent := testTraversableTempDir(t)
	native := filepath.Join(parent, "native")
	require.NoError(t, os.Mkdir(native, 0o700))

	return native, parent
}

// nativeOwnershipOwner reports the owning identity of a path as the kernel sees
// it, which is how these cases prove an inode did or did not change hands.
func nativeOwnershipOwner(t *testing.T, path string) (uint32, uint32) {
	t.Helper()

	var stat unix.Stat_t
	require.NoError(t, unix.Lstat(path, &stat))

	return stat.Uid, stat.Gid
}

// nativeOwnershipRequireTrusted asserts a path still belongs to the trusted
// identity, which is the effect assertion behind every refusal below: a refused
// tree must not have changed hands on the way out.
func nativeOwnershipRequireTrusted(t *testing.T, path string) {
	t.Helper()

	uid, gid := nativeOwnershipOwner(t, path)
	require.Equal(t, uint32(0), uid, path)
	require.Equal(t, uint32(0), gid, path)
}

// nativeOwnershipPathDescriptor returns an O_PATH descriptor. O_PATH
// descriptors answer fstat but reject every operation that reads or writes the
// inode, which is how these cases make an already-validated descriptor stop
// answering without racing the filesystem.
func nativeOwnershipPathDescriptor(t *testing.T, path string, directory bool) *os.File {
	t.Helper()

	flags := unix.O_PATH | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}

	fd, err := unix.Open(path, flags, 0)
	require.NoError(t, err)

	file := os.NewFile(uintptr(fd), path)
	t.Cleanup(func() { _ = file.Close() })

	return file
}

// TestNativeOwnershipHandoffRefusesRelativeRoot proves the handoff refuses a
// relative root before it opens a single descriptor. A relative walk would
// resolve against the working directory the agent controls rather than against
// the tree the caller named, so no ancestry proof it produced would be about the
// intended tree at all.
func TestNativeOwnershipHandoffRefusesRelativeRoot(t *testing.T) {
	require.ErrorContains(
		t,
		handoffGeneratedNativeTree("relative/native", nativeOwnershipIsolation()),
		"generated native path must be absolute",
	)
}

// TestNativeOwnershipWalkRefusesTheFilesystemRootAsATarget proves the
// filesystem root is validated before any component is opened and that it
// reaches the validator as the final component rather than as an ancestor: the
// handoff refuses "/" on the leaf contract — "/" is not exactly 0700 — instead
// of walking into it, and "/" keeps its owner.
func TestNativeOwnershipWalkRefusesTheFilesystemRootAsATarget(t *testing.T) {
	nativeOwnershipRequireRoot(t)

	var root unix.Stat_t
	require.NoError(t, unix.Stat("/", &root))

	require.ErrorContains(
		t,
		handoffGeneratedNativeTree("/", nativeOwnershipIsolation()),
		fmt.Sprintf(nativeOwnershipUnsafeRootMode, root.Mode&0o7777),
	)
	nativeOwnershipRequireTrusted(t, "/")
}

// TestNativeOwnershipWalkOpensTheFilesystemRootItself proves "/" is still a
// walkable target: the empty component the split of "/" produces is skipped
// instead of being handed to openat, and the descriptor that comes back is the
// root the walk started from rather than something beneath it. No production
// caller names "/", and the branch is only reachable when the root satisfies the
// leaf contract — exactly 0700 and trusted-owned — which is not a shape this
// suite can give a real filesystem root, so the root open is substituted through
// its seam. What is asserted is the identity of the descriptor the walk returns.
func TestNativeOwnershipWalkOpensTheFilesystemRootItself(t *testing.T) {
	native, _ := nativeOwnershipGeneratedRoot(t)

	substitute, err := unix.Open(native, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	require.NoError(t, err)

	previous := nativeOwnershipOpenFilesystemRoot
	nativeOwnershipOpenFilesystemRoot = func() (int, error) { return substitute, nil }

	t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })

	directory, err := openGeneratedNativeDirectory(
		"/", 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })

	var opened, expected unix.Stat_t
	require.NoError(t, unix.Fstat(int(directory.Fd()), &opened))
	require.NoError(t, unix.Stat(native, &expected))
	require.Equal(t, expected.Ino, opened.Ino, "the walk descended past the root it was handed")
	require.Equal(t, expected.Dev, opened.Dev)
}

// TestNativeOwnershipWalkPropagatesMissingComponent proves a component that
// does not exist surfaces the kernel's own ENOENT rather than being treated as
// an empty tree that needs no handoff, which would report success for a tree the
// isolated identity never received.
func TestNativeOwnershipWalkPropagatesMissingComponent(t *testing.T) {
	native, parent := nativeOwnershipGeneratedRoot(t)

	require.ErrorIs(
		t,
		handoffGeneratedNativeTree(filepath.Join(parent, "absent"), nativeOwnershipIsolation()),
		unix.ENOENT,
	)
	nativeOwnershipRequireTrusted(t, native)
}

// TestNativeOwnershipWalkRefusesWritableAncestorWithoutSticky proves an
// ancestor anyone may write to is refused unless the sticky bit stops a third
// party from replacing the entries beneath it. Without that guard the walk could
// be redirected between the moment it validates a component and the moment it
// opens the next one, and the tree would be chowned to the isolated identity
// anyway.
func TestNativeOwnershipWalkRefusesWritableAncestorWithoutSticky(t *testing.T) {
	native, parent := nativeOwnershipGeneratedRoot(t)
	require.NoError(t, os.Chmod(parent, 0o777))

	require.ErrorContains(
		t,
		handoffGeneratedNativeTree(native, nativeOwnershipIsolation()),
		nativeOwnershipWritableAncestor,
	)
	nativeOwnershipRequireTrusted(t, native)

	require.NoError(t, os.Chmod(parent, 0o777|os.ModeSticky))
	require.NoError(t, handoffGeneratedNativeTree(native, nativeOwnershipIsolation()))

	uid, gid := nativeOwnershipOwner(t, native)
	require.Equal(t, nativeOwnershipTargetUID, uid, "sticky protection did not readmit the ancestor")
	require.Equal(t, nativeOwnershipTargetGID, gid)
}

// TestNativeOwnershipWalkFailsClosedOnKernelFaults proves every descriptor
// syscall the walk depends on aborts the handoff. None can be driven through a
// real filesystem — the kernel answers for a descriptor the walk has just opened
// — so each is reached through its seam. A walk that swallowed any of them would
// hand back a descriptor whose ancestry it never proved, and the caller chowns
// everything beneath that descriptor; each case therefore asserts the tree still
// belongs to the trusted identity.
func TestNativeOwnershipWalkFailsClosedOnKernelFaults(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		fault func(*testing.T)
		want  error
	}{
		{
			name: "filesystem root unopenable",
			fault: func(t *testing.T) {
				t.Helper()

				previous := nativeOwnershipOpenFilesystemRoot
				nativeOwnershipOpenFilesystemRoot = func() (int, error) { return -1, unix.EMFILE }

				t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previous })
			},
			want: unix.EMFILE,
		},
		{
			name: "filesystem root unstattable",
			fault: func(t *testing.T) {
				t.Helper()

				previous := nativeOwnershipFstat
				nativeOwnershipFstat = func(int, *unix.Stat_t) error { return unix.EIO }

				t.Cleanup(func() { nativeOwnershipFstat = previous })
			},
			want: unix.EIO,
		},
		{
			name: "component unstattable",
			fault: func(t *testing.T) {
				t.Helper()

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
			},
			want: unix.EIO,
		},
		{
			name: "parent descriptor unreleasable",
			fault: func(t *testing.T) {
				t.Helper()

				previous := nativeOwnershipClose
				nativeOwnershipClose = func(fd int) error {
					_ = previous(fd)

					return unix.EIO
				}

				t.Cleanup(func() { nativeOwnershipClose = previous })
			},
			want: unix.EIO,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			native, _ := nativeOwnershipGeneratedRoot(t)
			seed := filepath.Join(native, "input")
			require.NoError(t, os.WriteFile(seed, []byte("seeded"), 0o600))

			testCase.fault(t)

			require.ErrorIs(t, handoffGeneratedNativeTree(native, nativeOwnershipIsolation()), testCase.want)

			nativeOwnershipRequireTrusted(t, native)
			nativeOwnershipRequireTrusted(t, seed)
		})
	}
}

// TestGeneratedNativeAncestorStatesEachRefusal pins the exact reason the
// ancestry validator refuses each unsafe shape. These reasons are the
// containment contract: an ancestor that is not a trusted-owned directory, a
// leaf that is not exactly 0700, an ancestor anyone may write to without sticky
// protection, or an ancestor the dropped identity cannot enter. The validator is
// a pure predicate over a stat, so the shapes a real filesystem cannot be talked
// into producing are stated directly.
func TestGeneratedNativeAncestorStatesEachRefusal(t *testing.T) {
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
			want: nativeOwnershipUntrustedAncestry,
		},
		{
			name: "ancestor owned by another identity",
			stat: directory(0o755, nativeOwnershipTargetUID, nativeOwnershipTargetGID),
			want: nativeOwnershipUntrustedAncestry,
		},
		{
			name:  "leaf is not exactly 0700",
			stat:  directory(0o750, 0, 0),
			final: true,
			want:  fmt.Sprintf(nativeOwnershipUnsafeRootMode, 0o750),
		},
		{
			name: "group-writable ancestor without sticky bit",
			stat: directory(0o771, 0, 0),
			want: nativeOwnershipWritableAncestor,
		},
		{
			name: "world-writable ancestor without sticky bit",
			stat: directory(0o717, 0, 0),
			want: nativeOwnershipWritableAncestor,
		},
		{
			name: "ancestor the target identity cannot traverse",
			stat: directory(0o700, 0, 0),
			want: generatedTreeHandoffRefusal,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateGeneratedNativeAncestor(
				testCase.stat, testCase.final, 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	for _, accepted := range []struct {
		stat  unix.Stat_t
		final bool
	}{
		{stat: directory(0o711, 0, 0)},
		{stat: directory(0o1777, 0, 0)},
		{stat: directory(0o700, 0, 0), final: true},
	} {
		require.NoError(t, validateGeneratedNativeAncestor(
			accepted.stat, accepted.final, 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID,
		))
	}
}

// TestGeneratedNativeAncestorUnderASharedIdentityAcceptsOnlyRootAncestors
// proves how far the ancestry rule relaxes when the trusted identity is also the
// target identity. Nothing separates the runtime from the identity it hands the
// tree to in that shape, so the root-owned directories every path is reached
// through are acceptable ancestors — and nothing else is: a third identity's
// ancestor, an ancestor root left writable without sticky protection, one the
// identity cannot enter, and a generated root root still owns are all refused.
// The last case proves the relaxation never reaches the isolated shape, where
// the refusal keeps its original wording.
func TestGeneratedNativeAncestorUnderASharedIdentityAcceptsOnlyRootAncestors(t *testing.T) {
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
			want: nativeOwnershipUntrustedAncestry,
		},
		{
			name: "ancestor owned by a third identity",
			stat: directory(0o755, 4242, 4242),
			want: fmt.Sprintf(nativeOwnershipSharedAncestor, 4242, 4242) + "; " + nativeOwnershipSharedRemedy,
		},
		{
			name: "ancestor owned by root with a foreign group",
			stat: directory(0o755, 0, 4242),
			want: fmt.Sprintf(nativeOwnershipSharedAncestor, 0, 4242),
		},
		{
			name: "world-writable root-owned ancestor without sticky protection",
			stat: directory(0o777, 0, 0),
			want: nativeOwnershipWritableAncestor,
		},
		{
			name: "root-owned ancestor the shared identity cannot traverse",
			stat: directory(0o700, 0, 0),
			want: generatedTreeHandoffRefusal,
		},
		{
			name:  "generated root still owned by root",
			stat:  directory(0o700, 0, 0),
			final: true,
			want:  nativeOwnershipUntrustedAncestry,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateGeneratedNativeAncestor(
				testCase.stat, testCase.final, sharedUID, sharedGID, sharedUID, sharedGID,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	for _, accepted := range []struct {
		stat  unix.Stat_t
		final bool
	}{
		{stat: directory(0o755, 0, 0)},
		{stat: directory(0o1777, 0, 0)},
		{stat: directory(0o711, sharedUID, sharedGID)},
		{stat: directory(0o700, sharedUID, sharedGID), final: true},
	} {
		require.NoError(t, validateGeneratedNativeAncestor(
			accepted.stat, accepted.final, sharedUID, sharedGID, sharedUID, sharedGID,
		), "the root-owned ancestry every home directory is reached through was refused")
	}

	require.EqualError(t, validateGeneratedNativeAncestor(
		directory(0o755, 0, 0), false, sharedUID, sharedGID,
		nativeOwnershipTargetUID, nativeOwnershipTargetGID,
	), nativeOwnershipUntrustedAncestry)
}

// TestNativeOwnershipWalkAcceptsARootOwnedAncestryUnderASharedIdentity proves
// the whole walk accepts the shape a runtime that never dropped privilege
// presents: its own identity is the isolated identity, and the generated tree
// hangs from a root-owned directory it will never own. The effective identity is
// staged through its seams so the proof does not depend on which identity runs
// the tests, and the filesystem root is substituted through its seam so the
// fixture's own ancestry cannot decide the outcome. The chowns behind the handoff
// still need the root the rest of this file requires.
func TestNativeOwnershipWalkAcceptsARootOwnedAncestryUnderASharedIdentity(t *testing.T) {
	nativeOwnershipRequireRoot(t)

	base := testTraversableTempDir(t)
	native := filepath.Join(base, "native")
	require.NoError(t, os.Mkdir(native, 0o700))

	seed := filepath.Join(native, "input")
	require.NoError(t, os.WriteFile(seed, []byte("seeded"), 0o600))
	require.NoError(t, os.Chown(seed, int(nativeOwnershipTargetUID), int(nativeOwnershipTargetGID)))
	require.NoError(t, os.Chown(native, int(nativeOwnershipTargetUID), int(nativeOwnershipTargetGID)))

	previousRoot := nativeOwnershipOpenFilesystemRoot
	nativeOwnershipOpenFilesystemRoot = func() (int, error) {
		return unix.Open(base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}

	t.Cleanup(func() { nativeOwnershipOpenFilesystemRoot = previousRoot })

	previousUID, previousGID := effectiveUIDSource, effectiveGIDSource
	effectiveUIDSource = func() int { return int(nativeOwnershipTargetUID) }
	effectiveGIDSource = func() int { return int(nativeOwnershipTargetGID) }

	t.Cleanup(func() { effectiveUIDSource, effectiveGIDSource = previousUID, previousGID })

	require.NoError(t, handoffGeneratedNativeTree("/native", nativeOwnershipIsolation()))

	uid, gid := nativeOwnershipOwner(t, seed)
	require.Equal(t, nativeOwnershipTargetUID, uid, "the tree the shared identity already owned was not handed over")
	require.Equal(t, nativeOwnershipTargetGID, gid)

	require.NoError(t, os.Chown(base, 4242, 4242))

	err := handoffGeneratedNativeTree("/native", nativeOwnershipIsolation())
	require.ErrorContains(t, err, fmt.Sprintf(nativeOwnershipSharedAncestor, 4242, 4242))
	require.ErrorContains(t, err, nativeOwnershipSharedRemedy)
}

// TestNativeIdentityTraversalUsesTheApplicableModeClass proves traversability is
// decided by the single mode class the kernel would apply — owner, then group,
// then other — and never by a union of them. Reading the wrong class would admit
// an ancestry the dropped identity cannot enter, or refuse one it can.
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

// TestNativeOwnershipTrustsNothingOnAnUnrepresentableEffectiveIdentity proves
// the width guards in effectiveUID and effectiveGID fail closed. Every ownership
// decision compares the caller's effective identity against an inode owner, so
// an identity the adapter cannot represent has to become one no inode can carry
// rather than truncating into an accidental match. Linux cannot report such an
// identity, so the guard is reached through the source seam that exists for it;
// what is asserted is the outcome — the walk then trusts nothing and the tree
// keeps its owner.
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
			native, _ := nativeOwnershipGeneratedRoot(t)
			seed := filepath.Join(native, "input")
			require.NoError(t, os.WriteFile(seed, []byte("seeded"), 0o600))

			testCase.fault(t)
			require.Equal(t, uint32(math.MaxUint32), testCase.guarded())

			require.ErrorContains(
				t,
				handoffGeneratedNativeTree(native, nativeOwnershipIsolation()),
				nativeOwnershipUntrustedAncestry,
			)

			nativeOwnershipRequireTrusted(t, native)
			nativeOwnershipRequireTrusted(t, seed)
		})
	}
}
