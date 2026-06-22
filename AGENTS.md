# acp-go-opencode

This repository provides a small Go wrapper around the native `opencode acp`
server. Keep the wrapper thin: OpenCode owns ACP behavior; this package owns
process launch, stream wiring, options, and embeddability.

## Development

- Run `make test` for local verification.
- Run `make audit` before larger changes when time allows.
- Keep stdout reserved for ACP JSON-RPC in the CLI. Logs and diagnostics belong
  on stderr.
- Prefer adding new wrapper options before introducing ACP method translation.
