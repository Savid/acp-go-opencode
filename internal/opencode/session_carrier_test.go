package opencode

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
)

func preserveSessionCarrierSeams(t *testing.T) {
	t.Helper()
	mkdirTemp, mkdirAll := sessionCarrierMkdirTemp, sessionCarrierMkdirAll
	writeFile, randReader := sessionCarrierWriteFile, openCodeRandReader
	listen, remove := sessionCarrierListen, sessionCarrierRemove
	t.Cleanup(func() {
		sessionCarrierMkdirTemp, sessionCarrierMkdirAll = mkdirTemp, mkdirAll
		sessionCarrierWriteFile, openCodeRandReader = writeFile, randReader
		sessionCarrierListen, sessionCarrierRemove = listen, remove
	})
}

func materializedCarrier(t *testing.T) (string, string, sessionCarrierPlugin) {
	t.Helper()
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(runtimeRoot, 0o700))
	plugin, err := materializeSessionCarrierPlugin(runtimeRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = plugin.Cleanup() })
	parsed, err := url.Parse(plugin.URL)
	require.NoError(t, err)
	content, err := os.ReadFile(parsed.Path)
	require.NoError(t, err)

	return parsed.Path, string(content), plugin
}

// carrierMarkFor names the generated search-path component the plugin and the
// wrapper share inside one carrier root.
func carrierMarkFor(root string) string {
	return filepath.Join(root, sessionCarrierPathMarkName)
}

// materializeCarrierWrapper writes the wrapper exactly as the runtime does. The
// wrapper is the whole of the carrier's shell state: there is nothing else to
// publish, because the login shell now arrives with each operation.
func materializeCarrierWrapper(t *testing.T, root string) (string, string) {
	t.Helper()

	mark := carrierMarkFor(root)
	source := sessionCarrierShellWrapperSource(mark)
	if source == "" {
		t.Skip("no shell wrapper on this platform")
	}

	wrapper := filepath.Join(root, sessionCarrierShellName)
	require.NoError(t, os.WriteFile(wrapper, []byte(source), 0o700))

	return wrapper, mark
}

// carrierSearchPath renders exactly what the plugin publishes for one operation
// of one workspace scope: the operation's own directories, this scope's
// login-shell address, and then the runtime's own path.
//
// The address is the marker, the number of separator-delimited segments the
// shell path splits into, and those segments in order. The runtime's path
// always follows as the final component, empty or not, which is what terminates
// the address's last segment.
func carrierSearchPath(mark string, loginShell string, dirs []string, base ...string) string {
	separator := string(os.PathListSeparator)
	segments := strings.Split(loginShell, separator)

	entries := append(append([]string{}, dirs...), mark, strconv.Itoa(len(segments)))
	entries = append(entries, segments...)

	return strings.Join(entries, separator) + separator + strings.Join(base, separator)
}

// carrierRawSearchPath renders a search path from components exactly as given,
// which is how the malformed-address cases reach the wrapper.
func carrierRawSearchPath(components ...string) string {
	return strings.Join(components, string(os.PathListSeparator))
}

// carrierLoginShellShim materializes an executable login shell that names
// itself before becoming the real one. It is how a wrapper test observes which
// shell the wrapper actually started, which is the property the runtime-wide
// shell file got wrong.
func carrierLoginShellShim(t *testing.T, dir string, identity string, loginShell string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))

	shim := filepath.Join(dir, filepath.Base(loginShell))
	require.NoError(t, os.WriteFile(shim,
		[]byte("#!/bin/sh\nprintf 'RAN=%s\\n' "+identity+"\nexec "+loginShell+" \"$@\"\n"), 0o700))

	return shim
}

func TestMaterializeSessionCarrierPlugin(t *testing.T) {
	preserveSessionCarrierSeams(t)
	path, content, plugin := materializedCarrier(t)
	require.Equal(t, sessionCarrierPluginFileName, filepath.Base(path))

	require.Equal(t, filepath.Join(filepath.Dir(path), sessionCarrierProbeDirName), plugin.Proof.Directory)
	require.DirExists(t, plugin.Proof.Directory)
	require.NotNil(t, plugin.Proof.Ready)
	require.Contains(t, content, `fetch(BROKER_ENDPOINT + "/ready"`)
	require.Contains(t, content, `method: "POST"`)
	require.Contains(t, content, `const BROKER_TOKEN = "`+plugin.Broker.token+`"`)

	root := filepath.Dir(path)
	mark := carrierMarkFor(root)
	require.Contains(t, content, `const PATH_MARK = "`+mark+`"`)

	// The login shell is not a thing the carrier holds. It is resolved per
	// workspace scope and published with each operation, so a second scope with
	// a second shell has nothing of the first's to overwrite.
	require.NotContains(t, content, "SHELL_FILE")
	require.NotContains(t, content, "session-carrier.shell")

	wrapper := filepath.Join(root, sessionCarrierShellName)
	if sessionCarrierShellWrapperSource(mark) == "" {
		require.NoFileExists(t, wrapper)
		require.Contains(t, content, `const SHELL_WRAPPER = ""`)
	} else {
		info, err := os.Stat(wrapper)
		require.NoError(t, err)
		require.NotZero(t, info.Mode().Perm()&0o100, "the wrapper is spawned, so it has to be executable")
		require.Contains(t, content, `const SHELL_WRAPPER = "`+wrapper+`"`)

		source, err := os.ReadFile(wrapper)
		require.NoError(t, err)
		require.Contains(t, string(source), "carrier_mark='"+mark+"'")
		require.NotContains(t, string(source), "session-carrier.shell")
	}

	// The generated tree carries the module, the proof's probe directory, and
	// the wrapper where one exists — and no runtime-wide shell state of any
	// kind for a later scope to overwrite.
	entries, err := os.ReadDir(root)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	want := []string{sessionCarrierPluginFileName, sessionCarrierProbeDirName}
	if sessionCarrierShellWrapperSource(mark) != "" {
		want = append(want, sessionCarrierShellName)
	}

	require.ElementsMatch(t, want, names)

	// The carrier is read off the addressed session, with the directory the
	// native SDK requires to resolve it.
	require.Contains(t, content, "path: { id: input.sessionID }")
	require.Contains(t, content, "query: { directory: input.cwd ?? directory }")
}

