# Minimal Client

Launches `acp-go-opencode` as a subprocess, creates one session in the current
directory, sends one prompt, and prints the streamed answer. Every permission
request is allowed once.

```sh
go run ./examples/minimal-client "Reply with a short hello from ACP"
```

A local `opencode` must be installed and authenticated; the session inherits your
environment and opencode's home exactly as running `opencode` in this directory would.
