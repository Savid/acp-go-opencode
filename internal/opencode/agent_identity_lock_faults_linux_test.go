//go:build linux

package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

// agentIdentityLockCovSeams restores the agent identity lock seam group,
// including the descriptor seams this file drives, when the test ends.
func agentIdentityLockCovSeams(t *testing.T) {
	t.Helper()

	restoreAgentIdentityLockTestSeams(t)

	fstat := agentIdentityLockFstat
	openat := agentIdentityLockOpenat
	closeFD := agentIdentityLockCloseFD
	fcntl := agentIdentityLockFcntl

	t.Cleanup(func() {
		agentIdentityLockFstat = fstat
		agentIdentityLockOpenat = openat
		agentIdentityLockCloseFD = closeFD
		agentIdentityLockFcntl = fcntl
	})
}

// agentIdentityLockCovAuthority bootstraps a trusted authority root and returns
// its open directory descriptor together with the runtime root it lives under.
func agentIdentityLockCovAuthority(t *testing.T) (*os.File, string) {
	t.Helper()

	agentIdentityLockCovSeams(t)

	root := configureAgentIdentityLockTestRoot(t)

	directory, err := bootstrapAgentIdentityLockDirectory(
		root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = directory.Close() })

	return directory, root
}

// agentIdentityLockCovNamedLock creates one permanent trusted lock inside the
// authority root.
func agentIdentityLockCovNamedLock(t *testing.T, directory *os.File, name string) *os.File {
	t.Helper()

	file, err := openAgentStandaloneNamedLock(
		directory, name, true, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })

	return file
}

// agentIdentityLockCovLocked creates a permanent lock and takes the requested
// lease on it, standing in for the host that holds a lease before handing a
// descriptor to an agent.
func agentIdentityLockCovLocked(t *testing.T, directory *os.File, name string, operation int) *os.File {
	t.Helper()

	file := agentIdentityLockCovNamedLock(t, directory, name)
	require.NoError(t, unix.Flock(int(file.Fd()), operation|unix.LOCK_NB))

	return file
}

// agentIdentityLockCovDuplicate hands out an independent descriptor on the same
// open file description, which is what an inherited handoff really is.
func agentIdentityLockCovDuplicate(t *testing.T, source *os.File) *os.File {
	t.Helper()

	duplicate, err := duplicateAgentIdentityLock(source)
	require.NoError(t, err)
	t.Cleanup(func() { _ = duplicate.Close() })

	return duplicate
}

// agentIdentityLockCovClosed returns an *os.File whose descriptor is already
// released, so every syscall through it answers EBADF.
func agentIdentityLockCovClosed(t *testing.T) *os.File {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "identity-lock-cov")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	return file
}

// agentIdentityLockCovFaultFstat makes the identity lock's descriptor metadata
// probe fail on its at-th call, standing in for a kernel that stops answering
// for a descriptor the adoption has already accepted. The caller restores the
// seam through agentIdentityLockCovSeams.
func agentIdentityLockCovFaultFstat(at int, failure error) {
	original := agentIdentityLockFstat
	calls := 0
	agentIdentityLockFstat = func(fd int, stat *unix.Stat_t) error {
		calls++
		if calls == at {
			return failure
		}

		return original(fd, stat)
	}
}

// agentIdentityLockCovFaultFstatat makes the named-entry probe fail on its
// at-th call. The first two calls always belong to the authority directory
// chain.
func agentIdentityLockCovFaultFstatat(at int, failure error) {
	original := agentIdentityDirectoryFstatat
	calls := 0
	agentIdentityDirectoryFstatat = func(dirfd int, path string, stat *unix.Stat_t, flags int) error {
		calls++
		if calls == at {
			return failure
		}

		return original(dirfd, path, stat, flags)
	}
}

// agentIdentityLockCovFaultClose makes the authority directory handoff close
// fail on its at-th call while still releasing the descriptor.
func agentIdentityLockCovFaultClose(at int, failure error) {
	calls := 0
	agentIdentityDirectoryClose = func(file *os.File) error {
		calls++
		if err := file.Close(); err != nil {
			return err
		}

		if calls == at {
			return failure
		}

		return nil
	}
}

// agentIdentityLockCovFdinfo renders an fdinfo payload claiming an flock of the
// given mode over the file's real inode, whether or not one is actually held.
func agentIdentityLockCovFdinfo(t *testing.T, file *os.File, mode string) []byte {
	t.Helper()

	var stat unix.Stat_t

	require.NoError(t, unix.Fstat(int(file.Fd()), &stat))

	return []byte(fmt.Sprintf(
		"pos:\t0\nlock:\t1: FLOCK ADVISORY %s %d 00:26:%d 0 EOF\n", mode, os.Getpid(), stat.Ino,
	))
}

