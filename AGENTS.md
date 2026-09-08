# AGENTS.md

Shared instructions for automated coding agents working in this repository.

## Purpose

This Go ACP agent builds directly on `github.com/coder/acp-go-sdk`. Each Agent
owns one shared `opencode serve` runtime. OpenCode owns model execution and native
state; this package owns ACP dispatch, runtime/XDG coordination, session and turn
routing, REST/SSE mapping, permission requests, config options, and portable
`opencode-sync-events-v1` storage.

## Project Map

- **ACP surface:** `agent*.go`, `ids.go`, and `session_validation.go` own
  construction, dispatch, session registration, admission, and validation.
- **Sessions:** `session*.go` owns prompts, cancellation, config, lifecycle
  delivery, environment carriers, and restore ownership. See
  [sessions](docs/core/sessions.mdx) and [storage](docs/features/session-store.mdx).
- **Media:** `image*.go` owns bounded input validation, handoff reads, output
  mapping, and durable artifact replay.
- **Provider auth:** `auth*.go` owns ordinary-only configured auth methods,
  connection/flow fencing, broker cleanup, and the values-free ledger. See
  [authentication](docs/features/authentication.mdx).
- **Options:** `options.go`, `request_builders.go`, and `plugin_seed.go` own
  builders and ordinary runtime plugin-cache selection.
- **Native boundary:** `internal/opencode` owns REST/SSE, process launch and
  containment, host-authority adapters, portable home locking, provider registry,
  dynamic MCP scopes, and plugin seeding. `internal/lifecycle` owns the local
  lifecycle reducer; `internal/observer` owns OpenTelemetry instrumentation.
- **CLI and docs:** `cmd/acp-go-opencode` owns flags and stdio serving;
  `docs/` and `docs.json` are the Mintlify guide. `integration/` contains native
  proofs behind explicit build tags and execution gates.

## Commands

Use the repository's pinned tooling:

```sh
make test
make lint
make modernize-check
make docs-audit
make audit
```

`make test` runs race-enabled, shuffled unit tests with the configured timeout.
`make audit` runs the full unit suite once with race detection, shuffle, and
coverage reporting, alongside formatting, build, cross-compilation,
vulnerabilities, modernization (`go fix -diff`), docs, and module verification. It can invoke module tooling; inspect the targets
before a task restricted to reading. `make fmt` applies formatting.

Native targets require task authorization, not just an installed/authenticated
CLI or enabled environment gate. The targets are `test-integration-smoke`,
`test-integration-live`, `test-integration-cover`, `test-integration-attended`,
`test-integration-keystore`, and `test-integration-native-browser`. Live prompts
add `ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1`; attended, keystore, and browser proofs
have separate prerequisites in the Makefile. The live model default lives in
`integration/helpers_test.go`; do not assume its current pricing.

## Coding Rules

- Follow standard Go idioms: `ctx` first, no context fields in structs, and `%w`
  for wrapped errors. Preserve public methods, request shapes, validation, and
  error identities.
- Keep ACP orchestration in the root package and native field spellings,
  structured protocol decoding, and launch behavior in `internal/opencode`.
  Shared code stays beside its owning domain; follow existing patterns.
- Update local docs with public API, CLI, ACP method, or `_meta` changes.
- Unless already authorized by the task, ask before changing permission or
  elicitation flow shape, adding extensions or `_meta`, changing the store
  contract, admitting OAuth methods to the reviewed native registry, or changing
  shared XDG ownership and native launch/teardown behavior.

## Testing Rules

- Use `testify/require` and table-driven protocol/builder cases. Add focused
  behavioral regressions; avoid duplicated tests or production seams added only
  to fill coverage. Review coverage alongside behavioral results; no percentage
  threshold replaces meaningful tests.
- Use focused meaningful tests during edits, `make test` for lifecycle/SSE/
  concurrency/cancellation changes, and `make lint` before completion. Run the
  combined full gate once changes settle, according to the task's scope.
- Keep canonical lifecycle fixture bytes unchanged. Deterministic fakes,
  in-memory transports, and self-exec helpers prove adapter behavior; claims
  about native behavior require authorized real-CLI evidence.
- Native tests use exclusive temporary XDG roots. Keep
  `ACP_GO_OPENCODE_TEST_PLUGIN_SEED_DIR` under test scratch space, never the real
  user cache. Use deterministic sentinel prompts and assert stop reasons plus
  streamed updates. Token gates do not provide authorization.

## Security And Boundaries

- Reserve stdout for ACP JSON-RPC; diagnostics go to stderr. Do not log auth
  material, prompts, secrets, tool input/output, or raw native event bodies by
  default. Stored environment overlays can contain secrets.
- Never bypass permission prompts. Fence permissions by session id and a
  published tool-call id in the owning foreground turn; never infer an ambiguous
  active session.
- ACP `logout` clears adapter-owned session state only. Native credential
  mutation belongs exclusively to configured namespaced provider-auth methods
  and reviewed native broker operations. Retain broker runtime and path ownership
  until shutdown and cleanup succeed, including after flow retirement.
- Keep the shared XDG root single-writer. Never open the live native database
  directly; portable state moves through allowlisted online sync events.
- With `WithHostAuthority`, route every native launch through it. Prepared trees
  remain inaccessible until reclaim succeeds; failed preparation remains
  host-owned. Pin media read roots disjoint from Home and scratch before native
  work; preserve those handles across runtime recovery. The host maintains
  directory-domain separation, including mount aliases and root replacement.
  Never fall back to ordinary execution. See
  [security](docs/operations/security.mdx).
- Require the versioned turn envelope for prompts and active cancellation.
  Prompt-causal updates, raw events, and elicitations retain that exact route;
  agent-origin work, lifecycle carriers, and history replay omit it.
- Reject unsupported extensions and provider mutations explicitly. Keep test
  filesystem and network activity limited to the boundary being exercised.
