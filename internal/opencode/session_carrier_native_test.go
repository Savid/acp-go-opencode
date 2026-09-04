//go:build integration

// The carrier's behavioral proofs live in their own file rather than beside
// internal/opencode/session_carrier.go's unit tests for a durable reason: they
// carry `//go:build integration`, so a mirror file would either drag the tag
// onto the unit tests or leave a tagged and an untagged half sharing one name.
package opencode

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	envNativeHarnessPath = "ACP_GO_OPENCODE_HARNESS_PATH"
	carrierProbeTool     = "acp-go-opencode-carrier-probe"
)

// nativeShellResult is the part of a native /session/{id}/shell response the
// carrier proofs read: the output of the command that actually ran.
type nativeShellResult struct {
	Parts []struct {
		Type  string `json:"type"`
		State struct {
			Output string `json:"output"`
		} `json:"state"`
	} `json:"parts"`
}

// runNativeShell executes one command at the real native shell boundary. It
// spends no model tokens: OpenCode runs the command directly and only records
// the surrounding message.
func runNativeShell(ctx context.Context, t *testing.T, client Client, sessionID string, command string) (string, error) {
	t.Helper()

	scope, ok := client.(*openCodeServer)
	require.True(t, ok, "the native shell boundary is only reachable from a directory scope")

	var result nativeShellResult

	err := scope.doJSON(ctx, http.MethodPost,
		"/session/"+url.PathEscape(sessionID)+"/shell", nil,
		map[string]any{"command": command, "agent": "build"}, &result)
	if err != nil {
		return "", err
	}

	output := make([]string, 0, len(result.Parts))
	for _, part := range result.Parts {
		if part.Type == "tool" && part.State.Output != "" {
			output = append(output, part.State.Output)
		}
	}

	return strings.Join(output, "\n"), nil
}

func requireNativeCarrierIntegration(t *testing.T) string {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run native OpenCode carrier proofs", envRunIntegration)
	}

	path := os.Getenv(envNativeHarnessPath)
	if path == "" {
		path = opencodeExecutableName
	}

	resolved, err := exec.LookPath(path)
	require.NoError(t, err, "find the OpenCode CLI")

	return resolved
}

// separatorFreeTestRoot hands back a temporary root that holds no search-path
// separator, whatever the ambient temporary directory happens to be named.
//
// The two halves of the carrier are legal in different alphabets, and these
// proofs have to state each one honestly. An operation's directories are
// adapter input: a separator-bearing `extraPathDirs` is refused at the ACP
// boundary before any native session exists, so a directory that inherited a
// separator from the ambient root would state a configuration this adapter
// cannot be asked to serve. The configured login shell is the opposite — it is
// OpenCode's own input and OpenCode accepts separators in it — so the shells
// under proof are given their separators here, deliberately, rather than
// inheriting them from wherever the suite happens to run.
func separatorFreeTestRoot(t *testing.T) string {
	t.Helper()

	if root := t.TempDir(); !strings.ContainsRune(root, os.PathListSeparator) {
		return root
	}

	root, err := os.MkdirTemp(filepath.Join(string(filepath.Separator), "tmp"), "acp-go-opencode-carrier-")
	require.NoError(t, err)
	require.NotContains(t, root, string(os.PathListSeparator))
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	return root
}

func writeCarrierProbeTool(t *testing.T, dir string, marker string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, carrierProbeTool),
		[]byte("#!/bin/sh\necho "+marker+"\n"), 0o700))
}

// carrierProbeCommand reports the environment the command itself resolves
// against, which is the only environment that decides which binary runs.
const carrierProbeCommand = `printf 'PATH=%s\n' "$PATH"
printf 'SHELL_KIND=%s\n' "$(basename "$0")"
printf 'TOKEN=<%s>\n' "$WAGIE_API_TOKEN"
printf 'CLEARED=<%s>\n' "${CARRIER_CLEARED-UNSET}"
printf 'STARTUP=%s\n' "$CARRIER_STARTUP_RAN"
printf 'ADAPTER_VARS=%s\n' "$(env | grep -c '^ACP_GO' || true)"
printf 'RESOLVED=%s\n' "$(command -v ` + carrierProbeTool + `)"
` + carrierProbeTool

type nativeCarrierProbe struct {
	runtime    Client
	executable string
	root       string
	work       string
	shadow     string
	first      string
	second     string
}

