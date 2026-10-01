# Native Fixtures

Each fixture holds frames from `/global/event` SSE of an installed
`opencode serve` running in temporary XDG directories, in delivery order.
Session, message, part, call, and event IDs and the temporary directory are
normalized; payloads and timestamps are otherwise unchanged.

## agent-origin.json

Captured from OpenCode 1.18.30 on 2026-09-14 with `opencode serve --pure`.
A direct native `POST /session/{id}/message` with `noReply: true` stored the
probe text; `POST /session/{id}/abort` then published idle. No ACP prompt was
active and no model service was called. The fixture retains message and idle
frames. The frames exercise adapter ownership and settlement; no model turn is
involved.

## compaction.json

Captured from OpenCode 1.18.33 on 2026-10-01. The configuration declared a
custom `@ai-sdk/openai-compatible` provider, `omp`, routed to an
OpenAI-compatible gateway, whose model entry carries no context limit. A
native prompt asked `openrouter/qwen/qwen3.8-flash` to read a file and run
`ls`, which took seven tool-using model calls. `POST /session/{id}/summarize`
then compacted the session, and a second native prompt ran one more call.

## steer.json

Captured from OpenCode 1.18.33 on 2026-10-01 with the built-in `openrouter`
provider and `qwen/qwen3.8-flash`, whose catalog context limit is 1,000,000.
While a native prompt's first call ran a slow `ls`, a second native prompt
arrived; opencode answered it in the same run under the new user message.

The last two fixtures retain message, part, status, idle, and compaction
frames and drop text deltas and frames unrelated to the session. Every model
call reports its usage only on its `step-finish` part.