// TestAgentIdentityAuthorityBootstrapHandoffFaultsFailClosed proves the
// two-step creation of the agent identity authority root never hands back a
// directory it could not fully establish: a failed creation, reopen, metadata
// probe or handoff close all abort with no directory returned, and a failed
// creation leaves the authority path absent rather than half made.
func TestAgentIdentityAuthorityBootstrapHandoffFaultsFailClosed(t *testing.T) {
	for name, testCase := range map[string]struct {
		fault  func(error)
		unmade string
	}{
		"owner directory handoff close": {
			fault: func(failure error) { agentIdentityLockCovFaultClose(2, failure) },
		},
		"authority directory handoff close": {
			fault: func(failure error) { agentIdentityLockCovFaultClose(4, failure) },
		},
		"authority directory creation": {
			fault: func(failure error) {
				original := agentIdentityDirectoryMkdirat
				calls := 0
				agentIdentityDirectoryMkdirat = func(dirfd int, path string, mode uint32) error {
					calls++
					if calls == 2 {
						return failure
					}

					return original(dirfd, path, mode)
				}
			},
			unmade: "agent-identities",
		},
		"owner directory open": {
			fault: func(failure error) {
				agentIdentityDirectoryOpenat = func(int, string, int, uint32) (int, error) {
					return -1, failure
				}
			},
			unmade: "agent-identities",
		},
		"runtime root metadata": {
			fault:  func(failure error) { agentIdentityLockCovFaultFstat(1, failure) },
			unmade: ".",
		},
		"named owner directory metadata": {
			fault: func(failure error) { agentIdentityLockCovFaultFstat(3, failure) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			agentIdentityLockCovSeams(t)

			root := configureAgentIdentityLockTestRoot(t)
			failure := errors.New("injected " + name + " failure")

			testCase.fault(failure)

			directory, err := bootstrapAgentIdentityLockDirectory(
				root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
			)
			if directory != nil {
				_ = directory.Close()

				t.Fatal("authority bootstrap returned a directory despite an injected fault")
			}

			require.ErrorIs(t, err, failure)

			if testCase.unmade == "" {
				return
			}

			path := filepath.Join(root, "acp-go", testCase.unmade)
			_, statErr := os.Stat(path)
			require.ErrorIs(t, statErr, os.ErrNotExist,
				"a refused bootstrap must not leave the authority path behind",
			)
		})
	}

	t.Run("absent runtime root", func(t *testing.T) {
		agentIdentityLockCovSeams(t)

		root := filepath.Join(configureAgentIdentityLockTestRoot(t), "absent")

		_, err := bootstrapAgentIdentityLockDirectory(
			root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
		)
		require.ErrorIs(t, err, unix.ENOENT)
		require.ErrorContains(t, err, "open agent identity runtime root")
	})
}

// TestAgentIdentityAuthorityOpenHandoffFaultsFailClosed proves that reopening
// an already-established authority root refuses whenever any step of the
// descriptor chain — the runtime root, either handoff close, or the authority
// directory itself — cannot be completed, so no caller ever operates on a
// partially proven authority.
func TestAgentIdentityAuthorityOpenHandoffFaultsFailClosed(t *testing.T) {
	for name, testCase := range map[string]struct {
		prepare   func(root string, failure error) error
		absent    bool
		wantError string
	}{
		"absent runtime root": {
			absent:    true,
			wantError: "open agent identity runtime root",
		},
		"owner directory handoff close": {
			prepare: func(_ string, failure error) error {
				agentIdentityLockCovFaultClose(1, failure)

				return nil
			},
		},
		"authority directory handoff close": {
			prepare: func(_ string, failure error) error {
				agentIdentityLockCovFaultClose(2, failure)

				return nil
			},
		},
		"authority directory is gone": {
			prepare: func(root string, _ error) error {
				authority := filepath.Join(root, "acp-go", "agent-identities")

				return os.Rename(authority, authority+"-moved")
			},
			wantError: "open existing agent identity lock directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			directory, root := agentIdentityLockCovAuthority(t)
			source := agentIdentityLockCovLocked(t, directory, "1250.lock", unix.LOCK_EX)
			failure := errors.New("injected " + name + " failure")
			handed := agentIdentityLockCovDuplicate(t, source)
			adoptRoot := root

			if testCase.absent {
				adoptRoot = filepath.Join(root, "absent")
			}

			if testCase.prepare != nil {
				require.NoError(t, testCase.prepare(root, failure))
			}

			_, err := adoptAgentIdentityLock(handed, 1250, true, adoptRoot)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
			} else {
				require.ErrorIs(t, err, failure)
			}

			require.ErrorIs(t, handed.Close(), os.ErrClosed,
				"a refused adoption must close the descriptor it was handed",
			)
		})
	}
}

