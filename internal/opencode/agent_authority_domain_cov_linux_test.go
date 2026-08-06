//go:build linux

package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

// agentAuthorityDomainCovSeams restores every agentAuthorityDomain* seam when
// the test ends, so a fault injected for one kernel fact cannot leak into the
// next case.
func agentAuthorityDomainCovSeams(t *testing.T) {
	t.Helper()

	fstat := agentAuthorityDomainFstat
	fstatat := agentAuthorityDomainFstatat
	fstatfs := agentAuthorityDomainFstatfs
	stat := agentAuthorityDomainStat
	statfs := agentAuthorityDomainStatfs
	readFile := agentAuthorityDomainReadFile

	t.Cleanup(func() {
		agentAuthorityDomainFstat = fstat
		agentAuthorityDomainFstatat = fstatat
		agentAuthorityDomainFstatfs = fstatfs
		agentAuthorityDomainStat = stat
		agentAuthorityDomainStatfs = statfs
		agentAuthorityDomainReadFile = readFile
	})
}

// agentAuthorityDomainCovAuthority bootstraps an empty trusted authority root
// and returns its open directory descriptor together with the path the domain
// record must occupy inside it.
func agentAuthorityDomainCovAuthority(t *testing.T) (*os.File, string) {
	t.Helper()

	restoreAgentIdentityLockTestSeams(t)
	agentAuthorityDomainCovSeams(t)

	root := configureAgentIdentityLockTestRoot(t)

	directory, err := bootstrapAgentIdentityLockDirectory(
		root, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, directory.Close(), "close authority domain fixture")
	})

	return directory, filepath.Join(root, "acp-go", "agent-identities", agentAuthorityDomainRecordName)
}

// agentAuthorityDomainCovID is a well formed authority id, which every record
// this file writes carries so no case is refused for the wrong reason.
const agentAuthorityDomainCovID = "0123456789abcdef0123456789abcdef"

// agentAuthorityDomainCovFields returns the current domain of the authority
// root as a mutable JSON object, so each case can corrupt exactly one member of
// an otherwise acceptable record.
func agentAuthorityDomainCovFields(t *testing.T, directory *os.File) map[string]json.RawMessage {
	t.Helper()

	record, err := currentAgentAuthorityDomain(directory)
	require.NoError(t, err)

	record.AuthorityID = agentAuthorityDomainCovID

	payload, err := json.Marshal(record)
	require.NoError(t, err)

	var fields map[string]json.RawMessage

	require.NoError(t, json.Unmarshal(payload, &fields))

	return fields
}

// agentAuthorityDomainCovPayload renders the base record with the named members
// replaced by raw JSON, which is how a case states the one thing it corrupts.
func agentAuthorityDomainCovPayload(
	t *testing.T,
	base map[string]json.RawMessage,
	overrides map[string]string,
) []byte {
	t.Helper()

	fields := maps.Clone(base)
	for name, value := range overrides {
		fields[name] = json.RawMessage(value)
	}

	payload, err := json.Marshal(fields)
	require.NoError(t, err)

	return append(payload, '\n')
}

// agentAuthorityDomainCovWrite publishes payload as the trusted domain record.
func agentAuthorityDomainCovWrite(t *testing.T, path string, payload []byte) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, payload, 0o600))
}

// agentAuthorityDomainCovReadFile makes the named /proc file answer with
// payload and failure, leaving every other file answered by the kernel. The
// caller restores the seam through agentAuthorityDomainCovSeams.
func agentAuthorityDomainCovReadFile(path string, payload []byte, failure error) {
	original := agentAuthorityDomainReadFile
	agentAuthorityDomainReadFile = func(name string) ([]byte, error) {
		if name == path {
			return payload, failure
		}

		return original(name)
	}
}

// agentAuthorityDomainCovStat makes the named namespace link answer with mutate
// and failure, leaving every other path answered by the kernel. The caller
// restores the seam through agentAuthorityDomainCovSeams.
func agentAuthorityDomainCovStat(path string, mutate func(*unix.Stat_t), failure error) {
	original := agentAuthorityDomainStat
	agentAuthorityDomainStat = func(name string, stat *unix.Stat_t) error {
		if name != path {
			return original(name, stat)
		}

		if failure != nil {
			return failure
		}

		if err := original(name, stat); err != nil {
			return err
		}

		mutate(stat)

		return nil
	}
}

