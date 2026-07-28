# AGENTS.md

Shared instructions for automated coding agents working in this repository.

## Purpose

This project is a Go implementation of an ACP agent for OpenCode. Each Agent
owns one shared `opencode serve` runtime and builds directly on
`github.com/coder/acp-go-sdk`. OpenCode owns model execution and native state;
this package owns ACP dispatch, supervised runtime/XDG ownership,
directory/session/turn routing, REST/SSE event mapping, permission requests,
config options, and `opencode-sync-events-v1` session storage.

## Project Map

Organized by domain. The public surface lives in the root package
`opencodeacp`; the native OpenCode protocol glue lives under
`internal/opencode`.

- **Entrypoint** (`cmd/acp-go-opencode`): process entrypoint (`main.go`), flag
  parsing, logger and signal setup, version reporting, OpenTelemetry wiring
  (`otel.go`), and the ACP stdio serve loop. `stdout` is reserved for ACP
  JSON-RPC; diagnostics go to `stderr`.
- **ACP agent surface** (root package: `agent.go`, `agent_connection.go`,
  `agent_session.go`, `agent_goroutine.go`, `ids.go`, `session_validation.go`):
  the `Agent` type, `NewAgent`, `Serve`, `Initialize` capabilities and `_meta`,
  session lifecycle handlers (new, load, resume, list, close, delete, fork),
  the extension-method handler, goroutine panic recovery, concurrency limits,
  session ID generation, and start-path validation.
- **Session orchestration** (`session.go`, `session_prompt.go`,
  `session_config.go`, `session_meta.go`): per-session turn state, prompt and
  cancel handling, native REST/SSE event mapping, permission and question
  reconciliation, plan and message emission, config-option (model/mode)
  resolution, and session metadata parsing.
- **Options and builders** (`options.go`, `request_builders.go`): agent
  `Option` constructors (executable path, scratch directory, default model, env,
  session store, telemetry providers, and OpenCode runtime toggles) and the
  exported request builders, `OpenCodeOptions`, MCP server builders, and fork
  call helper.
- **Session storage** (`session_store.go`, `session_state_store.go`,
  `session_restore_ownership.go`): the
  `SessionStore` interface, `InMemorySessionStore`, the
  `opencode-sync-events-v1` format constant, allowlisted native-event capture,
  durable restore ownership, path rebasing, and online replay verification.
- **Raw events** (`raw_events.go`): opt-in raw native event gating and the
  `_opencode/rawEvent` notification config.
- **Native OpenCode client** (`internal/opencode`, package `opencode`): launch
  and readiness of the loopback `opencode serve` process, native REST and
  directory-scoped SSE, dynamic MCP scopes, portable home locking, and the
  per-GOOS dual-supervisor process-tree fence.
- **Observability** (`internal/observer`): OpenTelemetry instrumentation
  helpers (trace/metric definitions, trace-context propagation) and the
  instrumentation name.
- **Live tests** (`integration`, build tag `integration`): integration tests
  that launch the real local `opencode` CLI through `opencode serve`.
- **Docs** (`docs/`, `docs.json`): Mintlify guide. Update alongside public API,
  CLI flag, ACP method, or `_meta` field changes.

## Commands

```sh
make test
make audit
```

`make test` runs the unit suite (`go test -race -shuffle=on ./...`). `make audit` runs the full
local gate: format, lint, build, unit tests, cross-compile checks, coverage
gate, vulnerability scan, modernization check, docs audit, and module tidy and
verification. `make lint`, `make fmt`, and `make vuln` are available
individually. Lint details live in `.golangci.yml`.

`make docs-audit` checks that the required docs files exist, that every CLI
flag is registered in both `docs/reference/cli.mdx` and the command source,
and that public docs and examples do not reintroduce removed public terms.
Keep `stdout` reserved for ACP JSON-RPC in the CLI; logs and diagnostics
belong on `stderr`.

