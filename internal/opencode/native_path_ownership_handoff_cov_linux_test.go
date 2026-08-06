//go:build linux

package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestNativeOwnershipHandoffTransfersTheWholeTreeAndNothingAbove proves the
// handoff is recursive and bounded: a nested directory and the file inside it
// change hands with their modes intact, so the dropped identity ends up owning
// the whole generated tree, while the caller root above it keeps its owner.
func TestNativeOwnershipHandoffTransfersTheWholeTreeAndNothingAbove(t *testing.T) {
	native, parent := nativeOwnershipGeneratedRoot(t)
	nested := filepath.Join(native, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))

	leaf := filepath.Join(nested, "leaf")
	require.NoError(t, os.WriteFile(leaf, []byte("seeded"), 0o600))

	require.NoError(t, handoffGeneratedNativeTree(native, nativeOwnershipIsolation()))

	for path, mode := range map[string]os.FileMode{native: 0o700, nested: 0o700, leaf: 0o600} {
		uid, gid := nativeOwnershipOwner(t, path)
		require.Equal(t, nativeOwnershipTargetUID, uid, path)
		require.Equal(t, nativeOwnershipTargetGID, gid, path)

		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm(), path)
	}

	nativeOwnershipRequireTrusted(t, parent)
}

// TestNativeOwnershipHandoffRefusesUnsafeEntries proves the handoff refuses
// every inode shape that would leak trusted state to the dropped identity, and
// that it refuses each for its own stated reason rather than by a single
// catch-all. A symlink is never followed, a FIFO or other non-regular inode is
// never transferred, a file with a second name is never transferred because the
// other name would follow it, a file or directory whose mode is broader than the
// contract is never transferred, and an entry that already belongs to somebody
// else is never treated as trusted content. Each case asserts the generated root
// still belongs to the trusted identity, so nothing changed hands on the way to
// the refusal.
func TestNativeOwnershipHandoffRefusesUnsafeEntries(t *testing.T) {
	for _, testCase := range []struct {
		name string
		seed func(*testing.T, string)
		want string
		is   error
	}{
		{
			name: "symlink entry is never followed",
			seed: func(t *testing.T, native string) {
				t.Helper()
				require.NoError(t, os.Symlink("/etc/passwd", filepath.Join(native, "entry")))
			},
			want: nativeOwnershipEntryOpenRefusal,
			is:   unix.ELOOP,
		},
		{
			name: "non-regular entry",
			seed: func(t *testing.T, native string) {
				t.Helper()
				require.NoError(t, unix.Mkfifo(filepath.Join(native, "channel"), 0o600))
			},
			want: "unsupported type",
		},
		{
			name: "file with a second name",
			seed: func(t *testing.T, native string) {
				t.Helper()

				first := filepath.Join(native, "first")
				require.NoError(t, os.WriteFile(first, []byte("seeded"), 0o600))
				require.NoError(t, os.Link(first, filepath.Join(native, "second")))
			},
			want: "generated native file has 2 links",
		},
		{
			name: "file readable beyond its owner",
			seed: func(t *testing.T, native string) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(native, "entry"), []byte("seeded"), 0o644))
			},
			want: "generated native file mode 0644 is unsafe",
		},
		{
			name: "subdirectory reachable beyond its owner",
			seed: func(t *testing.T, native string) {
				t.Helper()
				require.NoError(t, os.Mkdir(filepath.Join(native, "nested"), 0o750))
			},
			want: "generated native directory mode 0750 is unsafe",
		},
		{
			name: "entry already owned by another identity",
			seed: func(t *testing.T, native string) {
				t.Helper()

				entry := filepath.Join(native, "entry")
				require.NoError(t, os.WriteFile(entry, []byte("seeded"), 0o600))
				require.NoError(
					t, os.Chown(entry, int(nativeOwnershipTargetUID), int(nativeOwnershipTargetGID)),
				)
			},
			want: "generated native inode owner changed to uid=65534 gid=65534",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			native, _ := nativeOwnershipGeneratedRoot(t)
			testCase.seed(t, native)

			err := handoffGeneratedNativeTree(native, nativeOwnershipIsolation())
			require.ErrorContains(t, err, testCase.want)

			if testCase.is != nil {
				require.ErrorIs(t, err, testCase.is)
			}

			nativeOwnershipRequireTrusted(t, native)
		})
	}
}

// TestNativeOwnershipHandoffRefusesUnenumerableDirectory proves a directory
// whose contents cannot be listed is refused rather than chowned blind. Handing
// the root over without enumerating it would transfer whatever it contains
// unexamined. A descriptor the walk produced always enumerates, so the case
// supplies an O_PATH descriptor: it answers the fstat the handoff validates with
// and then refuses to be read.
func TestNativeOwnershipHandoffRefusesUnenumerableDirectory(t *testing.T) {
	native, _ := nativeOwnershipGeneratedRoot(t)
	seed := filepath.Join(native, "input")
	require.NoError(t, os.WriteFile(seed, []byte("seeded"), 0o600))

	directory := nativeOwnershipPathDescriptor(t, native, true)

	require.ErrorIs(
		t,
		handoffGeneratedNativeDirectory(
			directory, 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID,
		),
		unix.EBADF,
	)

	nativeOwnershipRequireTrusted(t, native)
	nativeOwnershipRequireTrusted(t, seed)
}

