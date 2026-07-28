package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// BrokerLeaseFileName is the lease a broker server writes inside its own home.
// It is keyed by that home and never by the runtime key: concurrent pending
// flows each own a distinct home, and one shared file would have every reaper
// verify the wrong PID and kill the wrong server.
const BrokerLeaseFileName = "acp-go-opencode-broker.lease.json"

const brokerLeaseFileMode = 0o600

// brokerLeaseReapTimeout bounds how long the reaper waits for a leased process
// to leave after each rung of the shutdown ladder.
var brokerLeaseReapTimeout = 3 * time.Second

// brokerLeaseReapPoll is the interval between liveness checks while waiting.
var brokerLeaseReapPoll = 20 * time.Millisecond

// BrokerLease records what a later startup needs to tell an abandoned broker
// home from a live one. Without it a crashed adapter leaks an orphan server
// still holding a pending flow — and, after native completion, a live refresh
// token in a directory no ledger entry names.
type BrokerLease struct {
	PID          int    `json:"pid"`
	Port         int    `json:"port"`
	StartedAt    int64  `json:"startedAtUnixMilli"`
	IdentityHash string `json:"identityHash"`
	// ProcessStart is the leased process's own start time, which is what
	// separates the process this lease named from an unrelated one that reused
	// its PID.
	ProcessStart string `json:"processStartTime,omitempty"`
	// OwnerPID and OwnerStart identify the adapter process that created the
	// home. A home whose owner is still running belongs to a live flow, and a
	// reaper that killed it would terminate a concurrent pending login rather
	// than an orphan.
	OwnerPID   int    `json:"ownerPid"`
	OwnerStart string `json:"ownerProcessStartTime,omitempty"`
}

var (
	leaseMarshal     = json.Marshal
	leaseWriteFile   = os.WriteFile
	leaseReadFile    = os.ReadFile
	leaseStartTime   = processStartTime
	leaseGetpid      = os.Getpid
	leaseNow         = time.Now
	leaseSleep       = time.Sleep
	leaseSignalGroup = signalLeasedProcessGroup
)

// BrokerIdentityHash binds the lease to the credentials the leased server
// authenticates with, so the lease names one server rather than a port number
// anything could be listening on. The secret itself never enters the file.
func BrokerIdentityHash(username string, password string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + password))

	return hex.EncodeToString(sum[:])
}

// newBrokerLease captures the adapter's own identity beside the server's. It is
// called before the server starts, when the port and the credentials are
// already chosen and the PID is not yet known.
func newBrokerLease(port int, identityHash string) BrokerLease {
	owner := leaseGetpid()
	lease := BrokerLease{
		Port:         port,
		StartedAt:    leaseNow().UnixMilli(),
		IdentityHash: identityHash,
		OwnerPID:     owner,
	}

	if start, err := leaseStartTime(owner); err == nil {
		lease.OwnerStart = start
	}

	return lease
}

func brokerLeasePath(home string) string {
	return filepath.Join(home, BrokerLeaseFileName)
}

func writeBrokerLease(home string, lease BrokerLease) error {
	contents, err := leaseMarshal(lease)
	if err != nil {
		return fmt.Errorf("encode broker lease: %w", err)
	}

	if err := leaseWriteFile(brokerLeasePath(home), contents, brokerLeaseFileMode); err != nil {
		return fmt.Errorf("write broker lease: %w", err)
	}

	return nil
}

// adoptBrokerLease records the PID the spawn produced against the lease that
// was already on disk before it. Until it lands the lease names no process,
// which is correct: nothing had been started to name.
func adoptBrokerLease(home string, lease BrokerLease, pid int) error {
	lease.PID = pid

	if start, err := leaseStartTime(pid); err == nil {
		lease.ProcessStart = start
	}

	return writeBrokerLease(home, lease)
}

