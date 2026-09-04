# acp-go-opencode

Go ACP agent that exposes the local OpenCode CLI as an [Agent Client Protocol](https://agentclientprotocol.com/) agent.

[![Go Reference](https://pkg.go.dev/badge/github.com/savid/acp-go-opencode.svg)](https://pkg.go.dev/github.com/savid/acp-go-opencode)
[![CI](https://github.com/savid/acp-go-opencode/actions/workflows/go-test.yml/badge.svg)](https://github.com/savid/acp-go-opencode/actions/workflows/go-test.yml)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

It runs one authenticated `opencode serve` runtime for all sessions owned by an
agent instance, speaks ACP over JSON-RPC streams, and builds on
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk).

Use it as either:

- a standalone ACP subprocess: `acp-go-opencode`
- an embedded Go adapter through `opencodeacp.Serve`

## Install

Library:

```sh
go get github.com/savid/acp-go-opencode
```

CLI:

```sh
go install github.com/savid/acp-go-opencode/cmd/acp-go-opencode@latest
```

The `acp-go-opencode` binary speaks ACP over stdin/stdout and reserves stdout
for ACP JSON-RPC while diagnostics go to stderr; an editor or ACP host launches
it as a subprocess rather than a human-facing chat UI.

## Quickstart

The example programs run from a checkout of this repo, so clone it first:

```sh
git clone https://github.com/savid/acp-go-opencode && cd acp-go-opencode
```

Run a tiny local client against the agent:

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP."
```

Start an interactive session against the agent:

```sh
go run ./examples/interactive-chat
```

Load and resume a stored session transcript:

```sh
go run ./examples/resume-from-file -file ./examples/resume-from-file/session.jsonl
```

## Embedded Go

```go
package main

import (
	"context"
	"log"
	"os"

	opencodeacp "github.com/savid/acp-go-opencode"
)

func main() {
	err := opencodeacp.Serve(context.Background(), os.Stdin, os.Stdout,
		opencodeacp.WithDefaultModel("openai/gpt-default"),
	)
	if err != nil {
		log.Fatal(err)
	}
}
```

See the [Go API reference](https://pkg.go.dev/github.com/savid/acp-go-opencode)
for options such as the OpenCode executable path, the exclusive runtime home,
scratch parent, default model, process environment, session storage, and
OpenTelemetry providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  fork.
- One Agent-owned `opencode serve` process and shared XDG root. `-home` /
  `WithHome` selects that exclusive root; `-scratch-dir` / `WithScratchDir`
  selects the parent used when the adapter materializes one. The root must be
  on an approved local filesystem; network, overlay, and unknown filesystem
  semantics fail closed before the native process starts.
- Native sessions remain independently routed inside the shared runtime.
  Directory-scoped MCP is bound to one live session principal per canonical
  working directory. Routine cancel and timeout interrupt only the addressed
  session and await its native idle acknowledgement.
- Native stream gaps, runtime exits, and host delivery failures fence and retire
  the exact producing runtime generation before replacement admission. Omitting
  `WithHostAuthority` selects ordinary same-identity execution. An embedded
  managed host supplies `HostAuthority`; every native launch then uses its exact
  environment and process/tree operations, and authority loss fails closed with
  no ordinary-launch retry. Runtime teardown closes the native protocol first,
  revokes the process tree, waits for terminality, reclaims prepared trees, and
  only then removes generated roots.
- A native-server crash fails the active turn, retains loaded logical
  sessions, and reconstructs a session from its last committed sync-event
  generation before a following prompt can reach the replacement runtime.
- Native OpenCode REST calls and a bounded, ordered, lossless SSE event/terminal
  channel mapped to ACP streaming for messages, reasoning, plans, tool calls,
  usage, and session metadata. EOF or delivery failure fences the exact native
  generation; it is never hidden by reconnecting the same generation.
- Prompt image and resource-blob input gated before a turn starts, with the
  effective per-image and per-prompt byte bounds advertised at initialize under
  `acp-go.dev/mediaEnvelope` so a host can pre-check against the exact numbers
  the gates enforce. `WithInputHandoffRoot` additionally accepts a
  digest-verified local handoff form: an image block whose bytes are read from
  beneath that read root instead of being carried inline. Unset, no inbound path
  is ever read.
- Brokered provider logins through the seven `_opencode/auth/*` extension
  methods during ordinary execution, advertised only while both
  `-provider-auth-root` / `WithProviderAuthRoot` and `-home` / `WithHome` are
  configured. The surface is withheld when `WithHostAuthority` is supplied. The adapter
  installs a completed credential into OpenCode's own durable store and hands
  none back: there is no credential leg and no injection key. Each device or
  paste-back flow runs in a short-lived broker home destroyed on every terminal
  transition, and the durable ledger under the auth root records slot identity
  and provenance only, never credential material.
- Permission and question requests bridged to ACP permission and elicitation
  flows.
- MCP stdio and streamable HTTP server configuration through ACP session
  requests.
- Model and mode selection through ACP session config options and
  `_meta.opencode.options`.
- Durable, credential-free native event snapshots through a host-provided
  `SessionStore`; stored rows use `opencode-sync-events-v1`, keyed by
  `{SessionID, Subpath}` and requiring OpenCode `1.18.3` or newer.
- Versioned `acp-go.dev/route` envelopes bind each prompt and its causal updates,
  raw events, and elicitations to one turn nonce. Agent-origin work between
  prompts omits a route and never borrows a later nonce. Permission requests are
  fenced structurally by session id plus a published tool-call id. Permission
  and elicitation JSON-RPC requests are registered before their ordered pending
  action update.
- Optional raw native event notifications through `_opencode/rawEvent`.
- OpenTelemetry adapter telemetry without recording prompt or tool secrets by
  default.

## Slash Commands

Native OpenCode commands are refreshed from the running `opencode serve` process
and projected into ACP `AvailableCommand` entries as the session's command set
changes. A slash-prefixed prompt that matches a command runs it natively.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Go API](docs/reference/go-api.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Observability](docs/operations/observability.mdx)

Full Go API reference:
[pkg.go.dev/github.com/savid/acp-go-opencode](https://pkg.go.dev/github.com/savid/acp-go-opencode).

## Development

```sh
make audit
make test-integration-smoke
make test-integration-live
make test-integration-cover
```

`make audit` runs the full local gate: format, lint, build, unit tests,
coverage, cross-compile, vuln, and docs checks. Live integration tests require a
local authenticated OpenCode `1.18.3`-or-newer CLI. `make test-integration-smoke` sets
`ACP_GO_OPENCODE_RUN_INTEGRATION=1` and avoids model spend;
`make test-integration-live` additionally sets `ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1`
and may spend model tokens; `make test-integration-cover` runs the smoke suite
against a coverage-instrumented binary. Live tests always launch OpenCode under
an isolated runtime scratch directory.

## License

Distributed under the GNU General Public License v3.0. See [LICENSE](LICENSE).
