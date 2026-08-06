//go:build linux

package opencodeacp

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/savid/acp-go-opencode/internal/opencode"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestGeneratedNativeTreeDistinctIdentityTraversal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	parent, err := os.MkdirTemp("/tmp", "acp-go-opencode-ownership-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })

	if chmodErr := os.Chmod(parent, 0o711); chmodErr != nil {
		t.Fatal(chmodErr)
	}

	control := filepath.Join(parent, "control")
	native := filepath.Join(parent, "native")
	if mkdirErr := os.Mkdir(control, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(control, "secret"), []byte("root"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if mkdirErr := os.Mkdir(native, 0o700); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(native, "input"), []byte("ok"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	isolation := &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}}
	if handoffErr := handoffGeneratedNativeTree(native, isolation); handoffErr != nil {
		t.Fatal(handoffErr)
	}

	command := exec.Command(
		"/bin/sh",
		"-c",
		`set -eu
test "$(cat "$1/input")" = ok
printf native >"$1/output"
if cat "$2/secret" >/dev/null 2>&1; then exit 42; fi`,
		"sh",
		native,
		control,
	)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: isolation.UID, Gid: isolation.GID, Groups: []uint32{}},
	}
	if output, combinedErr := command.CombinedOutput(); combinedErr != nil {
		t.Fatalf("dropped-identity proof: %v: %s", combinedErr, output)
	}

	contents, err := os.ReadFile(filepath.Join(native, "output"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "native" {
		t.Fatalf("native output = %q", contents)
	}
	if contents, err := os.ReadFile(filepath.Join(control, "secret")); err != nil || string(contents) != "root" {
		t.Fatalf("trusted control changed: %q, %v", contents, err)
	}
}

func TestImplicitRuntimeHomeCanBeHandedToDistinctIdentity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	parent, err := os.MkdirTemp("/tmp", "acp-go-opencode-runtime-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if chmodErr := os.Chmod(parent, 0o711); chmodErr != nil {
		t.Fatal(chmodErr)
	}

	agent := NewAgent(WithScratchDir(parent))
	runtimeHome := agent.homeRoot()
	if filepath.Dir(runtimeHome) != parent {
		t.Fatalf("implicit runtime home %q has an intermediate ancestor beneath scratch %q", runtimeHome, parent)
	}
	if _, createErr := opencode.CreateRuntimeXDGDirs(runtimeHome); createErr != nil {
		t.Fatal(createErr)
	}

	isolation := &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}}
	if handoffErr := handoffGeneratedNativeTree(runtimeHome, isolation); handoffErr != nil {
		t.Fatal(handoffErr)
	}

	output := filepath.Join(runtimeHome, "state", "identity-proof")
	command := exec.Command("/bin/sh", "-c", `printf native >"$1"`, "sh", output)
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: isolation.UID, Gid: isolation.GID, Groups: []uint32{}},
	}
	if commandOutput, combinedErr := command.CombinedOutput(); combinedErr != nil {
		t.Fatalf("dropped-identity runtime write: %v: %s", combinedErr, commandOutput)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "native" {
		t.Fatalf("runtime output = %q", contents)
	}
}

func TestGeneratedNativeTreeRejectsUntraversableCallerRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	parent, err := os.MkdirTemp("/tmp", "acp-go-opencode-caller-root-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })

	native := filepath.Join(parent, "native")
	if err := os.Mkdir(native, 0o700); err != nil {
		t.Fatal(err)
	}

	isolation := &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}}
	if err := handoffGeneratedNativeTree(native, isolation); err == nil {
		t.Fatal("0700 caller root accepted")
	}
	if err := os.Chmod(parent, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := handoffGeneratedNativeTree(native, isolation); err != nil {
		t.Fatalf("0711 protected caller root: %v", err)
	}
}

