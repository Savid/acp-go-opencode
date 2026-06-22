// Package opencodeacp exposes the native OpenCode ACP server as an embeddable
// Go process wrapper.
//
// OpenCode already implements the Agent Client Protocol through `opencode acp`.
// This package keeps that protocol implementation intact and focuses on
// process launch, proxy stream handling, and lifecycle ownership. Most hosts
// should use [Serve] with their own ACP JSON-RPC streams.
//
// The provided input and output streams are reserved for ACP traffic. Logs,
// debug output, and diagnostics must use stderr or another side channel.
package opencodeacp
