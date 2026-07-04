# acp-go-opencode

`acp-go-opencode` is a Go ACP agent for OpenCode. It speaks ACP on the
provided stdin/stdout streams and runs one isolated `opencode serve` process for
each ACP session.

OpenCode owns model execution and native state. This package owns ACP dispatch,
process launch, per-session XDG isolation, REST/SSE event mapping, permission
requests, config options, and `opencode-state-v1` session storage.

## Install

```sh
go install github.com/savid/acp-go-opencode/cmd/acp-go-opencode@latest
```

Local run:

```sh
go run ./cmd/acp-go-opencode -path "$(command -v opencode)"
```

The command reserves stdout for ACP JSON-RPC. Diagnostics go to stderr.

## Embedded Go

```go
err := opencodeacp.Serve(ctx, input, output,
	opencodeacp.WithExecutablePath("opencode"),
	opencodeacp.WithHome("/tmp/opencode-acp-home"),
	opencodeacp.WithDefaultModel("opencode/big-pickle"),
)
```

## Public Surface

The package exports `NewAgent`, `Serve`, common process options, OpenCode
options, request builders, `ForkSessionMethod`, `RawEventMethod`, and the
`SessionStore` API. Session durability uses:

```go
const SessionStoreFormat = "opencode-state-v1"
```

Forking is available only through `_opencode/session/fork`.

## Examples

```sh
go run ./examples/minimal-client "Reply with hello from ACP"
go run ./examples/interactive-chat
go run ./examples/resume-from-file
```

## Development

```sh
make test
make test-integration-smoke
make audit
```

Live prompt, permission, and elicitation checks are guarded by
`ACP_GO_OPENCODE_RUN_LIVE_TOKENS=1`.