// startNativeCarrierRuntime launches one real shared OpenCode runtime whose
// login shell startup file prepends a directory that shadows the operation
// tool. That shadow is the whole point: an environment-only carrier loses to it.
func startNativeCarrierRuntime(t *testing.T) nativeCarrierProbe {
	t.Helper()
	executable := requireNativeCarrierIntegration(t)

	root := separatorFreeTestRoot(t)
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	probe := nativeCarrierProbe{
		executable: executable,
		root:       root,
		work:       filepath.Join(root, "work"),
		shadow:     filepath.Join(home, "shadow"),
		first:      filepath.Join(root, "first"),
		second:     filepath.Join(root, "second"),
	}
	require.NoError(t, os.MkdirAll(probe.work, 0o700))
	writeCarrierProbeTool(t, probe.shadow, "SHADOW")
	writeCarrierProbeTool(t, probe.first, "OPERATION-FIRST")
	writeCarrierProbeTool(t, probe.second, "OPERATION-SECOND")

	startup := `export PATH="` + probe.shadow + `:$PATH"` + "\nexport CARRIER_STARTUP_RAN=yes\n"
	for _, name := range []string{".zshrc", ".bashrc", ".zshenv", ".bash_profile"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte(startup), 0o600))
	}

	shell := "/bin/zsh"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/bash"
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	runtime, err := StartServer(ctx, StartOptions{
		Root:           filepath.Join(root, "runtime"),
		ExecutablePath: executable,
		NativeEnvironment: func() map[string]string {
			return map[string]string{
				"PATH":  "/usr/bin:/bin:/usr/sbin:/sbin",
				"HOME":  home,
				"SHELL": shell,
			}
		},
		HealthTimeout: 120 * time.Second,
		PluginSeedDir: sharedPluginSeedDir(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	probe.runtime = runtime

	return probe
}

func TestNativeCarrierDoesNotPersistOperationEnvironment(t *testing.T) {
	probe := startNativeCarrierRuntime(t)
	const operationValue = "operation-capability-must-not-persist"
	brokerAuthorization := probe.runtime.(*openCodeServer).sessionCarrierBroker.token

	scope, sessionID := probe.session(t, map[string]string{
		"WAGIE_API_TOKEN":          operationValue,
		"SECOND_SECRET":            "second-operation-value-must-not-persist",
		"OPENCODE_SERVER_PASSWORD": "carrier-must-not-restore-private-runtime-env",
	})
	scopedServer := scope.(*openCodeServer)
	require.NotEmpty(t, scopedServer.sessionCarrierReference)

	output, err := runNativeShell(t.Context(), t, scope, sessionID,
		`test -n "$WAGIE_API_TOKEN" && test -n "$SECOND_SECRET" && `+
			`test -z "${OPENCODE_CONFIG_CONTENT:-}" && test -z "${OPENCODE_PID:-}" && `+
			`test -z "${OPENCODE_SERVER_PASSWORD:-}" && test -z "${OPENCODE_SERVER_USERNAME:-}" && `+
			`test -z "${XDG_CONFIG_HOME:-}" && test -z "${XDG_DATA_HOME:-}" && printf 'CARRIER=present\n'`)
	require.NoError(t, err)
	require.Equal(t, "present", carrierProbeField(t, output, "CARRIER"))
	require.NoError(t, scope.Close(t.Context()))
	require.NoError(t, probe.runtime.Shutdown(t.Context()))

	nativeLog, err := os.ReadFile(filepath.Join(scopedServer.xdg.Data, "opencode", "log", "opencode.log"))
	require.NoError(t, err)
	require.NotContains(t, string(nativeLog), operationValue)
	require.NotContains(t, string(nativeLog), "second-operation-value-must-not-persist")

	require.NoError(t, filepath.WalkDir(probe.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		require.NotContains(t, string(content), operationValue, "operation environment persisted in %s", path)
		require.NotContains(t, string(content), "second-operation-value-must-not-persist",
			"operation environment persisted in %s", path)
		require.NotContains(t, string(content), brokerAuthorization,
			"broker authorization persisted in %s", path)

		return nil
	}))
}

func TestNativeCarrierCannotDrivePeerShellOrReadPeerBroker(t *testing.T) {
	probe := startNativeCarrierRuntime(t)
	firstScope, firstID := probe.session(t, map[string]string{"WAGIE_API_TOKEN": "first-bearer"})
	secondScope, secondID := probe.session(t, map[string]string{"WAGIE_API_TOKEN": "peer-bearer"})
	peer := secondScope.(*openCodeServer)
	root := probe.runtime.(*openCodeServer)
	peerResponse := filepath.Join(probe.root, "peer-shell-response")
	brokerResponse := filepath.Join(probe.root, "peer-broker-response")
	command := "peer_status=$(/usr/bin/curl -sS -o " + nativeShellWord(peerResponse) +
		" -w '%{http_code}' -u \"${OPENCODE_SERVER_USERNAME:-}:${OPENCODE_SERVER_PASSWORD:-}\"" +
		" -H 'Content-Type: application/json' --data '{\"command\":\"printf PEER_SHELL_REACHED\",\"agent\":\"build\"}' " +
		nativeShellWord(root.baseURL+"/session/"+url.PathEscape(secondID)+"/shell") + "); " +
		"broker_status=$(/usr/bin/curl -sS -o " + nativeShellWord(brokerResponse) +
		" -w '%{http_code}' -H 'Authorization: Bearer proof-token-cannot-authorize' " +
		nativeShellWord(root.sessionCarrierBroker.endpoint+sessionCarrierRoute+peer.sessionCarrierReference) + "); " +
		"test \"$peer_status\" = 401 && test \"$broker_status\" = 401 && " +
		"! grep -q PEER_SHELL_REACHED " + nativeShellWord(peerResponse) + " && " +
		"! grep -q peer-bearer " + nativeShellWord(brokerResponse) + " && printf 'DENIED=yes\\n'"

	output, err := runNativeShell(t.Context(), t, firstScope, firstID, command)
	require.NoError(t, err)
	require.Equal(t, "yes", carrierProbeField(t, output, "DENIED"))
	requireNoRuntimeWideCarrierShellState(t, probe.runtime)
}

func nativeShellWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (p nativeCarrierProbe) session(t *testing.T, env map[string]string, dirs ...string) (Client, string) {
	t.Helper()

	scope, err := p.runtime.Scope(t.Context(), ScopeOptions{
		Directory: p.work, Env: env, ExtraPathDirs: dirs,
	})
	require.NoError(t, err)

	native, err := scope.CreateSession(t.Context(), "carrier")
	require.NoError(t, err)

	return scope, native.ID
}

func carrierProbeField(t *testing.T, output string, name string) string {
	t.Helper()

	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), name+"="); ok {
			return value
		}
	}

	t.Fatalf("field %q missing from native shell output:\n%s", name, output)

	return ""
}

