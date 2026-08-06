//go:build linux

package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

// agentStandaloneCovOwner builds a well-formed standalone owner tuple so a case
// only has to state the field it wants to collide.
func agentStandaloneCovOwner(uid, gid uint32, ownerID, stateRoot string, dev, ino uint64) agentStandaloneOwner {
	return agentStandaloneOwner{
		Version: 1, UID: uid, GID: gid, Kind: agentStandaloneOwnerKind,
		Provider: agentStandaloneOwnerID, OwnerID: ownerID,
		StateRoot: agentStandaloneStateRoot{Path: stateRoot, Dev: dev, Ino: ino},
	}
}

// agentStandaloneCovStaticOwner is the tuple most owner-identity cases claim.
// Its state root is a plausible bound inode rather than a real directory,
// because every case here settles the domain before the state root is revalidated.
func agentStandaloneCovStaticOwner(uid, gid uint32, ownerID string) agentStandaloneOwner {
	return agentStandaloneCovOwner(uid, gid, ownerID, "/srv/opencode/"+ownerID, 101, 102)
}

// agentStandaloneCovPermanentLock creates the permanent registry lock a claim
// expects to already exist, and hands back nothing, because the claim opens its
// own descriptor on it.
func agentStandaloneCovPermanentLock(t *testing.T, directory *os.File, name string) {
	t.Helper()
	lock := createAgentStandaloneTestLock(t, directory, name, uint32(os.Geteuid()), uint32(os.Getegid()))
	require.NoError(t, lock.Close())
}

// agentStandaloneCovPristineDomainFixture stages a registry that has its
// permanent domain lock but has never published an authority record.
func agentStandaloneCovPristineDomainFixture(t *testing.T) (*os.File, uint32, uint32) {
	t.Helper()
	directory := openAgentStandaloneTestDirectory(t)
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	agentStandaloneCovPermanentLock(t, directory, "domain.lock")

	return directory, ownerUID, ownerGID
}

// agentStandaloneCovHoldDomainShared takes a shared lease on the permanent
// domain lock, which lets other shared readers in but blocks any contender
// that needs the exclusive lease.
func agentStandaloneCovHoldDomainShared(t *testing.T, directory *os.File) *os.File {
	t.Helper()
	ownerUID, ownerGID := agentStandaloneTestAuthorityIDs()
	held, err := openAgentStandaloneNamedLock(directory, "domain.lock", false, ownerUID, ownerGID)
	require.NoError(t, err)
	require.NoError(t, unix.Flock(int(held.Fd()), unix.LOCK_SH|unix.LOCK_NB))

	return held
}

// agentStandaloneCovDomainRecordImage is the published record as bytes plus the
// inode it lives on, which together are what "the peer's record survived" means:
// an adopting claim must neither rewrite the payload nor rename a fresh file
// over it.
type agentStandaloneCovDomainRecordImage struct {
	payload []byte
	dev     uint64
	ino     uint64
}

func agentStandaloneCovReadDomainRecordImage(directory *os.File) (agentStandaloneCovDomainRecordImage, error) {
	payload, readErr := os.ReadFile(filepath.Join(directory.Name(), "domain.json"))
	if readErr != nil {
		return agentStandaloneCovDomainRecordImage{}, readErr
	}

	var stat unix.Stat_t
	if statErr := unix.Fstatat(
		int(directory.Fd()), "domain.json", &stat, unix.AT_SYMLINK_NOFOLLOW,
	); statErr != nil {
		return agentStandaloneCovDomainRecordImage{}, statErr
	}

	return agentStandaloneCovDomainRecordImage{payload: payload, dev: stat.Dev, ino: stat.Ino}, nil
}

