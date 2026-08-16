package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// sessionCarrierPluginFileName is the native plugin module the runtime
	// registers. One module serves every addressed session: it reads an opaque
	// reference off the session the shell boundary names, then resolves the
	// carrier from the adapter's in-memory broker.
	sessionCarrierPluginFileName = "session-carrier.mjs"

	// sessionCarrierProofFileName is the marker the plugin writes as OpenCode
	// instantiates it. OpenCode treats a plugin it cannot load as non-fatal, so
	// this file is the only evidence the adapter has that the hooks the whole
	// fail-closed design rests on are actually installed.
	sessionCarrierProofFileName = "session-carrier.loaded"

	// sessionCarrierProbeDirName is the directory the startup proof addresses.
	// OpenCode instantiates a plugin lazily, with the first directory-scoped
	// request, so the proof needs a directory of its own that carries no
	// session and no user content.
	sessionCarrierProbeDirName = "probe"

	// sessionCarrierShellName is the shell OpenCode is pointed at when the
	// resolved user shell would otherwise be started as a login shell. The name
	// deliberately matches none of OpenCode's known shells, which is what makes
	// OpenCode hand it the plain "-c command" form and leaves the login
	// sequence for the wrapper to reproduce.
	sessionCarrierShellName = "acp-go-opencode-shell"

	// sessionCarrierURLScheme is the scheme OpenCode's plugin list requires for
	// a module that lives on disk rather than in a registry.
	sessionCarrierURLScheme = "file"

	// sessionCarrierPathMarkName opens the login-shell address the plugin writes
	// between an operation's directories and the runtime's own search path. It is
	// the wrapper's only channel for both halves of what it needs, it is
	// published per operation rather than per runtime, and the wrapper removes
	// the whole address before the login shell starts: nothing the adapter owns
	// survives into the environment the user's command runs under.
	//
	// The address is counted rather than delimited, because the search-path
	// separator is a legal character in the executable path being addressed. See
	// sessionCarrierShellWrapperTemplate for the frame the two halves share.
	sessionCarrierPathMarkName = "session-carrier-path-mark"
)

// sessionCarrierProof is the evidence a runtime must produce before it may
// serve a session: the marker file the generated plugin writes when OpenCode
// instantiates it, the exact content that marker has to carry, and the
// directory whose first scoped request forces that instantiation.
type sessionCarrierProof struct {
	Path      string
	Token     string
	Directory string
}

// sessionCarrierPlugin is the generated native plugin the runtime registers
// together with the proof the runtime requires before it hands a session out.
type sessionCarrierPlugin struct {
	URL    string
	Path   string
	Proof  sessionCarrierProof
	Broker *sessionCarrierBroker
}

// sessionCarrierPluginSource renders the native plugin that carries one
// addressed session's environment, search-path directories, and login shell to
// the shell boundary.
//
// The plugin reads the session OpenCode itself names and resolves its opaque
// reference through the adapter, so native persistence receives none of the
// carrier values. Two sessions of the same runtime never see each other's
// values and a rebind takes effect on the next command. Every native or broker
// read error and every malformed carrier throws: the shell operation fails
// rather than degrading to the shared process environment.
//
// shellWrapper is the wrapper the config hook substitutes for a login shell. It
// is empty where no wrapper was materialized, in which case the environment the
// hook returns is already the environment the command runs under. pathMark is
// the generated search-path component that separates the operation's own
// directories from the login shell and the runtime's path.
func sessionCarrierPluginSource(
	shellWrapper string,
	pathMark string,
	proof sessionCarrierProof,
	broker *sessionCarrierBroker,
) string {
	return fmt.Sprintf(sessionCarrierPluginTemplate,
		jsStringLiteral(shellWrapper),
		jsStringLiteral(pathMark),
		jsStringLiteral(proof.Path),
		jsStringLiteral(proof.Token),
		jsStringLiteral(broker.endpoint),
		jsStringLiteral(broker.token),
		jsStringLiteral(sessionCarrierMetadataKey),
		jsStringLiteral(sessionCarrierRefKey),
	)
}

// jsStringLiteral renders a value as a JS string literal. JSON encoding is the
// escaping rule: it is a subset of JS string syntax, and encoding a Go string
// cannot fail.
func jsStringLiteral(value string) string {
	encoded, _ := json.Marshal(value)

	return string(encoded)
}

