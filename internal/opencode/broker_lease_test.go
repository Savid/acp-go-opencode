package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restoreLeaseHooks restores every injectable lease seam when the test ends.
func restoreLeaseHooks(t *testing.T) {
	t.Helper()

	write, read := leaseWriteFile, leaseReadFile
	start, getpid := leaseStartTime, leaseGetpid
	now, sleep, signal := leaseNow, leaseSleep, leaseSignalGroup
	timeout, poll := brokerLeaseReapTimeout, brokerLeaseReapPoll

	t.Cleanup(func() {
		leaseWriteFile, leaseReadFile = write, read
		leaseStartTime, leaseGetpid = start, getpid
		leaseNow, leaseSleep, leaseSignalGroup = now, sleep, signal
		brokerLeaseReapTimeout, brokerLeaseReapPoll = timeout, poll
	})
}

// TestProcessStartTimeIdentifiesThisProcess pins the one fact the reaper's kill
// decision rests on: a live PID answers with a stable start time, and a PID
// nothing owns answers with nothing at all.
func TestProcessStartTimeIdentifiesThisProcess(t *testing.T) {
	start, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("start time: %v", err)
	}

	if start == "" {
		t.Fatal("a live process reported an empty start time")
	}

	again, err := processStartTime(os.Getpid())
	if err != nil || again != start {
		t.Fatalf("start time is not stable: %q then %q (%v)", start, again, err)
	}

	if _, err := processStartTime(0); err == nil {
		t.Fatal("pid 0 reported a start time")
	}
}

// TestBrokerLeaseLandsBeforeTheServerAndAdoptsItsPID pins the ordering the
// whole rule exists for: the file naming the port and the credentials is on
// disk before anything is spawned, and the PID joins it as soon as one exists.
func TestBrokerLeaseLandsBeforeTheServerAndAdoptsItsPID(t *testing.T) {
	restoreLeaseHooks(t)

	home := t.TempDir()
	identity := BrokerIdentityHash("opencode", "secret")

	lease := newBrokerLease(4321, identity)
	if err := writeBrokerLease(home, lease); err != nil {
		t.Fatalf("write lease: %v", err)
	}

	written, ok := readBrokerLease(home)
	if !ok {
		t.Fatal("the lease was not readable")
	}

	if written.PID != 0 {
		t.Fatalf("a lease written before the spawn named pid %d", written.PID)
	}

	if written.Port != 4321 || written.IdentityHash != identity || written.StartedAt == 0 {
		t.Fatalf("lease = %+v", written)
	}

	if written.OwnerPID != os.Getpid() || written.OwnerStart == "" {
		t.Fatalf("lease owner = %d/%q", written.OwnerPID, written.OwnerStart)
	}

	// The secret itself is never a lease field, only the hash that binds the
	// lease to it.
	contents, err := os.ReadFile(filepath.Join(home, BrokerLeaseFileName))
	if err != nil {
		t.Fatalf("read lease file: %v", err)
	}

	if string(contents) == "" || strings.Contains(string(contents), "secret") {
		t.Fatal("the lease carried the server credential")
	}

	info, err := os.Stat(filepath.Join(home, BrokerLeaseFileName))
	if err != nil {
		t.Fatalf("stat lease: %v", err)
	}

	if info.Mode().Perm() != brokerLeaseFileMode {
		t.Fatalf("lease mode = %v", info.Mode().Perm())
	}

	if err := adoptBrokerLease(home, lease, os.Getpid()); err != nil {
		t.Fatalf("adopt lease: %v", err)
	}

	adopted, _ := readBrokerLease(home)
	if adopted.PID != os.Getpid() || adopted.ProcessStart == "" {
		t.Fatalf("adopted lease = %+v", adopted)
	}
}

func TestBrokerLeaseWriteAndReadFailures(t *testing.T) {
	restoreLeaseHooks(t)

	home := t.TempDir()

	if _, ok := readBrokerLease(home); ok {
		t.Fatal("an absent lease read cleanly")
	}

	if err := os.WriteFile(filepath.Join(home, BrokerLeaseFileName), []byte("not json"), 0o600); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	if _, ok := readBrokerLease(home); ok {
		t.Fatal("a corrupt lease read cleanly")
	}

	leaseMarshal = func(any) ([]byte, error) { return nil, errors.New("encode") }

	if err := writeBrokerLease(home, BrokerLease{}); err == nil {
		t.Fatal("an unencodable lease reported success")
	}

	leaseMarshal = json.Marshal
	leaseWriteFile = func(string, []byte, os.FileMode) error { return errors.New("write") }

	if err := writeBrokerLease(home, BrokerLease{}); err == nil {
		t.Fatal("an unwritable lease reported success")
	}

	if err := adoptBrokerLease(home, BrokerLease{}, 1); err == nil {
		t.Fatal("an unwritable adoption reported success")
	}

	// A lease whose owner identity cannot be read records none, which is what
	// keeps the reaper from deciding anything about it later.
	leaseWriteFile = os.WriteFile
	leaseStartTime = func(int) (string, error) { return "", errors.ErrUnsupported }

	if lease := newBrokerLease(1, "hash"); lease.OwnerStart != "" {
		t.Fatalf("owner start = %q", lease.OwnerStart)
	}
}