// TestAgentAuthorityDomainRecordRejectsEveryMalformedShape proves the domain
// record is parsed strictly: every object must carry exactly its declared
// members, every scalar must have its declared type and range, and no
// structural trick — a duplicate key, a widened integer, a spare array element
// — is allowed to reach the decoded record.
func TestAgentAuthorityDomainRecordRejectsEveryMalformedShape(t *testing.T) {
	directory, recordPath := agentAuthorityDomainCovAuthority(t)
	base := agentAuthorityDomainCovFields(t, directory)
	agentAuthorityDomainCovWrite(t, recordPath, agentAuthorityDomainCovPayload(t, base, nil))

	_, err := loadAgentAuthorityDomainRecord(
		directory, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	)
	require.NoError(t, err, "the trusted record must load before anything corrupts it")

	for name, testCase := range map[string]struct {
		overrides map[string]string
		raw       string
		wantError string
	}{
		"duplicate member": {
			raw:       `{"version":1,"version":1}`,
			wantError: `duplicate key "version"`,
		},
		"record is not an object": {
			raw:       `[1,2]`,
			wantError: "cannot unmarshal array",
		},
		"authority root is not an object": {
			overrides: map[string]string{"authorityRoot": `5`},
			wantError: "invalid agent authority root",
		},
		"authority root names the wrong member": {
			overrides: map[string]string{"authorityRoot": `{"dev":1,"inode":2}`},
			wantError: `invalid agent authority root: object is missing required field "ino"`,
		},
		"filesystem carries a spare member": {
			overrides: map[string]string{"filesystem": `{"type":1,"id":[1,2],"spare":3}`},
			wantError: "invalid agent authority filesystem",
		},
		"filesystem id has three components": {
			overrides: map[string]string{"filesystem": `{"type":1,"id":[1,2,3]}`},
			wantError: "filesystem id must contain exactly two integers",
		},
		"filesystem id component is not an integer": {
			overrides: map[string]string{"filesystem": `{"type":1,"id":["a",2]}`},
			wantError: "filesystem id contains an invalid signed 32-bit integer",
		},
		"pid namespace names the wrong member": {
			overrides: map[string]string{"pidNamespace": `{"dev":1,"inode":2}`},
			wantError: "invalid agent authority PID namespace",
		},
		"user namespace names the wrong member": {
			overrides: map[string]string{"userNamespace": `{"dev":1,"inode":2}`},
			wantError: "invalid agent authority user namespace",
		},
		"version is not a number": {
			overrides: map[string]string{"version": `"one"`},
			wantError: "cannot unmarshal string",
		},
		"version is unsupported": {
			overrides: map[string]string{"version": `2`},
			wantError: "agent authority domain record is incomplete",
		},
		"authority root inode is absent": {
			overrides: map[string]string{"authorityRoot": `{"dev":1,"ino":0}`},
			wantError: "agent authority domain record is incomplete",
		},
		"authority id is not hexadecimal": {
			overrides: map[string]string{"authorityId": `"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"`},
			wantError: "agent authority domain id is invalid",
		},
		"authority id is upper case": {
			overrides: map[string]string{"authorityId": `"0123456789ABCDEF0123456789ABCDEF"`},
			wantError: "agent authority domain id is invalid",
		},
		"boot id is not canonical": {
			overrides: map[string]string{"bootId": `"not-a-boot-identifier-at-all-0000000"`},
			wantError: "agent authority domain boot id is invalid",
		},
		"uid map extent has no length": {
			overrides: map[string]string{"uidMap": `[{"inside":0,"outside":0,"length":0}]`},
			wantError: "invalid agent authority uid map",
		},
		"uid map extent is missing a member": {
			overrides: map[string]string{"uidMap": `[{"inside":0,"outside":0}]`},
			wantError: "invalid agent authority uid map: object does not contain its exact required fields",
		},
		"gid map extent has no length": {
			overrides: map[string]string{"gidMap": `[{"inside":0,"outside":0,"length":0}]`},
			wantError: "invalid agent authority gid map",
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := []byte(testCase.raw + "\n")
			if testCase.raw == "" {
				payload = agentAuthorityDomainCovPayload(t, base, testCase.overrides)
			}

			agentAuthorityDomainCovWrite(t, recordPath, payload)

			_, loadErr := loadAgentAuthorityDomainRecord(
				directory, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
			)
			require.ErrorContains(t, loadErr, testCase.wantError)
		})
	}
}