const sessionCarrierPluginTemplate = `import { accessSync, constants, statSync, writeFileSync } from "node:fs"

const SHELL_WRAPPER = %s
const PATH_MARK = %s
const PROOF_PATH = %s
const PROOF_TOKEN = %s
const BROKER_ENDPOINT = %s
const BROKER_TOKEN = %s
const NAMESPACE = %s
const REF_KEY = %s

const PRIVATE_RUNTIME_ENV_KEYS = [
  "OPENCODE_CONFIG",
  "OPENCODE_CONFIG_CONTENT",
  "OPENCODE_CONFIG_DIR",
  "OPENCODE_DB",
  "OPENCODE_ENABLE_QUESTION_TOOL",
  "OPENCODE_PID",
  "OPENCODE_SERVER_PASSWORD",
  "OPENCODE_SERVER_USERNAME",
  "XDG_CACHE_HOME",
  "XDG_CONFIG_HOME",
  "XDG_DATA_HOME",
  "XDG_RUNTIME_DIR",
  "XDG_STATE_HOME",
]

// OpenCode starts exactly these shells as login shells, which is what lets a
// user startup file rewrite the search path after the environment hook has run.
const LOGIN_SHELLS = new Set(["bash", "zsh"])

// The shell OpenCode itself starts when nothing it can resolve is configured.
const OPENCODE_DEFAULT_SHELL = "/bin/zsh"

let proofPending = true

const separator = () => (process.platform === "win32" ? ";" : ":")

const baseName = (value) => value.split(/[\\/]/).pop().toLowerCase()

const runnable = (candidate) => {
  try {
    accessSync(candidate, constants.X_OK)

    return statSync(candidate).isFile()
  } catch {
    return false
  }
}

// resolveLoginShell answers one question for one workspace scope: which shell
// would OpenCode itself have started here, and can this wrapper reproduce it?
// It mirrors OpenCode's own resolution — the scope's configured shell, then the
// process shell, then OpenCode's built-in default — and returns "" for every
// shell the wrapper must not stand in for, which leaves OpenCode's own
// behaviour in place rather than guessing at it.
const resolveLoginShell = (configured) => {
  const named = configured || process.env.SHELL || ""
  const located = named.includes("/")
    ? named
    : named &&
      (process.env.PATH ?? "")
        .split(separator())
        .filter((entry) => entry.length > 0)
        .map((entry) => entry + "/" + named)
        .find(runnable)
  const resolved = located || OPENCODE_DEFAULT_SHELL
  if (!LOGIN_SHELLS.has(baseName(resolved))) return ""

  return runnable(resolved) ? resolved : ""
}

const fail = (reason) => {
  throw new Error("acp-go-opencode session carrier: " + reason)
}

export const AcpGoOpenCodeSessionCarrier = async ({ client, directory }) => {
  // OpenCode logs a plugin it cannot load and serves every shell operation
  // anyway, so nothing below runs unless this module was really instantiated.
  // The adapter refuses to start a runtime that cannot show this marker.
  if (proofPending) {
    writeFileSync(PROOF_PATH, PROOF_TOKEN)
    proofPending = false
  }

  // The login shell of this workspace scope, and of no other. OpenCode builds
  // one plugin instance per scope and runs its config hook against that scope's
  // own configuration, so a shell held anywhere the runtime as a whole can
  // reach it is the neighbouring scope's shell as often as it is this one's.
  let loginShell = ""

  return {
    config: (config) => {
      if (!SHELL_WRAPPER) return
      const resolved = resolveLoginShell(typeof config.shell === "string" ? config.shell : "")
      if (!resolved) return

      loginShell = resolved
      config.shell = SHELL_WRAPPER
    },
    "shell.env": async (input, output) => {
      for (const key of PRIVATE_RUNTIME_ENV_KEYS) output.env[key] = ""

      // A pseudo-terminal environment carries no session, and there is no
      // addressed session to read a carrier from.
      if (!input.sessionID) return

      const response = await client.session.get({
        path: { id: input.sessionID },
        query: { directory: input.cwd ?? directory },
      })
      if (response.error || !response.data) fail("addressed session read failed")

      const carrier = response.data.metadata?.[NAMESPACE]
      if (!carrier || typeof carrier !== "object" || Array.isArray(carrier)) fail("addressed session carrier is missing")

      const reference = carrier[REF_KEY]
      if (typeof reference !== "string" || reference.length === 0) fail("addressed session carrier reference is malformed")

      const carrierResponse = await fetch(BROKER_ENDPOINT + "/carrier/" + encodeURIComponent(reference), {
        cache: "no-store",
        headers: { Authorization: "Bearer " + BROKER_TOKEN },
      })
      if (!carrierResponse.ok) fail("addressed session carrier read failed")

      const payload = await carrierResponse.json()
      if (!payload || typeof payload !== "object" || Array.isArray(payload)) fail("addressed session carrier payload is malformed")

      const env = payload.env
      if (!env || typeof env !== "object" || Array.isArray(env)) fail("addressed session carrier environment is malformed")

      const dirs = payload.extraPathDirs
      if (!Array.isArray(dirs)) fail("addressed session carrier path directories are malformed")

      for (const [key, value] of Object.entries(env)) {
        if (typeof value !== "string") fail("addressed session carrier environment is malformed")
        output.env[key] = value
      }

      for (const key of PRIVATE_RUNTIME_ENV_KEYS) output.env[key] = ""

      for (const dir of dirs) {
        if (typeof dir !== "string" || dir.length === 0) fail("addressed session carrier path directories are malformed")
      }

      if (!loginShell && dirs.length === 0) return

      // Only Windows resolves environment names case-insensitively, so only
      // there is an existing spelling worth adopting. Matching case-insensitively
      // elsewhere would let enumeration order pick an inert Path over the real
      // PATH and turn this operation's directories into a silent no-op.
      const pathKey =
        process.platform === "win32"
          ? (Object.keys(process.env).find((key) => key.toUpperCase() === "PATH") ?? "PATH")
          : "PATH"
      const base = process.env[pathKey] ?? ""
      const sep = separator()

      // Where OpenCode runs the shell itself there is no wrapper to address and
      // nothing to remove again: the operation's directories simply lead the
      // runtime's own path.
      if (!loginShell) {
        output.env[pathKey] = [...dirs, base].filter((entry) => entry.length > 0).join(sep)

        return
      }

      // Where the wrapper runs, this operation's directories lead, then the
      // login-shell address, then the runtime's own path. The address is the
      // marker, the number of separator-delimited segments this scope's shell
      // path splits into, and those segments in order — counted rather than
      // delimited, because the separator is a legal character in a POSIX
      // executable path and a delimited field could not carry one. The
      // runtime's path always follows as the final component, empty or not, so
      // the address's last segment is always terminated.
      //
      // The wrapper reads back exactly that many segments, rejoins them into
      // the executable this scope configured, drops the whole address, and
      // reapplies the directories once the login shell has finished rewriting
      // the search path — the only point at which the order is the order the
      // command resolves against. Publishing the shell here rather than
      // anywhere durable is what keeps one scope's shell out of another's
      // operations.
      const segments = loginShell.split(sep)
      output.env[pathKey] = [...dirs, PATH_MARK, String(segments.length), ...segments, base].join(sep)
    },
  }
}
`