func readBrokerLease(home string) (BrokerLease, bool) {
	contents, err := leaseReadFile(brokerLeasePath(home))
	if err != nil {
		return BrokerLease{}, false
	}

	var lease BrokerLease
	if err := json.Unmarshal(contents, &lease); err != nil {
		return BrokerLease{}, false
	}

	return lease, true
}

// leasedProcessLives reports whether the exact process the lease named is still
// running. A process whose start time cannot be read is reported as gone: this
// decides whether to signal, and signalling something that cannot be identified
// is how a reaper kills an unrelated process that reused a PID.
func leasedProcessLives(pid int, start string) bool {
	if pid <= 0 || start == "" {
		return false
	}

	current, err := leaseStartTime(pid)
	if err != nil {
		return false
	}

	return current == start
}

// reapBrokerLease terminates the orphan server an abandoned home still holds.
// It returns whether the home is safe to remove: a home whose owner is still
// running belongs to a live flow, and one whose orphan survived the ladder
// keeps its lease so the next startup retries.
func reapBrokerLease(lease BrokerLease) (bool, error) {
	// An owner whose identity could not be recorded leaves abandonment
	// unestablished, and an unestablished answer never authorizes a kill.
	if lease.OwnerStart == "" || leasedProcessLives(lease.OwnerPID, lease.OwnerStart) {
		return false, nil
	}

	if !leasedProcessLives(lease.PID, lease.ProcessStart) {
		return true, nil
	}

	if err := terminateLeasedProcess(lease); err != nil {
		return false, err
	}

	return true, nil
}

// terminateLeasedProcess runs the shutdown ladder against the identified orphan
// and verifies it is gone. The broker process leads its own group, so the group
// is the boundary the native server sits inside.
func terminateLeasedProcess(lease BrokerLease) error {
	if err := leaseSignalGroup(lease.PID, false); err != nil {
		return fmt.Errorf("terminate orphan broker server %d: %w", lease.PID, err)
	}

	if waitLeasedProcessGone(lease) {
		return nil
	}

	if err := leaseSignalGroup(lease.PID, true); err != nil {
		return fmt.Errorf("kill orphan broker server %d: %w", lease.PID, err)
	}

	if waitLeasedProcessGone(lease) {
		return nil
	}

	return fmt.Errorf("orphan broker server %d survived termination", lease.PID)
}

func waitLeasedProcessGone(lease BrokerLease) bool {
	deadline := leaseNow().Add(brokerLeaseReapTimeout)

	for {
		if !leasedProcessLives(lease.PID, lease.ProcessStart) {
			return true
		}

		if !leaseNow().Before(deadline) {
			return false
		}

		leaseSleep(brokerLeaseReapPoll)
	}
}

// leasePendingServer writes the lease that names the server about to start. It
// lands before the spawn, so no window exists in which a server runs under a
// home that names nothing.
func leasePendingServer(
	options StartOptions,
	port int,
	username string,
	password string,
	cancel context.CancelFunc,
) (BrokerLease, error) {
	lease := newBrokerLease(port, BrokerIdentityHash(username, password))
	if options.LeaseDir == "" {
		return lease, nil
	}

	if err := writeBrokerLease(options.LeaseDir, lease); err != nil {
		cancel()

		return lease, err
	}

	return lease, nil
}

// leaseStartedServer records the spawned PID against the lease already on disk.
// A server whose lease cannot name it is torn down rather than left running
// under a home no later startup can identify.
func leaseStartedServer(
	options StartOptions,
	lease BrokerLease,
	process *os.Process,
	control io.Closer,
	cancel context.CancelFunc,
) error {
	if options.LeaseDir == "" {
		return nil
	}

	err := adoptBrokerLease(options.LeaseDir, lease, process.Pid)
	if err == nil {
		return nil
	}

	_ = killOpenCodeProcess(process, process.Pid)

	if control != nil {
		_ = control.Close()
	}

	cancel()

	return err
}