// TestNativeCarrierPutsOperationDirectoriesFirstAtTheRealShellBoundary is the
// acceptance proof for the search-path half. It calls the real no-token
// /session/{id}/shell boundary of a real OpenCode process and asserts the
// environment the command resolved against, not the metadata that produced it.
func TestNativeCarrierPutsOperationDirectoriesFirstAtTheRealShellBoundary(t *testing.T) {
	probe := startNativeCarrierRuntime(t)

	scope, sessionID := probe.session(t,
		map[string]string{"WAGIE_API_TOKEN": "bearer-first", "CARRIER_CLEARED": ""},
		probe.first, probe.second, probe.first)

	output, err := runNativeShell(t.Context(), t, scope, sessionID, carrierProbeCommand)
	require.NoError(t, err)

	require.Equal(t, "yes", carrierProbeField(t, output, "STARTUP"),
		"the login startup file must still run, or the proof is not against a rewritten PATH")

	entries := filepath.SplitList(carrierProbeField(t, output, "PATH"))
	require.Equal(t, []string{probe.first, probe.second, probe.first}, entries[:3],
		"the final PATH must start with the operation directories, in order, duplicates kept")

	for _, entry := range entries {
		require.NotEmpty(t, entry, "the final PATH must not carry an empty component")
	}

	require.Equal(t, "0", carrierProbeField(t, output, "ADAPTER_VARS"),
		"the carrier leaves no variable of the adapter's own in the command's environment")

	require.Equal(t, filepath.Join(probe.first, carrierProbeTool), carrierProbeField(t, output, "RESOLVED"))
	require.Contains(t, output, "OPERATION-FIRST")
	require.NotContains(t, output, "SHADOW")
	require.Equal(t, "<bearer-first>", carrierProbeField(t, output, "TOKEN"))
	require.Equal(t, "<>", carrierProbeField(t, output, "CLEARED"),
		"an empty carrier value must reach the command as an empty value, not as an absent one")
}

// TestNativeCarrierKeepsTwoSessionsAndOneRotationApart covers concurrent
// sessions, a second turn, rotation, and the untouched peer, all at the real
// native boundary.
func TestNativeCarrierKeepsTwoSessionsAndOneRotationApart(t *testing.T) {
	probe := startNativeCarrierRuntime(t)

	firstScope, firstID := probe.session(t,
		map[string]string{"WAGIE_API_TOKEN": "bearer-first", "CARRIER_CLEARED": "first-only"}, probe.first)
	secondScope, secondID := probe.session(t,
		map[string]string{"WAGIE_API_TOKEN": "bearer-second"}, probe.second)

	firstOutput, err := runNativeShell(t.Context(), t, firstScope, firstID, carrierProbeCommand)
	require.NoError(t, err)
	secondOutput, err := runNativeShell(t.Context(), t, secondScope, secondID, carrierProbeCommand)
	require.NoError(t, err)

	require.Contains(t, firstOutput, "OPERATION-FIRST")
	require.Equal(t, "<bearer-first>", carrierProbeField(t, firstOutput, "TOKEN"))
	require.NotContains(t, firstOutput, "bearer-second")

	require.Contains(t, secondOutput, "OPERATION-SECOND")
	require.Equal(t, "<bearer-second>", carrierProbeField(t, secondOutput, "TOKEN"))
	require.NotContains(t, secondOutput, "bearer-first")
	require.Equal(t, "<UNSET>", carrierProbeField(t, secondOutput, "CLEARED"),
		"a peer must not inherit a key it never asked for")

	// A second turn on the same session keeps the same carrier.
	repeat, err := runNativeShell(t.Context(), t, firstScope, firstID, carrierProbeCommand)
	require.NoError(t, err)
	require.Equal(t, "<bearer-first>", carrierProbeField(t, repeat, "TOKEN"))
	require.Contains(t, repeat, "OPERATION-FIRST")

	// Rebinding the same native session with a rotated bearer and directory is
	// what a restored or reassigned operation does.
	rotatedScope, err := probe.runtime.Scope(t.Context(), ScopeOptions{
		Directory: probe.work,
		Env:       map[string]string{"WAGIE_API_TOKEN": "bearer-rotated"},
		ExtraPathDirs: []string{
			probe.second,
		},
	})
	require.NoError(t, err)

	adopted, err := rotatedScope.GetSession(t.Context(), firstID)
	require.NoError(t, err)
	require.Equal(t, firstID, adopted.ID)

	rotated, err := runNativeShell(t.Context(), t, rotatedScope, firstID, carrierProbeCommand)
	require.NoError(t, err)
	require.Equal(t, "<bearer-rotated>", carrierProbeField(t, rotated, "TOKEN"))
	require.NotContains(t, rotated, "bearer-first")
	require.Equal(t, "<UNSET>", carrierProbeField(t, rotated, "CLEARED"),
		"the replaced environment must not retain a stale key")
	require.Contains(t, rotated, "OPERATION-SECOND")

	// The peer is untouched by its neighbour's rotation.
	peer, err := runNativeShell(t.Context(), t, secondScope, secondID, carrierProbeCommand)
	require.NoError(t, err)
	require.Equal(t, "<bearer-second>", carrierProbeField(t, peer, "TOKEN"))
	require.NotContains(t, peer, "bearer-rotated")
}