// TestAgentAuthorityDomainRecordRequiresItsTrustedBoundedInode proves the
// domain record is only read from the exact trusted named inode: a record that
// is group-readable, multiply linked, oversized, not valid UTF-8, or that the
// kernel will no longer describe or hand over its bytes is refused rather than
// parsed.
func TestAgentAuthorityDomainRecordRequiresItsTrustedBoundedInode(t *testing.T) {
	directory, recordPath := agentAuthorityDomainCovAuthority(t)
	base := agentAuthorityDomainCovFields(t, directory)
	trusted := agentAuthorityDomainCovPayload(t, base, nil)
	authorityRoot := filepath.Dir(recordPath)

	load := func() error {
		_, err := loadAgentAuthorityDomainRecord(
			directory, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
		)

		return err
	}

	agentAuthorityDomainCovWrite(t, recordPath, trusted)
	require.NoError(t, os.Chmod(recordPath, 0o640))
	require.ErrorContains(t, load(), "trusted bounded named inode",
		"a group-readable record must never be parsed",
	)
	require.NoError(t, os.Chmod(recordPath, 0o600))

	linked := filepath.Join(authorityRoot, "domain.json.link")
	require.NoError(t, os.Link(recordPath, linked))
	require.ErrorContains(t, load(), "trusted bounded named inode",
		"a second link to the record is a second name that could replace it",
	)
	require.NoError(t, os.Remove(linked))

	agentAuthorityDomainCovWrite(t, recordPath, bytes.Repeat([]byte("x"), agentAuthorityDomainMaxSize+1))
	require.ErrorContains(t, load(), "trusted bounded named inode")

	agentAuthorityDomainCovWrite(t, recordPath, []byte{'{', 0xff, 0xfe, '}', '\n'})
	require.ErrorContains(t, load(), "not valid UTF-8")

	agentAuthorityDomainCovWrite(t, recordPath, trusted)

	statFailure := errors.New("kernel stopped describing the domain record")
	agentAuthorityDomainFstat = func(int, *unix.Stat_t) error { return statFailure }
	require.ErrorIs(t, load(), statFailure)
	agentAuthorityDomainFstat = unix.Fstat

	namedFailure := errors.New("kernel stopped resolving the domain record name")
	agentAuthorityDomainFstatat = func(int, string, *unix.Stat_t, int) error { return namedFailure }
	require.ErrorIs(t, load(), namedFailure)
	agentAuthorityDomainFstatat = unix.Fstatat

	// A record inode the kernel describes as a bounded trusted regular file but
	// which refuses to hand over any bytes must abort the domain proof, never
	// be read as an empty record.
	require.NoError(t, os.Remove(recordPath))
	require.NoError(t, os.Mkdir(recordPath, 0o700))

	agentAuthorityDomainFstat = func(fd int, stat *unix.Stat_t) error {
		if err := unix.Fstat(fd, stat); err != nil {
			return err
		}

		stat.Mode = unix.S_IFREG | 0o600
		stat.Nlink = 1
		stat.Size = int64(len(trusted))

		return nil
	}
	require.ErrorIs(t, load(), unix.EISDIR)
}

