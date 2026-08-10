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
	withLinuxProcessIsolation(t)
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
	withLinuxProcessIsolation(t)
	_, err := buildProcessEnvironment(&ProcessIsolation{UID: 0, GID: 2, BaseEnvironment: map[string]string{}})
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

func TestImplicitProcessEnvironmentIsCapturedAndScrubbed(t *testing.T) {
	original := processEnviron
	t.Cleanup(func() { processEnviron = original })
	processEnviron = func() []string {
		return []string{"PATH=/usr/bin:/bin", "AMBIENT=present", supervisorModeEnv + "=" + supervisorModeGuardian}
	}

	env, err := buildProcessEnvironment(nil)
	require.NoError(t, err)
	require.Equal(t, "present", env["AMBIENT"])
	require.NotContains(t, env, supervisorModeEnv)
}

func TestProcessIsolationValidationAndExecutableResolutionBranches(t *testing.T) {
	withLinuxProcessIsolation(t)
	valid := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	require.ErrorContains(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2}), "base environment")
	require.Error(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"BAD=KEY": "x"}}))
	require.Error(t, validateEnvironmentMap(map[string]string{"OK": "bad\x00value"}))
	_, err := buildProcessEnvironment(valid, map[string]string{"BAD=KEY": "x"})
	require.Error(t, err)
	require.NoError(t, validateProcessSearchPath(""))

	_, err = resolveProcessExecutable(" ", nil, true)
	require.ErrorContains(t, err, "empty")
	_, err = resolveProcessExecutable("relative/tool", nil, true)
	require.ErrorContains(t, err, "not absolute")
	missing := filepath.Join(t.TempDir(), "missing")
	_, err = resolveProcessExecutable(missing, nil, true)
	require.ErrorContains(t, err, "stat executable")
	_, err = resolveProcessExecutable(t.TempDir(), nil, true)
	require.ErrorContains(t, err, "not executable")
	nonExecutable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("tool"), 0o600))
	_, err = resolveProcessExecutable(nonExecutable, nil, true)
	require.ErrorContains(t, err, "not executable")
	executable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))
	resolved, err := resolveProcessExecutable(executable, nil, true)
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
	_, err = resolveProcessExecutable("tool", []string{"HOME=/tmp"}, true)
	require.ErrorContains(t, err, "process isolation PATH is empty")
	_, err = resolveProcessExecutable("tool", []string{"PATH=relative"}, true)
	require.ErrorContains(t, err, "non-absolute")
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
	_, err = resolveProcessExecutable("child", []string{"PATH=" + blocked}, true)
	require.Error(t, err)
	_, err = resolveProcessExecutable("missing", []string{"PATH=" + t.TempDir()}, true)
	require.ErrorIs(t, err, exec.ErrNotFound)
}

// TestOrdinaryExecutableResolutionDoesNotInheritPolicyPathRules proves policy
// omission is not a startup blocker. A relative configured executable and a
// relative PATH entry are ordinary shell shapes, and the absolute-entry rule
// belongs to the closed explicit policy alone. The strict arm keeps refusing
// both, so the split is a split rather than a relaxation.
func TestOrdinaryExecutableResolutionDoesNotInheritPolicyPathRules(t *testing.T) {
	directory := t.TempDir()
	tool := filepath.Join(directory, "opencode")
	require.NoError(t, os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o700))

	relativeDirectory, err := filepath.Rel(t.TempDir(), directory)
	require.NoError(t, err)

	resolved, err := resolveProcessExecutable("opencode", []string{"PATH=" + relativeDirectory + ":" + directory}, false)
	require.NoError(t, err)
	require.Equal(t, tool, resolved)

	_, err = resolveProcessExecutable("opencode", []string{"PATH=" + relativeDirectory + ":" + directory}, true)
	require.ErrorContains(t, err, "process isolation PATH contains non-absolute entry")

	relativeTool, err := filepath.Rel(mustGetwd(t), tool)
	require.NoError(t, err)
	ordinary, err := resolveProcessExecutable(relativeTool, nil, false)
	require.NoError(t, err)
	require.Equal(t, relativeTool, ordinary)

	_, err = resolveProcessExecutable(relativeTool, nil, true)
	require.ErrorContains(t, err, "not absolute")

	_, err = resolveProcessExecutable("opencode", []string{"HOME=/tmp"}, false)
	require.ErrorContains(t, err, "find opencode: PATH is empty")
	_, err = resolveProcessExecutable("opencode", []string{"PATH=" + t.TempDir()}, false)
	require.ErrorContains(t, err, "find opencode in PATH")
	_, err = resolveProcessExecutable(filepath.Join(t.TempDir(), "missing"), nil, false)
	require.ErrorContains(t, err, "stat executable")
	ordinaryBlocked := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.WriteFile(ordinaryBlocked, []byte("blocked"), 0o600))
	_, err = resolveProcessExecutable("opencode", []string{"PATH=" + filepath.Dir(ordinaryBlocked)}, false)
	require.ErrorContains(t, err, "not executable")
	_, err = resolveProcessExecutable(filepath.Dir(ordinaryBlocked), nil, false)
	require.ErrorContains(t, err, "not executable")

	// An ordinary environment carrying a relative PATH entry still builds.
	values, err := buildProcessEnvironmentFrom(nil, map[string]string{"PATH": "bin:/usr/bin"})
	require.NoError(t, err)
	require.Equal(t, "bin:/usr/bin", values["PATH"])
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	require.NoError(t, err)

	return working
}