func TestGeneratedNativeTreeRejectsUnsafeEntries(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	for _, testCase := range []struct {
		name string
		seed func(string) error
	}{
		{name: "symlink", seed: func(root string) error {
			return os.Symlink("/etc/passwd", filepath.Join(root, "entry"))
		}},
		{name: "hardlink", seed: func(root string) error {
			first := filepath.Join(root, "first")
			if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
				return err
			}

			return os.Link(first, filepath.Join(root, "second"))
		}},
		{name: "broad mode", seed: func(root string) error {
			return os.WriteFile(filepath.Join(root, "entry"), []byte("x"), 0o644)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			parent, err := os.MkdirTemp("/tmp", "acp-go-opencode-unsafe-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(parent) })
			if err := os.Chmod(parent, 0o711); err != nil {
				t.Fatal(err)
			}

			native := filepath.Join(parent, "native")
			if err := os.Mkdir(native, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := testCase.seed(native); err != nil {
				t.Fatal(err)
			}

			isolation := &ProcessIsolation{UID: 65534, GID: 65534, BaseEnvironment: map[string]string{}}
			if err := handoffGeneratedNativeTree(native, isolation); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe tree result = %v", err)
			}
		})
	}
}

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

// nativeOwnershipGeneratedRoot builds a trusted 0700 generated tree under a
// 0711 caller root, which is the shape the generated-tree handoff accepts.
func nativeOwnershipGeneratedRoot(t *testing.T) string {
	t.Helper()
	requireNativeOwnershipRoot(t)

	parent, err := os.MkdirTemp("/tmp", "acp-go-opencode-refusal-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	require.NoError(t, os.Chmod(parent, 0o711))

	native := filepath.Join(parent, "native")
	require.NoError(t, os.Mkdir(native, 0o700))

	return native
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

// openNativeOwnershipPathDescriptor returns an O_PATH descriptor. O_PATH
// descriptors answer fstat but reject every operation that reads or writes the
// inode, which is how these cases make an already-validated descriptor stop
// answering without racing the filesystem.
func openNativeOwnershipPathDescriptor(t *testing.T, path string, directory bool) *os.File {
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

// TestNativeOwnershipTraversalRejectsRelativeRoot proves both traversal users
// refuse a relative root outright. A relative walk would resolve against the
// working directory the agent controls rather than against the tree the caller
// named, so the refusal has to land before the first descriptor is opened.
func TestNativeOwnershipTraversalRejectsRelativeRoot(t *testing.T) {
	isolation := nativeOwnershipTestIsolation()

	require.ErrorContains(
		t, handoffGeneratedNativeTree("relative/native", isolation), "native path must be absolute",
	)
	require.ErrorContains(
		t, validateNativeOwnedDirectory("relative/home", isolation), "native path must be absolute",
	)
}

// TestNativeOwnershipTraversalRefusesTheFilesystemRootAsATarget proves the
// filesystem root is validated before any component is opened, and that it
// reaches the validator as the final component rather than as an ancestor. Both
// users therefore refuse "/" on the leaf contract rather than on the ancestry
// contract — the generated handoff because "/" is not exactly 0700, the durable
// check because "/" does not belong to the dropped identity — and "/" keeps its
// owner.
func TestNativeOwnershipTraversalRefusesTheFilesystemRootAsATarget(t *testing.T) {
	requireNativeOwnershipRoot(t)

	isolation := nativeOwnershipTestIsolation()

	require.ErrorContains(t, handoffGeneratedNativeTree("/", isolation), "generated native root mode 0755 is unsafe")
	require.ErrorContains(t, validateNativeOwnedDirectory("/", isolation), nativeOwnedHomeRefusal)

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

// TestNativeOwnershipTraversalPropagatesMissingComponent proves a component
// that does not exist surfaces the kernel's own ENOENT rather than being
// treated as an empty tree that needs no handoff.
func TestNativeOwnershipTraversalPropagatesMissingComponent(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)

	err := handoffGeneratedNativeTree(
		filepath.Join(filepath.Dir(native), "absent"), nativeOwnershipTestIsolation(),
	)
	require.ErrorIs(t, err, unix.ENOENT)
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

// TestGeneratedNativeAncestorStatesEachRefusal pins the exact reason the
// generated-tree ancestry validator refuses each unsafe shape. These reasons
// are the containment contract: an ancestor that is not a trusted-owned
// directory, a leaf that is not exactly 0700, an ancestor anyone may write to
// without sticky protection, or an ancestor the dropped identity cannot enter.
func TestGeneratedNativeAncestorStatesEachRefusal(t *testing.T) {
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
			want: "not a trusted directory",
		},
		{
			name: "ancestor owned by another identity",
			stat: directory(0o755, targetUID, targetGID),
			want: "not a trusted directory",
		},
		{
			name:  "leaf is not exactly 0700",
			stat:  directory(0o750, trustedUID, trustedGID),
			final: true,
			want:  "generated native root mode 0750 is unsafe",
		},
		{
			name: "group-writable ancestor without sticky bit",
			stat: directory(0o771, trustedUID, trustedGID),
			want: "0771 is writable without sticky protection",
		},
		{
			name: "world-writable ancestor without sticky bit",
			stat: directory(0o717, trustedUID, trustedGID),
			want: "0717 is writable without sticky protection",
		},
		{
			name: "ancestor the target identity cannot traverse",
			stat: directory(0o700, trustedUID, trustedGID),
			want: "not traversable by the target identity",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateGeneratedNativeAncestor(
				testCase.stat, testCase.final, trustedUID, trustedGID, targetUID, targetGID,
			)
			require.ErrorContains(t, err, testCase.want)
		})
	}

	require.NoError(t, validateGeneratedNativeAncestor(
		directory(0o711, trustedUID, trustedGID), false, trustedUID, trustedGID, targetUID, targetGID,
	))
	require.NoError(t, validateGeneratedNativeAncestor(
		directory(0o1777, trustedUID, trustedGID), false, trustedUID, trustedGID, targetUID, targetGID,
	))
	require.NoError(t, validateGeneratedNativeAncestor(
		directory(0o700, trustedUID, trustedGID), true, trustedUID, trustedGID, targetUID, targetGID,
	))
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

// nativeOwnershipTestEntry is a directory entry whose name the kernel would
// never produce through ReadDir.
type nativeOwnershipTestEntry struct {
	name string
}

func (entry nativeOwnershipTestEntry) Name() string         { return entry.name }
func (nativeOwnershipTestEntry) IsDir() bool                { return false }
func (nativeOwnershipTestEntry) Type() os.FileMode          { return 0 }
func (nativeOwnershipTestEntry) Info() (os.FileInfo, error) { return nil, unix.EBADF }

// TestNativeOwnershipHandoffRefusesEscapingEntryName proves the handoff never
// resolves an entry name that could leave the directory it is walking. Every
// name reached here is fed straight back to openat against the directory
// descriptor, so "..", "." or any name carrying a separator would hand the
// dropped identity an inode outside the generated tree. The kernel cannot
// produce such a name, so the listing is staged on the readdir seam.
func TestNativeOwnershipHandoffRefusesEscapingEntryName(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)

	directory, err := os.Open(native)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })

	for _, name := range []string{".", parentPathSegment, "nested/leaf"} {
		t.Run(name, func(t *testing.T) {
			previous := nativeOwnershipReadDir
			nativeOwnershipReadDir = func(*os.File) ([]os.DirEntry, error) {
				return []os.DirEntry{nativeOwnershipTestEntry{name: name}}, nil
			}

			t.Cleanup(func() { nativeOwnershipReadDir = previous })

			require.ErrorContains(
				t,
				handoffNativeOwnershipDirectory(directory, 0, 0, 65534, 65534),
				"invalid generated native entry",
			)

			uid, gid := nativeOwnershipOwner(t, native)
			require.Equal(t, uint32(0), uid, "an escaping entry name still reached the handoff")
			require.Equal(t, uint32(0), gid)
		})
	}
}