// TestAgentAuthorityDomainRecordMustDescribeTheRunningDomain proves that a
// syntactically perfect record which describes a different boot, PID namespace
// or user namespace is refused, so a record carried across a reboot or into
// another namespace can never be adopted as this host's authority.
func TestAgentAuthorityDomainRecordMustDescribeTheRunningDomain(t *testing.T) {
	directory, recordPath := agentAuthorityDomainCovAuthority(t)
	base := agentAuthorityDomainCovFields(t, directory)
	agentAuthorityDomainCovWrite(t, recordPath, agentAuthorityDomainCovPayload(t, base, nil))
	require.NoError(t, validateAgentAuthorityDomainRecord(
		directory, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	))

	agentAuthorityDomainCovWrite(t, recordPath, agentAuthorityDomainCovPayload(t, base, map[string]string{
		"bootId": `"3f2504e0-4f89-11d3-9a0c-0305e82c3301"`,
	}))
	require.ErrorContains(t, validateAgentAuthorityDomainRecord(
		directory, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	), "belongs to another PID/user namespace domain")

	agentAuthorityDomainCovWrite(t, recordPath, agentAuthorityDomainCovPayload(t, base, nil))

	domainFailure := errors.New("kernel stopped answering for the authority root")
	agentAuthorityDomainFstatfs = func(int, *unix.Statfs_t) error { return domainFailure }
	require.ErrorIs(t, validateAgentAuthorityDomainRecord(
		directory, agentIdentityLockTrustedUID, agentIdentityLockTrustedGID,
	), domainFailure, "a record must never be accepted against a domain that could not be determined")
}

// TestCurrentAgentAuthorityDomainRefusesUnprovenKernelFacts proves that every
// fact the running domain is built from — the authority filesystem, the boot
// id, the PID namespace and its visibility of every task, the user namespace
// and both id maps — must be answered and canonical, and that a missing or
// implausible answer aborts the domain instead of producing a partial one.
func TestCurrentAgentAuthorityDomainRefusesUnprovenKernelFacts(t *testing.T) {
	directory, _ := agentAuthorityDomainCovAuthority(t)

	_, err := currentAgentAuthorityDomain(directory)
	require.NoError(t, err, "the running domain must be determinable before anything is faulted")

	probeFailure := errors.New("kernel fact unavailable")

	for name, testCase := range map[string]struct {
		seam      func()
		wantError string
	}{
		"authority root is not described": {
			seam: func() {
				agentAuthorityDomainFstat = func(int, *unix.Stat_t) error { return probeFailure }
			},
			wantError: probeFailure.Error(),
		},
		"authority filesystem is not described": {
			seam: func() {
				agentAuthorityDomainFstatfs = func(int, *unix.Statfs_t) error { return probeFailure }
			},
			wantError: probeFailure.Error(),
		},
		"authority filesystem has no identity": {
			seam: func() {
				agentAuthorityDomainFstatfs = func(int, *unix.Statfs_t) error { return nil }
			},
			wantError: "agent authority filesystem id is unavailable",
		},
		"boot id is unreadable": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/sys/kernel/random/boot_id", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"boot id is not canonical": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/sys/kernel/random/boot_id", []byte("nope\n"), nil)
			},
			wantError: "kernel agent authority boot id is not canonical",
		},
		"pid namespace is not described": {
			seam: func() {
				agentAuthorityDomainCovStat("/proc/self/ns/pid", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"child pid namespace is not described": {
			seam: func() {
				agentAuthorityDomainCovStat("/proc/self/ns/pid_for_children", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"child pid namespace differs": {
			seam: func() {
				agentAuthorityDomainCovStat("/proc/self/ns/pid_for_children", func(stat *unix.Stat_t) {
					stat.Ino++
				}, nil)
			},
			wantError: "requires self and child PID namespaces to match",
		},
		"proc is not procfs": {
			seam: func() {
				agentAuthorityDomainStatfs = func(string, *unix.Statfs_t) error { return probeFailure }
			},
			wantError: "agent authority requires /proc to be procfs",
		},
		"proc mounts are unreadable": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/mounts", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"proc hides other tasks": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/mounts", []byte(
					"proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=2 0 0\n",
				), nil)
			},
			wantError: `agent authority rejects procfs option "hidepid=2"`,
		},
		"proc mount is unidentifiable": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/mounts", []byte("tmpfs /tmp tmpfs rw 0 0\n"), nil)
			},
			wantError: "cannot identify the root procfs mount",
		},
		"user namespace is not described": {
			seam: func() {
				agentAuthorityDomainCovStat("/proc/self/ns/user", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"uid map is unreadable": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/self/uid_map", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"gid map is unreadable": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/self/gid_map", nil, probeFailure)
			},
			wantError: probeFailure.Error(),
		},
		"uid map extent is truncated": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/self/uid_map", []byte("0 0\n"), nil)
			},
			wantError: "agent authority id map has an invalid extent",
		},
		"uid map extent value is not a number": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/self/uid_map", []byte("0 0 many\n"), nil)
			},
			wantError: "agent authority id map has an invalid extent value",
		},
		"uid map extent has no length": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/self/uid_map", []byte("0 0 0\n"), nil)
			},
			wantError: "agent authority id map has an invalid extent value",
		},
		"uid map extents overlap outside": {
			seam: func() {
				agentAuthorityDomainCovReadFile("/proc/self/uid_map", []byte(
					"0 1000 10\n100 0 10\n200 500 10\n300 500 10\n",
				), nil)
			},
			wantError: "id map extents overlap by outside id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			agentAuthorityDomainCovSeams(t)
			testCase.seam()

			_, domainErr := currentAgentAuthorityDomain(directory)
			require.ErrorContains(t, domainErr, testCase.wantError)
		})
	}

	t.Run("disjoint multi-extent id map", func(t *testing.T) {
		agentAuthorityDomainCovSeams(t)
		agentAuthorityDomainCovReadFile(
			"/proc/self/uid_map", []byte("0 1000 10\n100 0 10\n200 500 10\n"), nil,
		)

		domain, domainErr := currentAgentAuthorityDomain(directory)
		require.NoError(t, domainErr)
		require.Equal(t, []agentAuthorityDomainExtent{
			{Inside: 0, Outside: 1000, Length: 10},
			{Inside: 100, Outside: 0, Length: 10},
			{Inside: 200, Outside: 500, Length: 10},
		}, domain.UIDMap, "an ascending map that is disjoint in both id spaces is canonical")
	})
}

