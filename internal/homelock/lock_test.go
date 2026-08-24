package homelock

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndependentLocksFailClosedAndAreNeverUnlinked(t *testing.T) {
	home := t.TempDir()
	claim, err := AcquireClaim(home)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, secondErr := AcquireClaim(home); secondErr == nil {
		t.Fatal("second claim succeeded")
	}
	liveness, err := AcquireLiveness(home)
	if err != nil {
		t.Fatalf("liveness: %v", err)
	}
	if _, err := AcquireLiveness(home); err == nil {
		t.Fatal("second liveness claim succeeded")
	}
	if err := liveness.Release(); err != nil {
		t.Fatalf("release liveness: %v", err)
	}
	if err := claim.Release(); err != nil {
		t.Fatalf("release claim: %v", err)
	}

	for _, name := range []string{ClaimFileName, LivenessFileName} {
		info, err := os.Stat(filepath.Join(home, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
}

func TestAcquireCombinedAndFailureShapes(t *testing.T) {
	lock, err := Acquire(t.TempDir())
	require.NoError(t, err)
	require.Len(t, lock.files, 2)
	require.NoError(t, lock.Release())
	require.NoError(t, lock.Release(), "release must be idempotent")
	require.NoError(t, (*Lock)(nil).Release())

	_, err = Acquire("")
	require.ErrorContains(t, err, "writable home is required")

	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	require.NoError(t, os.WriteFile(notDirectory, []byte("x"), 0o600))
	_, err = AcquireClaim(filepath.Join(notDirectory, "child"))
	require.ErrorContains(t, err, "create OpenCode home")

	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, ClaimFileName), 0o700))
	_, err = AcquireClaim(home)
	require.ErrorContains(t, err, "open runtime lock")

	home = t.TempDir()
	liveness, err := AcquireLiveness(home)
	require.NoError(t, err)
	_, err = Acquire(home)
	require.Error(t, err)
	claim, err := AcquireClaim(home)
	require.NoError(t, err, "failed combined acquisition must release its claim")
	require.NoError(t, claim.Release())
	require.NoError(t, liveness.Release())
}

func TestVerifyLockedPathFailures(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.ErrorContains(t, verifyLockedPath(file, path), "stat held")

	file, err = os.OpenFile(path, os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.ErrorContains(t, verifyLockedPath(file, path), "stat runtime lock path")
	require.NoError(t, file.Close())

	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	file, err = os.OpenFile(first, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(second, []byte("different"), 0o600))
	require.ErrorContains(t, verifyLockedPath(file, second), "path changed")
	require.NoError(t, file.Close())
}

func TestReleaseJoinsUnlockAndCloseErrors(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "closed")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	lock := &Lock{files: []*os.File{file}}
	err = lock.Release()
	require.Error(t, err)
}

func TestAcquireInjectedChmodAndPlatformFailures(t *testing.T) {
	oldChmod := chmodLockFile
	oldLock := lockPlatform
	oldVerify := verifyLockPath
	oldValidateFS := validateFS
	t.Cleanup(func() {
		chmodLockFile = oldChmod
		lockPlatform = oldLock
		verifyLockPath = oldVerify
		validateFS = oldValidateFS
	})

	chmodLockFile = func(*os.File, os.FileMode) error { return os.ErrPermission }
	_, err := AcquireClaim(t.TempDir())
	require.ErrorContains(t, err, "chmod runtime lock")

	chmodLockFile = oldChmod
	validateFS = func(*os.File) error { return os.ErrInvalid }
	_, err = AcquireClaim(t.TempDir())
	require.ErrorContains(t, err, "validate OpenCode writable-home filesystem")

	validateFS = oldValidateFS
	lockPlatform = func(*os.File) error { return os.ErrPermission }
	_, err = AcquireClaim(t.TempDir())
	require.ErrorContains(t, err, "claim OpenCode writable home")

	lockPlatform = oldLock
	verifyLockPath = func(*os.File, string) error { return os.ErrInvalid }
	_, err = AcquireClaim(t.TempDir())
	require.ErrorIs(t, err, os.ErrInvalid)
}

// TestConstructionFailsWithTheUnsupportedLockSentinel proves the platform gate is
// a sentinel a caller can test for rather than a message it has to match. A
// platform with no advisory-lock primitive fails construction with
// ErrRuntimeLockUnsupported: the runtime home is single-writer by that lock
// alone, and one nobody can claim exclusively would let two runtimes write one
// native database and call it ownership.
func TestConstructionFailsWithTheUnsupportedLockSentinel(t *testing.T) {
	originalLock := lockPlatform
	originalFS := validateFS

	t.Cleanup(func() {
		lockPlatform = originalLock
		validateFS = originalFS
	})

	home := t.TempDir()

	lockPlatform = func(*os.File) error { return ErrRuntimeLockUnsupported }

	claim, err := AcquireClaim(home)
	require.Nil(t, claim, "an unsupported platform handed back a lock anyway")
	require.ErrorIs(t, err, ErrRuntimeLockUnsupported)

	_, err = Acquire(home)
	require.ErrorIs(t, err, ErrRuntimeLockUnsupported, "the paired acquisition swallowed the sentinel")

	lockPlatform = originalLock
	validateFS = func(*os.File) error { return ErrRuntimeLockUnsupported }

	_, err = AcquireLiveness(home)
	require.ErrorIs(t, err, ErrRuntimeLockUnsupported,
		"a filesystem the lock primitive cannot cover reported something else")
}