func TestMaterializeSessionCarrierPluginReportsEveryFailure(t *testing.T) {
	preserveSessionCarrierSeams(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(runtimeRoot, 0o700))

	plugin, err := materializeSessionCarrierPlugin(runtimeRoot)
	require.NoError(t, err)
	require.NoError(t, plugin.Cleanup())

	want := errors.New("carrier seam")

	sessionCarrierMkdirTemp = func(string, string) (string, error) { return "", want }
	_, err = materializeSessionCarrierPlugin(runtimeRoot)
	require.ErrorIs(t, err, want)

	sessionCarrierMkdirTemp = os.MkdirTemp

	randReader := openCodeRandReader
	openCodeRandReader = iotest.ErrReader(want)
	_, err = materializeSessionCarrierPlugin(runtimeRoot)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "authorization")
	openCodeRandReader = randReader

	sessionCarrierListen = func(string, string) (net.Listener, error) { return nil, want }
	_, err = materializeSessionCarrierPlugin(runtimeRoot)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "listen for OpenCode session carrier")
	sessionCarrierListen = net.Listen

	sessionCarrierMkdirAll = func(string, os.FileMode) error { return want }
	_, err = materializeSessionCarrierPlugin(runtimeRoot)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "create OpenCode session carrier probe")
	sessionCarrierMkdirAll = os.MkdirAll

	sessionCarrierWriteFile = func(string, []byte, os.FileMode) error { return want }
	_, err = materializeSessionCarrierPlugin(runtimeRoot)
	require.ErrorIs(t, err, want)

	// The wrapper is written first, so faulting only the plugin write proves
	// the second write is reported too.
	writes := 0
	sessionCarrierWriteFile = func(path string, data []byte, mode os.FileMode) error {
		writes++
		if filepath.Base(path) == sessionCarrierPluginFileName {
			return want
		}

		return os.WriteFile(path, data, mode)
	}
	_, err = materializeSessionCarrierPlugin(runtimeRoot)
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "write OpenCode session carrier")
}

func TestSessionCarrierReadinessRequiresBrokerAuthorization(t *testing.T) {
	preserveSessionCarrierSeams(t)
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(runtimeRoot, 0o700))
	plugin, err := materializeSessionCarrierPlugin(runtimeRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, plugin.Cleanup()) })

	request, err := http.NewRequest(http.MethodPost, plugin.Broker.endpoint+"/ready", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusUnauthorized, response.StatusCode)
	select {
	case <-plugin.Proof.Ready:
		t.Fatal("unauthorized readiness closed the proof")
	default:
	}

	request, err = http.NewRequest(http.MethodPost, plugin.Broker.endpoint+"/ready", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+plugin.Broker.token)
	response, err = http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	requireSignalClosed(t, plugin.Proof.Ready, "authorized readiness did not close the proof")
}

func TestSessionCarrierPluginSourceRendersConstants(t *testing.T) {
	source := sessionCarrierPluginSource(`/carrier/"quoted"/shell`, `/carrier/"quoted"/mark`,
		&sessionCarrierBroker{endpoint: "http://127.0.0.1:1234", token: "broker-token"})
	require.Contains(t, source, `const SHELL_WRAPPER = "/carrier/\"quoted\"/shell"`)
	require.Contains(t, source, `const PATH_MARK = "/carrier/\"quoted\"/mark"`)
	require.Contains(t, source, `const BROKER_ENDPOINT = "http://127.0.0.1:1234"`)
	require.Contains(t, source, `const BROKER_TOKEN = "broker-token"`)
	require.Contains(t, source, `const NAMESPACE = "acp-go-opencode"`)
	require.Contains(t, source, `const REF_KEY = "ref"`)
	require.Contains(t, source, `let proofPending = true`)
	require.Contains(t, source, `if (proofPending) {`)
	require.Contains(t, source, `proofPending = false`)
	require.Contains(t, source, `"OPENCODE_SERVER_PASSWORD"`)
	require.Contains(t, source, `"OPENCODE_CONFIG_CONTENT"`)
	require.Contains(t, source, `"XDG_DATA_HOME"`)
	require.Contains(t, source, `output.env[key] = ""`)

	// The adapter owns no environment variable of its own at this boundary:
	// both halves of what the wrapper needs travel as generated paths.
	// The family's own variable prefix, deliberately spelled without its
	// trailing underscore so this assertion is not itself a declaration of one.
	require.NotContains(t, source, "ACP_GO")
}

// TestSessionCarrierPluginResolvesThePathVariableByEnvironmentIdentity pins the
// platform gate on the search-path key. Only Windows resolves environment names
// case-insensitively, so only there is an existing spelling worth adopting.
// Adopting one elsewhere would let enumeration order pick an inert Path over the
// real PATH the runtime resolves against, and silently turn the operation's
// directories into a no-op.
func TestSessionCarrierPluginResolvesThePathVariableByEnvironmentIdentity(t *testing.T) {
	source := sessionCarrierPluginSource("/carrier/shell", "/carrier/mark",
		&sessionCarrierBroker{endpoint: "http://127.0.0.1:1234", token: "broker-token"})

	require.Contains(t, strings.Join(strings.Fields(source), " "),
		`const pathKey = process.platform === "win32" `+
			`? (Object.keys(process.env).find((key) => key.toUpperCase() === "PATH") ?? "PATH") `+
			`: "PATH"`)
}