// TestExplicitProcessIsolationIsLinuxOnly proves the embedded Go API refuses an
// explicit policy off Linux rather than only the command's policy loader
// refusing it. Nothing downstream reads the verdict and retries ordinary
// execution, and omission stays supported on the same platform.
func TestExplicitProcessIsolationIsLinuxOnly(t *testing.T) {
	original := processIsolationGOOS
	t.Cleanup(func() { processIsolationGOOS = original })

	for _, platform := range []string{"darwin", "freebsd", "windows", "openbsd"} {
		processIsolationGOOS = platform
		require.EqualError(t, validateProcessIsolation(&ProcessIsolation{
			UID: 65534, GID: 65534, BaseEnvironment: map[string]string{},
			StandaloneOwnerID: "test-owner", StandaloneStateRoot: testStandaloneStateRootPath,
		}), errExplicitProcessIsolationPlatform)
		require.NoError(t, validateProcessIsolation(nil), "omission stays supported on "+platform)
	}
}

func TestProcessIsolationValidatesLinuxIdentityDisposition(t *testing.T) {
	withLinuxProcessIsolation(t)
	require.ErrorContains(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{}}), "standalone owner")
}

// TestAdapterOwnedStateNeverReachesANativeEnvironment proves the private and
// managed carriers are scrubbed from the base and from every overlay alike,
// whatever their case. HOME is deliberately retained: it names the home of the
// identity the native process actually runs as, and every root OpenCode could
// redirect state through is replaced by the launch.
func TestAdapterOwnedStateNeverReachesANativeEnvironment(t *testing.T) {
	original := processEnviron
	t.Cleanup(func() { processEnviron = original })
	processEnviron = func() []string {
		return []string{
			"PATH=/usr/bin:/bin",
			"HOME=/home/operator",
			"AMBIENT=present",
			privateAdapterEnvPrefix + "SPOOF=leaked",
			strings.ToLower(privateAdapterEnvPrefix) + "spoof=leaked",
			supervisorModeEnv + "=" + supervisorModeGuardian,
			DarwinRuntimeIDEnv + "=leaked",
			DarwinScratchRootEnv + "=/leaked",
			"OPENCODE_DB=/leaked/opencode.db",
			"OPENCODE_CONFIG_DIR=/leaked/config",
			"XDG_RUNTIME_DIR=/leaked/run",
		}
	}

	env, err := buildProcessEnvironment(nil, map[string]string{
		"OVERLAY":                             "kept",
		privateAdapterEnvPrefix + "OVERLAY":   "leaked",
		"opencode_db":                         "/leaked/overlay.db",
		strings.ToLower(DarwinScratchRootEnv): "/leaked/overlay",
	})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"PATH":    "/usr/bin:/bin",
		"HOME":    "/home/operator",
		"AMBIENT": "present",
		"OVERLAY": "kept",
	}, env)
}