// TestNativeOwnershipHandoffRefusesUnsafeRootMode proves a generated root whose
// mode drifted off 0700 between the walk and the handoff is refused before any
// inode below it changes hands. The drift is real rather than staged: the
// descriptor is opened first and the directory chmodded afterwards, which is
// exactly the race the re-read exists to catch.
func TestNativeOwnershipHandoffRefusesUnsafeRootMode(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)
	seed := filepath.Join(native, "input")
	require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

	directory := openNativeOwnershipPathDescriptor(t, native, true)
	require.NoError(t, os.Chmod(native, 0o750))

	require.ErrorContains(
		t,
		handoffNativeOwnershipDirectory(directory, 0, 0, 65534, 65534),
		"generated native directory mode 0750 is unsafe",
	)

	uid, gid := nativeOwnershipOwner(t, seed)
	require.Equal(t, uint32(0), uid)
	require.Equal(t, uint32(0), gid)
}

// TestNativeOwnershipHandoffRefusesUnenumerableDirectory proves a directory
// whose contents cannot be listed is refused rather than chowned blind. Handing
// the root over without enumerating it would transfer whatever it contains
// unexamined.
func TestNativeOwnershipHandoffRefusesUnenumerableDirectory(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)
	seed := filepath.Join(native, "input")
	require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

	directory := openNativeOwnershipPathDescriptor(t, native, true)

	require.Error(t, handoffNativeOwnershipDirectory(directory, 0, 0, 65534, 65534))

	uid, gid := nativeOwnershipOwner(t, native)
	require.Equal(t, uint32(0), uid, "an unenumerable root was handed over anyway")
	require.Equal(t, uint32(0), gid)
}