// TestAgentIdentityLockDuplicationRefusesUnusableDescriptors proves the
// duplication path never fabricates a descriptor: a missing lock, a released
// lock and a closed descriptor are all refused, while a live lock duplicates
// onto the same inode as an independent descriptor that the holder can also
// hand back as its inherited file.
func TestAgentIdentityLockDuplicationRefusesUnusableDescriptors(t *testing.T) {
	directory, _ := agentIdentityLockCovAuthority(t)

	_, err := duplicateAgentIdentityLock(nil)
	require.ErrorContains(t, err, "agent identity lock descriptor is required")

	_, err = duplicateAgentIdentityLock(agentIdentityLockCovClosed(t))
	require.ErrorIs(t, err, unix.EBADF)

	for name, lock := range map[string]*agentIdentityLock{
		"released lock": {},
		"absent lock":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, duplicateErr := lock.Duplicate()
			require.ErrorContains(t, duplicateErr, "agent identity lock is unavailable")
			require.Nil(t, lock.InheritedFile(), "an unusable lock has no descriptor to inherit")
		})
	}

	source := agentIdentityLockCovLocked(t, directory, "1260.lock", unix.LOCK_EX)
	lock := &agentIdentityLock{file: source}

	duplicate, err := lock.Duplicate()
	require.NoError(t, err)

	defer duplicate.Close()

	require.Same(t, source, lock.InheritedFile(), "the live lock hands back its own descriptor")

	var original, copied unix.Stat_t

	require.NoError(t, unix.Fstat(int(source.Fd()), &original))
	require.NoError(t, unix.Fstat(int(duplicate.Fd()), &copied))
	require.Equal(t, original.Dev, copied.Dev)
	require.Equal(t, original.Ino, copied.Ino)
	require.NotEqual(t, source.Fd(), duplicate.Fd(),
		"the duplicate must be an independent descriptor on the same inode",
	)

	released := &agentIdentityLock{file: agentIdentityLockCovClosed(t)}
	require.ErrorIs(t, released.Close(), os.ErrClosed)
	require.Nil(t, released.InheritedFile(), "a closed lock releases its descriptor reference")

	var absent *agentIdentityLock

	require.NoError(t, absent.Close(), "closing an absent lock is a no-op, not a fault")
}

// TestAdoptAgentIdentityLockRefusesEveryUnprovenHandoff proves the adoption of
// an inherited uid lock re-proves every claim about the descriptor it is
// handed, and closes that descriptor on refusal instead of leaving a lease the
// caller believes was rejected.
func TestAdoptAgentIdentityLockRefusesEveryUnprovenHandoff(t *testing.T) {
	directory, root := agentIdentityLockCovAuthority(t)
	source := agentIdentityLockCovLocked(t, directory, "1261.lock", unix.LOCK_EX)

	agentIdentityLockCovNamedLock(t, directory, "1262.lock")

	_, err := adoptAgentIdentityLock(nil, 1261, false, "")
	require.ErrorContains(t, err, "inherited agent identity lock descriptor is unavailable")

	handed := agentIdentityLockCovDuplicate(t, source)
	_, err = adoptAgentIdentityLock(handed, 1261, true, "")
	require.ErrorContains(t, err, "test agent identity lock root is required")
	require.ErrorIs(t, handed.Close(), os.ErrClosed)

	handed = agentIdentityLockCovDuplicate(t, source)
	_, err = adoptAgentIdentityLock(handed, 1261, false, root)
	require.ErrorContains(t, err, "test agent identity lock root is forbidden")
	require.ErrorIs(t, handed.Close(), os.ErrClosed)

	adopted, err := adoptAgentIdentityLock(agentIdentityLockCovDuplicate(t, source), 1261, true, root)
	require.NoError(t, err, "adopt through the test authority root")
	require.NoError(t, adopted.Close())

	_, err = adoptAgentIdentityLock(agentIdentityLockCovClosed(t), 1261, false, "")
	require.ErrorIs(t, err, unix.EBADF)

	_, err = adoptAgentIdentityLock(agentIdentityLockCovDuplicate(t, source), 1262, false, "")
	require.ErrorContains(t, err, "is not the trusted named lock 1262.lock")

	for name, fault := range map[string]func(error){
		"handed lock metadata": func(failure error) { agentIdentityLockCovFaultFstat(6, failure) },
		"lock inode":           func(failure error) { agentIdentityLockCovFaultFstat(7, failure) },
		"named lock resolution": func(failure error) {
			agentIdentityLockCovFaultFstatat(3, failure)
		},
		"flock state": func(failure error) {
			agentIdentityLockReadFile = func(string) ([]byte, error) { return nil, failure }
		},
		"close on exec flags": func(failure error) {
			agentIdentityLockFcntl = func(uintptr, int, int) (int, error) { return 0, failure }
		},
		"close on exec protection": func(failure error) {
			original := agentIdentityLockFcntl
			calls := 0
			agentIdentityLockFcntl = func(fd uintptr, request, argument int) (int, error) {
				calls++
				if calls == 2 {
					return 0, failure
				}

				return original(fd, request, argument)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			agentIdentityLockCovSeams(t)

			failure := errors.New("injected " + name + " failure")
			handed := agentIdentityLockCovDuplicate(t, source)

			fault(failure)

			_, adoptErr := adoptAgentIdentityLock(handed, 1261, false, "")
			require.ErrorIs(t, adoptErr, failure)
			require.ErrorIs(t, handed.Close(), os.ErrClosed,
				"a refused adoption must close the descriptor it was handed",
			)
		})
	}

	var stat unix.Stat_t

	require.NoError(t, unix.Fstat(int(source.Fd()), &stat))
	require.NoError(t, validateInheritedAgentIdentityFlock(source, stat, "WRITE"),
		"refused adoptions must not have disturbed the host's exclusive lease",
	)
}