// TestNativeOwnershipHandoffRefusesDriftedRootMode proves a generated root whose
// mode drifted off 0700 after the walk accepted it is refused before any inode
// below it changes hands. The drift is real rather than staged: the descriptor is
// opened first and the directory chmodded afterwards, which is exactly the race
// the re-read of the held descriptor exists to catch.
func TestNativeOwnershipHandoffRefusesDriftedRootMode(t *testing.T) {
	native, _ := nativeOwnershipGeneratedRoot(t)
	seed := filepath.Join(native, "input")
	require.NoError(t, os.WriteFile(seed, []byte("seeded"), 0o600))

	directory, err := os.Open(native)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })

	require.NoError(t, os.Chmod(native, 0o750))

	require.ErrorContains(
		t,
		handoffGeneratedNativeDirectory(
			directory, 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID,
		),
		"generated native directory mode 0750 is unsafe",
	)

	nativeOwnershipRequireTrusted(t, native)
	nativeOwnershipRequireTrusted(t, seed)
}

// TestNativeOwnershipEntryRefusesUnusableDescriptor proves an entry descriptor
// the kernel no longer answers for is refused instead of being classified by a
// zero-valued stat, which would read as an unsupported type at best and as a
// directory to descend into at worst.
func TestNativeOwnershipEntryRefusesUnusableDescriptor(t *testing.T) {
	entry, err := os.Open(os.DevNull)
	require.NoError(t, err)
	require.NoError(t, entry.Close())

	require.ErrorIs(
		t,
		handoffGeneratedNativeEntry(entry, 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID),
		unix.EBADF,
	)
}

// TestValidateGeneratedNativeInodeRefusesDriftedInodes proves the pre-chown
// revalidation catches the two ways an accepted descriptor can stop describing
// the inode the walk approved that no filesystem arrangement can stage: a
// descriptor that has stopped answering at all, and one whose inode is not the
// type the caller is about to treat it as.
func TestValidateGeneratedNativeInodeRefusesDriftedInodes(t *testing.T) {
	native, _ := nativeOwnershipGeneratedRoot(t)
	regular := filepath.Join(native, "file")
	require.NoError(t, os.WriteFile(regular, []byte("seeded"), 0o600))

	file, err := os.Open(regular)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	require.ErrorIs(
		t,
		validateGeneratedNativeInode(
			-1, unix.S_IFREG, 0, 0, nativeOwnershipTargetUID, nativeOwnershipTargetGID, true,
		),
		unix.EBADF,
	)

	require.ErrorContains(
		t,
		validateGeneratedNativeInode(
			int(file.Fd()), unix.S_IFDIR, 0, 0,
			nativeOwnershipTargetUID, nativeOwnershipTargetGID, false,
		),
		nativeOwnershipInodeTypeRefusal,
	)

	nativeOwnershipRequireTrusted(t, regular)
}

// TestChownGeneratedNativeInodeProvesTheTransfer proves the handoff never
// reports success on a transfer it has not proven. The chown itself must
// succeed; the re-read that follows must succeed; and the re-read must confirm
// both the expected inode type and the new owner, with a file still carrying
// exactly one link. Reporting success without that proof would tell the caller
// the isolated identity owns a tree it does not.
func TestChownGeneratedNativeInodeProvesTheTransfer(t *testing.T) {
	native, _ := nativeOwnershipGeneratedRoot(t)
	regular := filepath.Join(native, "file")
	require.NoError(t, os.WriteFile(regular, []byte("seeded"), 0o600))

	t.Run("descriptor cannot be chowned", func(t *testing.T) {
		descriptor := nativeOwnershipPathDescriptor(t, regular, false)

		require.ErrorIs(
			t,
			chownGeneratedNativeInode(
				int(descriptor.Fd()), unix.S_IFREG,
				nativeOwnershipTargetUID, nativeOwnershipTargetGID, true,
			),
			unix.EBADF,
		)

		nativeOwnershipRequireTrusted(t, regular)
	})

	t.Run("transferred inode cannot be re-read", func(t *testing.T) {
		file, err := os.Open(regular)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		t.Cleanup(func() { require.NoError(t, unix.Fchown(int(file.Fd()), 0, 0)) })

		previous := nativeOwnershipFstat
		nativeOwnershipFstat = func(int, *unix.Stat_t) error { return unix.EIO }

		t.Cleanup(func() { nativeOwnershipFstat = previous })

		require.ErrorIs(
			t,
			chownGeneratedNativeInode(
				int(file.Fd()), unix.S_IFREG,
				nativeOwnershipTargetUID, nativeOwnershipTargetGID, true,
			),
			unix.EIO,
		)
	})

	t.Run("transferred inode is not the expected type", func(t *testing.T) {
		file, err := os.Open(regular)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		t.Cleanup(func() { require.NoError(t, unix.Fchown(int(file.Fd()), 0, 0)) })

		require.ErrorContains(
			t,
			chownGeneratedNativeInode(
				int(file.Fd()), unix.S_IFDIR,
				nativeOwnershipTargetUID, nativeOwnershipTargetGID, false,
			),
			nativeOwnershipUnprovenHandoff,
		)

		uid, gid := nativeOwnershipOwner(t, regular)
		require.Equal(t, nativeOwnershipTargetUID, uid, "the chown that the refusal covers never happened")
		require.Equal(t, nativeOwnershipTargetGID, gid)
	})

	t.Run("transferred file gained a second name", func(t *testing.T) {
		linked := filepath.Join(native, "linked")
		require.NoError(t, os.WriteFile(linked, []byte("seeded"), 0o600))
		require.NoError(t, os.Link(linked, filepath.Join(native, "alias")))

		file, err := os.Open(linked)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		t.Cleanup(func() { require.NoError(t, unix.Fchown(int(file.Fd()), 0, 0)) })

		require.ErrorContains(
			t,
			chownGeneratedNativeInode(
				int(file.Fd()), unix.S_IFREG,
				nativeOwnershipTargetUID, nativeOwnershipTargetGID, true,
			),
			nativeOwnershipUnprovenHandoff,
		)
	})
}
