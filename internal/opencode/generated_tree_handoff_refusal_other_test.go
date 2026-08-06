//go:build !linux

package opencode

// generatedTreeHandoffRefusal is why every non-Linux platform refuses to hand a
// generated tree to a foreign identity: there is no ownership transfer to
// perform, so the handoff is refused outright.
const generatedTreeHandoffRefusal = "ownership handoff is unsupported"