// TestSessionCarrierShellWrapperQuotesItsGeneratedPaths pins the one input a
// generated path can carry that a single-quoted shell word cannot hold as is.
func TestSessionCarrierShellWrapperQuotesItsGeneratedPaths(t *testing.T) {
	source := sessionCarrierShellWrapperSource(`/carrier/o'clock/mark`)
	if source == "" {
		t.Skip("no shell wrapper on this platform")
	}

	require.Contains(t, source, `carrier_mark='/carrier/o'\''clock/mark'`)
	// The family's own variable prefix, deliberately spelled without its
	// trailing underscore so this assertion is not itself a declaration of one.
	require.NotContains(t, source, "ACP_GO")
}

func TestRuntimeConfigRegistersSessionCarrierLast(t *testing.T) {
	content, _, err := runtimeConfigContent(map[string]string{
		"opencode.json": `{"plugin":["first"]}`,
	}, "file:///carrier.mjs")
	require.NoError(t, err)
	require.JSONEq(t, `{"$schema":"https://opencode.ai/config.json","plugin":["first","file:///carrier.mjs"]}`, content)

	_, _, err = runtimeConfigContent(map[string]string{
		"opencode.json": `{"plugin":"invalid"}`,
	}, "file:///carrier.mjs")
	require.ErrorContains(t, err, "plugin must be an array")
}

func TestProveSessionCarrierLoadedRequiresBrokerReadiness(t *testing.T) {
	newProof := func(t *testing.T) sessionCarrierProof {
		t.Helper()
		root := t.TempDir()

		return sessionCarrierProof{
			Directory: filepath.Join(root, sessionCarrierProbeDirName),
			Ready:     make(chan struct{}),
		}
	}

	t.Run("the scoped request that forces the load must succeed", func(t *testing.T) {
		proof := newProof(t)
		native := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(native.Close)

		server := &openCodeServer{httpClient: native.Client(), baseURL: native.URL}
		require.ErrorContains(t, server.proveSessionCarrierLoaded(t.Context(), proof),
			"drive the OpenCode session carrier plugin")
	})

	t.Run("a plugin that never ran refuses the runtime", func(t *testing.T) {
		proof := newProof(t)
		native := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(t, writer, map[string]any{})
		}))
		t.Cleanup(native.Close)

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		t.Cleanup(cancel)

		server := &openCodeServer{httpClient: native.Client(), baseURL: native.URL}
		require.ErrorContains(t, server.proveSessionCarrierLoaded(ctx, proof), "did not load")
	})

	t.Run("the addressed probe publishes readiness", func(t *testing.T) {
		proof := newProof(t)
		ready := make(chan struct{})
		proof.Ready = ready

		var addressed string

		native := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			addressed = request.URL.Query().Get("directory")

			close(ready)
			writeJSON(t, writer, map[string]any{})
		}))
		t.Cleanup(native.Close)

		server := &openCodeServer{httpClient: native.Client(), baseURL: native.URL}
		require.NoError(t, server.proveSessionCarrierLoaded(t.Context(), proof))
		require.Equal(t, proof.Directory, addressed,
			"the load is forced against the carrier's own probe directory, never a user directory")
	})
}

func TestStartServerReportsSessionCarrierMaterializationFailure(t *testing.T) {
	preserveSessionCarrierSeams(t)
	want := errors.New("carrier root refused")
	sessionCarrierMkdirTemp = func(string, string) (string, error) { return "", want }

	_, err := StartServer(t.Context(), StartOptions{
		Root: filepath.Join(t.TempDir(), "runtime"), ExecutablePath: "/missing",
	})
	require.ErrorIs(t, err, want)
}

// TestStartServerRefusesARuntimeWhoseCarrierNeverLoaded drives the startup gate
// end to end against a native process that reaches readiness and answers every
// route, and whose plugin module is simply not there to load. A runtime like
// that serves shell operations with no bearer, no operation directories and no
// error, so it must never be handed back.
func TestStartServerRefusesARuntimeWhoseCarrierNeverLoaded(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	preserveSessionCarrierSeams(t)

	// The plugin the runtime registers is never materialized, which is what a
	// module OpenCode cannot resolve looks like from the outside.
	write := sessionCarrierWriteFile
	sessionCarrierWriteFile = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == sessionCarrierPluginFileName {
			return nil
		}

		return write(path, data, mode)
	}

	_, err := StartServer(t.Context(), StartOptions{
		Root:            t.TempDir(),
		ExecutablePath:  fakeOpenCodeExecutable(t),
		MinVersion:      "1.18.3",
		HealthTimeout:   2 * time.Second,
		SkipVersionGate: false,
	})
	require.ErrorContains(t, err, "did not load")
}

func TestPureScopeRejectsSessionCarrier(t *testing.T) {
	server := &openCodeServer{pure: true}
	_, err := server.Scope(t.Context(), ScopeOptions{
		Directory: t.TempDir(), ExtraPathDirs: []string{"/session/bin"},
	})
	require.ErrorContains(t, err, "pure mode")

	_, err = server.Scope(t.Context(), ScopeOptions{
		Directory: t.TempDir(), Env: map[string]string{"WAGIE_API_TOKEN": "token"},
	})
	require.ErrorContains(t, err, "pure mode")

	require.False(t, (&openCodeServer{directory: "/repo", pure: true}).carriesSession())
	require.False(t, (&openCodeServer{}).carriesSession())
	require.True(t, (&openCodeServer{directory: "/repo"}).carriesSession())
}

func TestSessionCarrierMetadataReplacesOnlyTheAdapterNamespace(t *testing.T) {
	scope := &openCodeServer{
		directory:               "/repo",
		sessionCarrierReference: "current-reference",
	}

	metadata := scope.sessionCarrierMetadata(map[string]any{
		"native":              "kept",
		"acp-go-opencode-old": "kept too",
		sessionCarrierMetadataKey: map[string]any{
			sessionCarrierRefKey: "stale-reference",
		},
	})

	require.Equal(t, "kept", metadata["native"])
	require.Equal(t, "kept too", metadata["acp-go-opencode-old"])
	require.Equal(t, map[string]any{
		sessionCarrierRefKey: "current-reference",
	}, metadata[sessionCarrierMetadataKey])

	// An empty reference still publishes the namespace, so a session without a
	// registered carrier fails closed in the plugin.
	empty := (&openCodeServer{directory: "/repo"}).sessionCarrierMetadata(nil)
	require.Equal(t, map[string]any{
		sessionCarrierRefKey: "",
	}, empty[sessionCarrierMetadataKey])
}