func TestAdoptedIdentityDispositionAndInheritedDescriptorFailures(t *testing.T) {
	withLinuxProcessIsolation(t)
	adopted := &ProcessIsolation{identityAuthorityAdopted: true}
	require.NoError(t, validateStandaloneIdentityDisposition(adopted))
	adopted.IdentityLock = testProcessIdentityCapability{}
	require.ErrorContains(t, validateStandaloneIdentityDisposition(adopted), "cannot carry")

	ordinary := exec.Command("/usr/bin/true")
	require.NoError(t, applyProcessCredential(ordinary, nil))
	require.Nil(t, ordinary.SysProcAttr)
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

// requireNoProcessCredential asserts a command asks the kernel for no identity
// change. Process-group hygiene may still be configured; what must be absent is
// a credential, which only an explicit policy ever sets.
func requireNoProcessCredential(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	require.True(t, cmd.SysProcAttr == nil || cmd.SysProcAttr.Credential == nil,
		"ordinary execution requests no credential change")
}

// TestOrdinaryDirectExecutionSelectsThePortableArm pins which platforms run an
// omitted policy through the guardian/liveness pair and which run it directly.
// Only Linux and an opted-in Darwin can prove anything with the pair; every
// other platform would refuse before native start, which is why omission takes
// the direct arm there rather than becoming a dead platform. An explicit policy
// never reaches either question.
func TestOrdinaryDirectExecutionSelectsThePortableArm(t *testing.T) {
	original := processIsolationGOOS
	t.Cleanup(func() { processIsolationGOOS = original })

	for _, testCase := range []struct {
		goos       string
		bestEffort bool
		want       bool
	}{
		{goos: processIsolationLinux, want: false},
		{goos: processIsolationLinux, bestEffort: true, want: false},
		{goos: processIsolationDarwin, want: true},
		{goos: processIsolationDarwin, bestEffort: true, want: false},
		{goos: "windows", want: true},
		{goos: "freebsd", want: true},
	} {
		t.Run(testCase.goos, func(t *testing.T) {
			processIsolationGOOS = testCase.goos
			require.Equal(t, testCase.want, ordinaryDirectExecution(nil, testCase.bestEffort))
			require.Equal(t, testCase.want, ordinaryProcessBackend(supervisorConfig{
				DarwinBestEffort: testCase.bestEffort,
			}))
			require.False(t, ordinaryDirectExecution(testProcessIsolation(), testCase.bestEffort),
				"an explicit policy never selects an ordinary backend")
		})
	}
}

// TestSupervisorIdentityDispositionSeparatesOrdinaryFromExplicit proves each
// process in the tree re-derives the arm from the identity it actually runs as.
// An ordinary stamp is checked against that identity and against the emptiness
// of every authority field; an explicit stamp is refused outright if it claims
// the shared identity, because the hardened backend has no such launch to make.
func TestSupervisorIdentityDispositionSeparatesOrdinaryFromExplicit(t *testing.T) {
	withLinuxProcessIsolation(t)

	uid, gid, err := currentProcessIdentity()
	require.NoError(t, err)

	ordinary := supervisorConfig{
		OrdinaryExecution: true, SharedIdentity: true,
		IsolationUID: uid, IsolationGID: gid,
	}
	require.NoError(t, validateSupervisorIdentityDisposition(ordinary))

	foreign := ordinary
	foreign.IsolationUID = uid + 1
	require.ErrorContains(t, validateSupervisorIdentityDisposition(foreign),
		"ordinary supervisor identity disposition is invalid")

	authoritative := ordinary
	authoritative.StandaloneAuthority = true
	require.ErrorContains(t, validateSupervisorIdentityDisposition(authoritative),
		"ordinary supervisor identity disposition is invalid")

	require.NoError(t, validateSupervisorIdentityDisposition(supervisorConfig{
		IsolationUID: 65534, IsolationGID: 65534, StandaloneAuthority: true,
	}))
	require.ErrorContains(t, validateSupervisorIdentityDisposition(supervisorConfig{
		IsolationUID: 65534, IsolationGID: 65534, SharedIdentity: true,
	}), "explicit supervisor identity disposition cannot claim a shared identity")
}