// TestNativeOwnershipHandoffDescendsSubdirectories proves the handoff is
// recursive: a nested directory and its contents change hands with their modes
// intact, so the dropped identity ends up owning the whole generated tree and
// nothing outside it.
func TestNativeOwnershipHandoffDescendsSubdirectories(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)
	nested := filepath.Join(native, "nested")
	require.NoError(t, os.Mkdir(nested, 0o700))

	leaf := filepath.Join(nested, "leaf")
	require.NoError(t, os.WriteFile(leaf, []byte("x"), 0o600))

	isolation := nativeOwnershipTestIsolation()
	require.NoError(t, handoffGeneratedNativeTree(native, isolation))

	for path, mode := range map[string]os.FileMode{
		native: 0o700,
		nested: 0o700,
		leaf:   0o600,
	} {
		uid, gid := nativeOwnershipOwner(t, path)
		require.Equal(t, isolation.UID, uid, path)
		require.Equal(t, isolation.GID, gid, path)

		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm(), path)
	}

	parent := filepath.Dir(native)
	uid, gid := nativeOwnershipOwner(t, parent)
	require.Equal(t, uint32(0), uid, "the handoff climbed above the generated tree")
	require.Equal(t, uint32(0), gid)
}

// TestNativeOwnershipHandoffRefusesNonRegularEntry proves the handoff refuses
// any inode that is neither a directory nor a regular file. Chowning a FIFO or a
// device node to the dropped identity would hand it a channel the trusted
// process still holds open.
func TestNativeOwnershipHandoffRefusesNonRegularEntry(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)
	fifo := filepath.Join(native, "channel")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))

	require.ErrorContains(
		t, handoffGeneratedNativeTree(native, nativeOwnershipTestIsolation()), "unsupported type",
	)

	uid, gid := nativeOwnershipOwner(t, fifo)
	require.Equal(t, uint32(0), uid)
	require.Equal(t, uint32(0), gid)
}

// TestNativeOwnershipEntryRejectsUnusableDescriptor proves an entry descriptor
// the kernel no longer answers for is refused instead of being classified by a
// zero-valued stat, which would read as an unsupported type at best and as a
// directory at worst.
func TestNativeOwnershipEntryRejectsUnusableDescriptor(t *testing.T) {
	entry, err := os.Open(os.DevNull)
	require.NoError(t, err)
	require.NoError(t, entry.Close())

	require.ErrorIs(t, handoffNativeOwnershipEntry(entry, 0, 0, 65534, 65534), unix.EBADF)
}

