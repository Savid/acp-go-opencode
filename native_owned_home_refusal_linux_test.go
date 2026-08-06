//go:build linux

package opencodeacp

// nativeOwnedHomeRefusal is why Linux refuses a durable native home owned by
// someone other than the isolated identity: the real ownership walk reaches the
// home and finds the wrong owner.
const nativeOwnedHomeRefusal = "native-owned directory is not owned by the target identity"