// TestInheritedAgentIdentityLockOwnershipProofFailsClosed proves adoption does
// not believe the descriptor's own flock claim: it independently opens the
// trusted named lock and proves a fresh contender is blocked by it. Every way
// that proof can fail to complete — the contender cannot be opened, released,
// described, matched, contended, or is not blocked at all — refuses the
// handoff.
func TestInheritedAgentIdentityLockOwnershipProofFailsClosed(t *testing.T) {
	directory, root := agentIdentityLockCovAuthority(t)
	source := agentIdentityLockCovLocked(t, directory, "1270.lock", unix.LOCK_EX)

	agentIdentityLockCovNamedLock(t, directory, "1271.lock")

	unlocked := agentIdentityLockCovNamedLock(t, directory, "1272.lock")
	unlockedPath := filepath.Join(root, "acp-go", "agent-identities", "1272.lock")
	unlockedFdinfo := agentIdentityLockCovFdinfo(t, unlocked, "WRITE")

	for name, testCase := range map[string]struct {
		uid       uint32
		handed    string
		fault     func(error)
		wantError string
	}{
		"contender cannot be opened": {
			uid: 1270,
			fault: func(failure error) {
				agentIdentityLockOpenat = func(int, string, int, uint32) (int, error) {
					return -1, failure
				}
			},
		},
		"contender descriptor is unusable": {
			uid: 1270,
			fault: func(error) {
				agentIdentityLockOpenat = func(int, string, int, uint32) (int, error) {
					return 999999, nil
				}
			},
			wantError: "close inherited agent identity lock 1270.lock ownership contender",
		},
		"contender inode is not described": {
			uid:   1270,
			fault: func(failure error) { agentIdentityLockCovFaultFstat(9, failure) },
		},
		"contender is a different lock": {
			uid: 1270,
			fault: func(error) {
				original := agentIdentityLockOpenat
				agentIdentityLockOpenat = func(dirfd int, _ string, flags int, mode uint32) (int, error) {
					return original(dirfd, "1271.lock", flags, mode)
				}
			},
			wantError: "ownership contender is not the trusted named lock 1270.lock",
		},
		"contender is not blocked": {
			uid:    1272,
			handed: unlockedPath,
			fault: func(error) {
				agentIdentityLockReadFile = func(string) ([]byte, error) { return unlockedFdinfo, nil }
			},
			wantError: "inherited agent identity lock 1272.lock was not locked before handoff",
		},
		"contender cannot contend": {
			uid: 1270,
			fault: func(error) {
				agentIdentityLockOpenat = func(dirfd int, path string, _ int, _ uint32) (int, error) {
					return unix.Openat(dirfd, path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				}
			},
			wantError: "contend for inherited agent identity lock 1270.lock",
		},
	} {
		t.Run(name, func(t *testing.T) {
			agentIdentityLockCovSeams(t)

			failure := errors.New("injected " + name + " failure")
			handed := agentIdentityLockCovDuplicate(t, source)

			if testCase.handed != "" {
				opened, openErr := os.OpenFile(testCase.handed, os.O_RDWR, 0)
				require.NoError(t, openErr)

				handed = opened

				t.Cleanup(func() { _ = handed.Close() })
			}

			testCase.fault(failure)

			_, adoptErr := adoptAgentIdentityLock(handed, testCase.uid, false, "")
			if testCase.wantError == "" {
				require.ErrorIs(t, adoptErr, failure)

				return
			}

			require.ErrorContains(t, adoptErr, testCase.wantError)
		})
	}

	var stat unix.Stat_t

	require.NoError(t, unix.Fstat(int(source.Fd()), &stat))
	require.NoError(t, validateInheritedAgentIdentityFlock(source, stat, "WRITE"),
		"refused ownership proofs must not have disturbed the host's exclusive lease",
	)
}

// agentIdentityLockCovDomainRecord publishes the running authority domain so a
// domain lock handoff can be adopted end to end.
func agentIdentityLockCovDomainRecord(t *testing.T, directory *os.File, root string) {
	t.Helper()

	record, err := currentAgentAuthorityDomain(directory)
	require.NoError(t, err)

	record.AuthorityID = agentAuthorityDomainCovID

	payload, err := json.Marshal(record)
	require.NoError(t, err)

	path := filepath.Join(root, "acp-go", "agent-identities", agentAuthorityDomainRecordName)
	require.NoError(t, os.WriteFile(path, append(payload, '\n'), 0o600))
}

// TestAdoptAgentAuthorityDomainRefusesEveryUnprovenHandoff proves the shared
// authority domain lease is adopted only when the descriptor is the trusted
// named domain.lock and is really holding a lease that blocks an exclusive
// contender, and that any step the kernel cannot complete refuses the handoff
// and closes the descriptor.
func TestAdoptAgentAuthorityDomainRefusesEveryUnprovenHandoff(t *testing.T) {
	directory, root := agentIdentityLockCovAuthority(t)
	agentIdentityLockCovDomainRecord(t, directory, root)

	source := agentIdentityLockCovLocked(t, directory, agentAuthorityDomainLockName, unix.LOCK_SH)
	foreign := agentIdentityLockCovLocked(t, directory, "1280.lock", unix.LOCK_SH)

	_, err := adoptAgentAuthorityDomain(nil, false, "")
	require.ErrorContains(t, err, "inherited agent authority domain descriptor is unavailable")

	handed := agentIdentityLockCovDuplicate(t, source)
	_, err = adoptAgentAuthorityDomain(handed, true, "")
	require.ErrorContains(t, err, "test agent identity lock root is required")
	require.ErrorIs(t, handed.Close(), os.ErrClosed)

	handed = agentIdentityLockCovDuplicate(t, source)
	_, err = adoptAgentAuthorityDomain(handed, false, root)
	require.ErrorContains(t, err, "test agent identity lock root is forbidden")
	require.ErrorIs(t, handed.Close(), os.ErrClosed)

	adopted, err := adoptAgentAuthorityDomain(agentIdentityLockCovDuplicate(t, source), true, root)
	require.NoError(t, err, "adopt through the test authority root")
	require.NoError(t, adopted.Close())

	_, err = adoptAgentAuthorityDomain(agentIdentityLockCovClosed(t), false, "")
	require.ErrorIs(t, err, unix.EBADF)

	_, err = adoptAgentAuthorityDomain(agentIdentityLockCovDuplicate(t, foreign), false, "")
	require.ErrorContains(t, err, "is not the trusted named domain.lock")

	for name, testCase := range map[string]struct {
		fault     func(error)
		wantError string
	}{
		"handed domain metadata": {
			fault: func(failure error) { agentIdentityLockCovFaultFstat(6, failure) },
		},
		"domain inode": {
			fault: func(failure error) { agentIdentityLockCovFaultFstat(7, failure) },
		},
		"named domain resolution": {
			fault: func(failure error) { agentIdentityLockCovFaultFstatat(3, failure) },
		},
		"contender cannot be opened": {
			fault: func(failure error) {
				agentIdentityLockOpenat = func(int, string, int, uint32) (int, error) {
					return -1, failure
				}
			},
		},
		"contender cannot contend": {
			fault: func(error) {
				agentIdentityLockOpenat = func(dirfd int, path string, _ int, _ uint32) (int, error) {
					return unix.Openat(dirfd, path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
				}
			},
			wantError: "contend for inherited agent authority domain",
		},
		"contender cannot be released": {
			fault: func(failure error) {
				original := agentIdentityLockCloseFD
				agentIdentityLockCloseFD = func(fd int) error {
					_ = original(fd)

					return failure
				}
			},
		},
		"close on exec flags": {
			fault: func(failure error) {
				agentIdentityLockFcntl = func(uintptr, int, int) (int, error) { return 0, failure }
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			agentIdentityLockCovSeams(t)

			failure := errors.New("injected " + name + " failure")
			handed := agentIdentityLockCovDuplicate(t, source)

			testCase.fault(failure)

			_, adoptErr := adoptAgentAuthorityDomain(handed, false, "")
			if testCase.wantError != "" {
				require.ErrorContains(t, adoptErr, testCase.wantError)
			} else {
				require.ErrorIs(t, adoptErr, failure)
			}

			require.ErrorIs(t, handed.Close(), os.ErrClosed,
				"a refused adoption must close the descriptor it was handed",
			)
		})
	}
}

// TestAdoptAgentAuthorityDomainRefusesAnUnheldLease proves the domain handoff
// is refused when the descriptor's own fdinfo claims a shared lease that is not
// really held: the adoption contends for the named domain lock itself, and an
// exclusive contender that succeeds is proof no lease was handed over.
func TestAdoptAgentAuthorityDomainRefusesAnUnheldLease(t *testing.T) {
	directory, root := agentIdentityLockCovAuthority(t)
	agentIdentityLockCovDomainRecord(t, directory, root)

	unlocked := agentIdentityLockCovNamedLock(t, directory, agentAuthorityDomainLockName)
	require.NoError(t, unlocked.Close())

	handed, err := os.OpenFile(
		filepath.Join(root, "acp-go", "agent-identities", agentAuthorityDomainLockName), os.O_RDWR, 0,
	)
	require.NoError(t, err)

	t.Cleanup(func() { _ = handed.Close() })

	payload := agentIdentityLockCovFdinfo(t, handed, "READ")
	agentIdentityLockReadFile = func(string) ([]byte, error) { return payload, nil }

	_, err = adoptAgentAuthorityDomain(handed, false, "")
	require.ErrorContains(t, err, "was not locked before handoff")
}

// agentIdentityLockCovOwnerRecord seals a well formed permanent owner binding
// for uid under the authority root, together with the permanent UID lock and
// owners.lock that binding is only allowed to exist behind.
func agentIdentityLockCovOwnerRecord(t *testing.T, directory *os.File, root string, uid, gid uint32) {
	t.Helper()

	owner := agentStandaloneOwner{
		Version:  1,
		UID:      uid,
		GID:      gid,
		Kind:     agentStandaloneOwnerKind,
		Provider: agentStandaloneOwnerID,
		OwnerID:  "borrowed-collision",
		StateRoot: agentStandaloneStateRoot{
			Path: "/srv/opencode/borrowed-collision", Dev: 101, Ino: 102,
		},
	}

	payload, err := json.Marshal(owner)
	require.NoError(t, err)

	name := strconv.FormatUint(uint64(uid), 10)

	agentIdentityLockCovNamedLock(t, directory, name+".lock")
	agentIdentityLockCovNamedLock(t, directory, "owners.lock")

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "acp-go", "agent-identities", name+".owner"),
		append(payload, '\n'), 0o600,
	))
}