// TestNativeCarrierFailsClosedWithoutItsNamespace proves the plugin refuses the
// shell operation rather than degrading to the shared process environment when
// the addressed session carries no carrier.
func TestNativeCarrierFailsClosedWithoutItsNamespace(t *testing.T) {
	probe := startNativeCarrierRuntime(t)

	scope, sessionID := probe.session(t, map[string]string{"WAGIE_API_TOKEN": "bearer-first"}, probe.first)

	server, ok := scope.(*openCodeServer)
	require.True(t, ok)

	// Remove the adapter namespace the way a foreign writer would.
	var ignored NativeSession
	require.NoError(t, server.doJSON(t.Context(), http.MethodPatch,
		routeSession+"/"+url.PathEscape(sessionID), nil,
		map[string]any{fieldMetadata: map[string]any{sessionCarrierMetadataKey: nil}}, &ignored))

	_, err := runNativeShell(t.Context(), t, scope, sessionID, "echo SHOULD-NOT-RUN")
	require.Error(t, err, "a missing carrier must fail the shell operation")
}

// TestNativeCarrierLeavesExecutableLookupAlone proves the carrier never reaches
// the process that resolves the OpenCode executable and its version: a
// directory shipping a counterfeit `opencode` cannot be reached from a session.
func TestNativeCarrierLeavesExecutableLookupAlone(t *testing.T) {
	probe := startNativeCarrierRuntime(t)

	counterfeit := separatorFreeTestRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(counterfeit, opencodeExecutableName),
		[]byte("#!/bin/sh\nexit 97\n"), 0o700))

	scope, sessionID := probe.session(t, map[string]string{}, counterfeit)

	output, err := runNativeShell(t.Context(), t, scope, sessionID,
		fmt.Sprintf("printf 'FIRST=%%s\\n' \"$(command -v %s)\"", opencodeExecutableName))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(counterfeit, opencodeExecutableName), carrierProbeField(t, output, "FIRST"),
		"the session's own shell does see its directories")

	// The executable the runtime resolved and version-probed is the genuine
	// binary, not the counterfeit that the session's own shell now prefers: the
	// version it reported at readiness is the version the real binary reports,
	// and a counterfeit that exits 97 could not have reported one at all.
	reported, err := exec.CommandContext(t.Context(), probe.executable, "--version").Output()
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(reported)), probe.runtime.NativeVersion(),
		"the runtime version-probed the real executable, before any session existed")

	// It is also still serving, which the counterfeit could not do.
	peer, peerID := probe.session(t, map[string]string{})
	peerOutput, err := runNativeShell(t.Context(), t, peer, peerID, "printf 'ALIVE=%s\\n' yes")
	require.NoError(t, err)
	require.Equal(t, "yes", carrierProbeField(t, peerOutput, "ALIVE"))
}

// TestNativeCarrierKeepsTheLoginShellForASessionWithoutDirectories is the
// acceptance proof for the default configuration: a session that carries an
// environment and no directories at all.
//
// The wrapper governs every operation of the runtime, so this session's shell
// tools have to stay on the user's login shell with the user's login search
// path. Falling to /bin/sh with the bare runtime path would silently strip a
// plain ACP session of everything the user installed.
func TestNativeCarrierKeepsTheLoginShellForASessionWithoutDirectories(t *testing.T) {
	probe := startNativeCarrierRuntime(t)

	scope, sessionID := probe.session(t, map[string]string{"WAGIE_API_TOKEN": "bearer-plain"})

	output, err := runNativeShell(t.Context(), t, scope, sessionID, carrierProbeCommand)
	require.NoError(t, err)

	require.Equal(t, "opencode", carrierProbeField(t, output, "SHELL_KIND"),
		"the command must run under the login shell OpenCode itself would have started")
	require.Equal(t, "yes", carrierProbeField(t, output, "STARTUP"),
		"the user's startup files must still run for a session that asked for no directories")
	require.Equal(t, "<bearer-plain>", carrierProbeField(t, output, "TOKEN"))
	require.Equal(t, "0", carrierProbeField(t, output, "ADAPTER_VARS"),
		"the carrier leaves no variable of the adapter's own in the command's environment")

	entries := filepath.SplitList(carrierProbeField(t, output, "PATH"))
	require.Equal(t, probe.shadow, entries[0],
		"with no operation directories the login search path is the search path, unchanged")
	require.Greater(t, len(entries), 1)

	for _, entry := range entries {
		require.NotEmpty(t, entry, "the final PATH must not carry an empty component")
	}

	require.Equal(t, filepath.Join(probe.shadow, carrierProbeTool), carrierProbeField(t, output, "RESOLVED"))
	require.Contains(t, output, "SHADOW", "a tool the user installed still resolves")
}

// nativeScopeShellProbe is one real runtime serving several workspace scopes
// whose own configurations name different shells.
type nativeScopeShellProbe struct {
	runtime Client
	home    string
	scopes  map[string]string
	shells  map[string]string
	first   string
	second  string
}