// TestReapBrokerLeaseLeavesALiveOwnerAlone pins the case a naive reaper gets
// catastrophically wrong: a concurrent pending flow in this very process owns a
// home whose supervisor holds its locks, and killing it terminates a live login
// rather than an orphan.
func TestReapBrokerLeaseLeavesALiveOwnerAlone(t *testing.T) {
	restoreLeaseHooks(t)

	lease := newBrokerLease(1, "hash")
	lease.PID = 4242
	lease.ProcessStart = "leased"

	signalled := 0
	leaseSignalGroup = func(int, bool) error {
		signalled++

		return nil
	}

	removable, err := reapBrokerLease(lease)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}

	if removable || signalled != 0 {
		t.Fatalf("a live owner's home was reclaimed (removable=%v, signals=%d)", removable, signalled)
	}
}

func TestReapBrokerLeaseDecisions(t *testing.T) {
	cases := map[string]struct {
		lease     BrokerLease
		alive     map[int]string
		removable bool
	}{
		"no owner identity": {
			lease:     BrokerLease{OwnerPID: 7, PID: 8, ProcessStart: "eight"},
			alive:     map[int]string{8: "eight"},
			removable: false,
		},
		"owner gone and nothing leased": {
			lease:     BrokerLease{OwnerPID: 7, OwnerStart: "seven"},
			removable: true,
		},
		"owner gone and the leased pid was reused": {
			lease:     BrokerLease{OwnerPID: 7, OwnerStart: "seven", PID: 8, ProcessStart: "eight"},
			alive:     map[int]string{8: "a different process"},
			removable: true,
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			restoreLeaseHooks(t)

			leaseStartTime = func(pid int) (string, error) {
				if start, live := testCase.alive[pid]; live {
					return start, nil
				}

				return "", os.ErrNotExist
			}

			signalled := 0
			leaseSignalGroup = func(int, bool) error {
				signalled++

				return nil
			}

			removable, err := reapBrokerLease(testCase.lease)
			if err != nil {
				t.Fatalf("reap: %v", err)
			}

			if removable != testCase.removable {
				t.Fatalf("removable = %v, want %v", removable, testCase.removable)
			}

			if signalled != 0 {
				t.Fatalf("an unidentified process was signalled %d times", signalled)
			}
		})
	}
}

// TestReapBrokerLeaseKillsAnIdentifiedOrphan pins the ladder: the group is
// asked to leave, then killed, and the home is reclaimed only once the process
// the lease named is verifiably gone.
func TestReapBrokerLeaseKillsAnIdentifiedOrphan(t *testing.T) {
	restoreLeaseHooks(t)

	lease := BrokerLease{OwnerPID: 7, OwnerStart: "seven", PID: 8, ProcessStart: "eight"}

	alive := true
	leaseStartTime = func(pid int) (string, error) {
		if pid == 8 && alive {
			return "eight", nil
		}

		return "", os.ErrNotExist
	}

	var forced []bool

	leaseSignalGroup = func(pid int, force bool) error {
		if pid != 8 {
			t.Fatalf("the reaper signalled %d", pid)
		}

		forced = append(forced, force)

		// The orphan ignores the first rung and leaves on the kill.
		if force {
			alive = false
		}

		return nil
	}

	brokerLeaseReapTimeout = 10 * time.Millisecond
	brokerLeaseReapPoll = time.Millisecond

	removable, err := reapBrokerLease(lease)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}

	if !removable {
		t.Fatal("a reaped orphan's home was not reclaimed")
	}

	if len(forced) != 2 || forced[0] || !forced[1] {
		t.Fatalf("shutdown ladder = %v", forced)
	}
}

func TestReapBrokerLeaseFailures(t *testing.T) {
	cases := map[string]struct {
		signal func(int, bool) error
	}{
		"terminate failed": {
			signal: func(_ int, force bool) error {
				if force {
					return nil
				}

				return errors.New("terminate")
			},
		},
		"kill failed": {
			signal: func(_ int, force bool) error {
				if force {
					return errors.New("kill")
				}

				return nil
			},
		},
		"the orphan survived": {
			signal: func(int, bool) error { return nil },
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			restoreLeaseHooks(t)

			lease := BrokerLease{OwnerPID: 7, OwnerStart: "seven", PID: 8, ProcessStart: "eight"}

			leaseStartTime = func(pid int) (string, error) {
				if pid == 8 {
					return "eight", nil
				}

				return "", os.ErrNotExist
			}

			leaseSignalGroup = testCase.signal
			brokerLeaseReapTimeout = 2 * time.Millisecond
			brokerLeaseReapPoll = time.Millisecond

			removable, err := reapBrokerLease(lease)
			if err == nil {
				t.Fatal("a surviving orphan reported success")
			}

			// The home is not reclaimed, so its lease survives and the next
			// startup retries the ladder.
			if removable {
				t.Fatal("a home whose orphan survived was reclaimed")
			}
		})
	}
}

