# Resume From File

This example reads an OpenCode `opencode-state-v1` transcript from
`session.jsonl` in this directory into a `SessionStore`, loads the session
through ACP `session/load` so previous state is rehydrated, then sends one
no-tools smoke-test prompt and reports the stop reason.

Use it with a saved OpenCode session-state transcript:

```sh
cd examples/resume-from-file
go run . -session <session-id> -cwd /absolute/path/to/project
```

Flags:

- `-file` — path to the transcript JSONL (defaults to `session.jsonl`).
- `-session` — session id; if omitted, it is inferred from the `sessionId`
  field found in the JSONL rows.
- `-cwd` — session cwd; if omitted, it is inferred from the JSONL `cwd`, then
  falls back to the current working directory.
- `-prompt` — prompt sent after loading (defaults to a no-tools smoke test).
- `-path` — path to the `opencode` CLI.
- `-home` — parent root for isolated OpenCode session state.

Each JSONL row is a valid `opencode-state-v1` `SessionStoreEntry`: the state
snapshot carries `session.sessionId` and `session.cwd`, and the idmap row
carries the top-level `sessionId`. Loading uses normal ACP `session/load`, and
the smoke turn uses normal ACP `session/prompt`. A local `opencode` CLI must be
installed and authenticated to load a real session and run the prompt.