// TestSessionCarrierShellWrapperPutsOperationDirectoriesFirst runs the wrapper
// exactly as OpenCode runs it, against a real login shell whose startup file
// rewrites the search path the way a user's own startup file does. It asserts
// the environment the command actually resolves against rather than the
// environment handed to the shell.
func TestSessionCarrierShellWrapperPutsOperationDirectoriesFirst(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	shadow := filepath.Join(home, "shadow")
	require.NoError(t, os.MkdirAll(shadow, 0o700))
	writeProbeTool(t, shadow, "probe-tool", "SHADOW")

	// The user's startup file prepends its own directory, which is exactly the
	// rewrite that leaves an environment-only carrier at the tail.
	startup := `export PATH="` + shadow + `:$PATH"` + "\nexport PROBE_STARTUP_RAN=yes\n"
	require.NoError(t, os.WriteFile(filepath.Join(home, ".zshrc"), []byte(startup), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".bashrc"), []byte(startup), 0o600))

	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")

	for _, dir := range []string{first, second} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}

	writeProbeTool(t, first, "probe-tool", "OPERATION")
	writeProbeTool(t, second, "probe-tool", "PEER")

	// Exactly the search path the plugin publishes for this operation: the
	// directories, the marker, this scope's login shell, and then the runtime's
	// own path.
	carried := carrierSearchPath(mark, loginShell, []string{first, second}, "/usr/bin", "/bin")

	output := runCarrierWrapper(t, wrapper, root, []string{
		"HOME=" + home,
		"PATH=" + carried,
		"WAGIE_API_TOKEN=token-a",
	}, `printf 'PATH=%s\n' "$PATH"; printf 'STARTUP=%s\n' "$PROBE_STARTUP_RAN"; printf 'TOKEN=%s\n' "$WAGIE_API_TOKEN"; probe-tool`)

	require.Contains(t, output, "STARTUP=yes", "the login startup file must still run")
	require.Contains(t, output, "TOKEN=token-a")
	require.Contains(t, output, "OPERATION", "the operation directory must win over the shadow")
	require.NotContains(t, output, "SHADOW")
	require.NotContains(t, output, mark, "the marker never reaches the command's environment")
	require.NotContains(t, output, loginShell+string(os.PathListSeparator),
		"the login shell never reaches the command's search path")

	entries := filepath.SplitList(carrierProbeLine(t, output, "PATH"))
	require.GreaterOrEqual(t, len(entries), 3)
	require.Equal(t, []string{first, second}, entries[:2], "final PATH must start with the operation directories")
	require.NotContains(t, entries, loginShell,
		"the login shell the operation named is a value, not a search-path component")

	for _, entry := range entries {
		require.NotEmpty(t, entry, "the final PATH must not carry an empty component")
	}
}

// TestSessionCarrierShellWrapperWithoutDirectoriesKeepsTheLoginShell is the
// majority configuration: a session that sets an environment and no directories
// at all. The wrapper still has to be the login shell OpenCode would have
// started, sourcing the same startup files and inheriting the same login search
// path — dropping such a session to /bin/sh with the bare runtime path is the
// regression this pins shut.
//
// The production input is the one exercised here: the carrier publishes no
// directory list at all, so the marker is the first component of the search
// path and the login shell stands directly behind it.
func TestSessionCarrierShellWrapperWithoutDirectoriesKeepsTheLoginShell(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	installed := filepath.Join(home, "installed")
	require.NoError(t, os.MkdirAll(installed, 0o700))
	writeProbeTool(t, installed, "probe-tool", "LOGIN-PATH")

	startup := `export PATH="` + installed + `:$PATH"` + "\nexport PROBE_STARTUP_RAN=yes\n"
	for _, name := range []string{".zshrc", ".bashrc"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte(startup), 0o600))
	}

	probe := `printf 'SHELL_KIND=%s\n' "$(basename "$0")"
printf 'STARTUP=%s\n' "$PROBE_STARTUP_RAN"
printf 'TOKEN=%s\n' "$WAGIE_API_TOKEN"
printf 'SEARCH=%s\n' "$PATH"
probe-tool`

	// Exactly what a session that set no directories leaves behind: the search
	// path otherwise untouched, with the marker and this operation's own login
	// shell in front of it.
	output := runCarrierWrapper(t, wrapper, root, []string{
		"HOME=" + home,
		"PATH=" + carrierSearchPath(mark, loginShell, nil, "/usr/bin", "/bin"),
		"WAGIE_API_TOKEN=token-a",
	}, probe)

	require.Contains(t, output, "SHELL_KIND=opencode",
		"the login shell runs OpenCode's own argument form, not a plain /bin/sh -c")
	require.Contains(t, output, "STARTUP=yes", "the user's startup file must still run")
	require.Contains(t, output, "TOKEN=token-a")
	require.Contains(t, output, "LOGIN-PATH",
		"a session with no directories still resolves against the user's login search path")

	search := carrierProbeLine(t, output, "SEARCH")
	require.NotContains(t, search, mark, "the marker never reaches the command's search path")
	require.NotContains(t, filepath.SplitList(search), loginShell,
		"the login shell the operation named is a value, not a search-path component")
	require.Contains(t, filepath.SplitList(search), installed,
		"with no operation directories the login search path is the search path")
}

