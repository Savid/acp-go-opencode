# acp-go-opencode

Go ACP agent for the local OpenCode CLI. It runs one isolated `opencode serve`
process per session, speaks
[Agent Client Protocol](https://agentclientprotocol.com/) over JSON-RPC
streams, and is built on
[`github.com/coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk).

OpenCode owns model execution and native state. This package owns ACP dispatch,
process launch, per-session XDG isolation, REST/SSE event mapping, permission
requests, config options, and `opencode-state-v1` session storage.

Use it as either:

- a standalone ACP subprocess: `acp-go-opencode`
- an embedded Go adapter through `opencodeacp.Serve`

## Install

```sh
go install github.com/savid/acp-go-opencode/cmd/acp-go-opencode@latest
```

For local development:

```sh
go run ./cmd/acp-go-opencode -path "$(command -v opencode)"
```

The process speaks ACP over stdin/stdout and reserves stdout for ACP JSON-RPC;
diagnostics go to stderr. In normal use an editor or ACP host launches it as a
subprocess rather than a human-facing chat UI.

## Quickstart

Run a tiny local client against the agent:

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP"
```

Or try the interactive example:

```sh
go run ./examples/interactive-chat
```

Create, close, and resume an in-memory session to inspect resume behavior:

```sh
go run ./examples/resume-from-file
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
		opencodeacp.WithExecutablePath("opencode"),
		opencodeacp.WithHome("/tmp/opencode-acp-home"),
		opencodeacp.WithDefaultModel("opencode/big-pickle"),
	)
	if err != nil {
		log.Fatal(err)
	}
}
```

See [Go API docs](docs/reference/go-api.mdx) for options such as the OpenCode
executable path, the isolated home root, default model, environment overrides,
session storage, and OpenTelemetry providers.

## What It Provides

- ACP session lifecycle: create, prompt, cancel, close, list, load, resume, and
  fork.
- One isolated `opencode serve` process per session, with per-session XDG data,
  config, cache, and state directories rooted under a configurable home.
- Native OpenCode REST calls and an SSE event stream mapped to ACP prompt
  streaming for messages, reasoning, plans, tool calls, usage, and session
  metadata.
- Permission and question requests bridged to ACP permission and elicitation
  flows.
- MCP stdio and streamable HTTP server configuration through ACP session
  requests.
- Model and mode selection through ACP session config options and
  `_meta.opencode.options`.
- Durable mirroring through a host-provided `SessionStore`; stored rows use the
  `opencode-state-v1` format keyed by `{SessionID, Subpath}`.
- Optional raw native event notifications through `_opencode/rawEvent`.
- OpenTelemetry adapter telemetry without recording prompt or tool secrets by
  default.

The package exports `NewAgent`, `Serve`, agent `Option` constructors, OpenCode
request builders, `OpenCodeOptions`, `ForkSessionMethod`, `RawEventMethod`, and
the `SessionStore` API:

```go
const SessionStoreFormat = "opencode-state-v1"
```

Forking is available only through `_opencode/session/fork`.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Go API](docs/reference/go-api.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Observability](docs/operations/observability.mdx)

## Development

```sh
make test
make audit
make test-integration-smoke
make test-integration-live
```

`make test` runs the unit suite and `make audit` runs the full local gate. Live
integration tests require a local authenticated `opencode` CLI. The smoke
target sets `ACP_GO_OPENCODE_RUN_INTEGRATION=1` and avoids model spend; the live
target additionally sets `ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1` and may spend model
tokens. Live tests always launch OpenCode under an isolated per-session home.
