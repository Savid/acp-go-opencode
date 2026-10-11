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

A bare `-path` is resolved on the inherited PATH. `-home` maps `DIR/data`,
`DIR/config`, `DIR/cache`, and `DIR/state` to the four XDG home variables; omit
it to use native home resolution. Native CLI
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
API once, and again after the adapter changes any session's carrier. Child
sessions inherit that carrier through their native parent. OpenCode
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

Each model call reports a `usage_update` with its token breakdown when its
`step-finish` part arrives. Agent message and thought chunks carry no
`messageId`, and the breakdown carries no `responseId`: OpenCode keeps none of
the gateway's response ids, and its own message and part ids are not response ids.

### Persistence and runtime

`SessionStoreFormat` is `opencode-sync-events-v2`. The main subpath holds native
sync events for one conversation and its descendants. The `config` sidecar
holds accepted options, the session title, and captured local image bytes.
A session's title is its first prompt's first non-empty text; listing reads
stored sessions from their `config` record alone.

The first commit publishes the whole conversation graph. Each settled turn
then reads the events added since the last commit with a scoped `opencode db`
query, with the graph idle before and after the read, and appends them before
the prompt returns. Each read selects about 8 MiB of event data, so a long
history reads in several queries. A graph that starts running during a read is
read again once it is idle. Events OpenCode writes after a turn reports idle,
such as a generated title, join the next commit. A configuration change
appends only the new configuration and reads no native history; a commit with
neither writes nothing. A turn reads its own
messages a page at a time from the newest, never the whole session. The
native CLI opens its own database files.

Load imports missing events through the native sync API, one conversation at a
time with parents first, and replays ACP history. Resume imports without
replay. Restore adopts the stored history and commits only newer native events
and the configuration. Existing native history must agree at every
shared sequence; newer native events are adopted after the stored ones, which
stay an exact prefix of the restored history. Local image replay remains
available after its original file is removed. The default store is in memory;
supply a durable store to restore across adapter restarts.

Close releases one session while peers retain the shared server. A server crash
fails affected work, and the next operation starts a replacement and rebinds the
addressed session. Delete tombstones the store entry. Native state remains
available to OpenCode's CLI. A native-home file lock prevents two adapter servers
from owning the same home concurrently.

### Native memory settings

OpenCode reads these variables from its environment; the adapter leaves them
to the host. Hosts that bound memory can set them with `WithEnv` or in the
adapter's environment:

| Variable | Effect |
|---|---|
| `OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER=true` | No recursive file watcher |
| `OPENCODE_DISABLE_FFF=true` | File search runs ripgrep instead of keeping an in-process index |
| `OPENCODE_DISABLE_MODELS_FETCH=true` | No models.dev download or hourly refresh; the catalog comes from the cached or built-in snapshot, so a binary without either has an empty catalog |
| `OPENCODE_DISABLE_AUTOUPDATE=true` | No update check |

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

## Context compaction

Reports summary creation as progress, summary errors as failure or
cancellation, and the native compaction success event as completion. The
parent compaction part identifies the automatic or manual trigger when
present. Context counts are unavailable.

Notifications carry `acp-go.dev/compaction` on the notification’s `_meta`,
with an otherwise empty `session_info_update`. The value is `acp-go-core`
`wire.Compaction`: a required `compactionId` and `status`, and optional
`trigger`, `contextBefore`, and `contextAfter`. A start and its outcome share
an ID. Unknown facts are omitted. These are live notifications; historical
replay emits none. Usage accounting is independent.