// TestBorrowedAgentIdentityDispositionRefusesUnprovenModes proves the borrowed
// disposition check only runs against the authority root it was told to use,
// refuses when the authority root cannot be enumerated or the owner binding
// cannot be resolved rather than reading either as "nothing found", and never
// borrows a uid that already carries a permanent standalone owner binding.
func TestBorrowedAgentIdentityDispositionRefusesUnprovenModes(t *testing.T) {
	const (
		uid = uint32(62461)
		gid = uint32(62462)
	)

	t.Run("test root is required", func(t *testing.T) {
		agentIdentityLockCovAuthority(t)
		require.ErrorContains(t, validateBorrowedAgentIdentityDisposition(uid, gid, true, ""),
			"test agent identity lock root is required",
		)
	})

	t.Run("test root is forbidden", func(t *testing.T) {
		_, root := agentIdentityLockCovAuthority(t)
		require.ErrorContains(t, validateBorrowedAgentIdentityDisposition(uid, gid, false, root),
			"test agent identity lock root is forbidden",
		)
	})

	t.Run("owner binding cannot be resolved", func(t *testing.T) {
		_, root := agentIdentityLockCovAuthority(t)
		failure := errors.New("injected owner resolution failure")

		agentIdentityLockCovFaultFstatat(3, failure)
		require.ErrorIs(t, validateBorrowedAgentIdentityDisposition(uid, gid, true, root), failure)
	})

	t.Run("permanently bound uid cannot be borrowed", func(t *testing.T) {
		directory, root := agentIdentityLockCovAuthority(t)
		agentIdentityLockCovOwnerRecord(t, directory, root, uid, gid)
		require.ErrorContains(t, validateBorrowedAgentIdentityDisposition(uid, gid, true, root),
			"has a permanent owner binding",
		)
	})

	t.Run("authority root cannot be enumerated", func(t *testing.T) {
		agentIdentityLockCovSeams(t)

		root := configureAgentIdentityLockTestRoot(t)

		directory, err := bootstrapAgentIdentityLockDirectory(
			root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
		)
		require.NoError(t, err)
		require.NoError(t, directory.Close())
		require.ErrorIs(t, rejectAgentIdentityDispositionTemporaries(directory, uid), unix.EBADF)
	})
}