// TestAgentStandaloneCovDomainAcquisitionRereadsUnderTheExclusiveLease proves
// the claim decides on the record that is present once it holds the exclusive
// lease, not on the record it read before queueing for that lease, and that
// what it does with the winner depends on whether the winner describes this
// very domain.
func TestAgentStandaloneCovDomainAcquisitionRereadsUnderTheExclusiveLease(t *testing.T) {
	// A peer that publishes an authority record for this very domain while we
	// queue for the exclusive lease has minted the authority we were about to
	// mint ourselves, so the claim adopts that record rather than replacing it:
	// the file is left byte-for-byte on the same inode, the adopted authority id
	// is the peer's, and the lease handed back is downgraded to shared the way
	// every adopting branch does.
	t.Run("peer publishes a matching record while we queue", func(t *testing.T) {
		directory, ownerUID, ownerGID := agentStandaloneCovPristineDomainFixture(t)
		want := agentStandaloneCovStaticOwner(62903, 62904, "queued")
		record, err := currentAgentAuthorityDomain(directory)
		require.NoError(t, err)
		record.AuthorityID = "0123456789abcdef0123456789abcdef"
		held := agentStandaloneCovHoldDomainShared(t, directory)
		published := make(chan struct{})

		var image agentStandaloneCovDomainRecordImage

		go func() {
			time.Sleep(60 * time.Millisecond)
			publishErr := replaceAgentStandaloneDomainRecord(directory, ownerUID, ownerGID, record)

			var imageErr error
			if publishErr == nil {
				image, imageErr = agentStandaloneCovReadDomainRecordImage(directory)
			}

			closeErr := held.Close()
			if publishErr != nil || imageErr != nil || closeErr != nil {
				panic(errors.Join(publishErr, imageErr, closeErr))
			}

			close(published)
		}()

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, true, time.Now().Add(5*time.Second), nil, nil,
		)
		<-published
		require.NoError(t, err)
		require.NotNil(t, lease)

		defer func() { require.NoError(t, lease.Close()) }()

		reread, err := loadAgentAuthorityDomainRecord(directory, ownerUID, ownerGID)
		require.NoError(t, err)
		require.Equal(t, record.AuthorityID, reread.AuthorityID,
			"the adopting claim must adopt the peer's authority id, not mint its own",
		)
		after, err := agentStandaloneCovReadDomainRecordImage(directory)
		require.NoError(t, err)
		require.Equal(t, image.payload, after.payload,
			"the adopting claim must leave the peer's record byte-identical",
		)
		require.Equal(t, [2]uint64{image.dev, image.ino}, [2]uint64{after.dev, after.ino},
			"the adopting claim must leave the peer's record on its own inode, not rename a fresh one over it",
		)
		contender, err := openAgentStandaloneNamedLock(directory, "domain.lock", false, ownerUID, ownerGID)
		require.NoError(t, err)
		require.NoError(t, unix.Flock(int(contender.Fd()), unix.LOCK_SH|unix.LOCK_NB),
			"the adopted lease must be shared, so peers on the same authority may hold it too",
		)
		require.ErrorIs(t, unix.Flock(int(contender.Fd()), unix.LOCK_EX|unix.LOCK_NB), unix.EWOULDBLOCK,
			"the adopted lease must still exclude a contender that wants the domain to itself",
		)
		require.NoError(t, contender.Close())
	})

	// Adoption downgrades the exclusive lease to shared and only then reads the
	// record back, so a peer holding the same shared lease can still replace it
	// inside that window. The read-back is the only thing standing between that
	// peer and a lease handed out for an authority this claim never saw.
	t.Run("peer replaces the adopted record in the shared-lease window", func(t *testing.T) {
		directory, ownerUID, ownerGID := agentStandaloneCovPristineDomainFixture(t)
		want := agentStandaloneCovStaticOwner(62913, 62914, "adopted")
		record, err := currentAgentAuthorityDomain(directory)
		require.NoError(t, err)
		record.AuthorityID = "0123456789abcdef0123456789abcdef"
		successor := record
		successor.AuthorityID = "fedcba9876543210fedcba9876543210"
		held := agentStandaloneCovHoldDomainShared(t, directory)
		published := make(chan struct{})

		go func() {
			time.Sleep(60 * time.Millisecond)
			publishErr := replaceAgentStandaloneDomainRecord(directory, ownerUID, ownerGID, record)
			closeErr := held.Close()

			if publishErr != nil || closeErr != nil {
				panic(errors.Join(publishErr, closeErr))
			}

			close(published)
		}()

		// Only the lease downgrade flocks bare LOCK_SH; every acquisition on this
		// path adds LOCK_NB, so this lands the peer in the downgrade window and
		// nowhere else.
		previous := agentStandaloneFlock
		t.Cleanup(func() { agentStandaloneFlock = previous })

		replaced := false
		agentStandaloneFlock = func(fd, how int) error {
			if how == unix.LOCK_SH && !replaced {
				replaced = true

				require.NoError(t, replaceAgentStandaloneDomainRecord(directory, ownerUID, ownerGID, successor))
			}

			return previous(fd, how)
		}

		lease, err := acquireAgentStandaloneDomain(
			directory, want, ownerUID, ownerGID, true, time.Now().Add(5*time.Second), nil, nil,
		)
		<-published
		require.Nil(t, lease)
		require.ErrorContains(t, err, "changed during shared-lease transition")
		require.True(t, replaced, "the peer never reached the shared-lease window")
		reread, err := loadAgentAuthorityDomainRecord(directory, ownerUID, ownerGID)
		require.NoError(t, err)
		require.Equal(t, successor.AuthorityID, reread.AuthorityID,
			"the refusal must leave the peer's replacement in place",
		)
		contender, acquired, err := tryAgentStandaloneNamedLock(directory, "domain.lock", false, ownerUID, ownerGID)
		require.NoError(t, err)
		require.True(t, acquired, "the refused claim must release the domain lock")
		require.NoError(t, contender.Close())
	})
}