// scopeShellProbeCommand reports which shell actually ran. The startup file each
// shell sources is the ground truth: it is set by the shell itself, not by
// anything the adapter could have handed it.
const scopeShellProbeCommand = `printf 'RC=<%s>\n' "${CARRIER_RC-}"
printf 'ZSH=%s\n' "${ZSH_VERSION-}"
printf 'BASH=%s\n' "${BASH_VERSION-}"
printf 'SHELL_KIND=%s\n' "$(basename "$0")"
printf 'TOKEN=<%s>\n' "$WAGIE_API_TOKEN"
printf 'ADAPTER_VARS=%s\n' "$(env | grep -c '^ACP_GO' || true)"
printf 'PATH=%s\n' "$PATH"
printf 'RESOLVED=%s\n' "$(command -v ` + carrierProbeTool + ` || true)"
command -v ` + carrierProbeTool + ` >/dev/null 2>&1 && ` + carrierProbeTool + ` || true
`

// nativeScopeShells names one workspace scope's own configured shell.
//
// The two login shells are the collision this suite exists for. The custom one
// is a third login shell at a path of its own, which is what proves the exact
// configured executable reaches the boundary rather than merely the right kind.
// The colon one is that same proof at a path the search-path separator runs
// straight through — a path OpenCode accepts and configures, and the one a
// shell carried as a single search-path component truncated. The non-login one
// is the shell the wrapper must decline to stand in for.
func nativeScopeShells(t *testing.T, root string) map[string]string {
	t.Helper()

	shim := func(directory string, marker string) string {
		t.Helper()
		dir := filepath.Join(root, directory)
		require.NoError(t, os.MkdirAll(dir, 0o700))

		path := filepath.Join(dir, "zsh")
		require.NoError(t, os.WriteFile(path,
			[]byte("#!/bin/sh\nprintf '"+marker+"=%s\\n' yes\nexec /bin/zsh \"$@\"\n"), 0o700))

		return path
	}

	separator := string(os.PathListSeparator)

	return map[string]string{
		"zsh":   "/bin/zsh",
		"bash":  "/bin/bash",
		"plain": "/bin/sh",

		"custom": shim("custom-shell", "CUSTOM"),
		"colon":  shim("colon"+separator+"shell", "COLON"),
		"colons": shim("colons"+separator+separator+"shell"+separator+"deep", "COLONS"),
	}
}

// startNativeScopeShellRuntime launches one real shared runtime over several
// workspace directories, each carrying its own opencode.json naming its own
// shell. The process shell is deliberately none of them, so a shell that
// arrives from anywhere but the addressed operation's own scope is visible as a
// wrong answer rather than a lucky one.
func startNativeScopeShellRuntime(t *testing.T) nativeScopeShellProbe {
	t.Helper()
	executable := requireNativeCarrierIntegration(t)

	for _, shell := range []string{"/bin/zsh", "/bin/bash"} {
		if _, err := os.Stat(shell); err != nil {
			t.Skipf("this proof needs two distinct login shells; %s is missing", shell)
		}
	}

	root := separatorFreeTestRoot(t)
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	for name, marker := range map[string]string{
		".zshrc": "zshrc", ".zshenv": "zshenv", ".bashrc": "bashrc", ".bash_profile": "bash_profile",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name),
			[]byte("export CARRIER_RC="+marker+"\n"), 0o600))
	}

	probe := nativeScopeShellProbe{
		home:   home,
		scopes: map[string]string{},
		shells: nativeScopeShells(t, root),
		first:  filepath.Join(root, "first"),
		second: filepath.Join(root, "second"),
	}
	writeCarrierProbeTool(t, probe.first, "OPERATION-FIRST")
	writeCarrierProbeTool(t, probe.second, "OPERATION-SECOND")

	for name, shell := range probe.shells {
		directory := filepath.Join(root, name+"-scope")
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "opencode.json"),
			[]byte(`{"$schema":"https://opencode.ai/config.json","shell":"`+shell+`"}`), 0o600))
		probe.scopes[name] = directory
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	runtime, err := StartServer(ctx, StartOptions{
		Root:           filepath.Join(root, "runtime"),
		ExecutablePath: executable,
		NativeEnvironment: func() map[string]string {
			return map[string]string{
				"PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
				"HOME": home,
				// No scope's shell, and not a login shell at all.
				"SHELL": "/bin/sh",
			}
		},
		HealthTimeout: 120 * time.Second,
		PluginSeedDir: sharedPluginSeedDir(t),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })

	probe.runtime = runtime

	return probe
}

// session opens one addressed session inside one scope. With no dirs it is the
// default carrier shape, the one under which the operation publishes nothing of
// its own and the login shell is all the wrapper has to be told.
func (p nativeScopeShellProbe) session(t *testing.T, scope string, token string, dirs ...string) (Client, string) {
	t.Helper()

	directory, ok := p.scopes[scope]
	require.True(t, ok)

	client, err := p.runtime.Scope(t.Context(), ScopeOptions{
		Directory:     directory,
		Env:           map[string]string{"WAGIE_API_TOKEN": token},
		ExtraPathDirs: dirs,
	})
	require.NoError(t, err)

	native, err := client.CreateSession(t.Context(), "carrier")
	require.NoError(t, err)

	return client, native.ID
}

