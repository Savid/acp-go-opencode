# acp-go-opencode

`acp-go-opencode` exposes [OpenCode](https://opencode.ai) as an
[Agent Client Protocol](https://agentclientprotocol.com) agent. One
`opencode serve` process handles the Agent's sessions through authenticated
loopback HTTP and a shared event stream.

Sessions retain OpenCode's native storage. After closing the adapter,
continue a session in the same directory and native home:

```sh
opencode run --session NATIVE_SESSION_ID "Continue the task"
```

New, load, and resume responses and session-list entries expose the current
native ID as `_meta.opencode.nativeSessionId`. Use it for native CLI continuation.
ACP requests continue to use the stable ACP `sessionId`. The store's configuration
record saves both IDs with the matching native history.

## Install and run

```sh
go install github.com/savid/acp-go-opencode/cmd/acp-go-opencode@latest
acp-go-opencode [-path opencode] [-home DIR] [-scratch-dir DIR] [-model provider/id] [-seed-file rel=host]... [-debug]
```

Verified against OpenCode 1.18.31. A bare `-path` is resolved on the inherited
PATH. `-home` maps `DIR/data`, `DIR/config`, `DIR/cache`, and `DIR/state` to the
four XDG home variables; omit it to use native home resolution. Native CLI
continuation uses those same variables when a home was supplied.
`-seed-file` writes a relative file inside OpenCode's configuration directory.
`-scratch-dir` holds the temporary environment plugin. `-version` prints the
adapter version. Standard `OTEL_*` variables configure telemetry exporters.

## Embed

```go
err := opencodeacp.Serve(ctx, os.Stdin, os.Stdout,
    opencodeacp.WithHome("/srv/opencode"),
    opencodeacp.WithSessionStore(store),
)
```

Options: `WithExecutablePath`, `WithHome`, `WithScratchDir`,
`WithInputHandoffRoot`, `WithDefaultModel`, `WithConfiguredModels`, `WithEnv`,
`WithSeedFiles`, `WithSessionStore`, `WithConcurrencyLimits`, `WithImageLimits`,
`WithLogger`, `WithTracerProvider`, `WithMeterProvider`, `WithTextMapPropagator`,
`WithAgentName`, `WithAgentTitle`, `WithAgentVersion`.

### Session options

Pass `_meta.opencode.options` on new, load, or resume, or use
`WithSessionOpenCodeOptions` from Go.

| Field | Meaning |
|---|---|
| `model` | Provider-qualified model ID |
| `mode` | Native agent name, such as `build` or `plan` |
| `effort` | Native model variant, forwarded unchanged |
| `permission` | Native tool policy: `ask`, `allow`, or `deny`; default `ask` |
| `env` | Environment overlay for tools in this session |
| `extraPathDirs` | Absolute directories prepended to the session's PATH in order |
| `outputSchema` | Nonempty JSON Schema passed through native structured output |

The environment plugin reads the addressed session's metadata through OpenCode's
API. Child sessions inherit that carrier through their native parent. OpenCode
keeps its native authentication and configuration. Nonempty `mcpServers` and
unknown owned options are invalid parameters.

`session/set_config_option` accepts nonempty `model`, `mode`, and `effort`
values. The native provider catalog supplies model names, image capabilities,
context windows, and available variants. Configured and selected models are
included even when absent from the catalog. Structured results appear at
`_meta.opencode.structuredOutput` on the prompt response.

Native permission requests use ACP permissions; native questions use ACP form
elicitation. Missing or cancelled answers reject the native request. Commands
come from OpenCode's command catalog. An exact `/name` match against an
advertised command uses the native command endpoint; other text uses the
message endpoint.

Images enter as inline base64 or validated file handoffs. Output supports native
file parts and tool attachments, with bounded local reads and image limits.
Remote URLs become resource links. `_meta.opencode.rawEvent.enabled` enables
`_opencode/rawEvent`; image bytes are omitted from that diagnostic channel.
Optional lifecycle negotiation supplies ordered session and turn updates.

### Persistence and runtime

`SessionStoreFormat` is `opencode-sync-events-v1`. The main subpath holds native
sync events for one conversation and its descendants. The `config` sidecar
holds accepted options and captured local image bytes. Complete generations
commit atomically before a prompt returns. Scoped `opencode db` queries read
only that conversation graph; a second read fences each snapshot. The native
CLI opens its own database files.

Load imports missing events through the native sync API and replays ACP history.
Resume imports without replay. Existing native history must agree at every
shared sequence; newer native turns are adopted. Local image replay remains
available after its original file is removed. The default store is in memory;
supply a durable store to restore across adapter restarts.

Close releases one session while peers retain the shared server. A server crash
fails affected work, and the next operation starts a replacement and rebinds the
addressed session. Delete tombstones the store entry. Native state remains
available to OpenCode's CLI. A native-home file lock prevents two adapter servers
from owning the same home concurrently.

## Development

```sh
make test
make lint
make audit
make test-integration-smoke
ACP_GO_OPENCODE_MODEL=provider/model make test-integration-live
```

Unit tests use a scripted native HTTP server inside the test binary and require
no installed OpenCode or credentials. Smoke tests use the installed CLI without
model calls. Live tests copy native auth into temporary homes and spend tokens.

## Account usage

`AccountUsageMethod` (`_opencode/accountUsage`) accepts `sessionId` and
`providerId` (`opencode-go`, `openrouter`, `anthropic`, or `openai-codex`).
Initialization advertises the method, session scope, and supported providers.
A provider the catalog routes through a gateway that publishes a usage report is
read from that report with the catalog's key for it. Reads hold the session's
foreground gate and spend no model tokens.

The adapter resolves the directory's effective API key and route through the
native server. Only official endpoints and verified API-key routes are read;
authentication plugins for the requested provider or unverified authentication
overrides yield `not_reported`. Plugins for other providers do not block reads.
Credentials stay local. Provider HTTP reads come from `github.com/savid/acp-go-core/usage`.

OpenCode Go reports rolling, weekly, and monthly percentage windows. OpenRouter
reports key spending caps, lifetime spend, free-model request counts, and any
account credit balance accessible with the same key. Dollar amounts are USD;
a missing cap is explicitly uncapped, zero is a real value, and a negative
remaining balance is preserved. Account credits and key caps remain separate.
Each measurement retains its own observation and expiry times. Unavailable
optional account credits do not discard key data.

Claude and ChatGPT subscription usage require effective OAuth credentials from
the native runtime. The provider catalog exposes API-key routes only; plugin
credentials are private, so subscription usage is not advertised.
