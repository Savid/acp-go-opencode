//go:build linux

package opencode

// generatedTreeHandoffRefusal is why Linux refuses to hand a generated tree to a
// foreign identity: the real ownership walk stops at the first ancestor that
// identity cannot traverse.
const generatedTreeHandoffRefusal = "generated native path ancestry is not traversable by the target identity"