Run live integration tests only when a local OpenCode CLI is installed and
authenticated:

```sh
make test-integration-smoke
make test-integration-live
```

`make test-integration-smoke` sets `ACP_GO_OPENCODE_RUN_INTEGRATION=1` and
covers behavior that does not spend model tokens. `make test-integration-live`
additionally sets `ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1` and may spend model
tokens. `make test-integration-cover` runs the smoke suite against a compiled
binary named through `ACP_GO_OPENCODE_AGENT_BINARY` with `GOCOVERDIR`
coverage. The live suite reads `ACP_GO_OPENCODE_MODEL`,
`ACP_GO_OPENCODE_PERMISSION_PROMPT`, and `ACP_GO_OPENCODE_QUESTION_PROMPT` to
tune the model and the prompts used to exercise permission and question flows.
Live tests always launch OpenCode under an exclusive test runtime XDG root.

`make test-integration-attended` sets `ACP_GO_OPENCODE_RUN_ATTENDED=1` and runs
the provider-auth flows a human must approve at the provider.
`make test-integration-keystore` sets `ACP_GO_OPENCODE_RUN_KEYSTORE=1` and runs
the credential-residence matrix and the Linux browser-launcher proof against the
container fixture in `integration/keystore`; it fails rather than skips when no
container runtime is available. Neither target joins `make audit`.

## Coding Rules

- Follow standard Go idioms: `ctx` first, no `ctx` in structs, and `%w` for
  wrapped errors.
- Keep the public root package focused on the ACP surface; keep native REST/SSE
  glue, process launch, and their shared constants in `internal/opencode`.
- Prefer structured protocol types and JSON decoding over ad hoc string parsing;
  native OpenCode field spellings stay confined to `internal/opencode`.
- Preserve ACP method names, request/response shapes, and validation behavior.
- Keep protocol glue narrow, documented, and close to the ACP method it serves.
- Keep shared code next to the domain it serves; avoid generic catch-all
  packages such as `utils`, `helpers`, or `common`.
- Follow existing package patterns before introducing new abstractions.

## Testing Rules

- Use `testify/require` for assertions.
- Prefer table-driven tests for builder and protocol-mapping cases.
- Run `go test ./...` (or `make test`) for ordinary changes.
- Run `go test -race ./...` for session, SSE, concurrency, or cancellation
  changes.
- Run `make lint` before considering work complete.
- Live integration tests launch the actual `opencode` binary; unit tests use
  in-memory transports and fake OpenCode helpers.
- Keep live prompts deterministic with exact sentinel replies, and assert the
  ACP stop reason plus streamed updates where practical.
- Guard token-spending checks behind `ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1` so the
  smoke suite stays free of model spend.

## Ask Before

Unless explicitly requested, ask before:

- Changing the permission or question/elicitation flow shape.
- Adding new ACP extension methods or `_meta` fields.
- Changing the `opencode-sync-events-v1` session-store contract.
- Changing shared XDG ownership or the native process launch and teardown
  behavior.

## Security And Boundaries

- **IMPORTANT**: Do not silently bypass permission prompts. The permission flow
  is load-bearing for user trust in this agent.
- **IMPORTANT**: Do not manage OpenCode authentication state. ACP `logout`
  only clears adapter-owned session state.
- Do not log auth material, user secrets, prompts, tool input, tool output, or
  raw native OpenCode event bodies by default.
- Keep the shared XDG root single-writer and never open the live native
  database directly. Portable state moves only through allowlisted online sync
  events.
- Route every prompt, active cancellation, session update, raw event, and
  elicitation with the versioned turn envelope. Fence permissions structurally
  by session id plus a tool-call id pending in the current turn; never infer an
  ambiguous active session.
- Reject unsupported ACP extension or provider mutation methods with explicit
  protocol errors unless this agent implements a namespaced extension.
- Avoid broad filesystem or network behavior in tests unless the test is
  explicitly about that boundary.
