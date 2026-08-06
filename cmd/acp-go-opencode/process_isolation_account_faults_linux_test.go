//go:build linux

package main

import (
	"context"
	"errors"
	"os/user"
	"slices"
	"strings"
	"testing"
)

const (
	isolationCovPasswd = "acp-cov:x:20001:20002::/var/lib/acp-cov:/usr/sbin/nologin\n"
	isolationCovGroup  = "acp-cov:x:20002:\n"
	isolationCovStatus = "acp-cov L 2026-08-05 0 99999 7 -1\n"
	isolationCovDenial = "User acp-cov is not allowed to run sudo on runner.\n"
)

// isolationCovAccount is the target account every case in this file claims,
// matching the passwd and group records the scripted probes report.
func isolationCovAccount() *user.User {
	return &user.User{Username: "acp-cov", Uid: "20001", Gid: "20002", HomeDir: "/var/lib/acp-cov"}
}

// isolationCovAuthorityProbes installs scripted replacements for both
// account-authority probes and restores the production runners when the test
// ends. Every invocation is appended to the returned slice pointer so a test
// can prove which operating-system databases were interrogated, and in which
// order, before the refusal.
func isolationCovAuthorityProbes(
	t *testing.T,
	reply func(path string, args []string) ([]byte, error),
) *[][]string {
	t.Helper()

	command := processIsolationAccountAuthorityCommand
	combined := processIsolationAccountAuthorityCombined

	t.Cleanup(func() {
		processIsolationAccountAuthorityCommand = command
		processIsolationAccountAuthorityCombined = combined
	})

	calls := new([][]string)
	record := func(_ context.Context, path string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{path}, args...))

		return reply(path, args)
	}
	processIsolationAccountAuthorityCommand = record
	processIsolationAccountAuthorityCombined = record

	return calls
}

// isolationCovProvenAuthority answers every account-authority probe the way a
// correctly provisioned target account would, so a case can break exactly one
// stage and attribute the refusal to it.
func isolationCovProvenAuthority(path string, args []string) ([]byte, error) {
	switch {
	case path == "/usr/bin/sudo":
		return []byte(isolationCovDenial), errors.New("exit status 1")
	case path == "/usr/bin/passwd":
		return []byte(isolationCovStatus), nil
	case args[0] == "group":
		return []byte(isolationCovGroup), nil
	default:
		return []byte(isolationCovPasswd), nil
	}
}

// TestTargetAccountAuthorityRefusesEveryUnprovenStage proves that target
// account validation re-derives every fact from the live operating-system
// account databases and refuses the account as soon as one stage cannot be
// proven, rather than trusting the *user.User the caller already resolved. It
// also proves the probes stop at the stage that failed, so a refused account
// is never interrogated further.
func TestTargetAccountAuthorityRefusesEveryUnprovenStage(t *testing.T) {
	stageFailure := errors.New("probe unavailable")
	enumerateAccounts := []string{"/usr/bin/getent", "passwd"}
	enumerateGroups := []string{"/usr/bin/getent", "group"}
	readStatus := []string{"/usr/bin/passwd", "-S", "acp-cov"}
	readSudo := []string{"/usr/bin/sudo", "-n", "-U", "acp-cov", "-l"}

	for name, testCase := range map[string]struct {
		reply     func(path string, args []string) ([]byte, error)
		wantError string
		wantCalls [][]string
	}{
		"account enumeration unavailable": {
			reply:     func(string, []string) ([]byte, error) { return nil, stageFailure },
			wantError: "enumerate operating-system accounts",
			wantCalls: [][]string{enumerateAccounts},
		},
		"group enumeration unavailable": {
			reply: func(path string, args []string) ([]byte, error) {
				if args[0] == "group" {
					return nil, stageFailure
				}

				return isolationCovProvenAuthority(path, args)
			},
			wantError: "enumerate operating-system groups",
			wantCalls: [][]string{enumerateAccounts, enumerateGroups},
		},
		"account is not private": {
			reply: func(path string, args []string) ([]byte, error) {
				if path == "/usr/bin/getent" && args[0] == "passwd" {
					return []byte(isolationCovPasswd +
						"other:x:20001:30000::/nonexistent:/usr/sbin/nologin\n"), nil
				}

				return isolationCovProvenAuthority(path, args)
			},
			wantError: "is shared by another operating-system account",
			wantCalls: [][]string{enumerateAccounts, enumerateGroups},
		},
		"password status unavailable": {
			reply: func(path string, args []string) ([]byte, error) {
				if path == "/usr/bin/passwd" {
					return nil, stageFailure
				}

				return isolationCovProvenAuthority(path, args)
			},
			wantError: "read target account password status",
			wantCalls: [][]string{enumerateAccounts, enumerateGroups, readStatus},
		},
		"password is not locked": {
			reply: func(path string, args []string) ([]byte, error) {
				if path == "/usr/bin/passwd" {
					return []byte("acp-cov P 2026-08-05 0 99999 7 -1\n"), nil
				}

				return isolationCovProvenAuthority(path, args)
			},
			wantError: `target account "acp-cov" must have a locked password`,
			wantCalls: [][]string{enumerateAccounts, enumerateGroups, readStatus},
		},
		"sudo policy cannot be proven absent": {
			reply: func(path string, args []string) ([]byte, error) {
				if path == "/usr/bin/sudo" {
					return []byte("sudo: unknown user acp-cov\n"), errors.New("exit status 1")
				}

				return isolationCovProvenAuthority(path, args)
			},
			wantError: `cannot prove target account "acp-cov" has no sudo policy`,
			wantCalls: [][]string{enumerateAccounts, enumerateGroups, readStatus, readSudo},
		},
		"sudo policy exists": {
			reply: func(path string, args []string) ([]byte, error) {
				if path == "/usr/bin/sudo" {
					return []byte("(root) NOPASSWD: /bin/sh\n"), nil
				}

				return isolationCovProvenAuthority(path, args)
			},
			wantError: `target account "acp-cov" has a sudo policy`,
			wantCalls: [][]string{enumerateAccounts, enumerateGroups, readStatus, readSudo},
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := isolationCovAuthorityProbes(t, testCase.reply)

			err := validateTargetAccountAuthority(isolationCovAccount(), 20001, 20002)
			if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("authority error = %v, want one containing %q", err, testCase.wantError)
			}

			if !slices.EqualFunc(*calls, testCase.wantCalls, slices.Equal) {
				t.Fatalf("authority probes = %q, want %q", *calls, testCase.wantCalls)
			}
		})
	}
}