// carrierProbeLine reads one field out of the wrapper probe's output.
func carrierProbeLine(t *testing.T, output string, name string) string {
	t.Helper()

	for line := range strings.SplitSeq(output, "\n") {
		if value, ok := strings.CutPrefix(line, name+"="); ok {
			return value
		}
	}

	t.Fatalf("field %q missing from wrapper output:\n%s", name, output)

	return ""
}

// TestSessionCarrierShellWrapperRunsTheShellTheOperationNamed is the regression
// for a login shell held anywhere wider than the operation that named it.
//
// OpenCode builds one plugin instance per workspace scope and runs its config
// hook against that scope's own configuration, so two scopes resolve two login
// shells. One wrapper serves both. When the shell lived in a file the whole
// runtime shared, the second scope's config hook overwrote it and the first
// scope's sessions silently changed shell underneath themselves; here the two
// operations are interleaved through one wrapper and each still gets its own.
func TestSessionCarrierShellWrapperRunsTheShellTheOperationNamed(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	startup := "export PROBE_STARTUP_RAN=yes\n"
	for _, name := range []string{".zshrc", ".bashrc", ".zshenv"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte(startup), 0o600))
	}

	first := carrierLoginShellShim(t, filepath.Join(root, "scope-first"), "FIRST-SCOPE", loginShell)
	second := carrierLoginShellShim(t, filepath.Join(root, "scope-second"), "SECOND-SCOPE", loginShell)

	run := func(shell string) string {
		return runCarrierWrapper(t, wrapper, root, []string{
			"HOME=" + home,
			"PATH=" + carrierSearchPath(mark, shell, nil, "/usr/bin", "/bin"),
		}, `printf 'STARTUP=%s\n' "$PROBE_STARTUP_RAN"`)
	}

	// The second scope resolving its own shell is what used to overwrite the
	// first's, so the first scope runs again afterwards and must be unchanged.
	firstOutput := run(first)
	secondOutput := run(second)
	repeat := run(first)

	require.Contains(t, firstOutput, "RAN=FIRST-SCOPE")
	require.NotContains(t, firstOutput, "SECOND-SCOPE")
	require.Contains(t, firstOutput, "STARTUP=yes")

	require.Contains(t, secondOutput, "RAN=SECOND-SCOPE")
	require.NotContains(t, secondOutput, "FIRST-SCOPE")
	require.Contains(t, secondOutput, "STARTUP=yes")

	require.Contains(t, repeat, "RAN=FIRST-SCOPE",
		"a neighbouring scope resolving its own login shell must not reach this operation")
	require.NotContains(t, repeat, "SECOND-SCOPE")
}

// TestSessionCarrierShellWrapperKeepsConcurrentOperationsApart drives the two
// scopes at once. Interleaving in time is the weaker shape the case above
// covers; running them together is the shape a shared runtime actually serves.
//
// Half the scopes configure their shell at a path the search-path separator
// runs through, so the address has to stay lossless while it is being read back
// concurrently as well as in isolation.
func TestSessionCarrierShellWrapperKeepsConcurrentOperationsApart(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	const operations = 8

	shells := make([]string, operations)
	for index := range shells {
		identity := fmt.Sprintf("SCOPE-%d", index)

		directory := identity
		if index%2 == 1 {
			directory = identity + string(os.PathListSeparator) + "colon"
		}

		shells[index] = carrierLoginShellShim(t, filepath.Join(root, directory), identity, loginShell)
	}

	outputs := make([]string, operations)

	var group sync.WaitGroup

	group.Add(operations)

	for index, shell := range shells {
		go func() {
			defer group.Done()

			outputs[index] = runCarrierWrapper(t, wrapper, root, []string{
				"HOME=" + home,
				"PATH=" + carrierSearchPath(mark, shell, nil, "/usr/bin", "/bin"),
			}, `printf 'DONE=%s\n' ok`)
		}()
	}

	group.Wait()

	for index, output := range outputs {
		require.Contains(t, output, fmt.Sprintf("RAN=SCOPE-%d", index))
		require.Contains(t, output, "DONE=ok")

		for peer := range operations {
			if peer != index {
				require.NotContains(t, output, fmt.Sprintf("RAN=SCOPE-%d\n", peer))
			}
		}
	}
}