// TestRejectAgentIdentityDispositionTemporariesScopesByUID proves the
// disposition scan refuses only the temporaries that are the scanning uid's own
// fault. A uid-scoped temporary named for another participant is that
// participant's in-flight atomic write and must be tolerated; the scanning uid's
// own unresolved temporary of each class still refuses; a malformed name is
// fatal; and a domain-global temporary is transient by construction, so it is
// absorbed by a bounded re-read and refused only once it persists.
func TestRejectAgentIdentityDispositionTemporariesScopesByUID(t *testing.T) {
	const (
		owner     = uint32(63700)
		bystander = uint32(995)
		suffix    = "0123456789abcdef01234567"
	)

	directory, root := agentIdentityLockCovAuthority(t)
	authority := filepath.Join(root, "acp-go", "agent-identities")

	stage := func(t *testing.T, name string) string {
		t.Helper()

		path := filepath.Join(authority, name)
		require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
		t.Cleanup(func() {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, unix.ENOENT) {
				t.Errorf("remove staged temporary %q: %v", name, removeErr)
			}
		})

		return name
	}

	for _, name := range []string{
		strconv.FormatUint(uint64(bystander), 10) + ".quarantine.next-" + suffix,
		strconv.FormatUint(uint64(bystander), 10) + ".owner.next-" + suffix,
	} {
		t.Run("tolerates bystander "+name, func(t *testing.T) {
			staged := stage(t, name)
			require.NoErrorf(t, rejectAgentIdentityDispositionTemporaries(directory, owner),
				"bystander temporary %q refused, want tolerated", staged,
			)
		})
	}

	for _, name := range []string{
		strconv.FormatUint(uint64(owner), 10) + ".quarantine.next-" + suffix,
		strconv.FormatUint(uint64(owner), 10) + ".owner.next-" + suffix,
	} {
		t.Run("refuses own "+name, func(t *testing.T) {
			staged := stage(t, name)
			require.ErrorContains(t, rejectAgentIdentityDispositionTemporaries(directory, owner),
				"unresolved temporary",
			)
			require.ErrorContains(t, rejectAgentIdentityDispositionTemporaries(directory, owner), staged)
		})
	}

	for _, name := range []string{
		"bad.quarantine.next-" + suffix,
		"bad.owner.next-" + suffix,
	} {
		t.Run("refuses malformed "+name, func(t *testing.T) {
			staged := stage(t, name)
			require.Errorf(t, rejectAgentIdentityDispositionTemporaries(directory, owner),
				"malformed temporary %q was tolerated", staged,
			)
		})
	}

	for _, name := range []string{
		"domain.json.next-" + suffix,
		".authority-probe-" + suffix,
	} {
		t.Run("refuses persistent domain-global "+name, func(t *testing.T) {
			staged := stage(t, name)
			started := time.Now()
			require.ErrorContains(t, rejectAgentIdentityDispositionTemporaries(directory, owner),
				"unresolved temporary",
			)
			require.ErrorContains(t, rejectAgentIdentityDispositionTemporaries(directory, owner), staged)
			require.GreaterOrEqualf(t, time.Since(started), agentIdentityDispositionTemporaryReadDelay,
				"domain-global refusal skipped the bounded re-read",
			)
		})
	}

	t.Run("absorbs a domain-global rename in flight", func(t *testing.T) {
		staged := stage(t, "domain.json.next-"+suffix)
		go func() {
			time.Sleep(agentIdentityDispositionTemporaryReadDelay)
			_ = os.Remove(filepath.Join(authority, staged))
		}()
		require.NoError(t, rejectAgentIdentityDispositionTemporaries(directory, owner))
	})
}