// requireScopeShell asserts which login shell really ran for one operation, and
// that this operation's own directories are still what its commands resolve
// against.
func requireScopeShell(t *testing.T, output string, want string, token string, dirs ...string) {
	t.Helper()

	require.Equal(t, "<"+want+"rc>", carrierProbeField(t, output, "RC"),
		"the startup file the shell itself sourced names the shell that ran")
	require.Equal(t, "opencode", carrierProbeField(t, output, "SHELL_KIND"))
	require.Equal(t, "<"+token+">", carrierProbeField(t, output, "TOKEN"))
	require.Equal(t, "0", carrierProbeField(t, output, "ADAPTER_VARS"))

	zsh, bash := carrierProbeField(t, output, "ZSH"), carrierProbeField(t, output, "BASH")
	if want == "zsh" {
		require.NotEmpty(t, zsh, "zsh must report its own version")
		require.Empty(t, bash)
	} else {
		require.NotEmpty(t, bash, "bash must report its own version")
		require.Empty(t, zsh)
	}

	entries := filepath.SplitList(carrierProbeField(t, output, "PATH"))
	for _, entry := range entries {
		require.NotEmpty(t, entry, "the final PATH must not carry an empty component")
		require.NotContains(t, entry, sessionCarrierPathMarkName,
			"the marker never reaches the command's search path")
	}

	if len(dirs) == 0 {
		require.NotEmpty(t, entries, "a session with no directories still has the login search path")
		require.Empty(t, carrierProbeField(t, output, "RESOLVED"),
			"a session that asked for no directories resolves nothing out of another operation's")

		return
	}

	require.Equal(t, dirs, entries[:len(dirs)],
		"this operation's own directories must still be what its commands resolve against")
	require.Equal(t, filepath.Join(dirs[0], carrierProbeTool), carrierProbeField(t, output, "RESOLVED"))
}

// TestNativeCarrierKeepsTwoWorkspaceScopeShellsApart is the acceptance proof for
// the login shell being addressed rather than runtime-wide.
//
// OpenCode builds one plugin instance per workspace scope and runs its config
// hook against that scope's own configuration. A runtime serving two scopes
// therefore resolves two login shells through one generated wrapper, and when
// the wrapper learned the shell from anywhere the whole runtime could reach,
// the second scope's config hook overwrote the first's and the first scope's
// sessions changed shell underneath themselves. This drives the real binary
// over both scopes, interleaved, with sessions that carry an environment and no
// directories at all — the exact configuration under which the operation
// publishes nothing of its own.
func TestNativeCarrierKeepsTwoWorkspaceScopeShellsApart(t *testing.T) {
	probe := startNativeScopeShellRuntime(t)

	// Both sessions exist before either runs, so both config hooks have already
	// resolved by the time the first operation reaches the shell boundary.
	zshScope, zshSession := probe.session(t, "zsh", "bearer-zsh")
	bashScope, bashSession := probe.session(t, "bash", "bearer-bash")

	first, err := runNativeShell(t.Context(), t, zshScope, zshSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, first, "zsh", "bearer-zsh")

	second, err := runNativeShell(t.Context(), t, bashScope, bashSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, second, "bash", "bearer-bash")

	// The whole regression: the neighbouring scope has now resolved its own
	// login shell, and this session must be exactly where it was.
	repeat, err := runNativeShell(t.Context(), t, zshScope, zshSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, repeat, "zsh", "bearer-zsh")

	// A second session inside the already-overwritten scope, created last, is
	// the reverse order of the same collision.
	laterScope, laterSession := probe.session(t, "zsh", "bearer-later")
	later, err := runNativeShell(t.Context(), t, laterScope, laterSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, later, "zsh", "bearer-later")

	// The same matrix with directories: each scope keeps its own login shell and
	// its own operation-first search path at the same time.
	zshDirs, zshDirsSession := probe.session(t, "zsh", "bearer-zsh-dirs", probe.first)
	bashDirs, bashDirsSession := probe.session(t, "bash", "bearer-bash-dirs", probe.second)

	carried, err := runNativeShell(t.Context(), t, zshDirs, zshDirsSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, carried, "zsh", "bearer-zsh-dirs", probe.first)
	require.Contains(t, carried, "OPERATION-FIRST")

	carried, err = runNativeShell(t.Context(), t, bashDirs, bashDirsSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, carried, "bash", "bearer-bash-dirs", probe.second)
	require.Contains(t, carried, "OPERATION-SECOND")

	// And the first scope again, now that three neighbours have resolved.
	repeat, err = runNativeShell(t.Context(), t, zshScope, zshSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, repeat, "zsh", "bearer-zsh")

	// Nothing of the runtime's holds a shell for a later scope to overwrite.
	requireNoRuntimeWideCarrierShellState(t, probe.runtime)
}

