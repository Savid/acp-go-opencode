# acp-go-opencode

Go wrapper for the native OpenCode ACP server. It launches `opencode acp`
behind a small JSON-RPC proxy, forwards native ACP traffic by default, and
exposes the same behavior as an embeddable Go API.

Use it as either:

- a standalone ACP subprocess: `acp-go-opencode`
- an embedded Go adapter through `opencodeacp.Serve`

## Install

```sh
go install github.com/savid/acp-go-opencode/cmd/acp-go-opencode@latest
```

For local development:

```sh
go run ./cmd/acp-go-opencode
```

The process speaks ACP over stdin/stdout. In normal use, an editor or ACP host
launches it as a subprocess.

## Quickstart

Run a tiny local client against the embedded wrapper:

```sh
go run ./examples/minimal-client "Reply with hello from ACP"
```

Or try the line-based interactive example:

```sh
go run ./examples/interactive-chat
```

Or verify session load/resume/fork/import continuity with the saved OpenCode
export fixture in `examples/session-continuity/session.json`:

```sh
go run ./examples/session-continuity
```

After the imported session resumes, the example asks for one prompt. Press
Enter on a blank line to exit without spending model tokens. To send a live
prompt non-interactively:

```sh
go run ./examples/session-continuity -prompt "What continuity phrase was saved earlier?"
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
		opencodeacp.WithCwd("/workspace/project"),
		opencodeacp.WithPure(true),
		opencodeacp.WithHostname("127.0.0.1"),
		opencodeacp.WithPort(0),
	)
	if err != nil {
		log.Fatal(err)
	}
}
```

## Compatibility Helpers

The wrapper also exposes small helpers for embedders that want a stable Go
surface around ACP SDK and OpenCode CLI changes:

```go
session := opencodeacp.NewSessionRequest("/workspace/project")
prompt := opencodeacp.TextPromptRequest(sessionID, "Summarize this repo")

probe, err := opencodeacp.ProbeCLI(context.Background())
models, err := opencodeacp.NewModelCatalog().Load(context.Background())

cfg, err := opencodeacp.ParseConfig(map[string]any{
	"opencode-path": "/usr/bin/opencode",
	"env": map[string]any{
		"PATH": "/usr/bin",
	},
})
```

`ProbeCLI` and model loading are optional inspection helpers. `Serve` does not
depend on those commands succeeding.

## What It Provides

- Process supervision for `opencode acp`.
- A narrow ACP JSON-RPC proxy that passes native methods through, augments
  `initialize` metadata, and handles wrapper-owned `_opencode/session/export`,
  `_opencode/session/import`, and `_opencode/session/delete` methods.
- Caller-owned ACP streams for embedding in Go applications.
- Options for OpenCode path, working directory, pure mode, log forwarding,
  listener settings, CORS, mDNS, question-tool env, OpenCode telemetry env,
  isolated temp directories, environment overrides, and extra OpenCode flags.
- ACP request builders, map-based config validation, CLI probing, and
  soft-fail parsing for `opencode models --verbose`.
- Session file helpers for native `opencode export`, `opencode import`, and
  `opencode session delete`.
- A standalone CLI that keeps stdout reserved for ACP and sends diagnostics to
  stderr.

This wrapper intentionally does not reimplement native OpenCode ACP lifecycle
methods. OpenCode remains the ACP server; this package is the embeddable launch,
lifecycle, and small extension layer.

## Docs

- [Overview](docs/overview.mdx)
- [Run modes](docs/get-started/run-modes.mdx)
- [Install](docs/get-started/install.mdx)
- [Examples](docs/get-started/examples.mdx)
- [Sessions](docs/core/sessions.mdx)
- [Prompt streaming](docs/core/prompt-streaming.mdx)
- [Go API](docs/reference/go-api.mdx)
- [ACP methods](docs/reference/acp-methods.mdx)
- [Session import](docs/reference/session-import.mdx)
- [Updates](docs/reference/updates.mdx)
- [Security](docs/operations/security.mdx)
- [Observability](docs/operations/observability.mdx)

## Development

```sh
make test
make test-integration-smoke
make test-integration-live
make test-integration-cover
make audit
```

Live integration tests require a local `opencode` CLI. The smoke suite does not
spend model tokens; it starts `opencode acp`, verifies closed-input behavior,
drives ACP initialize/session lifecycle requests through the wrapper, and checks
the proxy session export/delete/import extensions. The live target sets
`ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1` and may spend model tokens.

`coverage-check` enforces 100% statement coverage for the Go wrapper and
examples. Native OpenCode ACP behavior is also validated by live integration
tests, with token-spending cases kept behind `ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1`.