// agentIdentityLockCovProtectedStateRoot creates the uid:gid-owned mode-0700
// directory a standalone owner binding is allowed to name as its state root.
func agentIdentityLockCovProtectedStateRoot(t *testing.T, uid, gid uint32) string {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("a protected standalone state root requires root to hand it to another identity")
	}

	stateRoot := filepath.Join(t.TempDir(), "state")

	require.NoError(t, os.Mkdir(stateRoot, 0o700))
	require.NoError(t, os.Chown(stateRoot, int(uid), int(gid)))
	require.NoError(t, os.Chmod(stateRoot, 0o700))

	return stateRoot
}

// TestAdoptedStandaloneDispositionRefusesEveryUnprovenAuthority proves the
// supervisor's re-admission of an adopted standalone identity is refused unless
// the state root still binds, the authority root opens, holds no unresolved
// temporary, audits clean, and still carries the exact owner binding. Each
// refusal is proved by the stage it belongs to, so a later stage can never
// stand in for an earlier one that was never reached.
func TestAdoptedStandaloneDispositionRefusesEveryUnprovenAuthority(t *testing.T) {
	const (
		uid = uint32(62481)
		gid = uint32(62482)
	)

	authority := func(t *testing.T) (string, string) {
		t.Helper()

		stateRoot := agentIdentityLockCovProtectedStateRoot(t, uid, gid)
		_, root := agentIdentityLockCovAuthority(t)

		return root, stateRoot
	}

	t.Run("state root does not bind", func(t *testing.T) {
		root, _ := authority(t)
		require.ErrorContains(t, validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "adopted", t.TempDir(), true, root,
		), "claimed UID:GID-owned mode-0700")
	})

	t.Run("test root is required", func(t *testing.T) {
		_, stateRoot := authority(t)
		require.ErrorContains(t, validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "adopted", stateRoot, true, "",
		), "test agent identity lock root is required")
	})

	t.Run("test root is forbidden", func(t *testing.T) {
		root, stateRoot := authority(t)
		require.ErrorContains(t, validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "adopted", stateRoot, false, root,
		), "test agent identity lock root is forbidden")
	})

	t.Run("authority holds an unresolved temporary", func(t *testing.T) {
		root, stateRoot := authority(t)
		require.NoError(t, os.WriteFile(filepath.Join(
			root, "acp-go", "agent-identities", "domain.json.next-0123456789abcdef01234567",
		), []byte("{}\n"), 0o600))
		require.ErrorContains(t, validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "adopted", stateRoot, true, root,
		), "unresolved temporary")
	})

	t.Run("authority holds an unaccountable entry", func(t *testing.T) {
		root, stateRoot := authority(t)
		require.NoError(t, os.WriteFile(
			filepath.Join(root, "acp-go", "agent-identities", "stray"), []byte("stray\n"), 0o600,
		))
		require.ErrorContains(t, validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "adopted", stateRoot, true, root,
		), `unknown entry "stray"`)
	})

	t.Run("owner binding is absent", func(t *testing.T) {
		root, stateRoot := authority(t)
		require.ErrorIs(t, validateAdoptedStandaloneAgentIdentityDisposition(
			uid, gid, "adopted", stateRoot, true, root,
		), unix.ENOENT)
	})
}