// TestTargetAccountAuthorityAcceptsOnlyAFullyProvenAccount proves the account
// is adopted only after every stage answered for it: the passwd and group
// databases name it as a private non-login account, its password is locked,
// and sudo itself reports it has no policy. All four probes must have run.
func TestTargetAccountAuthorityAcceptsOnlyAFullyProvenAccount(t *testing.T) {
	calls := isolationCovAuthorityProbes(t, isolationCovProvenAuthority)

	if err := validateTargetAccountAuthority(isolationCovAccount(), 20001, 20002); err != nil {
		t.Fatalf("fully proven target account was refused: %v", err)
	}

	want := [][]string{
		{"/usr/bin/getent", "passwd"},
		{"/usr/bin/getent", "group"},
		{"/usr/bin/passwd", "-S", "acp-cov"},
		{"/usr/bin/sudo", "-n", "-U", "acp-cov", "-l"},
	}
	if !slices.EqualFunc(*calls, want, slices.Equal) {
		t.Fatalf("authority probes = %q, want %q", *calls, want)
	}
}

// TestAccountAuthorityProbesRunWithAFixedEnvironment proves both probes
// execute with a fixed C-locale environment and a fixed PATH, so a poisoned
// supervisor environment can neither localise nor redirect what the account
// databases report, and that only the combined probe carries a refusal written
// to stderr back to its caller — which is why the sudo stage uses it.
func TestAccountAuthorityProbesRunWithAFixedEnvironment(t *testing.T) {
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")

	const wantEnvironment = "LANG=C\nLC_ALL=C\nPATH=/usr/bin:/bin"

	for name, probe := range map[string]func(context.Context, string, ...string) ([]byte, error){
		"output":   runAccountAuthorityCommand,
		"combined": runAccountAuthorityCombined,
	} {
		t.Run(name, func(t *testing.T) {
			output, err := probe(t.Context(), "/usr/bin/env")
			if err != nil {
				t.Fatalf("run account authority probe: %v", err)
			}

			got := strings.Split(strings.TrimSpace(string(output)), "\n")
			slices.Sort(got)

			if strings.Join(got, "\n") != wantEnvironment {
				t.Fatalf("probe environment = %q, want %q", got, wantEnvironment)
			}
		})
	}

	const script = "echo policy; echo refusal >&2"

	stdoutOnly, err := runAccountAuthorityCommand(t.Context(), "/usr/bin/env", "sh", "-c", script)
	if err != nil {
		t.Fatalf("run stdout-only probe: %v", err)
	}

	if !strings.Contains(string(stdoutOnly), "policy") || strings.Contains(string(stdoutOnly), "refusal") {
		t.Fatalf("stdout-only probe output = %q, want the stdout line without the stderr line", stdoutOnly)
	}

	both, err := runAccountAuthorityCombined(t.Context(), "/usr/bin/env", "sh", "-c", script)
	if err != nil {
		t.Fatalf("run combined probe: %v", err)
	}

	if !strings.Contains(string(both), "policy") || !strings.Contains(string(both), "refusal") {
		t.Fatalf("combined probe output = %q, want both the stdout and the stderr line", both)
	}
}

// TestValidatePrivateTargetAccountIgnoresUnrelatedRecords proves the private
// account proof only consults records that claim the target uid or gid, and
// still refuses when the target itself is absent from either database — an
// unmatched target must never be read as "nothing suspicious found".
func TestValidatePrivateTargetAccountIgnoresUnrelatedRecords(t *testing.T) {
	account := isolationCovAccount()
	noise := "root:x:0:0:root:/root:/bin/bash\ndaemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin\n"
	groupNoise := "root:x:0:\nadm:x:4:syslog\n"

	if err := validatePrivateTargetAccount(
		[]byte(noise+isolationCovPasswd), []byte(groupNoise+isolationCovGroup),
		account, 20001, 20002,
	); err != nil {
		t.Fatalf("unrelated passwd and group records were not ignored: %v", err)
	}

	if err := validatePrivateTargetAccount(
		[]byte(noise), []byte(groupNoise+isolationCovGroup), account, 20001, 20002,
	); err == nil || !strings.Contains(err.Error(), "resolve to 0 target accounts") {
		t.Fatalf("absent passwd record error = %v", err)
	}

	if err := validatePrivateTargetAccount(
		[]byte(noise+isolationCovPasswd), []byte(groupNoise), account, 20001, 20002,
	); err == nil || !strings.Contains(err.Error(), "resolves to 0 target groups") {
		t.Fatalf("absent group record error = %v", err)
	}
}