// TestAgentAuthorityExtentValidationBoundsTheIDMap proves the id map is
// accepted only as a bounded, ascending, non-overlapping mapping in both the
// inside and outside id spaces, and that the array actually decoded must still
// be the array that was validated.
func TestAgentAuthorityExtentValidationBoundsTheIDMap(t *testing.T) {
	require.ErrorContains(t, validateAgentAuthorityExtents(nil),
		"id map must contain between 1 and "+strconv.Itoa(agentAuthorityDomainMaxExtents)+" extents",
	)

	oversized := make([]agentAuthorityDomainExtent, agentAuthorityDomainMaxExtents+1)
	for index := range oversized {
		oversized[index] = agentAuthorityDomainExtent{
			Inside: uint32(index) * 2, Outside: uint32(index) * 2, Length: 1,
		}
	}

	require.ErrorContains(t, validateAgentAuthorityExtents(oversized), "extents")

	for name, extents := range map[string][]agentAuthorityDomainExtent{
		"inside overflows": {{Inside: 4294967295, Outside: 0, Length: 2}},
		"outside overflows": {
			{Inside: 0, Outside: 4294967295, Length: 2},
		},
		"inside descends": {
			{Inside: 100, Outside: 0, Length: 10},
			{Inside: 0, Outside: 100, Length: 10},
		},
		"inside overlaps": {
			{Inside: 0, Outside: 0, Length: 10},
			{Inside: 5, Outside: 100, Length: 10},
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, validateAgentAuthorityExtents(extents),
				"invalid, overflowing, overlapping, or noncanonical",
			)
		})
	}

	// The decoded extents are what the domain is compared on, so an id map
	// whose raw array no longer matches them has changed under the decoder and
	// must be refused rather than validated in its earlier form. Only the stage
	// itself can be handed such a pair: the record path derives both from the
	// same bytes.
	require.ErrorContains(t, validateAgentAuthorityExtentFields([]byte(`{}`), nil),
		"agent authority id map changed while decoding",
	)
	require.NoError(t, validateAgentAuthorityExtentFields(
		[]byte(`[{"inside":0,"outside":0,"length":10}]`),
		[]agentAuthorityDomainExtent{{Inside: 0, Outside: 0, Length: 10}},
	))
}

