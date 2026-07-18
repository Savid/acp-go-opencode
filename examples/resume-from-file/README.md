# Resume From File

This example reads an OpenCode `opencode-sync-events-v1` event bundle from
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

If the JSONL row carries `session.sessionId`, `-session` can be omitted and the
id is inferred; `-cwd` likewise defaults to the captured cwd or the current
directory. A different absolute cwd triggers validated event-path rebasing. Loading uses
normal ACP `session/load`, and the prompt uses normal ACP `session/prompt`.

Pass `-prompt "..."` to change the smoke-test turn, `-path` to point at a
specific `opencode` CLI, and `-scratch-dir` to set the parent directory for
shared runtime scratch (empty uses the system temp directory). A local OpenCode
`1.18.3` CLI must be installed and authenticated to load a real session and
run the prompt.