// TestSignalLeasedProcessGroupAnswersForAnAbsentProcess drives the real signal
// path against a PID nothing owns. A group that is already gone is not a
// failure to report: the reaper's whole purpose is that it be gone.
func TestSignalLeasedProcessGroupAnswersForAnAbsentProcess(t *testing.T) {
	restoreLeaseHooks(t)

	absent := absentProcessID(t)

	if err := signalLeasedProcessGroup(absent, false); err != nil {
		t.Fatalf("terminate an absent group: %v", err)
	}

	if err := signalLeasedProcessGroup(absent, true); err != nil {
		t.Fatalf("kill an absent group: %v", err)
	}
}

// absentProcessID finds a PID no process holds, so signalling it reaches
// nothing this machine is running.
func absentProcessID(t *testing.T) int {
	t.Helper()

	for pid := 1 << 22; pid > 1<<20; pid-- {
		if _, err := processStartTime(pid); err != nil {
			return pid
		}
	}

	t.Fatal("no unused process id was found")

	return 0
}

// TestStartServerWritesTheLeaseBeforeItSpawns pins the ordering at the seam
// that actually spawns: the lease naming the port and the credentials reaches
// disk while no process exists to name, and a second write adopts that
// process's PID once there is one.
func TestStartServerWritesTheLeaseBeforeItSpawns(t *testing.T) {
	restoreLeaseHooks(t)

	leaseDir := t.TempDir()

	var written []BrokerLease

	realWrite := leaseWriteFile
	leaseWriteFile = func(name string, contents []byte, mode os.FileMode) error {
		var lease BrokerLease
		if err := json.Unmarshal(contents, &lease); err != nil {
			t.Errorf("decode lease: %v", err)
		}

		written = append(written, lease)

		return realWrite(name, contents, mode)
	}

	client, err := StartServer(context.Background(), platformStartOptions(t, StartOptions{
		Root:            t.TempDir(),
		LeaseDir:        leaseDir,
		ExecutablePath:  fakeOpenCodeExecutable(t),
		HealthTimeout:   5 * time.Second,
		SkipVersionGate: true,
		Logger:          slog.New(slog.DiscardHandler),
	}))
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}

	t.Cleanup(func() { _ = client.Close(context.Background()) })

	if len(written) != 2 {
		t.Fatalf("the server wrote %d leases", len(written))
	}

	// The first write names no process because none existed yet, and it
	// already carries everything a reaper needs to recognise the server.
	if written[0].PID != 0 || written[0].Port == 0 || written[0].IdentityHash == "" || written[0].StartedAt == 0 {
		t.Fatalf("the lease written before the spawn = %+v", written[0])
	}

	if written[0].OwnerPID != os.Getpid() || written[0].OwnerStart == "" {
		t.Fatalf("the lease named no owner: %+v", written[0])
	}

	adopted, ok := readBrokerLease(leaseDir)
	if !ok {
		t.Fatal("no lease survived the start")
	}

	if adopted.PID <= 0 || adopted.ProcessStart == "" {
		t.Fatalf("the adopted lease = %+v", adopted)
	}

	if adopted.Port != written[0].Port || adopted.IdentityHash != written[0].IdentityHash {
		t.Fatalf("the adoption rewrote the server identity: %+v", adopted)
	}

	// The adopted PID is the live server's, which is what makes the reap
	// verifiable rather than a guess at a port.
	if start, startErr := processStartTime(adopted.PID); startErr != nil || start != adopted.ProcessStart {
		t.Fatalf("the adopted lease does not identify a live process: %q (%v)", start, startErr)
	}
}

func TestStartServerFailsClosedWhenTheLeaseCannotBeWritten(t *testing.T) {
	cases := map[string]int{"before the spawn": 1, "on adoption": 2}

	for name, failOn := range cases {
		t.Run(name, func(t *testing.T) {
			restoreLeaseHooks(t)

			writes := 0
			realWrite := leaseWriteFile

			leaseWriteFile = func(path string, contents []byte, mode os.FileMode) error {
				writes++
				if writes == failOn {
					return errors.New("lease write refused")
				}

				return realWrite(path, contents, mode)
			}

			_, err := StartServer(context.Background(), platformStartOptions(t, StartOptions{
				Root:            t.TempDir(),
				LeaseDir:        t.TempDir(),
				ExecutablePath:  fakeOpenCodeExecutable(t),
				HealthTimeout:   5 * time.Second,
				SkipVersionGate: true,
				Logger:          slog.New(slog.DiscardHandler),
			}))
			if err == nil {
				t.Fatal("a server started under a home no lease names")
			}
		})
	}
}