// TestAgentAuthorityBootIDRequiresTheCanonicalUUIDForm proves the boot id is
// accepted only in the exact lower-case dashed UUID form the kernel emits, so
// no other string can stand in for a boot identity.
func TestAgentAuthorityBootIDRequiresTheCanonicalUUIDForm(t *testing.T) {
	require.True(t, canonicalAgentAuthorityBootID("3f2504e0-4f89-11d3-9a0c-0305e82c3301"))

	for name, value := range map[string]string{
		"too short":     "3f2504e0-4f89-11d3-9a0c-0305e82c330",
		"wrong dash":    "3f2504e0-4f89-11d3-9a0c0-305e82c3301",
		"upper case":    "3F2504E0-4f89-11d3-9a0c-0305e82c3301",
		"non hex digit": "3f2504e0-4f89-11d3-9a0c-0305e82c330z",
	} {
		t.Run(name, func(t *testing.T) {
			require.False(t, canonicalAgentAuthorityBootID(value))
		})
	}
}

// TestAgentAuthorityDuplicateJSONKeyScannerWalksNestedValues proves the
// duplicate-key scanner descends into nested objects and arrays rather than
// only checking the top level, that it surfaces the decoder's own error when
// the document stops being well formed part-way through an object, and that a
// second value appended after the record is refused before anything decodes it.
func TestAgentAuthorityDuplicateJSONKeyScannerWalksNestedValues(t *testing.T) {
	require.NoError(t, rejectAgentAuthorityDuplicateJSONKeys([]byte(`{"a":{"b":[1,{"c":2}]},"d":[]}`)))

	for name, testCase := range map[string]struct {
		payload   string
		wantError string
	}{
		"duplicate at the top level": {
			payload:   `{"a":1,"a":2}`,
			wantError: `duplicate key "a"`,
		},
		"duplicate inside a nested object": {
			payload:   `{"a":{"b":1,"b":2}}`,
			wantError: `duplicate key "b"`,
		},
		"duplicate inside an array element": {
			payload:   `{"a":[{"b":1,"b":2}]}`,
			wantError: `duplicate key "b"`,
		},
		"object member separator is missing": {
			payload:   `{"a":1 "b":2}`,
			wantError: "after object key:value pair",
		},
		"nested object value is malformed": {
			payload:   `{"a":{"b":]}}`,
			wantError: "looking for beginning of value",
		},
		"array element is malformed": {
			payload:   `{"a":[1,]}`,
			wantError: "looking for beginning of value",
		},
		"a second value follows the record": {
			payload:   `{"a":1}{"b":2}`,
			wantError: "json contains multiple values",
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorContains(t, rejectAgentAuthorityDuplicateJSONKeys([]byte(testCase.payload)),
				testCase.wantError,
			)
		})
	}
}

// TestAgentAuthorityDomainRecordDecodeStageRefusesTrailingData proves the
// decode stage holds the whole payload to one value on its own account. The
// record path cannot reach this refusal — the shape stage rejects a second
// value first — so the guard is exercised at the stage boundary, where it is
// the only thing standing between a caller that skipped the scanner and a
// record decoded out of a document that carries more than the record.
func TestAgentAuthorityDomainRecordDecodeStageRefusesTrailingData(t *testing.T) {
	directory, _ := agentAuthorityDomainCovAuthority(t)
	base := agentAuthorityDomainCovFields(t, directory)
	trusted := agentAuthorityDomainCovPayload(t, base, nil)

	record, err := decodeAgentAuthorityDomainRecord(trusted)
	require.NoError(t, err)
	require.Equal(t, agentAuthorityDomainCovID, record.AuthorityID)

	appended := append(trusted[:len(trusted):len(trusted)], []byte(`{"version":1}`+"\n")...)

	_, err = decodeAgentAuthorityDomainRecord(appended)
	require.ErrorContains(t, err, "agent authority domain record contains trailing data")

	require.ErrorContains(t, rejectAgentAuthorityDuplicateJSONKeys(appended), "json contains multiple values",
		"the shape stage is why the record path never reaches the decode stage's own guard",
	)
}
