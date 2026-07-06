# Interactive Chat

This example is an interactive ACP client that creates an in-process
`opencodeacp` agent, initializes the connection, and creates a session, then
runs a read-eval-print loop that sends each line you type as a prompt and
prints the stop reason for each completed turn.

```sh
go run ./examples/interactive-chat
```

At the `> ` prompt, Enter submits the current line and the turn's stop reason
prints back in brackets. Submit a blank line or type `exit` to leave, and press
Ctrl-C to interrupt. A local `opencode` CLI must be installed and
authenticated.
