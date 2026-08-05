//go:build unix

package opencode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type testProcessIdentityCapability struct{}

func (testProcessIdentityCapability) Duplicate() (*os.File, error) {
	return nil, errors.New("test capability is not duplicable")
}

func TestProcessIdentityDispositionValidation(t *testing.T) {
	capability := testProcessIdentityCapability{}
	validStandalone := ProcessIsolation{StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go-opencode"}
	validBorrowed := ProcessIsolation{IdentityLock: capability, AuthorityDomain: capability}

	for name, isolation := range map[string]ProcessIsolation{
		"standalone":             validStandalone,
		"borrowed":               validBorrowed,
		"mixed capabilities":     {IdentityLock: capability},
		"borrowed owner":         {IdentityLock: capability, AuthorityDomain: capability, StandaloneOwnerID: "deployment-1"},
		"missing owner":          {StandaloneStateRoot: "/var/lib/acp-go-opencode"},
		"invalid owner prefix":   {StandaloneOwnerID: "-deployment", StandaloneStateRoot: "/var/lib/acp-go-opencode"},
		"invalid owner byte":     {StandaloneOwnerID: "deployment 1", StandaloneStateRoot: "/var/lib/acp-go-opencode"},
		"long owner":             {StandaloneOwnerID: "a" + strings.Repeat("b", 256), StandaloneStateRoot: "/var/lib/acp-go-opencode"},
		"relative root":          {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "relative"},
		"filesystem root":        {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/"},
		"authority root":         {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go/agent-identities"},
		"beneath authority root": {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go/agent-identities/provider"},
		"control in root":        {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/provider\u0085"},
		"invalid utf8 in root":   {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: string([]byte{'/', 0xff})},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateStandaloneIdentityDisposition(&isolation)
			if name == "standalone" || name == "borrowed" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestProcessIsolationEnvironmentIsReplacementAndOverlay(t *testing.T) {
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin", "BASE": "yes", "OVERLAY": "base"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	env, err := buildProcessEnvironment(policy, map[string]string{"OVERLAY": "option", "ONLY_OPTION": "yes"})
	require.NoError(t, err)
	require.NotContains(t, env, "ACP_PROCESS_AMBIENT_CANARY")
	require.Equal(t, "yes", env["BASE"])
	require.Equal(t, "option", env["OVERLAY"])
	require.Equal(t, "yes", env["ONLY_OPTION"])
}

func TestManagedRuntimeRootsCannotBeOverlaid(t *testing.T) {
	filtered := withoutManagedRootOverrides(map[string]string{
		"KEEP":                "yes",
		"HOME":                "/hostile/home",
		"XDG_DATA_HOME":       "/hostile/data",
		"OPENCODE_CONFIG":     "/hostile/config.json",
		"OPENCODE_CONFIG_DIR": "/hostile/config",
		"OPENCODE_DB":         "/hostile/opencode.db",
	})

	require.Equal(t, map[string]string{"KEEP": "yes"}, filtered)
}

func TestProcessIsolationFailsClosedAndClearsGroups(t *testing.T) {
	_, err := buildProcessEnvironment(nil)
	require.ErrorContains(t, err, "required")
	_, err = buildProcessEnvironment(&ProcessIsolation{UID: 0, GID: 2, BaseEnvironment: map[string]string{}})
	require.ErrorContains(t, err, "nonzero")
	_, err = buildProcessEnvironment(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "relative"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"})
	require.ErrorContains(t, err, "non-absolute")
	cmd := exec.Command("/usr/bin/true")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	require.NoError(t, applyProcessCredential(cmd, policy))
	require.Equal(t, uint32(123), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(456), cmd.SysProcAttr.Credential.Gid)
	require.Empty(t, cmd.SysProcAttr.Credential.Groups)
}

func TestProcessIsolationValidationAndExecutableResolutionBranches(t *testing.T) {
	valid := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	require.ErrorContains(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2}), "base environment")
	require.Error(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"BAD=KEY": "x"}}))
	require.Error(t, validateEnvironmentMap(map[string]string{"OK": "bad\x00value"}))
	_, err := buildProcessEnvironment(valid, map[string]string{"BAD=KEY": "x"})
	require.Error(t, err)
	require.NoError(t, validateProcessSearchPath(""))

	_, err = resolveProcessExecutable(" ", nil)
	require.ErrorContains(t, err, "empty")
	_, err = resolveProcessExecutable("relative/tool", nil)
	require.ErrorContains(t, err, "not absolute")
	missing := filepath.Join(t.TempDir(), "missing")
	_, err = resolveProcessExecutable(missing, nil)
	require.ErrorContains(t, err, "stat executable")
	_, err = resolveProcessExecutable(t.TempDir(), nil)
	require.ErrorContains(t, err, "not executable")
	nonExecutable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("tool"), 0o600))
	_, err = resolveProcessExecutable(nonExecutable, nil)
	require.ErrorContains(t, err, "not executable")
	executable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))
	resolved, err := resolveProcessExecutable(executable, nil)
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
	_, err = resolveProcessExecutable("tool", []string{"HOME=/tmp"})
	require.ErrorContains(t, err, "PATH is empty")
	_, err = resolveProcessExecutable("tool", []string{"PATH=relative"})
	require.ErrorContains(t, err, "non-absolute")
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
	_, err = resolveProcessExecutable("child", []string{"PATH=" + blocked})
	require.Error(t, err)
	_, err = resolveProcessExecutable("missing", []string{"PATH=" + t.TempDir()})
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestProcessIsolationValidatesLinuxIdentityDisposition(t *testing.T) {
	original := processIsolationGOOS
	processIsolationGOOS = "linux"
	t.Cleanup(func() { processIsolationGOOS = original })
	require.ErrorContains(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{}}), "standalone owner")
}

func TestAdoptedIdentityDispositionAndInheritedDescriptorFailures(t *testing.T) {
	adopted := &ProcessIsolation{identityAuthorityAdopted: true}
	require.NoError(t, validateStandaloneIdentityDisposition(adopted))
	adopted.IdentityLock = testProcessIdentityCapability{}
	require.ErrorContains(t, validateStandaloneIdentityDisposition(adopted), "cannot carry")

	require.Error(t, applyProcessCredential(exec.Command("/usr/bin/true"), nil))
	require.Error(t, closeInheritedOnExec(nil))
	file, err := os.CreateTemp(t.TempDir(), "descriptor")
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	original := inheritedDescriptorFcntl
	t.Cleanup(func() { inheritedDescriptorFcntl = original })
	inheritedDescriptorFcntl = func(uintptr, int, int) (int, error) { return 0, errors.New("fcntl") }
	require.ErrorContains(t, closeInheritedOnExec(file), "read inherited")
	call := 0
	inheritedDescriptorFcntl = func(uintptr, int, int) (int, error) {
		call++
		if call == 2 {
			return 0, errors.New("fcntl")
		}

		return 0, nil
	}
	require.ErrorContains(t, closeInheritedOnExec(file), "protect inherited")
	inheritedDescriptorFcntl = original
	require.NoError(t, closeInheritedOnExec(file))
}

func TestSupervisorConfigIsInheritedUnlinkedDescriptor(t *testing.T) {
	root := t.TempDir()
	file, err := writeSupervisorConfig(root, supervisorConfig{NativePath: "/usr/bin/true", Home: root, Scratch: root, IsolationUID: 1, IsolationGID: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	_, err = os.Stat(file.Name())
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = readSupervisorConfig(file)
	require.NoError(t, err)
}
