//go:build windows

package opencodeacp

// handoffOpenFlags adds nothing on Windows, where the handoff form is
// unreachable: a `file:///C:/...` URI keeps its leading slash through
// FromSlash, so the path names no volume and cannot be made relative to the
// read root, and the open is refused before any flag could matter. The form is
// left unreachable rather than half-enabled.
const handoffOpenFlags = 0
