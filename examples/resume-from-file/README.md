# Resume From File

This example reads an OpenCode `opencode-state-v1` transcript from
`session.jsonl` in this directory into a `SessionStore`, loads the session
through ACP so previous state is rehydrated, then sends one no-tools
smoke-test prompt in-process. It denies tool permissions by default so a
copied session cannot silently run commands while you are checking resume
behavior.

Use it with a saved OpenCode session-state transcript:

```sh
cd examples/resume-from-file
go run . -session <session-id> -cwd /absolute/path/to/project
```

If the JSONL rows carry a `sessionId` (the idmap row) or a `session.sessionId`
snapshot field, `-session` can be omitted and the id is inferred; `-cwd`
likewise defaults to the snapshot cwd or the current directory. Loading uses
normal ACP `session/load`, and the prompt uses normal ACP `session/prompt`.

Pass `-prompt "..."` to change the smoke-test turn, `-path` to point at a
specific `opencode` CLI, and `-scratch-dir` to set the parent directory for
ephemeral per-session scratch (empty uses the system temp directory). A local
`opencode` CLI must be installed and authenticated to load a real session and
run the prompt.