var (
	sessionCarrierMkdirTemp = os.MkdirTemp
	sessionCarrierMkdirAll  = os.MkdirAll
	sessionCarrierWriteFile = os.WriteFile
	sessionCarrierRemove    = os.Remove
	sessionCarrierHandoff   = handoffSessionCarrierGeneratedTree
)

func eraseSessionCarrierBootstrap(plugin sessionCarrierPlugin) error {
	return errors.Join(
		sessionCarrierRemove(plugin.Path),
		sessionCarrierRemove(plugin.Proof.Path),
	)
}

func materializeSessionCarrierPlugin(runtimeRoot string, isolation *ProcessIsolation) (sessionCarrierPlugin, func() error, error) {
	parent := filepath.Dir(runtimeRoot)

	root, err := sessionCarrierMkdirTemp(parent, ".acp-go-opencode-session-carrier-")
	if err != nil {
		return sessionCarrierPlugin{}, nil, fmt.Errorf("create OpenCode session carrier root: %w", err)
	}

	var broker *sessionCarrierBroker

	cleanup := func() error {
		return errors.Join(broker.Close(), openCodeRemoveAll(root))
	}

	token, err := randomPassword()
	if err != nil {
		return sessionCarrierPlugin{}, nil, errors.Join(fmt.Errorf("generate OpenCode session carrier proof: %w", err), cleanup())
	}

	proof := sessionCarrierProof{
		Path:      filepath.Join(root, sessionCarrierProofFileName),
		Token:     token,
		Directory: filepath.Join(root, sessionCarrierProbeDirName),
	}

	broker, err = startSessionCarrierBroker()
	if err != nil {
		return sessionCarrierPlugin{}, nil, errors.Join(err, cleanup())
	}

	if err := sessionCarrierMkdirAll(proof.Directory, 0o700); err != nil {
		return sessionCarrierPlugin{}, nil, errors.Join(fmt.Errorf("create OpenCode session carrier probe: %w", err), cleanup())
	}

	wrapper := ""
	mark := filepath.Join(root, sessionCarrierPathMarkName)

	if source := sessionCarrierShellWrapperSource(mark); source != "" {
		wrapper = filepath.Join(root, sessionCarrierShellName)
		if err := sessionCarrierWriteFile(wrapper, []byte(source), 0o700); err != nil {
			return sessionCarrierPlugin{}, nil, errors.Join(fmt.Errorf("write OpenCode session carrier shell: %w", err), cleanup())
		}
	}

	path := filepath.Join(root, sessionCarrierPluginFileName)
	if err := sessionCarrierWriteFile(path, []byte(sessionCarrierPluginSource(wrapper, mark, proof, broker)), 0o600); err != nil {
		return sessionCarrierPlugin{}, nil, errors.Join(fmt.Errorf("write OpenCode session carrier: %w", err), cleanup())
	}

	if err := sessionCarrierHandoff(root, isolation); err != nil {
		return sessionCarrierPlugin{}, nil, errors.Join(fmt.Errorf("handoff OpenCode session carrier: %w", err), cleanup())
	}

	return sessionCarrierPlugin{URL: sessionCarrierFileURL(path), Path: path, Proof: proof, Broker: broker}, cleanup, nil
}