// TestNativeCarrierRunsTheExactConfiguredLoginShell proves the carrier reaches
// the boundary with the executable the scope configured, not merely with a
// shell of the same kind. Each scope names a login shell at a path of its own
// that announces itself before becoming zsh.
//
// The separator-bearing paths are the regression. ':' is legal in a POSIX
// filename and OpenCode configures such a shell without complaint, so a
// transport that wrote the shell as one search-path component truncated it at
// its first ':' and refused a configuration the native binary had already
// accepted. Each case runs A -> B -> A across the separator boundary, so a
// neighbouring scope resolving its own shell is visible if it leaks either way.
func TestNativeCarrierRunsTheExactConfiguredLoginShell(t *testing.T) {
	probe := startNativeScopeShellRuntime(t)

	separator := string(os.PathListSeparator)

	// A neighbour resolves first, so the only way a scope's own executable can
	// reach the boundary is by travelling with the operation.
	peerScope, peerSession := probe.session(t, "bash", "bearer-bash")
	peer, err := runNativeShell(t.Context(), t, peerScope, peerSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, peer, "bash", "bearer-bash")

	for _, marker := range []string{"CUSTOM", "COLON", "COLONS"} {
		require.NotContains(t, peer, marker+"=yes")
	}

	for _, test := range []struct {
		scope     string
		marker    string
		separated bool
	}{
		{scope: "custom", marker: "CUSTOM"},
		{scope: "colon", marker: "COLON", separated: true},
		{scope: "colons", marker: "COLONS", separated: true},
	} {
		t.Run(test.scope, func(t *testing.T) {
			configured := probe.shells[test.scope]
			if test.separated {
				require.Contains(t, configured, separator,
					"this case is only a proof if the configured executable path holds a separator")
			}

			scope, session := probe.session(t, test.scope, "bearer-"+test.scope, probe.first)
			output, err := runNativeShell(t.Context(), t, scope, session, scopeShellProbeCommand)
			require.NoError(t, err)

			require.Contains(t, output, test.marker+"=yes",
				"the exact executable the scope configured is the one that ran")
			requireScopeShell(t, output, "zsh", "bearer-"+test.scope, probe.first)
			require.Contains(t, output, "OPERATION-FIRST")

			// The address carried the path whole and then removed all of it: no
			// piece of the configured executable is left as a search-path
			// component of the command's own environment.
			entries := filepath.SplitList(carrierProbeField(t, output, "PATH"))
			for segment := range strings.SplitSeq(configured, separator) {
				require.NotContains(t, entries, segment)
			}

			// A -> B -> A across the separator boundary.
			back, err := runNativeShell(t.Context(), t, peerScope, peerSession, scopeShellProbeCommand)
			require.NoError(t, err)
			requireScopeShell(t, back, "bash", "bearer-bash")
			require.NotContains(t, back, test.marker+"=yes",
				"a neighbouring scope's configured executable must not reach this operation")

			repeat, err := runNativeShell(t.Context(), t, scope, session, scopeShellProbeCommand)
			require.NoError(t, err)
			require.Contains(t, repeat, test.marker+"=yes")
			requireScopeShell(t, repeat, "zsh", "bearer-"+test.scope, probe.first)
		})
	}

	requireNoRuntimeWideCarrierShellState(t, probe.runtime)
}

// TestNativeCarrierRunsTheExactConfiguredLoginShellUnderLoad overlaps the
// separator-bearing scopes rather than interleaving them. Reading a counted
// address back is per-operation work with no shared state to serialise, and
// this is what requires it to stay that way: four sessions across three scopes
// whose configured executables differ only in the path each was installed at,
// all resolving at once.
func TestNativeCarrierRunsTheExactConfiguredLoginShellUnderLoad(t *testing.T) {
	probe := startNativeScopeShellRuntime(t)

	const rounds = 3

	operations := []struct {
		scope  string
		marker string
		dir    string
	}{
		{scope: "colon", marker: "COLON", dir: probe.first},
		{scope: "colons", marker: "COLONS", dir: probe.second},
		{scope: "custom", marker: "CUSTOM", dir: probe.first},
		{scope: "colon", marker: "COLON", dir: probe.second},
	}

	clients := make([]Client, len(operations))
	sessions := make([]string, len(operations))

	for index, op := range operations {
		clients[index], sessions[index] = probe.session(t, op.scope, "bearer-"+op.scope, op.dir)
	}

	outputs := make([][]string, len(operations))

	var group sync.WaitGroup

	group.Add(len(operations))

	for index, op := range operations {
		go func() {
			defer group.Done()

			for round := range rounds {
				output, err := runNativeShell(t.Context(), t, clients[index], sessions[index], scopeShellProbeCommand)
				if !assert.NoErrorf(t, err, "%s round %d", op.scope, round) {
					return
				}

				outputs[index] = append(outputs[index], output)
			}
		}()
	}

	group.Wait()

	markers := []string{"CUSTOM", "COLON", "COLONS"}

	for index, op := range operations {
		require.Len(t, outputs[index], rounds)

		for _, output := range outputs[index] {
			requireScopeShell(t, output, "zsh", "bearer-"+op.scope, op.dir)
			require.Contains(t, output, op.marker+"=yes")

			for _, marker := range markers {
				if marker != op.marker {
					require.NotContains(t, output, marker+"=yes",
						"a concurrent scope's configured executable must not reach this operation")
				}
			}
		}
	}

	requireNoRuntimeWideCarrierShellState(t, probe.runtime)
}

// TestNativeCarrierLeavesANonLoginShellToOpenCode is the other half of the
// wrapper's fail-closed rule. A scope that configures a valid shell OpenCode
// runs without a login sequence must never be routed through the wrapper: there
// is no startup file to reproduce and no search path to reapply, so the
// environment the hook returns is already the environment the command runs
// under.
func TestNativeCarrierLeavesANonLoginShellToOpenCode(t *testing.T) {
	probe := startNativeScopeShellRuntime(t)

	// A login-shell neighbour resolves first and installs the wrapper for its
	// own scope, which is exactly the state a runtime-wide wrapper would leak.
	loginScope, loginSession := probe.session(t, "zsh", "bearer-zsh")
	login, err := runNativeShell(t.Context(), t, loginScope, loginSession, scopeShellProbeCommand)
	require.NoError(t, err)
	requireScopeShell(t, login, "zsh", "bearer-zsh")

	scope, session := probe.session(t, "plain", "bearer-plain", probe.second)

	output, err := runNativeShell(t.Context(), t, scope, session, scopeShellProbeCommand)
	require.NoError(t, err)

	require.Equal(t, "sh", carrierProbeField(t, output, "SHELL_KIND"),
		"OpenCode runs a non-login shell itself, under its own argument form")
	require.Equal(t, "<>", carrierProbeField(t, output, "RC"),
		"a non-login shell sources no startup file, which is why there is nothing to reproduce")
	require.Equal(t, "<bearer-plain>", carrierProbeField(t, output, "TOKEN"),
		"the addressed carrier still reaches a shell the wrapper stands aside for")
	require.Equal(t, "0", carrierProbeField(t, output, "ADAPTER_VARS"))

	entries := filepath.SplitList(carrierProbeField(t, output, "PATH"))
	require.Equal(t, probe.second, entries[0],
		"with no login sequence to rewrite it, the hook's own search path is final")

	for _, entry := range entries {
		require.NotContains(t, entry, sessionCarrierPathMarkName,
			"a scope the wrapper stands aside for is never handed a marker")
		require.NotContains(t, entry, sessionCarrierShellName)
	}

	require.Contains(t, output, "OPERATION-SECOND")
}

