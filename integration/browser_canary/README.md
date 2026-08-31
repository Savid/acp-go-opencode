# Real-native browser canary

This fixture drives OpenCode's current Snowflake external-browser login through
the adapter's production provider-auth broker, then uses passive `execve`
tracing to require a real browser attempt and prove every launcher resolved
inside the production-generated shim. The runtime container has no GUI,
credentials, host mounts, network, or supplied host authority.

In this pinned release the Snowflake method launches its authorization URL and
then returns a loopback completion variant. The production broker rejects that
variant; the canary requires that exact rejection and the broker's cleanup in
addition to the independent launch trace.

- OpenCode: `1.18.13`, from the [official release](https://github.com/anomalyco/opencode/releases/tag/v1.18.13)
- linux x64 archive SHA-256: `8d500b20fed2d26e537e221895b1a575476571b4f0089bb29fb13eeb8eb9e937`
- linux arm64 archive SHA-256: `dd4ac8c2167a8338caf296b002c955141d52a2e9c95ee0a95f4ae9939c293ab0`
- Base image: `debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818`

`prepare.sh` downloads and verifies only the native release. Image construction
installs the exact `strace` package, and its context allowlist includes only the
two binaries, entrypoint, and decoy. The final container executes with Docker
`--network none`.
