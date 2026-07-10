// Package opencodeacp exposes the local OpenCode CLI as an Agent Client
// Protocol agent.
//
// Most hosts run the agent over a pair of JSON-RPC streams using [Serve].
// Serve launches one isolated loopback `opencode serve` process per ACP
// session, maps ACP requests into OpenCode REST calls, streams native SSE
// events back to the client as ACP session updates, and tears the process
// down when the session closes. Hosts must complete ACP initialization before
// issuing session or other agent methods.
//
// Hosts should use [Serve] for the JSON-RPC transport; hosts that embed the
// agent directly construct one with [NewAgent] and the same [Option] values.
// OpenCode authentication and provider credentials remain owned by the local
// OpenCode installation. Each session runs under its own XDG home derived
// from [WithHome], so native state never leaks between sessions.
//
// Hosts that need durable remote resume can provide [WithSessionStore]. A
// session store receives `opencode-state-v1` snapshots keyed by the
// ACP-visible session ID and subpath, can back session/list, and can hydrate
// a snapshot into a fresh per-session XDG home for session/load or
// session/resume when the local native state is absent.
//
// Hosts that need structured output can attach [OpenCodeOptions] with
// [WithSessionOpenCodeOptions] or use [WithSessionOutputSchema]. Parsed
// response objects are returned under _meta.opencode.structuredOutput.
//
// Hosts can call [CallForkSession] for the OpenCode fork extension method
// _opencode/session/fork. Raw OpenCode events are emitted as [RawEventMethod]
// notifications only when a session request opts in with
// [WithSessionRawEvents].
//
// Hosts that need adapter telemetry can provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]. The package never configures
// global OpenTelemetry providers; the acp-go-opencode binary handles
// env-based exporter setup for command-line use. Caller-supplied providers
// remain owned by the caller, including ForceFlush and Shutdown.
package opencodeacp
