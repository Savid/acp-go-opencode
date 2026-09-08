// Package opencodeacp exposes the local OpenCode CLI as an Agent Client
// Protocol agent.
//
// Most hosts run the agent over a pair of JSON-RPC streams using [Serve].
// Serve launches one authenticated loopback `opencode serve` process owned by
// the Agent, maps ACP requests into directory-scoped OpenCode REST/SSE calls,
// and keeps that runtime alive across session close. Hosts must complete ACP
// initialization before issuing session or other agent methods.
//
// Hosts should use [Serve] for the JSON-RPC transport; hosts that embed the
// agent directly construct one with [NewAgent] and the same [Option] values.
// OpenCode owns provider execution. In ordinary mode, configured provider-auth
// methods broker explicit credential operations. [WithHome] selects the
// Agent's exclusive shared XDG root. When it is empty, the adapter materializes
// the root beneath the parent
// selected by [WithScratchDir].
//
// [WithHostAuthority] delegates native environment, tree ownership, process
// launch, revocation, and terminality to the embedding host. Without it,
// OpenCode runs as an ordinary process under the current user.
//
// Hosts that need durable remote resume can provide [WithSessionStore]. A
// session store receives `opencode-sync-events-v1` native event bundles keyed
// by the ACP-visible session ID and subpath. The store is authoritative for
// session/list; each bundle restores one session's native aggregate through
// OpenCode's online sync API. The default store lasts for the Agent's lifetime.
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
// [RateLimitsMethod] reads structured OpenCode Go subscription windows and
// OpenRouter key allowances. [WithOpenCodeDirectAPI] controls direct account
// usage requests; quota reads do not submit prompts or spend inference tokens.
//
// Hosts that need adapter telemetry can provide OpenTelemetry providers with
// [WithTracerProvider] and [WithMeterProvider]. The package never configures
// global OpenTelemetry providers; the acp-go-opencode binary handles
// env-based exporter setup for command-line use. Caller-supplied providers
// remain owned by the caller, including ForceFlush and Shutdown.
package opencodeacp