// TestSessionCarrierShellWrapperCarriesASeparatorBearingShellPath is the
// regression for an address that could not hold every path it had to carry.
//
// ':' is a legal character in a POSIX filename, so a configured login shell can
// live at a path the search-path separator runs straight through. While the
// shell travelled as one search-path component, such a path truncated at its
// first ':' and the operation was refused — a valid configuration OpenCode
// itself accepts, rejected by the transport rather than by any rule. The
// counted address carries it whole, including a segment the split leaves empty.
func TestSessionCarrierShellWrapperCarriesASeparatorBearingShellPath(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	startup := "export PROBE_STARTUP_RAN=yes\n"
	for _, name := range []string{".zshrc", ".bashrc", ".zshenv"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte(startup), 0o600))
	}

	separator := string(os.PathListSeparator)

	operation := filepath.Join(root, "operation")
	require.NoError(t, os.MkdirAll(operation, 0o700))
	writeProbeTool(t, operation, "probe-tool", "OPERATION")

	// A separator-free neighbour, then the paths a component-shaped address
	// could not have held: one separator, several, and a doubled one whose split
	// leaves an empty segment in the middle.
	plain := carrierLoginShellShim(t, filepath.Join(root, "plain-scope"), "PLAIN-SCOPE", loginShell)

	for _, test := range []struct {
		name     string
		director string
		identity string
	}{
		{name: "one separator", director: "with" + separator + "colon", identity: "COLON-SCOPE"},
		{
			name:     "several separators",
			director: separator + "a" + separator + "b" + separator,
			identity: "MULTI-SCOPE",
		},
		{
			name:     "an empty segment",
			director: "weird" + separator + separator + "name",
			identity: "EMPTY-SEGMENT-SCOPE",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			shim := carrierLoginShellShim(t, filepath.Join(root, test.director), test.identity, loginShell)
			require.Contains(t, shim, separator, "this case is only a proof if the shell path holds a separator")

			run := func(shell string) string {
				return runCarrierWrapper(t, wrapper, root, []string{
					"HOME=" + home,
					"PATH=" + carrierSearchPath(mark, shell, []string{operation}, "/usr/bin", "/bin"),
				}, `printf 'STARTUP=%s\n' "$PROBE_STARTUP_RAN"; printf 'PATH=%s\n' "$PATH"; probe-tool`)
			}

			// A -> B -> A across the separator boundary: the neighbouring scope
			// resolving its own shell must not reach either operation.
			before := run(plain)
			addressed := run(shim)
			after := run(plain)

			require.Contains(t, before, "RAN=PLAIN-SCOPE")
			require.NotContains(t, before, test.identity)

			require.Contains(t, addressed, "RAN="+test.identity,
				"the exact configured executable must survive the trip whole")
			require.NotContains(t, addressed, "RAN=PLAIN-SCOPE")
			require.Contains(t, addressed, "STARTUP=yes")
			require.Contains(t, addressed, "OPERATION")

			require.Contains(t, after, "RAN=PLAIN-SCOPE")
			require.NotContains(t, after, test.identity)

			// The address left nothing of itself behind, component by component.
			entries := filepath.SplitList(carrierProbeLine(t, addressed, "PATH"))
			require.Equal(t, []string{operation}, entries[:1],
				"final PATH must still start with the operation directories")

			for _, entry := range entries {
				require.NotEmpty(t, entry, "the final PATH must not carry an empty component")
				require.NotContains(t, entry, sessionCarrierPathMarkName,
					"the marker never reaches the command's search path")
			}

			for segment := range strings.SplitSeq(shim, separator) {
				require.NotContains(t, entries, segment,
					"no segment of the addressed shell path is a search-path component")
			}
		})
	}
}

// TestSessionCarrierShellWrapperAddressesFromASeparatorBearingCarrierRoot is
// the other side of the same legality: the marker that opens the address is
// itself a generated absolute path, and the carrier's tree is created under the
// ambient temporary root, which may hold a separator of its own. The marker is
// therefore located as a whole string rather than as a search-path component,
// and an address opened by a marker that spans several components still reads
// back exactly.
func TestSessionCarrierShellWrapperAddressesFromASeparatorBearingCarrierRoot(t *testing.T) {
	loginShell := availableLoginShell(t)

	separator := string(os.PathListSeparator)

	root := filepath.Join(t.TempDir(), "carrier"+separator+"root")
	require.NoError(t, os.MkdirAll(root, 0o700))

	wrapper, mark := materializeCarrierWrapper(t, root)
	require.Contains(t, mark, separator, "this proof needs a marker that spans several components")

	home := filepath.Join(root, "home")
	require.NoError(t, os.MkdirAll(home, 0o700))

	for _, name := range []string{".zshrc", ".bashrc", ".zshenv"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, name), []byte("export PROBE_STARTUP_RAN=yes\n"), 0o600))
	}

	shim := carrierLoginShellShim(t, filepath.Join(root, "shell"+separator+"scope"), "SEPARATED-SCOPE", loginShell)

	output := runCarrierWrapper(t, wrapper, root, []string{
		"HOME=" + home,
		"PATH=" + carrierSearchPath(mark, shim, nil, "/usr/bin", "/bin"),
	}, `printf 'STARTUP=%s\n' "$PROBE_STARTUP_RAN"; printf 'PATH=%s\n' "$PATH"`)

	require.Contains(t, output, "RAN=SEPARATED-SCOPE")
	require.Contains(t, output, "STARTUP=yes")

	entries := filepath.SplitList(carrierProbeLine(t, output, "PATH"))
	for _, entry := range entries {
		require.NotContains(t, entry, sessionCarrierPathMarkName,
			"no component of the marker survives into the command's search path")
	}

	for segment := range strings.SplitSeq(shim, separator) {
		require.NotContains(t, entries, segment)
	}
}

// TestSessionCarrierShellWrapperRefusesAMalformedAddress drives the wrapper with
// every shape of an address that is present but cannot be read back. None of
// them may be repaired, guessed at, or partially honoured: an address the
// wrapper cannot read names no shell this operation can be shown to have asked
// for, and running the command anyway is exactly the fail-open the counted
// frame exists to remove.
//
// The production plugin cannot emit any of these. They are here because the
// wrapper is an executable on disk inside a shared runtime, and its refusal is
// the property the whole per-operation design rests on.
func TestSessionCarrierShellWrapperRefusesAMalformedAddress(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	separator := string(os.PathListSeparator)
	split := strings.Split(loginShell, separator)
	require.Len(t, split, 1, "this table addresses a separator-free shell deliberately")

	for _, test := range []struct {
		name   string
		path   string
		reason string
	}{
		{
			name:   "an address with nothing after the marker",
			path:   carrierRawSearchPath("/usr/bin", mark, ""),
			reason: "malformed login-shell address",
		},
		{
			name:   "a count that is not a number",
			path:   carrierRawSearchPath("/usr/bin", mark, "one", loginShell, "/bin"),
			reason: "malformed login-shell address",
		},
		{
			name:   "a count of no segments at all",
			path:   carrierRawSearchPath("/usr/bin", mark, "0", loginShell, "/bin"),
			reason: "malformed login-shell address",
		},
		{
			name:   "a count spelled with a leading zero",
			path:   carrierRawSearchPath("/usr/bin", mark, "01", loginShell, "/bin"),
			reason: "malformed login-shell address",
		},
		{
			name:   "a signed count",
			path:   carrierRawSearchPath("/usr/bin", mark, "-1", loginShell, "/bin"),
			reason: "malformed login-shell address",
		},
		{
			name:   "a count with trailing whitespace",
			path:   carrierRawSearchPath("/usr/bin", mark, "1 ", loginShell, "/bin"),
			reason: "malformed login-shell address",
		},
		{
			name:   "a count no integer can hold",
			path:   carrierRawSearchPath("/usr/bin", mark, "9999999999", loginShell, "/bin"),
			reason: "malformed login-shell address",
		},
		{
			name:   "a count the address never terminates",
			path:   carrierRawSearchPath("/usr/bin", mark+separator+"1"),
			reason: "malformed login-shell address",
		},
		{
			name:   "more segments claimed than published",
			path:   carrierRawSearchPath("/usr/bin", mark, "3", loginShell, "/bin"),
			reason: "truncated login-shell address",
		},
		{
			name:   "an unterminated final segment",
			path:   carrierRawSearchPath("/usr/bin", mark, "2", "/opt/a", "b/zsh"),
			reason: "truncated login-shell address",
		},
		{
			name: "an address whose segments rejoin to nothing runnable",
			path: carrierRawSearchPath("/usr/bin", mark, "2",
				filepath.Join(root, "missing"), "zsh", "/bin"),
			reason: "named no runnable login shell",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := tryCarrierWrapper(t, wrapper, root,
				[]string{"PATH=" + test.path, "SHELL=" + loginShell},
				"-c", `printf 'RAN=%s\n' yes`)
			require.Error(t, err, "wrapper output: %s", output)
			require.Contains(t, output, "acp-go-opencode session carrier: ")
			require.Contains(t, output, test.reason)
			require.NotContains(t, output, "RAN=yes", "the command must not run")
		})
	}
}