// TestNativeCarrierKeepsTwoWorkspaceScopesApartUnderLoad runs both scopes at
// once rather than in turn. Interleaving in time is the weaker shape the case
// above covers; overlapping in time is the shape a shared runtime actually
// serves, and it is the one a per-operation channel has to survive.
func TestNativeCarrierKeepsTwoWorkspaceScopesApartUnderLoad(t *testing.T) {
	probe := startNativeScopeShellRuntime(t)

	const rounds = 3

	type operation struct {
		scope string
		token string
	}

	operations := []operation{
		{scope: "zsh", token: "bearer-zsh"},
		{scope: "bash", token: "bearer-bash"},
		{scope: "zsh", token: "bearer-zsh-peer"},
		{scope: "bash", token: "bearer-bash-peer"},
	}

	clients := make([]Client, len(operations))
	sessions := make([]string, len(operations))

	for index, op := range operations {
		clients[index], sessions[index] = probe.session(t, op.scope, op.token)
	}

	outputs := make([][]string, len(operations))

	var group sync.WaitGroup

	group.Add(len(operations))

	for index, op := range operations {
		go func() {
			defer group.Done()

			for round := range rounds {
				output, err := runNativeShell(t.Context(), t, clients[index], sessions[index], scopeShellProbeCommand)
				if !assert.NoErrorf(t, err, "%s round %d", op.scope, round) {
					return
				}

				outputs[index] = append(outputs[index], output)
			}
		}()
	}

	group.Wait()

	for index, op := range operations {
		require.Len(t, outputs[index], rounds)

		for _, output := range outputs[index] {
			requireScopeShell(t, output, op.scope, op.token)
		}
	}

	requireNoRuntimeWideCarrierShellState(t, probe.runtime)
}

// requireNoRuntimeWideCarrierShellState walks the carrier's own generated tree
// and requires it to hold only the probe directory and wrapper. The bootstrap
// module and proof are erased after load so no live broker authorization stays
// on disk.
func requireNoRuntimeWideCarrierShellState(t *testing.T, runtime Client) {
	t.Helper()

	server, ok := runtime.(*openCodeServer)
	require.True(t, ok)
	require.Len(t, server.preparedTrees, 1, "the runtime owns one XDG residence")

	roots, err := filepath.Glob(filepath.Join(server.xdg.Root, ".session-carrier-*"))
	require.NoError(t, err)
	require.Len(t, roots, 1)

	var found []string

	require.NoError(t, filepath.WalkDir(roots[0], func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(roots[0], path)
		if err != nil {
			return err
		}

		found = append(found, relative)

		return nil
	}))

	require.ElementsMatch(t,
		[]string{".", sessionCarrierProbeDirName, sessionCarrierShellName},
		found,
		"the loaded runtime must retain neither broker authorization nor bootstrap proof")
}

// TestNativeRuntimeRefusesACarrierPluginItCannotLoad is the acceptance proof for
// the fail-closed property itself.
//
// OpenCode treats a plugin it cannot load as non-fatal: the server reaches
// readiness and every shell operation runs with no bearer, no operation
// directories, and no error. Nothing about the carrier is safe unless the
// runtime refuses to exist in that state, so this drives the real binary with a
// plugin URL that resolves to nothing and requires StartServer itself to fail.
func TestNativeRuntimeRefusesACarrierPluginItCannotLoad(t *testing.T) {
	executable := requireNativeCarrierIntegration(t)
	preserveSessionCarrierSeams(t)

	// Everything but the module is materialized, so the runtime registers a
	// plugin URL that resolves to nothing — the same shape an unreadable carrier
	// tree, a rejected seed, or a cleanup race produces.
	write := sessionCarrierWriteFile
	sessionCarrierWriteFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == sessionCarrierPluginFileName {
			return nil
		}

		return write(path, data, mode)
	}

	root := t.TempDir()
	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	t.Cleanup(cancel)

	runtime, err := StartServer(ctx, StartOptions{
		Root:           filepath.Join(root, "runtime"),
		ExecutablePath: executable,
		NativeEnvironment: func() map[string]string {
			return map[string]string{
				"PATH": "/usr/bin:/bin:/usr/sbin:/sbin",
				"HOME": home,
			}
		},
		HealthTimeout: 30 * time.Second,
		PluginSeedDir: sharedPluginSeedDir(t),
	})
	require.Error(t, err, "a runtime whose carrier plugin never loaded must never reach a session")
	require.ErrorContains(t, err, "session carrier plugin did not load")
	require.Nil(t, runtime)
}
