Captured from OpenCode 1.18.30 on 2026-09-14.

An installed `opencode serve --pure` ran in temporary XDG directories. A direct
native `POST /session/{id}/message` with `noReply: true` stored the probe text;
`POST /session/{id}/abort` then published idle. No ACP prompt was active and
no model service was called. These are frames from `/global/event` SSE.

The fixture retains message and idle frames in delivery order. Session,
message, part, and event IDs and the temporary directory are normalized;
payloads and timestamps are otherwise unchanged. The frames exercise adapter
ownership and settlement; no model turn is involved.