// TestSessionCarrierShellAddressingIsPlatformScoped states the platform
// contract the address belongs to. The wrapper — and therefore the whole
// addressing scheme — exists only where OpenCode starts a login shell that
// reruns startup files. On Windows OpenCode starts cmd with /c and PowerShell
// with -NoProfile, so the environment the hook returns is already the
// environment the command runs under, no wrapper is materialized, and the
// plugin publishes no marker and no address at all.
func TestSessionCarrierShellAddressingIsPlatformScoped(t *testing.T) {
	preserveSessionCarrierSeams(t)
	path, content, _ := materializedCarrier(t)

	root := filepath.Dir(path)
	source := sessionCarrierShellWrapperSource(carrierMarkFor(root))

	if runtime.GOOS == "windows" {
		require.Empty(t, source, "Windows reruns no startup file, so there is nothing to stand in for")
		require.Contains(t, content, `const SHELL_WRAPPER = ""`)
		require.NoFileExists(t, filepath.Join(root, sessionCarrierShellName))

		return
	}

	require.NotEmpty(t, source)
	require.FileExists(t, filepath.Join(root, sessionCarrierShellName))

	// The count is what makes the address lossless, so the wrapper reads it
	// rather than splitting the shell out of a single component.
	require.Contains(t, source, `carrier_count=${carrier_rest%%:*}`)
	require.Contains(t, source, `while [ "$carrier_taken" -lt "$carrier_count" ]; do`)
	require.NotContains(t, source, `carrier_shell=${carrier_rest%%:*}`,
		"a shell read as one component cannot hold the separator")

	// The plugin publishes the same frame, and nothing else.
	require.Contains(t, content, `const segments = loginShell.split(sep)`)
	require.Contains(t, content,
		`[...dirs, PATH_MARK, String(segments.length), ...segments, base].join(sep)`)
	require.NotContains(t, content, `PATH_MARK, loginShell`,
		"a shell published as one component cannot hold the separator")
}

// TestSessionCarrierShellWrapperFailsClosedWithoutAnAddressedShell covers every
// way an operation can reach the wrapper without naming a shell it can stand in
// for. None of them may guess: a wrapper that picked a default would be
// answering for a workspace scope that never spoke.
func TestSessionCarrierShellWrapperFailsClosedWithoutAnAddressedShell(t *testing.T) {
	loginShell := availableLoginShell(t)

	root := t.TempDir()
	wrapper, mark := materializeCarrierWrapper(t, root)

	separator := string(os.PathListSeparator)

	for _, test := range []struct {
		name      string
		env       []string
		arguments []string
		reason    string
	}{
		{
			name:   "no marker at all",
			env:    []string{"PATH=/usr/bin:/bin", "SHELL=" + loginShell},
			reason: "published no login shell",
		},
		{
			name:   "a marker that names nothing runnable",
			env:    []string{"PATH=" + carrierSearchPath(mark, filepath.Join(root, "missing"), nil, "/usr/bin")},
			reason: "named no runnable login shell",
		},
		{
			name:   "a marker at the very end of the search path",
			env:    []string{"PATH=/usr/bin" + separator + mark},
			reason: "published no login shell",
		},
		{
			name:   "a login shell the wrapper does not reproduce",
			env:    []string{"PATH=" + carrierSearchPath(mark, "/bin/sh", nil, "/usr/bin")},
			reason: "does not reproduce",
		},
		{
			name:      "an argument form OpenCode never uses for it",
			env:       []string{"PATH=" + carrierSearchPath(mark, loginShell, nil, "/usr/bin")},
			arguments: []string{"-l", "-c", "printf 'RAN=%s\\n' yes"},
			reason:    "form it does not stand in for",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := test.arguments
			if arguments == nil {
				arguments = []string{"-c", `printf 'RAN=%s\n' yes`}
			}

			output, err := tryCarrierWrapper(t, wrapper, root, test.env, arguments...)
			require.Error(t, err, "wrapper output: %s", output)
			require.Contains(t, output, "acp-go-opencode session carrier: ")
			require.Contains(t, output, test.reason)
			require.NotContains(t, output, "RAN=yes", "the command must not run")
		})
	}
}