// TestLinuxSupervisorAdoptedAuthorityRoutesStandaloneToItsOwnProof proves the
// supervisor's authority check dispatches a standalone configuration to the
// standalone disposition proof rather than the borrowed one, so a standalone
// agent is never re-admitted on the weaker ownerless-ACTIVE contract.
func TestLinuxSupervisorAdoptedAuthorityRoutesStandaloneToItsOwnProof(t *testing.T) {
	const (
		uid = uint32(62491)
		gid = uint32(62492)
	)

	err := validateLinuxSupervisorAdoptedAuthority(supervisorConfig{
		IsolationUID:        uid,
		IsolationGID:        gid,
		StandaloneOwnerID:   "supervisor-adopted",
		StandaloneStateRoot: t.TempDir(),
		StandaloneAuthority: true,
	})
	require.ErrorContains(t, err, "claimed UID:GID-owned mode-0700",
		"only the standalone proof binds a state root, so this refusal names the branch taken",
	)
}

// TestInheritedAgentIdentityFlockLineRejectsMalformedState proves the flock
// entry a descriptor reports must be a fully parseable advisory record: a
// non-numeric sequence, a negative or non-numeric owner, and an inode identity
// that is not exactly major:minor:inode are all refused.
func TestInheritedAgentIdentityFlockLineRejectsMalformedState(t *testing.T) {
	descriptor := unix.Stat_t{Dev: unix.Mkdev(0, 0x26), Ino: 52599113}
	valid := strings.Fields("lock: 1: FLOCK ADVISORY WRITE 0 00:26:52599113 0 EOF")

	require.NoError(t, validateInheritedAgentIdentityFlockLine(valid, descriptor, "WRITE"))

	for name, testCase := range map[string]struct {
		index     int
		value     string
		wantError string
	}{
		"sequence is not numeric":     {index: 1, value: "zz:", wantError: "malformed flock sequence"},
		"owner is not numeric":        {index: 5, value: "owner", wantError: "malformed flock owner"},
		"owner is negative":           {index: 5, value: "-1", wantError: "malformed flock owner"},
		"inode identity is truncated": {index: 6, value: "00:26", wantError: "malformed flock inode"},
	} {
		t.Run(name, func(t *testing.T) {
			fields := append([]string(nil), valid...)
			fields[testCase.index] = testCase.value
			require.ErrorContains(t,
				validateInheritedAgentIdentityFlockLine(fields, descriptor, "WRITE"), testCase.wantError,
			)
		})
	}
}