// TestValidateHandoffNativeInodeRefusesDriftedInodes proves the pre-chown
// revalidation catches every way the inode behind an accepted descriptor can
// stop being the trusted inode the walk approved.
func TestValidateHandoffNativeInodeRefusesDriftedInodes(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)
	regular := filepath.Join(native, "file")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0o600))

	file, err := os.Open(regular)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	t.Run("unusable descriptor", func(t *testing.T) {
		require.ErrorIs(
			t,
			validateHandoffNativeInode(-1, unix.S_IFREG, 0, 0, 65534, 65534, true),
			unix.EBADF,
		)
	})

	t.Run("inode type changed", func(t *testing.T) {
		require.ErrorContains(
			t,
			validateHandoffNativeInode(int(file.Fd()), unix.S_IFDIR, 0, 0, 65534, 65534, false),
			"generated native inode type 0100000 changed",
		)
	})

	t.Run("inode owner changed", func(t *testing.T) {
		require.NoError(t, unix.Fchown(int(file.Fd()), 65534, 65534))
		t.Cleanup(func() { require.NoError(t, unix.Fchown(int(file.Fd()), 0, 0)) })

		require.ErrorContains(
			t,
			validateHandoffNativeInode(int(file.Fd()), unix.S_IFREG, 0, 0, 65534, 65534, true),
			"generated native inode owner changed to uid=65534 gid=65534",
		)
	})
}

// TestChownAndVerifyNativeInodeProvesTheTransfer proves the handoff never
// reports success on an unproven transfer: the chown must succeed, the re-read
// must confirm both the new owner and the expected inode type, and a file must
// still carry exactly one link afterwards.
func TestChownAndVerifyNativeInodeProvesTheTransfer(t *testing.T) {
	native := nativeOwnershipGeneratedRoot(t)
	regular := filepath.Join(native, "file")
	require.NoError(t, os.WriteFile(regular, []byte("x"), 0o600))

	t.Run("descriptor cannot be chowned", func(t *testing.T) {
		descriptor := openNativeOwnershipPathDescriptor(t, regular, false)
		require.ErrorIs(
			t,
			chownAndVerifyNativeInode(int(descriptor.Fd()), unix.S_IFREG, 65534, 65534, true),
			unix.EBADF,
		)

		uid, gid := nativeOwnershipOwner(t, regular)
		require.Equal(t, uint32(0), uid)
		require.Equal(t, uint32(0), gid)
	})

	t.Run("transferred inode is not the expected type", func(t *testing.T) {
		file, err := os.Open(regular)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })
		t.Cleanup(func() { require.NoError(t, unix.Fchown(int(file.Fd()), 0, 0)) })

		require.ErrorContains(
			t,
			chownAndVerifyNativeInode(int(file.Fd()), unix.S_IFDIR, 65534, 65534, false),
			"generated native inode ownership handoff could not be proven",
		)
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
			chownAndVerifyNativeInode(int(file.Fd()), unix.S_IFREG, 65534, 65534, true),
			unix.EIO,
		)
	})

	t.Run("transferred file gained a link", func(t *testing.T) {
		linked := filepath.Join(native, "linked")
		require.NoError(t, os.WriteFile(linked, []byte("x"), 0o600))

		file, err := os.Open(linked)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })

		require.NoError(t, os.Link(linked, filepath.Join(native, "alias")))

		require.ErrorContains(
			t,
			chownAndVerifyNativeInode(int(file.Fd()), unix.S_IFREG, 65534, 65534, true),
			"generated native file has 2 links after handoff",
		)
	})
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
	} {
		t.Run(testCase.name, func(t *testing.T) {
			native := nativeOwnershipGeneratedRoot(t)
			seed := filepath.Join(native, "input")
			require.NoError(t, os.WriteFile(seed, []byte("x"), 0o600))

			testCase.fault(t)
			require.Equal(t, uint32(math.MaxUint32), testCase.guarded())

			require.ErrorContains(
				t,
				handoffGeneratedNativeTree(native, nativeOwnershipTestIsolation()),
				"generated native path ancestry is not a trusted directory",
			)

			uid, gid := nativeOwnershipOwner(t, seed)
			require.Equal(t, uint32(0), uid, "the tree changed hands on an unrepresentable identity")
			require.Equal(t, uint32(0), gid)
		})
	}
}

// nativeOwnedHomeRefusal is why Linux refuses a durable native home owned by
// someone other than the isolated identity: the real ownership walk reaches the
// home and finds the wrong owner.
const nativeOwnedHomeRefusal = "native-owned directory is not owned by the target identity"