// TestSessionCarrierKeepsNoShellStateOfItsOwn pins the property the fix rests
// on for the whole life of a carrier: running operations through the wrapper
// writes nothing into the generated tree, so there is nothing for a later scope
// to overwrite and nothing for cleanup to leave behind.
func TestSessionCarrierKeepsNoShellStateOfItsOwn(t *testing.T) {
	preserveSessionCarrierSeams(t)
	loginShell := availableLoginShell(t)

	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	require.NoError(t, os.Mkdir(runtimeRoot, 0o700))
	plugin, err := materializeSessionCarrierPlugin(runtimeRoot)
	require.NoError(t, err)

	parsed, err := url.Parse(plugin.URL)
	require.NoError(t, err)

	root := filepath.Dir(parsed.Path)
	before := carrierTreeEntries(t, root)

	wrapper := filepath.Join(root, sessionCarrierShellName)
	mark := carrierMarkFor(root)

	for _, identity := range []string{"FIRST-SCOPE", "SECOND-SCOPE"} {
		shim := carrierLoginShellShim(t, filepath.Join(t.TempDir(), identity), identity, loginShell)
		output := runCarrierWrapper(t, wrapper, root, []string{
			"HOME=" + t.TempDir(),
			"PATH=" + carrierSearchPath(mark, shim, nil, "/usr/bin", "/bin"),
		}, `printf 'DONE=%s\n' ok`)
		require.Contains(t, output, "RAN="+identity)
	}

	require.Equal(t, before, carrierTreeEntries(t, root),
		"a shell operation must leave no state of its own inside the carrier tree")

	require.NoError(t, plugin.Cleanup())
	require.NoDirExists(t, root)
}

// carrierTreeEntries lists every path the carrier's generated tree holds.
func carrierTreeEntries(t *testing.T, root string) []string {
	t.Helper()

	var entries []string

	require.NoError(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		entries = append(entries, relative)

		return nil
	}))

	slices.Sort(entries)

	return entries
}

func availableLoginShell(t *testing.T) string {
	t.Helper()

	if sessionCarrierShellWrapperSource("") == "" {
		t.Skip("no shell wrapper on this platform")
	}

	for _, candidate := range []string{"/bin/zsh", "/bin/bash"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}

	t.Skip("no login shell available")

	return ""
}

func writeProbeTool(t *testing.T, dir string, name string, marker string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho "+marker+"\n"), 0o700))
}

// runCarrierWrapper invokes the wrapper in the one form OpenCode uses for a
// shell it does not recognise, and requires it to succeed.
func runCarrierWrapper(t *testing.T, wrapper string, cwd string, env []string, command string) string {
	t.Helper()

	output, err := tryCarrierWrapper(t, wrapper, cwd, env, "-c", command)
	require.NoError(t, err, "wrapper output: %s", output)

	return output
}

// tryCarrierWrapper invokes the wrapper and hands back whatever it did, which
// is what the fail-closed cases need to inspect.
func tryCarrierWrapper(t *testing.T, wrapper string, cwd string, env []string, arguments ...string) (string, error) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell")
	}

	cmd := exec.CommandContext(t.Context(), wrapper, arguments...)
	cmd.Dir = cwd
	cmd.Env = env
	output, err := cmd.CombinedOutput()

	return string(output), err
}

// TestEraseSessionCarrierBootstrapRemovesTheTokenBearingModule proves the
// generated module does not outlive its load. It names the broker endpoint and
// carries its bearer token as a literal, and that token reads any session's
// carrier payload, so a module left in the runtime root would hand one
// session's shell the authorization to read its neighbour's environment.
func TestEraseSessionCarrierBootstrapRemovesTheTokenBearingModule(t *testing.T) {
	preserveSessionCarrierSeams(t)

	path, source, plugin := materializedCarrier(t)
	require.Contains(t, source, plugin.Broker.token, "the fixture must carry the token this erase exists to remove")
	require.FileExists(t, path)

	require.NoError(t, eraseSessionCarrierBootstrap(plugin))
	require.NoFileExists(t, path)

	// The probe directory and the shell wrapper are not authorization and stay.
	require.DirExists(t, plugin.Proof.Directory)

	// A carrier that was never materialized has no bootstrap to erase.
	require.NoError(t, eraseSessionCarrierBootstrap(sessionCarrierPlugin{}))

	want := errors.New("module is pinned")
	sessionCarrierRemove = func(string) error { return want }
	require.ErrorIs(t, eraseSessionCarrierBootstrap(plugin), want)
}

// TestStartServerRefusesARuntimeWhoseBootstrapSurvives proves the erase is
// fail-closed: a runtime that reached readiness but could not have its
// token-bearing module removed is torn down rather than handed to a session.
func TestStartServerRefusesARuntimeWhoseBootstrapSurvives(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	preserveSessionCarrierSeams(t)

	want := errors.New("module is pinned")
	sessionCarrierRemove = func(string) error { return want }

	_, err := StartServer(t.Context(), StartOptions{
		Root:            t.TempDir(),
		ExecutablePath:  fakeOpenCodeExecutable(t),
		MinVersion:      "1.18.3",
		HealthTimeout:   5 * time.Second,
		SkipVersionGate: false,
	})
	require.ErrorIs(t, err, want)
}

// TestStartServerLeavesAPreparedTreesBootstrapAlone proves the erase respects
// tree ownership: under host authority the runtime root belongs to the host
// from preparation until reclaim, so the adapter does not write into it.
func TestStartServerLeavesAPreparedTreesBootstrapAlone(t *testing.T) {
	restoreOpenCodeClientSeams(t)
	preserveSessionCarrierSeams(t)

	erased := false
	sessionCarrierRemove = func(string) error {
		erased = true

		return nil
	}

	client, err := StartServer(t.Context(), StartOptions{
		Root:            t.TempDir(),
		ExecutablePath:  fakeOpenCodeExecutable(t),
		HealthTimeout:   5 * time.Second,
		SkipVersionGate: true,
		PrepareTree:     func(context.Context, string) error { return nil },
		ReclaimTree:     func(context.Context, string) error { return nil },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	require.False(t, erased, "a prepared tree is not this adapter's to write")
}
